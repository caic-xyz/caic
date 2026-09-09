// Tests task-scoped MCP credential issuance and verification.

package auth

import "testing"

func TestNewTaskMCPTokenIssuer(t *testing.T) {
	t.Parallel()
	issuer, err := NewTaskMCPTokenIssuer([]byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		if _, err := NewTaskMCPTokenIssuer([]byte("01234567890123456789012345678901")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("error_short_secret", func(t *testing.T) {
		t.Parallel()
		if _, err := NewTaskMCPTokenIssuer([]byte("short")); err == nil {
			t.Fatal("NewTaskMCPTokenIssuer accepted short secret")
		}
	})
	t.Run("Issue", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			token := issuer.Issue("3TFTB7RMA000")
			if got, ok := issuer.Verify(token); !ok || got != "3TFTB7RMA000" {
				t.Fatalf("Verify() = %q, %v", got, ok)
			}
		})
	})
	token := issuer.Issue("3TFTB7RMA000")
	t.Run("Verify", func(t *testing.T) {
		t.Parallel()
		t.Run("error_tampered_token", func(t *testing.T) {
			t.Parallel()
			if _, ok := issuer.Verify(token + "x"); ok {
				t.Fatal("Verify accepted tampered token")
			}
		})
	})
}
