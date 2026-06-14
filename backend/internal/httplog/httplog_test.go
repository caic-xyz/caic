// Tests for slog HTTP access logging middleware.

package httplog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandler(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		h := Handler{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if _, ok := w.(http.Hijacker); ok {
					t.Error("ResponseWriter unexpectedly implements http.Hijacker")
				}
				_, _ = w.Write([]byte("hello"))
			}),
			Logger: logger,
			Attrs: func(*http.Request) []slog.Attr {
				return []slog.Attr{slog.String("ip", "203.0.113.1")}
			},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ok?ignored=true", http.NoBody)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
		}
		entry := decodeLogEntry(t, &buf)
		if entry["msg"] != "http" {
			t.Fatalf("msg = %v, want http", entry["msg"])
		}
		if entry["level"] != "DEBUG" {
			t.Errorf("level = %v, want DEBUG", entry["level"])
		}
		if entry["m"] != http.MethodGet || entry["p"] != "/ok" || entry["ip"] != "203.0.113.1" {
			t.Errorf("entry route attrs = %#v", entry)
		}
		if entry["s"] != float64(http.StatusOK) || entry["b"] != float64(len("hello")) {
			t.Errorf("entry response attrs = %#v", entry)
		}
	})

	t.Run("valid flush", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		h := Handler{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				flusher, ok := w.(http.Flusher)
				if !ok {
					t.Fatal("ResponseWriter does not implement http.Flusher")
				}
				flusher.Flush()
				_, _ = w.Write([]byte("event: ping\n\n"))
			}),
			Logger: logger,
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/events", http.NoBody)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		entry := decodeLogEntry(t, &buf)
		if entry["s"] != float64(http.StatusOK) || entry["b"] != float64(len("event: ping\n\n")) {
			t.Errorf("entry response attrs = %#v", entry)
		}
	})

	t.Run("valid hijack", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		w := newHijackResponseWriter(t)
		h := Handler{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Fatal("ResponseWriter does not implement http.Hijacker")
				}
				conn, _, err := hj.Hijack()
				if err != nil {
					t.Fatalf("Hijack: %v", err)
				}
				if err := conn.Close(); err != nil {
					t.Errorf("close hijacked conn: %v", err)
				}
			}),
			Logger: logger,
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/ws", http.NoBody)
		h.ServeHTTP(w, req)

		entry := decodeLogEntry(t, &buf)
		if entry["m"] != "HIJACKED" {
			t.Errorf("m = %v, want HIJACKED", entry["m"])
		}
		if entry["hijacked"] != true {
			t.Errorf("hijacked = %v, want true", entry["hijacked"])
		}
	})
}

func decodeLogEntry(t *testing.T, b *bytes.Buffer) map[string]any {
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(b.Bytes()), &entry); err != nil {
		t.Fatalf("decode log entry: %v: %q", err, b.String())
	}
	return entry
}

type hijackResponseWriter struct {
	header http.Header
	conn   net.Conn
}

func (w *hijackResponseWriter) Header() http.Header {
	return w.header
}

func (w *hijackResponseWriter) Write(b []byte) (int, error) {
	return io.Discard.Write(b)
}

func (w *hijackResponseWriter) WriteHeader(int) {}

func (w *hijackResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	brw := bufio.NewReadWriter(bufio.NewReader(w.conn), bufio.NewWriter(w.conn))
	return w.conn, brw, nil
}

func newHijackResponseWriter(t *testing.T) *hijackResponseWriter {
	server, client := net.Pipe()
	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
	})
	return &hijackResponseWriter{
		header: http.Header{},
		conn:   server,
	}
}
