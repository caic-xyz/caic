// Tests for provider configurations and userinfo parsing.

package oauthclient

import (
	"net/http"
	"net/url"
	"testing"
)

func TestParseGoogleUser(t *testing.T) {
	t.Parallel()

	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		data := []byte(`{"sub":"108","email":"alice@example.com","email_verified":true,"picture":"https://pic/a.png"}`)
		id, username, avatar, err := ParseGoogleUser(data)
		if err != nil {
			t.Fatalf("ParseGoogleUser: %v", err)
		}
		if id != "108" {
			t.Errorf("providerID = %q, want 108", id)
		}
		if username != "alice@example.com" {
			t.Errorf("username = %q, want alice@example.com", username)
		}
		if avatar != "https://pic/a.png" {
			t.Errorf("avatarURL = %q, want https://pic/a.png", avatar)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		cases := map[string]string{
			"unverified email": `{"sub":"108","email":"alice@example.com","email_verified":false}`,
			"missing sub":      `{"email":"alice@example.com","email_verified":true}`,
			"missing email":    `{"sub":"108","email_verified":true}`,
			"malformed json":   `{`,
		}
		for name, body := range cases {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				if _, _, _, err := ParseGoogleUser([]byte(body)); err == nil {
					t.Fatalf("ParseGoogleUser(%s) = nil error, want error", body)
				}
			})
		}
	})
}

func TestNewGoogleConfig(t *testing.T) {
	t.Parallel()
	cfg := NewGoogleConfig("client-1", "secret-1", func(*http.Request) string { return "https://app/cb" })

	if got := cfg.AuthParams.Get("access_type"); got != "offline" {
		t.Errorf("access_type = %q, want offline", got)
	}
	if got := cfg.AuthParams.Get("prompt"); got != "consent" {
		t.Errorf("prompt = %q, want consent", got)
	}

	authURL := cfg.AuthURL(&http.Request{}, "state-1")
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	q := u.Query()
	if q.Get("access_type") != "offline" {
		t.Errorf("auth url access_type = %q, want offline", q.Get("access_type"))
	}
	if q.Get("scope") != "openid email profile" {
		t.Errorf("auth url scope = %q, want openid email profile", q.Get("scope"))
	}
}
