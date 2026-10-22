// Decodes base64 image data URLs carried by harness prompt records.

package agent

import "strings"

// ParseImageDataURL splits a base64 image data URL into its media type and
// payload.
//
// It reports false for any other URL, including a non-image media type, a
// payload that is not base64, or an empty payload. It does not decode the
// payload: the owning boundary validates image bytes.
func ParseImageDataURL(s string) (mediaType, data string, ok bool) {
	rest, found := strings.CutPrefix(s, "data:")
	if !found {
		return "", "", false
	}
	mediaType, data, found = strings.Cut(rest, ";base64,")
	if !found || !strings.HasPrefix(mediaType, "image/") || mediaType == "image/" || data == "" {
		return "", "", false
	}
	return mediaType, data, true
}
