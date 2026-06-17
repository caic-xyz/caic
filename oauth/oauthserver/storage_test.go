// Tests for OAuth durable authorization state storage.

package oauthserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caic-xyz/caic/oauth"
)

func TestStore(t *testing.T) {
	t.Parallel()
	t.Run("valid save and load", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "oauth.json")
		now := time.Now().UTC().Truncate(time.Second)
		store, err := LoadStore(path)
		if err != nil {
			t.Fatalf("LoadStore: %v", err)
		}
		store.Clients["client-1"] = Client{ID: "client-1", Name: "Claude", RedirectURIs: []string{"https://example.com/callback"}, TokenEndpointAuthMethod: oauth.TokenEndpointAuthNone, CreatedAt: now}
		store.RefreshTokens["refresh-hash"] = RefreshToken{GrantID: "grant-1", UserID: "usr_1", ClientID: "client-1", Resource: "https://caic.example.com/mcp", Scope: "caic:mcp.read", ExpiresAt: now.Add(time.Hour)}
		store.Grants["grant-1"] = Grant{ID: "grant-1", UserID: "usr_1", ClientID: "client-1", ClientName: "Claude", Resource: "https://caic.example.com/mcp", Scope: "caic:mcp.read", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
		if err := store.Save(); err != nil {
			t.Fatalf("Save: %v", err)
		}

		reloaded, err := LoadStore(path)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if reloaded.Path() != path {
			t.Fatalf("Path = %q, want %q", reloaded.Path(), path)
		}
		if got := reloaded.Clients["client-1"].Name; got != "Claude" {
			t.Fatalf("client name = %q", got)
		}
		if got := reloaded.RefreshTokens["refresh-hash"].GrantID; got != "grant-1" {
			t.Fatalf("refresh grant = %q", got)
		}
		if got := reloaded.Grants["grant-1"].ClientName; got != "Claude" {
			t.Fatalf("grant client = %q", got)
		}
	})

	t.Run("valid list grants sorted newest first", func(t *testing.T) {
		t.Parallel()
		store, err := LoadStore("")
		if err != nil {
			t.Fatalf("LoadStore: %v", err)
		}
		now := time.Now().UTC()
		store.Grants["b"] = Grant{ID: "b", UserID: "usr_1", CreatedAt: now.Add(-time.Minute)}
		store.Grants["a"] = Grant{ID: "a", UserID: "usr_1", CreatedAt: now}
		store.Grants["other"] = Grant{ID: "other", UserID: "usr_2", CreatedAt: now.Add(time.Minute)}
		grants := store.ListUserGrants("usr_1")
		if len(grants) != 2 || grants[0].ID != "a" || grants[1].ID != "b" {
			t.Fatalf("grants = %+v", grants)
		}
	})

	t.Run("valid revoke user grant revokes matching refresh tokens", func(t *testing.T) {
		t.Parallel()
		store, err := LoadStore("")
		if err != nil {
			t.Fatalf("LoadStore: %v", err)
		}
		now := time.Now().UTC()
		store.Grants["grant-1"] = Grant{ID: "grant-1", UserID: "usr_1"}
		store.RefreshTokens["token-1"] = RefreshToken{GrantID: "grant-1", UserID: "usr_1"}
		store.RefreshTokens["token-2"] = RefreshToken{GrantID: "grant-2", UserID: "usr_1"}
		if !store.RevokeUserGrant("usr_1", "grant-1", now) {
			t.Fatal("RevokeUserGrant returned false")
		}
		if store.Grants["grant-1"].RevokedAt.IsZero() {
			t.Fatal("grant was not revoked")
		}
		if store.RefreshTokens["token-1"].RevokedAt.IsZero() {
			t.Fatal("matching refresh token was not revoked")
		}
		if !store.RefreshTokens["token-2"].RevokedAt.IsZero() {
			t.Fatal("unrelated refresh token was revoked")
		}
		if store.RevokeUserGrant("usr_2", "grant-1", now) {
			t.Fatal("RevokeUserGrant for wrong user returned true")
		}
	})

	t.Run("valid load prunes expired state", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "oauth.json")
		now := time.Now().UTC()
		file := storeFile{
			Version: storeVersion,
			Clients: map[string]Client{"client-1": {ID: "client-1"}},
			RefreshTokens: map[string]RefreshToken{
				"expired": {GrantID: "expired-grant", ExpiresAt: now.Add(-time.Hour)},
				"active":  {GrantID: "active-grant", ExpiresAt: now.Add(time.Hour)},
			},
			Grants: map[string]Grant{
				"expired-grant": {ID: "expired-grant", ExpiresAt: now.Add(-time.Hour)},
				"active-grant":  {ID: "active-grant", ExpiresAt: now.Add(time.Hour)},
			},
			Codes: map[string]Code{
				"expired-code": {ExpiresAt: now.Add(-time.Hour)},
				"active-code":  {ExpiresAt: now.Add(time.Hour)},
			},
			Consents: map[string]ConsentParams{
				"expired-consent": {ExpiresAt: now.Add(-time.Hour)},
				"active-consent":  {ExpiresAt: now.Add(time.Hour)},
			},
		}
		data, err := json.Marshal(file)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		store, err := LoadStore(path)
		if err != nil {
			t.Fatalf("LoadStore: %v", err)
		}
		if _, ok := store.RefreshTokens["expired"]; ok {
			t.Fatal("expired refresh token was loaded")
		}
		if _, ok := store.Grants["expired-grant"]; ok {
			t.Fatal("expired grant was loaded")
		}
		if _, ok := store.Codes["expired-code"]; ok {
			t.Fatal("expired code was loaded")
		}
		if _, ok := store.Consents["expired-consent"]; ok {
			t.Fatal("expired consent was loaded")
		}
		if _, ok := store.RefreshTokens["active"]; !ok {
			t.Fatal("active refresh token was pruned")
		}
		if _, ok := store.Codes["active-code"]; !ok {
			t.Fatal("active code was pruned")
		}
		if _, ok := store.Consents["active-consent"]; !ok {
			t.Fatal("active consent was pruned")
		}
	})
}
