// Package v1 defines version 1 of the durable queued-prompt sidecar schema.
package v1

// Image is an attached base64-encoded image.
type Image struct {
	MediaType string `json:"mediaType"`
	Data      string `json:"data"`
}

// Entry is a prompt waiting for a task's next idle turn.
type Entry struct {
	ID     string  `json:"id"`
	TaskID string  `json:"taskID"`
	Text   string  `json:"text"`
	Images []Image `json:"images,omitempty"`
}
