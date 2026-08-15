// Package httplog provides slog HTTP access logging middleware.

package httplog

import (
	"bufio"
	"context"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Handler wraps an HTTP handler and logs each request after it completes.
//
// It records the response status, transmitted byte count, duration, and
// hijack state. Successful 2xx responses are logged at debug level; all other
// responses are logged at info level.
type Handler struct {
	http.Handler

	Logger *slog.Logger
	Attrs  func(*http.Request) []slog.Attr
}

// ServeHTTP implements http.Handler.
func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rw := &recordingWriter{ResponseWriter: w}
	ww := http.ResponseWriter(rw)
	if _, ok := w.(http.Hijacker); ok {
		ww = &hijackRecordingWriter{recordingWriter: rw}
	}
	h.Handler.ServeHTTP(ww, r)
	h.log(r.Context(), r, rw, time.Since(start))
}

func (h Handler) log(ctx context.Context, r *http.Request, rw *recordingWriter, d time.Duration) {
	if h.Logger == nil {
		panic("logger is required")
	}
	status := rw.statusCode()
	level := slog.LevelInfo
	if status < http.StatusMultipleChoices {
		level = slog.LevelDebug
	}
	method := r.Method
	attrs := []slog.Attr{
		slog.String("m", method),
		slog.String("p", r.URL.Path),
		slog.Int("s", status),
		slog.Duration("d", roundDuration(d)),
		slog.Int("b", rw.size),
	}
	if rw.hijacked {
		attrs[0] = slog.String("m", "HIJACKED")
		attrs = append(attrs, slog.Bool("hijacked", true))
	}
	if h.Attrs != nil {
		attrs = append(attrs, h.Attrs(r)...)
	}
	h.Logger.LogAttrs(ctx, level, "http", attrs...)
}

// recordingWriter wraps http.ResponseWriter to capture status code and response size.
type recordingWriter struct {
	http.ResponseWriter

	wroteHeader bool
	hijacked    bool
	status      int
	size        int
}

func (rw *recordingWriter) WriteHeader(code int) {
	if rw.wroteHeader {
		return
	}
	rw.wroteHeader = true
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *recordingWriter) Write(b []byte) (int, error) {
	if !rw.wroteHeader {
		rw.WriteHeader(http.StatusOK)
	}
	n, err := rw.ResponseWriter.Write(b)
	rw.size += n
	return n, err
}

// Flush implements http.Flusher so streaming handlers can flush through the wrapper.
func (rw *recordingWriter) Flush() {
	if !rw.wroteHeader {
		rw.wroteHeader = true
		rw.status = http.StatusOK
	}
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the underlying ResponseWriter for http.ResponseController.
func (rw *recordingWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}

func (rw *recordingWriter) statusCode() int {
	if rw.status == 0 {
		return http.StatusOK
	}
	return rw.status
}

type hijackRecordingWriter struct {
	*recordingWriter
}

// Hijack implements http.Hijacker for handlers such as WebSocket upgrades.
func (rw *hijackRecordingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, brw, err := http.NewResponseController(rw.ResponseWriter).Hijack()
	if err == nil {
		rw.hijacked = true
	}
	return conn, brw, err
}

// roundDuration rounds d to 3 significant digits with minimum 1us precision.
func roundDuration(d time.Duration) time.Duration {
	for t := 100 * time.Second; t >= 100*time.Microsecond; t /= 10 {
		if d >= t {
			return d.Round(t / 100)
		}
	}
	return d.Round(time.Microsecond)
}
