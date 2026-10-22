// Benchmarks deriving conversation and pane width images from one large JPEG.

package server

import (
	"bytes"
	"image/jpeg"
	"strconv"
	"testing"
)

func BenchmarkDownscaleImage(b *testing.B) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, testImage(2752, 1536), &jpeg.Options{Quality: 90}); err != nil {
		b.Fatal(err)
	}
	data := buf.Bytes()
	b.Logf("fixture: %d bytes", len(data))
	for _, width := range []int{attachmentConversationWidth, attachmentPaneWidth} {
		b.Run(strconv.Itoa(width), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				if _, ok := downscaleImage(data, "image/jpeg", width); !ok {
					b.Fatal("downscaleImage reported no resize")
				}
			}
		})
	}
}
