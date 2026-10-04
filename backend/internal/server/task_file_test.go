// Tests runtime artifact retrieval, safe response headers, and file failures.

package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/klauspost/compress/gzip"

	"github.com/caic-xyz/caic/backend/internal/auth"
	"github.com/caic-xyz/caic/backend/internal/runtime"
	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
)

func TestTaskFile(t *testing.T) {
	t.Parallel()
	t.Run("foreign owner", func(t *testing.T) {
		t.Parallel()
		s, _, id := newTaskDiffTestRouter(t)
		entry, _ := s.taskMgr.GetEntry(id)
		entry.Task().OwnerID = "owner"
		w := httptest.NewRecorder()
		ctx := auth.NewContext(testHTTPContext(t), &auth.User{ID: "other"})
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/tasks/"+id.String()+"/file?path=/workspace/repo/notes.md", nil)
		testTaskHandlers(s).routes().ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
	})

	t.Run("HEAD stops before later reads", func(t *testing.T) {
		t.Parallel()
		s, _, id := newTaskDiffTestRouter(t)
		testTaskHandlers(s).taskSvc.runtimes = newTestRuntime(t, &failingFileRuntime{})
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodHead, "/tasks/"+id.String()+"/file?path=/file", nil)
		testTaskHandlers(s).routes().ServeHTTP(w, req)
		if w.Code != 200 || w.Body.Len() != 0 {
			t.Fatalf("HEAD status %d, body %d bytes", w.Code, w.Body.Len())
		}
	})
	t.Run("late read errors abort the download", func(t *testing.T) {
		t.Parallel()
		s, _, id := newTaskDiffTestRouter(t)
		testTaskHandlers(s).taskSvc.runtimes = newTestRuntime(t, &failingFileRuntime{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), httpLoggerKey{}, testTaskHandlers(s).log)
			testTaskHandlers(s).routes().ServeHTTP(w, r.WithContext(ctx))
		}))
		t.Cleanup(server.Close)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/tasks/"+id.String()+"/file?path=/file", http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := resp.Body.Close(); err != nil {
				t.Error(err)
			}
		})
		_, err = io.Copy(io.Discard, resp.Body)
		if err == nil {
			t.Fatal("partial file appeared to complete successfully")
		}
	})

	t.Run("modified files are read again", func(t *testing.T) {
		t.Parallel()
		s, _, id := newTaskDiffTestRouter(t)
		backend := &runtimetest.FakeBackend{Files: map[string][]byte{"/file": []byte("before")}}
		testTaskHandlers(s).taskSvc.runtimes = newTestRuntime(t, backend)
		for _, text := range []string{"before", "after"} {
			backend.Files["/file"] = []byte(text)
			w := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/tasks/"+id.String()+"/file?path=/file", nil)
			req.Header.Set("If-None-Match", "*")
			req.Header.Set("If-Modified-Since", "Wed, 21 Oct 2037 07:28:00 GMT")
			testTaskHandlers(s).routes().ServeHTTP(w, req)
			if w.Code != 200 || w.Body.String() != text {
				t.Fatalf("modified file status %d, content %q", w.Code, w.Body.String())
			}
		}
	})
	t.Run("shrinking range aborts", func(t *testing.T) {
		t.Parallel()
		s, _, id := newTaskDiffTestRouter(t)
		backend := &shrinkingFileRuntime{Files: map[string][]byte{"/file": []byte("0123456789")}}
		testTaskHandlers(s).taskSvc.runtimes = newTestRuntime(t, backend)
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/tasks/"+id.String()+"/file?path=/file", nil)
		req.Header.Set("Range", "bytes=0-9")
		defer func() {
			got := recover()
			err, ok := got.(error)
			if !ok || !errors.Is(err, http.ErrAbortHandler) {
				t.Fatalf("stream abort = %v", got)
			}
		}()
		testTaskHandlers(s).routes().ServeHTTP(w, req)
	})

	t.Run("byte ranges", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			header       string
			ifRange      string
			status       int
			body         string
			contentRange string
		}{
			{"bytes=2-4", "", 206, "234", "bytes 2-4/10"},
			{"bytes=6-", "", 206, "6789", "bytes 6-9/10"},
			{"bytes=-3", "", 206, "789", "bytes 7-9/10"},
			{"bytes=8-99", "", 206, "89", "bytes 8-9/10"},
			{"bytes=10-", "", 416, "", "bytes */10"},
			{"bytes=4-2", "", 416, "", "bytes */10"},
			{"bytes=-0", "", 416, "", "bytes */10"},
			{"bytes=99999999999999999999-", "", 416, "", "bytes */10"},
			{"bytes=0-1,3-4", "", 200, "0123456789", ""},
			{"bytes=2-4", "old-validator", 200, "0123456789", ""},
			{"", "", 200, "0123456789", ""},
		} {
			t.Run(tc.header+tc.ifRange, func(t *testing.T) {
				t.Parallel()
				s, _, id := newTaskDiffTestRouter(t)
				testTaskHandlers(s).taskSvc.runtimes = newTestRuntime(t, &runtimetest.FakeBackend{Files: map[string][]byte{"/file": []byte("0123456789")}})
				w := httptest.NewRecorder()
				req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/tasks/"+id.String()+"/file?path=/file", nil)
				req.Header.Set("Range", tc.header)
				req.Header.Set("Accept-Encoding", "gzip")
				req.Header.Set("If-Range", tc.ifRange)
				req.Header.Set("If-None-Match", "*")
				compressMiddleware(testTaskHandlers(s).routes()).ServeHTTP(w, req)
				if w.Code != tc.status || w.Header().Get("Content-Range") != tc.contentRange {
					t.Fatalf("status %d, range %q: %s", w.Code, w.Header().Get("Content-Range"), w.Body.String())
				}
				if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("ETag") != "" || w.Header().Get("Last-Modified") != "" {
					t.Fatal("mutable file response has cache validators")
				}
				body := w.Body.Bytes()
				if tc.status == 200 {
					if w.Header().Get("Content-Encoding") != "gzip" {
						t.Fatal("full file response was not compressed")
					}
					reader, err := gzip.NewReader(bytes.NewReader(body))
					if err != nil {
						t.Fatal(err)
					}
					body, err = io.ReadAll(reader)
					if err != nil {
						t.Fatal(err)
					}
					if err := reader.Close(); err != nil {
						t.Fatal(err)
					}
				}
				if tc.status != 416 && string(body) != tc.body {
					t.Fatalf("body = %q", body)
				}
				if tc.status == 206 && w.Header().Get("Content-Length") == "" {
					t.Fatal("range response missing length")
				}
				if tc.status == 206 && w.Header().Get("Content-Encoding") != "identity" {
					t.Fatal("file representation was compressed")
				}
			})
		}
	})
	t.Run("ranged image uses the original prefix", func(t *testing.T) {
		t.Parallel()
		s, _, id := newTaskDiffTestRouter(t)
		testTaskHandlers(s).taskSvc.runtimes = newTestRuntime(t, &runtimetest.FakeBackend{Files: map[string][]byte{"/image": []byte("\x89PNG\r\n\x1a\nimage")}})
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/tasks/"+id.String()+"/file?path=/image", nil)
		req.Header.Set("Range", "bytes=8-10")
		testTaskHandlers(s).routes().ServeHTTP(w, req)
		if w.Code != 206 || w.Body.String() != "ima" || w.Header().Get("Content-Type") != "image/png" {
			t.Fatalf("ranged image: %d, %q, %q", w.Code, w.Body.String(), w.Header().Get("Content-Type"))
		}
	})

	for _, tc := range []struct {
		name        string
		path        string
		content     string
		status      int
		disposition string
		contentType string
	}{
		{"AVIF image", "/home/user/screenshot.avif", "\x00\x00\x00\x18ftypavif\x00\x00\x00\x00avifmif1", 200, "inline", "image/avif"},
		{"BMP image", "/home/user/screenshot.bmp", "BM\x00\x00\x00\x00\x00\x00\x00\x00\x36\x00\x00\x00", 200, "inline", "image/bmp"},
		{"image", "/home/user/screenshot.png", "\x89PNG\r\n\x1a\nimage", 200, "inline", "image/png"},
		{"home-relative text", "~/notes.md", "# Home notes\n", 200, "inline", "text/plain; charset=utf-8"},
		{"relative text", "notes.md", "# Notes\n", 200, "inline", "text/plain; charset=utf-8"},
		{"HTML is downloaded", "/home/user/report.html", "<!DOCTYPE html><script>alert(1)</script>", 200, "attachment", "text/html; charset=utf-8"},
		{"missing", "/does-not-exist", "", 404, "", ""},
		{"empty", "", "", 400, "", ""},
		{"invalid", "/home/user/\x00file", "", 400, "", ""},
		{"large streamed file", "/home/user/large", strings.Repeat("x", 17<<20), 200, "inline", "text/plain; charset=utf-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, _, id := newTaskDiffTestRouter(t)
			name := tc.path
			if name == "~/notes.md" {
				name = "/home/user/notes.md"
			}
			if name == "notes.md" {
				name = "/home/user/src/repo/notes.md"
				entry, _ := s.taskMgr.GetEntry(id)
				entry.Task().Repos[0].ContainerPath = "~/src/repo"
			}
			files := map[string][]byte{}
			if tc.content != "" {
				files[name] = []byte(tc.content)
			}
			testTaskHandlers(s).taskSvc.runtimes = newTestRuntime(t, &runtimetest.FakeBackend{Files: files})
			w := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(testHTTPContext(t), http.MethodGet, "/tasks/"+id.String()+"/file?"+url.Values{"path": {tc.path}}.Encode(), nil)
			testTaskHandlers(s).routes().ServeHTTP(w, req)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("file response is cacheable")
			}
			if tc.status != 200 {
				return
			}
			if w.Body.String() != tc.content {
				t.Fatal("file content changed")
			}
			if !strings.HasPrefix(w.Header().Get("Content-Disposition"), tc.disposition+";") {
				t.Fatalf("disposition = %s", w.Header().Get("Content-Disposition"))
			}
			if got := w.Header().Get("Content-Type"); got != tc.contentType {
				t.Fatalf("content type = %q", got)
			}
			if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Security-Policy") != "sandbox; default-src 'none'" {
				t.Fatal("missing safe resource headers")
			}
		})
	}
}

// failingFileRuntime models a source that fails after committing the first chunk.
type failingFileRuntime struct{ runtimetest.FakeBackend }

func (*failingFileRuntime) ReadFile(context.Context, runtime.ID, string, int64, int64) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		if !yield([]byte(strings.Repeat("x", 64<<10)), nil) {
			return
		}
		yield(nil, errors.New("source disappeared"))
	}
}

// shrinkingFileRuntime models a file truncated after the range metadata probe.
type shrinkingFileRuntime struct{ runtimetest.FakeBackend }

func (f *shrinkingFileRuntime) FileSize(ctx context.Context, id runtime.ID, path string) (int64, error) {
	size, err := f.FakeBackend.FileSize(ctx, id, path)
	f.Files[path] = []byte("short")
	return size, err
}
