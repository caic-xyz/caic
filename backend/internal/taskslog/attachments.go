// Resolves prompt image payloads by physical task-log record, message, and image indexes.

package taskslog

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// ErrAttachmentNotFound reports that no prompt image exists at the requested log location.
var ErrAttachmentNotFound = errors.New("task log attachment not found")

// FindAttachment reads the image at a physical log record and zero-based message
// and image indexes. Replaying the cursor preserves native parser state and keeps
// addresses identical to history projection in plain and compressed logs.
func FindAttachment(ctx context.Context, path string, resolver WireResolver, recordID, messageIndex, imageIndex uint64) (mediaType string, data []byte, retErr error) {
	c, err := OpenRecordCursor(path, resolver)
	if err != nil {
		return "", nil, fmt.Errorf("open task log: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, c.Close()) }()
	for record, err := range c.Read(ctx) {
		if err != nil {
			return "", nil, err
		}
		if record.ID != recordID {
			continue
		}
		if messageIndex >= uint64(len(record.Messages)) {
			break
		}
		input, ok := record.Messages[messageIndex].Message.(*agent.UserInputMessage)
		if !ok || imageIndex >= uint64(len(input.Images)) {
			break
		}
		image := input.Images[imageIndex]
		data, err := base64.StdEncoding.DecodeString(image.Data)
		if err != nil {
			return "", nil, fmt.Errorf("decode prompt image: %w", err)
		}
		return image.MediaType, data, nil
	}
	return "", nil, ErrAttachmentNotFound
}
