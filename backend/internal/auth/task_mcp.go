// Task-scoped MCP credential issuance and verification.

package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
)

// TaskMCPTokenPrefix distinguishes task-scoped MCP credentials from user tokens.
const TaskMCPTokenPrefix = "caic_task."

// TaskMCPTokenIssuer signs opaque task-scoped MCP credentials.
//
// Credentials identify only one task. The MCP server additionally checks the
// task's durable CAIC MCP policy and lifecycle state before admitting a
// request, so a credential cannot outlive a disabled or terminal task.
type TaskMCPTokenIssuer struct{ key []byte }

// NewTaskMCPTokenIssuer creates a task-scoped credential issuer from secret.
func NewTaskMCPTokenIssuer(secret []byte) (*TaskMCPTokenIssuer, error) {
	if len(secret) < 32 {
		return nil, errors.New("task MCP credential secret must be at least 32 bytes")
	}
	return &TaskMCPTokenIssuer{key: append([]byte(nil), secret...)}, nil
}

// Issue creates the credential for taskID.
func (i *TaskMCPTokenIssuer) Issue(taskID string) string {
	mac := hmac.New(sha256.New, i.key)
	_, _ = mac.Write([]byte(taskID))
	return TaskMCPTokenPrefix + taskID + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Verify returns the credential's task ID when token was issued by i.
func (i *TaskMCPTokenIssuer) Verify(token string) (string, bool) {
	if !strings.HasPrefix(token, TaskMCPTokenPrefix) {
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(token, TaskMCPTokenPrefix), ".")
	if len(parts) != 2 || parts[0] == "" {
		return "", false
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, i.key)
	_, _ = mac.Write([]byte(parts[0]))
	if subtle.ConstantTimeCompare(signature, mac.Sum(nil)) != 1 {
		return "", false
	}
	return parts[0], true
}
