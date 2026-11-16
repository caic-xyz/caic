// Tests prompt image upload paths and that uploads complete before the prompt is sent.

package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/runtime"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

// recordingConn reports whether the attachment existed when the prompt was sent.
type recordingConn struct {
	Conn

	root    string
	sent    []Prompt
	present []bool
}

func (c *recordingConn) SendPrompt(p Prompt, _ []byte) error {
	c.sent = append(c.sent, p)
	for _, img := range p.Images {
		path, err := AttachmentPath(img)
		if err != nil {
			return err
		}
		_, err = os.Stat(filepath.Join(c.root, path))
		c.present = append(c.present, err == nil)
	}
	return nil
}

func TestAttachmentConn(t *testing.T) {
	// The fake ssh runs the remote shell command beneath a temporary root.
	root := t.TempDir()
	bin := t.TempDir()
	script := "#!/bin/sh\nset -eu\nwhile [ \"$1\" = -o ]; do shift 2; done\nshift\ncmd=$(printf '%s' \"$*\" | sed \"s|" + RelayDir + "|$FAKE_ROOT" + RelayDir + "|g\")\nexec /bin/sh -c \"$cmd\"\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o700); err != nil { //nolint:gosec // Private SSH test fixtures must be executable.
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_ROOT", root)
	target := runtime.ConnectionTarget{SSHHost: "task"}

	t.Run("uploads before sending", func(t *testing.T) { //nolint:paralleltest // The parent replaces PATH with the fake ssh.
		inner := &recordingConn{root: root}
		c := NewAttachmentConn(t.Context(), inner, target)
		img := v3.ImageData{MediaType: "image/png", Data: "aW1hZ2U="}
		if err := c.SendPrompt(Prompt{Text: "describe", Images: []v3.ImageData{img}}, nil); err != nil {
			t.Fatal(err)
		}
		if len(inner.present) != 1 || !inner.present[0] {
			t.Fatalf("attachment present at send = %v", inner.present)
		}
		path, err := AttachmentPath(img)
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(root, path)) //nolint:gosec // The path is beneath the test's temporary root.
		if err != nil || !bytes.Equal(got, []byte("image")) {
			t.Fatalf("attachment = %q, %v", got, err)
		}
	})
	t.Run("text only skips upload", func(t *testing.T) {
		t.Setenv("FAKE_ROOT", "/nonexistent")
		inner := &recordingConn{root: root}
		if err := NewAttachmentConn(t.Context(), inner, target).SendPrompt(Prompt{Text: "hi"}, nil); err != nil || len(inner.sent) != 1 {
			t.Fatalf("sent = %v, err = %v", inner.sent, err)
		}
	})
	t.Run("failed upload rejects prompt", func(t *testing.T) { //nolint:paralleltest // The parent replaces PATH with the fake ssh.
		inner := &recordingConn{root: root}
		err := NewAttachmentConn(t.Context(), inner, target).SendPrompt(Prompt{Images: []v3.ImageData{{MediaType: "image/png", Data: "not base64!"}}}, nil)
		if err == nil || !strings.Contains(err.Error(), "decode prompt image") || len(inner.sent) != 0 {
			t.Fatalf("sent = %v, err = %v", inner.sent, err)
		}
	})
}

func TestAttachmentPath(t *testing.T) {
	t.Parallel()
	png, err := AttachmentPath(v3.ImageData{MediaType: "image/png", Data: "aW1hZ2U="})
	if err != nil || !strings.HasPrefix(png, AttachmentDir+"/") || !strings.HasSuffix(png, ".png") {
		t.Fatalf("png path = %q, %v", png, err)
	}
	other, err := AttachmentPath(v3.ImageData{MediaType: "image/png", Data: "b3RoZXI="})
	if err != nil || other == png {
		t.Fatalf("distinct images share path %q, %v", other, err)
	}
	if _, err := AttachmentPath(v3.ImageData{MediaType: "text/plain", Data: "aW1hZ2U="}); err == nil {
		t.Fatal("non-image media type accepted")
	}
}
