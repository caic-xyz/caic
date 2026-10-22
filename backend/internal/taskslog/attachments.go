// Finds prompt image payloads inside task logs by content hash.

package taskslog

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

// ErrAttachmentNotFound reports that a task log holds no image with the
// requested digest.
var ErrAttachmentNotFound = errors.New("task log attachment not found")

const (
	// imageDataURLPrefix starts every base64 image payload in a task-log
	// record, whether caic wrote it as prompt input or a harness echoed it.
	imageDataURLPrefix = "data:image/"

	// maxAttachmentRecordBytes bounds one scanned record. Task-log writers cap
	// a record below this, so a longer line means a corrupt log.
	maxAttachmentRecordBytes = 40 << 20

	// imageMediaTypeField and imageDataField name the fields of one image in a
	// caic user-input control record.
	imageMediaTypeField = `"media_type":"`
	imageDataField      = `"data":"`

	// imageMimeTypePrefix identifies a harness prompt record that separates the
	// payload from its MIME type, as Pi and OpenCode do.
	imageMimeTypePrefix = `"mimeType":"image/`
	imageMimeTypeField  = `"mimeType":"`
)

// FindAttachment returns the media type and decoded bytes of the prompt image
// in the task log at path whose SHA-256 digest is digest.
//
// The task log is the only durable copy of a prompt image, so this scans it
// instead of reading an index. A settled log is decompressed while it is
// scanned. Records are read as bytes: a base64 payload contains no quote or
// escape, so it ends at the first '"' of the enclosing JSON string.
func FindAttachment(ctx context.Context, path string, digest [sha256.Size]byte) (mediaType string, data []byte, err error) {
	rc, err := openLogReader(path)
	if err != nil {
		return "", nil, fmt.Errorf("open task log: %w", err)
	}
	defer func() { _ = rc.Close() }()
	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 64<<10), maxAttachmentRecordBytes)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return "", nil, err
		}
		if mediaType, data, ok := findImagePayload(scanner.Bytes(), digest); ok {
			return mediaType, data, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", nil, fmt.Errorf("scan task log: %w", err)
	}
	return "", nil, ErrAttachmentNotFound
}

// findImagePayload scans one log record for the image whose decoded bytes hash
// to digest. Raw harness records carry data URLs; caic's own user-input control
// records carry a media type and payload as separate fields.
func findImagePayload(record []byte, digest [sha256.Size]byte) (mediaType string, data []byte, ok bool) {
	if mediaType, data, ok := findDataURLPayload(record, digest); ok {
		return mediaType, data, true
	}
	if mediaType, data, ok := findControlPayload(record, digest); ok {
		return mediaType, data, true
	}
	return findPromptImagePayload(record, digest)
}

// findDataURLPayload scans one record for a base64 image data URL.
func findDataURLPayload(record []byte, digest [sha256.Size]byte) (mediaType string, data []byte, ok bool) {
	for rest := record; ; {
		i := bytes.Index(rest, []byte(imageDataURLPrefix))
		if i < 0 {
			return "", nil, false
		}
		rest = rest[i:]
		end := bytes.IndexByte(rest, '"')
		if end < 0 {
			return "", nil, false
		}
		mediaType, encoded, found := agent.ParseImageDataURL(string(rest[:end]))
		if found {
			if data, matched := decodeImagePayload(encoded, digest); matched {
				return mediaType, data, true
			}
		}
		rest = rest[end:]
	}
}

// findControlPayload scans one user-input control record for an image field
// pair. The scan stays byte-oriented so it never parses unrelated records.
func findControlPayload(record []byte, digest [sha256.Size]byte) (mediaType string, data []byte, ok bool) {
	for rest := record; ; {
		i := bytes.Index(rest, []byte(imageMediaTypeField))
		if i < 0 {
			return "", nil, false
		}
		rest = rest[i+len(imageMediaTypeField):]
		end := bytes.IndexByte(rest, '"')
		if end < 0 {
			return "", nil, false
		}
		mediaType = string(rest[:end])
		rest = rest[end:]
		if !strings.HasPrefix(mediaType, "image/") {
			continue
		}
		j := bytes.Index(rest, []byte(imageDataField))
		if j < 0 {
			return "", nil, false
		}
		rest = rest[j+len(imageDataField):]
		end = bytes.IndexByte(rest, '"')
		if end < 0 {
			return "", nil, false
		}
		if data, matched := decodeImagePayload(string(rest[:end]), digest); matched {
			return mediaType, data, true
		}
		rest = rest[end:]
	}
}

// findPromptImagePayload scans a harness prompt record that stores the payload
// and its MIME type as sibling fields. Both known harnesses write the payload
// first, so the media type follows the matched payload.
func findPromptImagePayload(record []byte, digest [sha256.Size]byte) (mediaType string, data []byte, ok bool) {
	if !bytes.Contains(record, []byte(imageMimeTypePrefix)) {
		return "", nil, false
	}
	for rest := record; ; {
		i := bytes.Index(rest, []byte(imageDataField))
		if i < 0 {
			return "", nil, false
		}
		rest = rest[i+len(imageDataField):]
		end := bytes.IndexByte(rest, '"')
		if end < 0 {
			return "", nil, false
		}
		if data, matched := decodeImagePayload(string(rest[:end]), digest); matched {
			j := bytes.Index(rest[end:], []byte(imageMimeTypeField))
			if j < 0 {
				return "", nil, false
			}
			rest = rest[end+j+len(imageMimeTypeField):]
			end = bytes.IndexByte(rest, '"')
			if end < 0 {
				return "", nil, false
			}
			return string(rest[:end]), data, true
		}
		rest = rest[end:]
	}
}

// decodeImagePayload decodes one base64 payload and reports whether it is the
// requested image.
func decodeImagePayload(encoded string, digest [sha256.Size]byte) ([]byte, bool) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || sha256.Sum256(data) != digest {
		return nil, false
	}
	return data, true
}
