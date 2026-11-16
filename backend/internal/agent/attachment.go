// Writes prompt images into the runtime so harnesses without native image input read them by path.

package agent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"path"
	"strings"

	"github.com/caic-xyz/caic/backend/internal/runtime"
	v3 "github.com/caic-xyz/caic/backend/internal/taskslog/data/v3"
)

// AttachmentDir holds prompt images for harnesses that read them by path.
// It is outside every repository, so attachments never dirty a worktree.
const AttachmentDir = RelayDir + "/attachments"

// AttachmentPath returns the content-addressed runtime path of img. The same
// image always maps to the same path, so encoding a prompt needs no upload
// state and replaying a recorded prompt names the file the agent read. The
// media subtype, with jpeg shortened to the usual jpg, is the extension harness
// read tools use to detect the format; the prompt boundary already restricts
// images to png, jpeg, gif, and webp.
func AttachmentPath(img v3.ImageData) (string, error) {
	subtype, ok := strings.CutPrefix(img.MediaType, "image/")
	if !ok || subtype == "" {
		return "", fmt.Errorf("unsupported prompt image media type %q", img.MediaType)
	}
	if subtype == "jpeg" {
		subtype = "jpg"
	}
	sum := sha256.Sum256([]byte(img.Data))
	return AttachmentDir + "/" + hex.EncodeToString(sum[:16]) + "." + subtype, nil
}

// DeployAttachments writes images to their AttachmentPath in the runtime with
// one SSH invocation. Rewriting an existing attachment writes identical bytes.
func DeployAttachments(ctx context.Context, target runtime.ConnectionTarget, images []v3.ImageData) error {
	files := make([]deployFile, 0, len(images))
	for _, img := range images {
		p, err := AttachmentPath(img)
		if err != nil {
			return err
		}
		data, err := base64.StdEncoding.DecodeString(img.Data)
		if err != nil {
			return fmt.Errorf("decode prompt image: %w", err)
		}
		files = append(files, deployFile{name: path.Base(p), mode: 0o600, data: data})
	}
	return deployFiles(ctx, target.SSHHost, AttachmentDir, files)
}

// NewAttachmentConn returns a Conn that writes prompt images into target
// before c sends the prompt that references them by AttachmentPath. A failed
// upload rejects the prompt; the session records it as a failed attempt.
// Wrap c before adding a MessageInterceptor, which must stay outermost.
func NewAttachmentConn(ctx context.Context, c Conn, target runtime.ConnectionTarget) Conn {
	return &attachmentConn{Conn: c, ctx: ctx, target: target}
}

type attachmentConn struct {
	Conn

	ctx    context.Context
	target runtime.ConnectionTarget
}

func (c *attachmentConn) SendPrompt(p Prompt, data []byte) error {
	if len(p.Images) > 0 {
		if err := DeployAttachments(c.ctx, c.target, p.Images); err != nil {
			return fmt.Errorf("deploy prompt images: %w", err)
		}
	}
	return c.Conn.SendPrompt(p, data)
}
