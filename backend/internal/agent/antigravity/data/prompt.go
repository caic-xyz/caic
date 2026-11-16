// Package data defines caic's extension of agy stream-json prompt records.
package data

// InputMessage is one user turn written to agy's stdin.
type InputMessage struct {
	Event   string           `json:"event"`
	Message InputUserMessage `json:"message,omitzero"`
}

// InputUserMessage carries the content blocks of an InputMessage.
type InputUserMessage struct {
	Content []ContentBlock `json:"content,omitzero"`
}

// ContentBlock is an agy text block. agy 1.5.9 accepts only text blocks and
// ignores unknown fields, so CaicImage keeps a prompt image in the recorded
// native input while the model sees only Text, which names the file holding
// that image.
type ContentBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	CaicImage *Image `json:"caic_image,omitzero"`
}

// Image is a base64-encoded prompt image.
type Image struct {
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}
