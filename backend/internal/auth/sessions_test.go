// Tests persistent tracked-session binding, individual revocation, expiry, and issuance failures.

package auth

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreSessions(t *testing.T) {
	t.Parallel()
	t.Run("independent cookie and bearer revocation survives restart", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "users.json")
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		user, err := store.UpsertUser(&User{Provider: ProviderGitHub, ProviderID: "1", Username: "alice"})
		if err != nil {
			t.Fatal(err)
		}
		secret := []byte("0123456789abcdef0123456789abcdef")
		first, _, err := store.IssueToken(&user, secret, time.Hour, SessionInfo{Source: "provider"})
		if err != nil {
			t.Fatal(err)
		}
		second, _, err := store.IssueToken(&user, secret, time.Hour, SessionInfo{Source: "device"})
		if err != nil {
			t.Fatal(err)
		}
		if first == second {
			t.Fatal("independent sessions share credentials")
		}
		persisted, err := os.ReadFile(path + ".sessions") //nolint:gosec // Test-owned temporary path.
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(persisted), first) || strings.Contains(string(persisted), second) {
			t.Fatal("persisted raw credential")
		}
		sessions := store.Sessions(user.ID)
		if len(sessions) != 2 {
			t.Fatalf("sessions=%v", sessions)
		}
		firstClaims, err := validateToken(first, secret)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RevokeSession("other-user", firstClaims.SessionID); !os.IsNotExist(err) {
			t.Fatalf("cross-user revoke=%v", err)
		}
		if err := store.RevokeSession(user.ID, firstClaims.SessionID); err != nil {
			t.Fatal(err)
		}
		store, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		h := Middleware(store, secret, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := UserFromContext(r.Context()); ok {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusUnauthorized)
			}
		}))
		for _, transport := range []string{"cookie", "bearer"} {
			t.Run(transport, func(t *testing.T) {
				t.Parallel()
				for _, tc := range []struct {
					token  string
					status int
				}{{first, 401}, {second, 200}, {"invalid", 401}} {
					req := httptest.NewRequestWithContext(t.Context(), "GET", "http://localhost/", http.NoBody)
					if transport == "cookie" {
						req.AddCookie(&http.Cookie{Name: "caic_session", Value: tc.token, Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode})
					} else {
						req.Header.Set("Authorization", "Bearer "+tc.token)
					}
					w := httptest.NewRecorder()
					h.ServeHTTP(w, req)
					if w.Code != tc.status {
						t.Fatalf("status=%d want %d", w.Code, tc.status)
					}
				}
			})
		}
		if len(store.Sessions(user.ID)) != 1 {
			t.Fatal("revocation did not persist")
		}
	})
	t.Run("expired missing and mismatched sessions fail closed", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "users.json")
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		user, err := store.UpsertUser(&User{Provider: ProviderGitHub, ProviderID: "1"})
		if err != nil {
			t.Fatal(err)
		}
		secret := []byte("0123456789abcdef0123456789abcdef")
		token, _, err := store.IssueToken(&user, secret, time.Hour, SessionInfo{})
		if err != nil {
			t.Fatal(err)
		}
		c, err := validateToken(token, secret)
		if err != nil {
			t.Fatal(err)
		}
		c.UserID = "someone-else"
		if _, valid := store.validateSession(c, token); valid {
			t.Fatal("wrong-user binding accepted")
		}
		c.UserID = user.ID
		if _, valid := store.validateSession(c, token+"x"); valid {
			t.Fatal("wrong-token binding accepted")
		}
		expired, _, err := store.IssueToken(&user, secret, -time.Hour, SessionInfo{})
		if err != nil {
			t.Fatal(err)
		}
		store, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := validateToken(expired, secret); err == nil {
			t.Fatal("expired JWT accepted")
		}
		if len(store.Sessions(user.ID)) != 1 {
			t.Fatal("expired session listed")
		}
		raw, _ := json.Marshal(jwtPayload{UserID: user.ID, IssuedAt: time.Now().Unix(), Expiry: time.Now().Add(time.Hour).Unix()})
		input := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(raw)
		if _, err := validateToken(input+"."+hmacSHA256(secret, input), secret); err == nil {
			t.Fatal("untracked token accepted")
		}
	})
	t.Run("persistence failure never returns a credential or publishes a session", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "users.json")
		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		user, err := store.UpsertUser(&User{Provider: ProviderGitHub, ProviderID: "1"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path+".sessions", 0o700); err != nil {
			t.Fatal(err)
		}
		token, _, err := store.IssueToken(&user, []byte("0123456789abcdef0123456789abcdef"), time.Hour, SessionInfo{})
		if err == nil || token != "" || len(store.Sessions(user.ID)) != 0 {
			t.Fatal("failed persistence published a session")
		}
	})
}
