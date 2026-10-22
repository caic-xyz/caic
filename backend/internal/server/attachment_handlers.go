// Serves prompt images stored in task logs as immutable, optionally downscaled resources.

package server

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"net/http"
	"strings"

	"golang.org/x/image/draw"

	"github.com/caic-xyz/caic/backend/internal/server/api"
	"github.com/caic-xyz/caic/backend/internal/taskslog"
)

const (
	// attachmentVariant versions the derived bytes so a decoder or encoder
	// change cannot be served from an immutable cache entry.
	attachmentVariant = "1"

	// attachmentConversationWidth renders a conversation thumbnail; the pane
	// asks for attachmentPaneWidth. Only these widths are accepted: every other
	// value would add an unbounded decode cost to the immutable cache key.
	attachmentConversationWidth = 480
	attachmentPaneWidth         = 1280

	// attachmentCacheControl marks a served image immutable for a year: its bytes
	// derive only from the digest, the width, and attachmentVariant.
	attachmentCacheControl = "private, max-age=31536000, immutable"

	// attachmentMaxPixels bounds a resized decode. A larger image is served
	// unchanged instead of allocating a full-frame decode per request.
	attachmentMaxPixels = 40 << 20
)

// attachmentError maps a lookup failure to its client-facing status.
func attachmentError(err error) error {
	if errors.Is(err, taskslog.ErrAttachmentNotFound) {
		return &api.Error{Status: http.StatusNotFound, Code: api.CodeNotFound, Message: "image not found"}
	}
	return err
}

// etagMatches reports whether an If-None-Match header value names etag.
func etagMatches(header, etag string) bool {
	for token := range strings.SplitSeq(header, ",") {
		if token = strings.TrimSpace(token); token == etag || token == "*" {
			return true
		}
	}
	return false
}

// imageExtension returns the filename extension for a served image type.
func imageExtension(mediaType string) string {
	switch mediaType {
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".jpg"
	}
}

// downscaleImage returns a width-sized encode of a PNG or JPEG image. It
// reports false when the image must be served unchanged: an animated format,
// an orientation this decoder would lose, or a frame larger than the resize
// budget.
func downscaleImage(data []byte, mediaType string, width int) ([]byte, bool) {
	if mediaType != "image/png" && mediaType != "image/jpeg" {
		return nil, false
	}
	if mediaType == "image/jpeg" && jpegOrientation(data) != 1 {
		// The standard decoder ignores EXIF rotation, so a re-encode would show
		// a rotated thumbnail. The browser rotates the original correctly.
		return nil, false
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, false
	}
	if cfg.Width*cfg.Height > attachmentMaxPixels || cfg.Width <= width {
		return nil, false
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, false
	}
	height := max(1, cfg.Height*width/cfg.Width)
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	var buf bytes.Buffer
	if mediaType == "image/png" {
		if err := png.Encode(&buf, dst); err != nil {
			return nil, false
		}
	} else if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 82}); err != nil {
		return nil, false
	}
	return buf.Bytes(), true
}

// jpegOrientation returns the EXIF orientation tag of a JPEG image. It returns
// 1 when the image carries no Exif segment and 0 when the segment or the
// container is unreadable, so the caller can serve the original bytes.
func jpegOrientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return 0
	}
	for i := 2; i+1 < len(data); {
		if data[i] != 0xff {
			return 0
		}
		marker := data[i+1]
		switch {
		case marker == 0xda || marker == 0xd9:
			// Start of scan or end of image: no Exif segment follows.
			return 1
		case marker == 0x01 || (marker >= 0xd0 && marker <= 0xd7):
			i += 2
			continue
		}
		if i+4 > len(data) {
			return 0
		}
		length := int(data[i+2])<<8 | int(data[i+3])
		if length < 2 || i+2+length > len(data) {
			return 0
		}
		segment := data[i+4 : i+2+length]
		if marker == 0xe1 {
			if orientation, ok := exifOrientation(segment); ok {
				return orientation
			}
			return 1
		}
		i += 2 + length
	}
	// The scan ended without reaching the start of scan: undecodable container.
	return 0
}

// exifOrientation reads tag 0x0112 from an APP1 Exif payload.
func exifOrientation(segment []byte) (int, bool) {
	const exifHeader = "Exif\x00\x00"
	if !bytes.HasPrefix(segment, []byte(exifHeader)) {
		return 0, false
	}
	tiff := segment[len(exifHeader):]
	if len(tiff) < 8 {
		return 0, false
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 0, false
	}
	if order.Uint16(tiff[2:4]) != 42 {
		return 0, false
	}
	dir := int(order.Uint32(tiff[4:8]))
	if dir < 8 || dir+2 > len(tiff) {
		return 0, false
	}
	count := int(order.Uint16(tiff[dir : dir+2]))
	for i := range count {
		entry := dir + 2 + i*12
		if entry+12 > len(tiff) {
			return 0, false
		}
		if order.Uint16(tiff[entry:entry+2]) != 0x0112 {
			continue
		}
		orientation := int(order.Uint16(tiff[entry+8 : entry+10]))
		if orientation >= 1 && orientation <= 8 {
			return orientation, true
		}
		return 0, false
	}
	return 1, true
}
