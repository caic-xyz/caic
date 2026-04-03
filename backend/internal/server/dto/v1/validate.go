// Request validation methods (excluded from tygo generation).
package v1

import (
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/server/dto"
)

// Validate checks that prompt or images are provided.
func (r *InputReq) Validate() error {
	if r.Prompt.Text == "" && len(r.Prompt.Images) == 0 {
		return dto.BadRequest("prompt or images required")
	}
	return validateImages(r.Prompt.Images)
}

// Validate is a no-op; prompt is optional (read from container plan file if empty).
func (r *RestartReq) Validate() error { return nil }

// Validate is a no-op; instructions are optional.
func (r *CompactReq) Validate() error { return nil }

// Validate checks that the sync target is valid.
func (r SyncReq) Validate() error {
	switch r.Target {
	case "", SyncTargetBranch, SyncTargetDefault:
		return nil
	default:
		return dto.BadRequest("invalid sync target: " + string(r.Target))
	}
}

// Validate checks that prompt and harness are valid. Repos is optional (empty
// means no git repository is associated with the task).
func (r *CreateTaskReq) Validate() error {
	if r.InitialPrompt.Text == "" && len(r.InitialPrompt.Images) == 0 {
		return dto.BadRequest("prompt or images required")
	}
	if r.Harness == "" {
		return dto.BadRequest("harness is required")
	}
	seen := make(map[string]struct{}, len(r.Repos))
	for _, rs := range r.Repos {
		if rs.Name == "" {
			return dto.BadRequest("repos contains entry with empty name")
		}
		if _, dup := seen[rs.Name]; dup {
			return dto.BadRequest("repos contains duplicate name: " + rs.Name)
		}
		seen[rs.Name] = struct{}{}
	}
	return validateImages(r.InitialPrompt.Images)
}

// allowedImageTypes is the set of MIME types accepted for image uploads.
var allowedImageTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// pathSegmentRe matches valid path segments: starts with alphanumeric, then alphanumeric, dots, hyphens, or underscores.
var pathSegmentRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// Validate checks that the clone URL is provided and the optional path is safe.
func (r *CloneRepoReq) Validate() error {
	if r.URL == "" {
		return dto.BadRequest("url is required")
	}
	if r.Depth < 0 {
		return dto.BadRequest("depth must be non-negative")
	}
	if r.Path != "" {
		if filepath.IsAbs(r.Path) {
			return dto.BadRequest("path must be relative")
		}
		cleaned := filepath.Clean(r.Path)
		if cleaned != r.Path {
			return dto.BadRequest("path must be clean (use filepath.Clean form)")
		}
		if strings.Contains(cleaned, "..") {
			return dto.BadRequest("path must not contain '..' segments")
		}
		if len(r.Path) > 255 {
			return dto.BadRequest("path too long (max 255 characters)")
		}
		segments := strings.Split(cleaned, string(filepath.Separator))
		if len(segments) > 3 {
			return dto.BadRequest("path too deep (max 3 segments)")
		}
		for _, seg := range segments {
			if !pathSegmentRe.MatchString(seg) {
				return dto.BadRequest("path segment contains invalid characters: " + seg)
			}
		}
	}
	return nil
}

// Validate checks that the URL is non-empty and has an http or https scheme.
func (r *WebFetchReq) Validate() error {
	if r.URL == "" {
		return dto.BadRequest("url is required")
	}
	u, err := url.Parse(r.URL)
	if err != nil {
		return dto.BadRequest("invalid url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return dto.BadRequest("url must have http or https scheme")
	}
	return nil
}

// Validate checks that the repo field is provided.
func (r *BotFixCIReq) Validate() error {
	if r.Repo == "" {
		return dto.BadRequest("repo is required")
	}
	return nil
}

// Validate checks that the taskId field is provided.
func (r *BotFixPRReq) Validate() error {
	if r.TaskID == "" {
		return dto.BadRequest("taskId is required")
	}
	return nil
}

// Validate is a no-op; all settings values are accepted.
func (r *UpdatePreferencesReq) Validate() error { return nil }

// Validate checks that the SDP offer is provided.
func (r *VoiceRTCOfferReq) Validate() error {
	if r.SDP == "" {
		return dto.BadRequest("sdp is required")
	}
	return nil
}

// validateImages checks that each ImageData entry has a valid media type and non-empty data.
func validateImages(images []ImageData) error {
	for _, img := range images {
		if img.MediaType == "" {
			return dto.BadRequest("image mediaType is required")
		}
		if !allowedImageTypes[img.MediaType] {
			return dto.BadRequest("unsupported image mediaType: " + img.MediaType)
		}
		if img.Data == "" {
			return dto.BadRequest("image data is required")
		}
	}
	return nil
}
