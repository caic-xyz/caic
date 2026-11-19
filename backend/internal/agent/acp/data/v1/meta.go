// Package v1 defines version 1 of caic's ACP extension metadata.
package v1

// PromptImageMeta is the _meta of a resource_link prompt block that references
// a caic prompt image by path. It keeps the image in the recorded native input;
// ACP agents ignore _meta keys they do not define.
type PromptImageMeta struct {
	Caic PromptImage `json:"caic"`
}

// PromptImage is a base64-encoded prompt image.
type PromptImage struct {
	ImageData string `json:"imageData"`
}
