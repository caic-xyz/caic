// Builds immutable prompt image descriptors with shared thumbnail dimension limits.

package apiconv

import (
	"fmt"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

const (
	// QueuedImageMaxDimension is the compact pending-prompt thumbnail dimension limit.
	QueuedImageMaxDimension = 96
	// ConversationImageMaxDimension is the conversation preview dimension limit.
	ConversationImageMaxDimension = 480
	// PaneImageMaxDimension bounds the document pane's initial image decode.
	PaneImageMaxDimension = 1280
)

// InputImage describes an original resource and its immutable thumbnail and inline preview variants.
func InputImage(url string) v1.InputImage {
	return v1.InputImage{URL: url, ThumbnailURL: fmt.Sprintf("%s?max=%d", url, QueuedImageMaxDimension), PreviewURL: fmt.Sprintf("%s?max=%d", url, ConversationImageMaxDimension)}
}
