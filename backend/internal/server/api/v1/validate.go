// Request validation helpers shared by the DTO Validate methods (excluded from SDK generation).

package v1

import (
	"encoding/base64"
	"fmt"
	"regexp"

	"github.com/caic-xyz/md"

	"github.com/caic-xyz/caic/backend/internal/server/api"
)

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

func validateContainerPlatform(platform Platform) error {
	if platform == PlatformDefault {
		return nil
	}
	if err := md.Platform(platform).Validate(); err != nil {
		return api.BadRequest(fmt.Sprintf("unsupported platform %q; use linux/amd64 or linux/arm64", platform))
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
