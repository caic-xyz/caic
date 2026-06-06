// Request validation methods (excluded from SDK generation).

package v1

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/caic-xyz/md"

	"github.com/caic-xyz/caic/backend/internal/server/api"
)

// Validate checks that prompt or images are provided.
func (r *InputReq) Validate() error {
	if r.Prompt.Text == "" && len(r.Prompt.Images) == 0 {
		return api.BadRequest("prompt or images required")
	}
	return validateImages(r.Prompt.Images)
}

// Validate is a no-op; prompt is optional (read from runtime plan file if empty).
func (r *RestartReq) Validate() error { return nil }

// Validate is a no-op; instructions are optional.
func (r *CompactReq) Validate() error { return nil }

// Validate checks that the sync target is valid.
func (r SyncReq) Validate() error {
	switch r.Target {
	case "", SyncTargetBranch, SyncTargetDefault:
		return nil
	default:
		return api.BadRequest("invalid sync target: " + string(r.Target))
	}
}

// Validate checks that prompt and harness are valid. Repos is optional (empty
// means no git repository is associated with the task).
func (r *CreateTaskReq) Validate() error {
	if r.InitialPrompt.Text == "" && len(r.InitialPrompt.Images) == 0 {
		return api.BadRequest("prompt or images required")
	}
	if r.Harness == "" {
		return api.BadRequest("harness is required")
	}
	if err := validateRepoSpecs(r.Repos, "repos"); err != nil {
		return err
	}
	return validateImages(r.InitialPrompt.Images)
}

// allowedImageTypes is the set of MIME types accepted for image uploads.
var allowedImageTypes = map[string]struct{}{
	"image/png":  {},
	"image/jpeg": {},
	"image/gif":  {},
	"image/webp": {},
}

const (
	maxImageBytes       = 10 << 20
	maxPromptImageBytes = 20 << 20
)

// pathSegmentRe matches valid path segments: starts with alphanumeric, then alphanumeric, dots, hyphens, or underscores.
var pathSegmentRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// Validate checks that the clone URL is provided and the optional path is safe.
func (r *CloneRepoReq) Validate() error {
	if r.URL == "" {
		return api.BadRequest("url is required")
	}
	if r.Depth < 0 {
		return api.BadRequest("depth must be non-negative")
	}
	if r.Path != "" {
		if filepath.IsAbs(r.Path) {
			return api.BadRequest("path must be relative")
		}
		cleaned := filepath.Clean(r.Path)
		if cleaned != r.Path {
			return api.BadRequest("path must be clean (use filepath.Clean form)")
		}
		if strings.Contains(cleaned, "..") {
			return api.BadRequest("path must not contain '..' segments")
		}
		if len(r.Path) > 255 {
			return api.BadRequest("path too long (max 255 characters)")
		}
		segments := strings.Split(cleaned, string(filepath.Separator))
		if len(segments) > 3 {
			return api.BadRequest("path too deep (max 3 segments)")
		}
		for _, seg := range segments {
			if !pathSegmentRe.MatchString(seg) {
				return api.BadRequest("path segment contains invalid characters: " + seg)
			}
		}
	}
	return nil
}

// Validate checks that the URL is non-empty and has an http or https scheme.
func (r *WebFetchReq) Validate() error {
	if r.URL == "" {
		return api.BadRequest("url is required")
	}
	u, err := url.Parse(r.URL)
	if err != nil {
		return api.BadRequest("invalid url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return api.BadRequest("url must have http or https scheme")
	}
	return nil
}

// Validate checks that the repo field is provided.
func (r *BotFixCIReq) Validate() error {
	if r.Repo == "" {
		return api.BadRequest("repo is required")
	}
	return nil
}

// Validate checks that the taskId field is provided.
func (r *BotFixPRReq) Validate() error {
	if r.TaskID == "" {
		return api.BadRequest("taskId is required")
	}
	return nil
}

// Validate checks that a prompt is provided, images are valid, and extra repos have no duplicates.
func (r *ForkTaskReq) Validate() error {
	if r.Prompt.Text == "" && len(r.Prompt.Images) == 0 {
		return api.BadRequest("prompt or images required")
	}
	if err := validateRepoSpecs(r.ExtraRepos, "extraRepos"); err != nil {
		return err
	}
	return validateImages(r.Prompt.Images)
}

// UnmarshalJSON decodes UpdatePreferencesReq while preserving strict unknown
// field behavior despite the custom settings-present check.
func (r *UpdatePreferencesReq) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	settings, ok := raw["settings"]
	r.settingsSet = ok
	if ok {
		d := json.NewDecoder(bytes.NewReader(settings))
		d.DisallowUnknownFields()
		if err := d.Decode(&r.Settings); err != nil {
			return err
		}
		delete(raw, "settings")
	}
	if len(raw) > 0 {
		keys := slices.Collect(maps.Keys(raw))
		slices.Sort(keys)
		return fmt.Errorf("json: unknown field %q", keys[0])
	}
	return nil
}

// Validate checks that the complete settings object is present and references valid cache names.
func (r *UpdatePreferencesReq) Validate() error {
	if !r.settingsSet {
		return api.BadRequest("settings is required")
	}
	for name := range r.Settings.WellKnownCaches {
		if _, ok := md.WellKnownCaches[name]; !ok {
			return api.BadRequest("unknown cache: " + name)
		}
	}
	if err := validateContainerPlatform(r.Settings.ContainerPlatform); err != nil {
		return err
	}
	if err := validateCacheMappings(r.Settings.CacheMappings); err != nil {
		return err
	}
	if err := validateMountMappings(r.Settings.CustomMounts); err != nil {
		return err
	}
	return nil
}

func validateContainerPlatform(platform string) error {
	switch platform {
	case "", "linux/amd64", "linux/arm64":
		return nil
	default:
		return api.BadRequest(fmt.Sprintf("unsupported platform %q; use linux/amd64 or linux/arm64", platform))
	}
}

// Validate checks that the signal is SIGTERM or SIGKILL.
func (r *SignalProcessReq) Validate() error {
	if r.PID < 1 {
		return api.BadRequest("invalid pid")
	}
	switch r.Signal {
	case "SIGTERM", "SIGKILL":
		return nil
	default:
		return api.BadRequest("signal must be SIGTERM or SIGKILL")
	}
}

// Validate checks that the SDP offer is provided.
func (r *VoiceRTCOfferReq) Validate() error {
	if r.SDP == "" {
		return api.BadRequest("sdp is required")
	}
	return nil
}

// validateRepoSpecs checks that each RepoSpec has a non-empty name and no duplicates.
func validateRepoSpecs(specs []RepoSpec, field string) error {
	seen := make(map[string]struct{}, len(specs))
	for _, rs := range specs {
		if rs.Name == "" {
			return api.BadRequest(field + " contains entry with empty name")
		}
		if _, dup := seen[rs.Name]; dup {
			return api.BadRequest(field + " contains duplicate name: " + rs.Name)
		}
		seen[rs.Name] = struct{}{}
	}
	return nil
}

func validateCacheMappings(mappings []CacheMappingResp) error {
	for i, m := range mappings {
		if m.HostPath == "" {
			return api.BadRequest(fmt.Sprintf("cacheMappings[%d]: hostPath is required", i))
		}
		if m.ContainerPath == "" {
			return api.BadRequest(fmt.Sprintf("cacheMappings[%d]: containerPath is required", i))
		}
	}
	return nil
}

func validateMountMappings(mappings []MountMappingResp) error {
	for i, m := range mappings {
		if m.HostPath == "" {
			return api.BadRequest(fmt.Sprintf("customMounts[%d]: hostPath is required", i))
		}
		if m.ContainerPath == "" {
			return api.BadRequest(fmt.Sprintf("customMounts[%d]: containerPath is required", i))
		}
	}
	return nil
}

// validateImages checks that each ImageData entry has a valid media type,
// valid base64 payload, and bounded decoded size.
func validateImages(images []ImageData) error {
	var total int
	for _, img := range images {
		if img.MediaType == "" {
			return api.BadRequest("image mediaType is required")
		}
		if _, ok := allowedImageTypes[img.MediaType]; !ok {
			return api.BadRequest("unsupported image mediaType: " + img.MediaType)
		}
		if img.Data == "" {
			return api.BadRequest("image data is required")
		}
		if base64.StdEncoding.DecodedLen(len(img.Data)) > maxImageBytes {
			return api.BadRequest("image data too large")
		}
		total += base64.StdEncoding.DecodedLen(len(img.Data))
		if total > maxPromptImageBytes {
			return api.BadRequest("image data total too large")
		}
	}
	return nil
}
