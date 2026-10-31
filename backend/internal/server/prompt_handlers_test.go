// Tests multipart prompt decoding, failure isolation, and submission allocations.

package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"net/textproto"
	"testing"
	"testing/iotest"

	"github.com/caic-xyz/caic/backend/internal/agent"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	logv3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

type promptPart struct {
	name, mediaType string
	data            []byte
}

func multipartPromptBody(t testing.TB, request any, parts ...promptPart) (data []byte, contentType string) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	p, err := w.CreateFormField("request")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(p).Encode(request); err != nil {
		t.Fatal(err)
	}
	for _, part := range parts {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", `form-data; name="`+part.name+`"; filename="image"`)
		header.Set("Content-Type", part.mediaType)
		p, err := w.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Write(part.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes(), w.FormDataContentType()
}

func TestPromptSubmission(t *testing.T) {
	t.Parallel()
	image := bytes.Repeat([]byte{7}, 512)
	copy(image, []byte{0x89, 'P', 'N', 'G', 13, 10, 26, 10})
	ref := v1.InputImage{URL: "cid:image-0"}
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		for _, text := range []string{"", "look at this"} {
			t.Run(text, func(t *testing.T) {
				t.Parallel()
				body, typ := multipartPromptBody(t, v1.CreateTaskReq{Harness: "claude", InitialPrompt: v1.Prompt{Text: text, Images: []v1.InputImage{ref, {URL: "cid:image-1"}}}},
					promptPart{"image-0", "image/png", image}, promptPart{"image-1", "image/png", image})
				r := httptest.NewRequestWithContext(testHTTPContext(t), "POST", "/", bytes.NewReader(body))
				r.Header.Set("Content-Type", typ)
				var got agent.Prompt
				h := handlePrompt(func(_ context.Context, req *v1.CreateTaskReq, images []logv3.ImageData) (*v1.StatusResp, error) {
					var err error
					got, err = promptToAgent(req.InitialPrompt, images)
					return &v1.StatusResp{Status: "ok"}, err
				})
				w := httptest.NewRecorder()
				h(w, r)
				if w.Code != 200 || got.Text != text || len(got.Images) != 2 {
					t.Fatalf("response %d %s; prompt=%+v", w.Code, w.Body, got)
				}
				for _, img := range got.Images {
					data, err := base64.StdEncoding.DecodeString(img.Data)
					if err != nil || img.MediaType != "image/png" || !bytes.Equal(data, image) {
						t.Fatalf("image bytes changed: %v", err)
					}
				}
			})
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			refs []v1.InputImage
			part promptPart
		}{
			{"type mismatch", []v1.InputImage{ref}, promptPart{"image-0", "image/jpeg", image}},
			{"empty image", []v1.InputImage{ref}, promptPart{"image-0", "image/png", nil}},
			{"unsupported type", []v1.InputImage{ref}, promptPart{"image-0", "text/plain", image}},
			{"wrong part order", []v1.InputImage{ref}, promptPart{"image-1", "image/png", image}},
			{"duplicate request", []v1.InputImage{ref}, promptPart{"request", "image/png", image}},
			{"unreferenced file", nil, promptPart{"image-0", "image/png", image}},
			{"wrong reference", []v1.InputImage{{URL: "cid:image-1"}}, promptPart{"image-0", "image/png", image}},
			{"external reference", []v1.InputImage{{URL: "https://other.test/image.png"}}, promptPart{"image-0", "image/png", image}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				body, typ := multipartPromptBody(t, v1.InputReq{Prompt: v1.Prompt{Text: "test", Images: tc.refs}}, tc.part)
				r := httptest.NewRequestWithContext(testHTTPContext(t), "POST", "/", bytes.NewReader(body))
				r.Header.Set("Content-Type", typ)
				w := httptest.NewRecorder()
				in, images, ok := readPromptRequest[v1.InputReq](w, r)
				if ok {
					if _, err := promptToAgent(in.Prompt, images); err == nil {
						t.Fatal("invalid prompt accepted")
					}
				} else if w.Code != 400 {
					t.Fatalf("status = %d: %s", w.Code, w.Body)
				}
			})
		}
		for _, name := range []string{"truncated", "late read failure", "canceled", "missing file", "unknown metadata field"} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				metadata := map[string]any{"prompt": v1.Prompt{Text: "test", Images: []v1.InputImage{ref}}}
				if name == "unknown metadata field" {
					metadata["unknown"] = true
				}
				body, typ := multipartPromptBody(t, metadata, promptPart{"image-0", "image/png", image})
				if name == "truncated" {
					body = body[:len(body)-10]
				}
				if name == "missing file" {
					body, typ = multipartPromptBody(t, metadata)
				}
				ctx, cancel := context.WithCancel(testHTTPContext(t))
				t.Cleanup(cancel)
				if name == "canceled" {
					cancel()
				}
				r := httptest.NewRequestWithContext(ctx, "POST", "/", bytes.NewReader(body))
				r.Header.Set("Content-Type", typ)
				if name == "late read failure" {
					r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), iotest.ErrReader(errors.New("transport failure"))))
				}
				w := httptest.NewRecorder()
				in, images, ok := readPromptRequest[v1.InputReq](w, r)
				if ok {
					if _, err := promptToAgent(in.Prompt, images); err == nil {
						t.Fatal("invalid request accepted")
					}
				} else if w.Code != 400 {
					t.Fatalf("status = %d: %s", w.Code, w.Body)
				}
			})
		}
	})
}

func BenchmarkInputImageSubmission(b *testing.B) {
	data := make([]byte, 1<<20)
	copy(data, []byte{0x89, 'P', 'N', 'G', 13, 10, 26, 10})
	body, typ := multipartPromptBody(b, v1.CreateTaskReq{Harness: "claude", InitialPrompt: v1.Prompt{Text: "image", Images: []v1.InputImage{{URL: "cid:image-0"}}}}, promptPart{"image-0", "image/png", data})
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		r := httptest.NewRequestWithContext(b.Context(), "POST", "/", bytes.NewReader(body))
		r.Header.Set("Content-Type", typ)
		in, images, ok := readPromptRequest[v1.CreateTaskReq](httptest.NewRecorder(), r)
		if !ok {
			b.Fatal("decode failed")
		}
		if _, err := promptToAgent(in.InitialPrompt, images); err != nil {
			b.Fatal(err)
		}
	}
}
