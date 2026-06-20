// Discovery manifest HTTP handler for Go Mode hosts.

package gomode

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// cacheControl lets clients cache the manifest briefly while still revalidating
// cheaply via the ETag after likely auth or configuration changes.
const cacheControl = "public, max-age=300"

// NewHandler returns a Go Mode service metadata HTTP handler.
func NewHandler(settings *Settings) (http.Handler, error) {
	if settings == nil {
		return nil, errors.New("go mode settings are required")
	}
	// The manifest is static per process: marshal it once and derive a strong
	// ETag from the body so requests serve precomputed bytes and revalidate
	// without re-encoding.
	body, err := json.Marshal(settings)
	if err != nil {
		return nil, fmt.Errorf("marshal go mode settings: %w", err)
	}
	sum := sha256.Sum256(body)
	h := &handler{body: body, etag: fmt.Sprintf("%q", hex.EncodeToString(sum[:16]))}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+settingsPath, h.handleSettings)
	return mux, nil
}

type handler struct {
	body []byte
	etag string
}

func (h *handler) handleSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Set("ETag", h.etag)
	if match := r.Header.Get("If-None-Match"); match == h.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(h.body)
}
