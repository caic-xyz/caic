// Request validation methods (excluded from tygo generation).
package v1

import "github.com/caic-xyz/caic/backend/internal/server/dto"

// Validate checks that prompt or images are provided.
func (r *InputReq) Validate() error {
	if r.Prompt.Text == "" && len(r.Prompt.Images) == 0 {
		return dto.BadRequest("prompt or images required")
	}
	return validateImages(r.Prompt.Images)
}

// Validate is a no-op; prompt is optional (read from container plan file if empty).
func (r *RestartReq) Validate() error { return nil }

// Validate checks that the sync target is valid.
func (r SyncReq) Validate() error {
	switch r.Target {
	case "", SyncTargetBranch, SyncTargetDefault:
		return nil
	default:
		return dto.BadRequest("invalid sync target: " + string(r.Target))
	}
}

// Validate checks that prompt, repo, and harness are valid.
func (r *CreateTaskReq) Validate() error {
	if r.InitialPrompt.Text == "" && len(r.InitialPrompt.Images) == 0 {
		return dto.BadRequest("prompt or images required")
	}
	if r.Repo == "" {
		return dto.BadRequest("repo is required")
	}
	if r.Harness == "" {
		return dto.BadRequest("harness is required")
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
