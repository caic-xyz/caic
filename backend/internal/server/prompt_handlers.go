// Streams multipart prompt submissions into agent image payloads.

package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"slices"
	"strconv"

	"github.com/caic-xyz/caic/backend/internal/agent"
	"github.com/caic-xyz/caic/backend/internal/server/api"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/backend/internal/task/taskmgr"
	logv3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

func handlePrompt[In any, PtrIn interface {
	*In
	validatable
}, Out any](fn func(context.Context, PtrIn, []logv3.ImageData) (*Out, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		in, images, ok := readPromptRequest[In, PtrIn](w, r)
		if !ok {
			return
		}
		out, err := fn(r.Context(), in, images)
		writeJSONResponse(r.Context(), w, out, err)
	}
}

func handleTaskPrompt[In any, PtrIn interface {
	*In
	validatable
}, Out any](resolver taskEntryResolver, fn func(context.Context, *taskmgr.Entry, PtrIn, []logv3.ImageData) (*Out, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		entry, err := resolver.getTask(r)
		if err != nil {
			writeError(r.Context(), w, err)
			return
		}
		in, images, ok := readPromptRequest[In, PtrIn](w, r)
		if !ok {
			return
		}
		out, err := fn(r.Context(), entry, in, images)
		if err == nil {
			resolver.notifyTaskChange(entry.Task().ID)
		}
		writeJSONResponse(r.Context(), w, out, err)
	}
}

func readPromptRequest[In any, PtrIn interface {
	*In
	validatable
}](w http.ResponseWriter, r *http.Request) (PtrIn, []logv3.ImageData, bool) {
	in := PtrIn(new(In))
	var images []logv3.ImageData
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType == "multipart/form-data" && err == nil {
		images, err = readPromptMultipart(r, params["boundary"], in)
		if err != nil {
			writeError(r.Context(), w, &api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: err.Error()})
			return in, nil, false
		}
	} else if !readAndDecodeBody(w, r, in) {
		return in, nil, false
	}
	populatePathParams(r, in)
	if err := in.Validate(); err != nil {
		writeError(r.Context(), w, err)
		return in, nil, false
	}
	return in, images, true
}

// readPromptMultipart streams binary parts into the agent's base64 representation.
func readPromptMultipart(r *http.Request, boundary string, input any) (_ []logv3.ImageData, retErr error) {
	source := requestBodyReader{ctx: r.Context(), r: r.Body}
	defer func() { retErr = errors.Join(retErr, source.err, r.Body.Close()) }()
	reader := multipart.NewReader(&source, boundary)
	part, err := reader.NextPart()
	if err != nil {
		return nil, fmt.Errorf("read prompt metadata: %w", err)
	}
	if part.FormName() != "request" || part.FileName() != "" {
		return nil, errors.New("first multipart part must be request JSON")
	}
	decoder := json.NewDecoder(part)
	decoder.DisallowUnknownFields()
	decodeErr := decoder.Decode(input)
	_, drainErr := io.Copy(io.Discard, part)
	if err := errors.Join(decodeErr, drainErr); err != nil {
		return nil, fmt.Errorf("invalid prompt metadata: %w", err)
	}
	var images []logv3.ImageData
	for {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			// Surface late transport errors after the closing multipart boundary.
			_, err = io.Copy(io.Discard, &source)
			if err != nil {
				return nil, err
			}
			return images, r.Context().Err()
		}
		if err != nil {
			return nil, fmt.Errorf("read prompt image: %w", err)
		}
		if part.FormName() != "image-"+strconv.Itoa(len(images)) {
			return nil, errors.New("multipart images must be named image-N in order")
		}
		mediaType := part.Header.Get("Content-Type")
		if !slices.Contains(v1.ImageUploadConstraints().AllowedMediaTypes, mediaType) {
			return nil, errors.New("unsupported image type")
		}
		source := bufio.NewReader(part)
		prefix, err := source.Peek(512)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("read image content: %w", err)
		}
		if len(prefix) == 0 || http.DetectContentType(prefix) != mediaType {
			return nil, errors.New("image content does not match its media type")
		}
		var encoded bytes.Buffer
		encoder := base64.NewEncoder(base64.StdEncoding, &encoded)
		_, err = io.Copy(encoder, source)
		if err := errors.Join(err, encoder.Close()); err != nil {
			return nil, fmt.Errorf("read image payload: %w", err)
		}
		images = append(images, logv3.ImageData{MediaType: mediaType, Data: encoded.String()})
	}
}

func promptToAgent(p v1.Prompt, images []logv3.ImageData) (agent.Prompt, error) {
	if len(p.Images) != len(images) {
		return agent.Prompt{}, &api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "prompt images must reference matching multipart files"}
	}
	for i := range p.Images {
		if p.Images[i].URL != "cid:image-"+strconv.Itoa(i) {
			return agent.Prompt{}, &api.Error{Status: http.StatusBadRequest, Code: api.CodeBadRequest, Message: "image URL must reference its multipart image-N part"}
		}
	}
	return agent.Prompt{Text: p.Text, Images: images}, nil
}
