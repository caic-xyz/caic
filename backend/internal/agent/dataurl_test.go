// Tests image data URL parsing and the URLs it rejects.

package agent

import "testing"

func TestParseImageDataURL(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name      string
			url       string
			mediaType string
			data      string
		}{
			{"jpeg", "data:image/jpeg;base64,/9j/AA==", "image/jpeg", "/9j/AA=="},
			{"png", "data:image/png;base64,aW1hZ2U=", "image/png", "aW1hZ2U="},
			{"webp", "data:image/webp;base64,UklGRg==", "image/webp", "UklGRg=="},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				mediaType, data, ok := ParseImageDataURL(tc.url)
				if !ok {
					t.Fatalf("ParseImageDataURL(%q) = false, want true", tc.url)
				}
				if mediaType != tc.mediaType || data != tc.data {
					t.Errorf("ParseImageDataURL(%q) = (%q, %q), want (%q, %q)", tc.url, mediaType, data, tc.mediaType, tc.data)
				}
			})
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			url  string
		}{
			{"empty", ""},
			{"plain path", "/home/user/picture.jpg"},
			{"remote url", "https://example.com/picture.jpg"},
			{"text data url", "data:text/plain;base64,aGVsbG8="},
			{"empty media subtype", "data:image/;base64,aW1hZ2U="},
			{"not base64", "data:image/png,aW1hZ2U="},
			{"empty payload", "data:image/png;base64,"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				if mediaType, data, ok := ParseImageDataURL(tc.url); ok {
					t.Errorf("ParseImageDataURL(%q) = (%q, %q, true), want false", tc.url, mediaType, data)
				}
			})
		}
	})
}
