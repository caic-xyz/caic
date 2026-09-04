// Tests for OAuth durable authorization state storage.

package oauthserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestStoreTransactions(t *testing.T) {
	t.Parallel()

	t.Run("version one migrates without losing grants or clients", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "oauth.json")
		now := time.Now().UTC().Truncate(time.Second)
		legacy := storeFile{
			Version: 1,
			Clients: map[string]Client{"client": {ID: "client", Name: "legacy"}},
			Grants:  map[string]Grant{"grant": {ID: "grant", ClientID: "client", ExpiresAt: now.Add(time.Hour)}},
			RefreshTokens: map[string]RefreshToken{
				oauth.RefreshTokenKey("legacy-secret"): {GrantID: "grant", ClientID: "client", ExpiresAt: now.Add(time.Hour)},
			},
		}
		data, err := json.Marshal(legacy)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		var encoded map[string]any
		if err := json.Unmarshal(data, &encoded); err != nil {
			t.Fatalf("Unmarshal legacy fixture: %v", err)
		}
		encoded["deviceCodes"] = map[string]any{"device-digest": map[string]any{"userCode": "ABCDEFGH", "clientID": "client", "status": "pending", "expiresAt": now.Add(time.Hour)}}
		data, err = json.Marshal(encoded)
		if err != nil {
			t.Fatalf("Marshal legacy device fixture: %v", err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		store, err := LoadStore(path)
		if err != nil {
			t.Fatalf("LoadStore: %v", err)
		}
		if store.Clients["client"].Name != "legacy" || store.Grants["grant"].ClientID != "client" {
			t.Fatalf("legacy state was not preserved: clients=%+v grants=%+v", store.Clients, store.Grants)
		}
		if device := store.DeviceCodes["device-digest"]; device == nil || device.UserCode != "" || device.UserCodeKey != oauth.RefreshTokenKey("ABCDEFGH") {
			t.Fatalf("legacy device user code was not migrated to a digest: %+v", device)
		}
		if err := store.transact(func(next *storeFile) bool {
			next.Clients["client-2"] = Client{ID: "client-2"}
			return true
		}); err != nil {
			t.Fatalf("transact: %v", err)
		}
		reloaded, err := LoadStore(path)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if len(reloaded.Clients) != 2 || reloaded.Grants["grant"].ClientID != "client" {
			t.Fatalf("migrated state = clients=%+v grants=%+v", reloaded.Clients, reloaded.Grants)
		}
		onDisk, err := os.ReadFile(path) //nolint:gosec // test-owned path.
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if strings.Contains(string(onDisk), "legacy-secret") || strings.Contains(string(onDisk), "ABCDEFGH") {
			t.Fatal("raw credential was persisted after migration")
		}
		var migrated storeFile
		if err := json.Unmarshal(onDisk, &migrated); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if migrated.Version != storeVersion {
			t.Fatalf("version = %d, want %d", migrated.Version, storeVersion)
		}
	})

	t.Run("failed atomic replacement rolls back every credential mutation", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "oauth.json")
		now := time.Now().UTC()
		store, err := LoadStore(path)
		if err != nil {
			t.Fatalf("LoadStore: %v", err)
		}
		store.Clients["client"] = Client{ID: "client"}
		store.Codes["code-digest"] = Code{ClientID: "client", ExpiresAt: now.Add(time.Hour)}
		store.Grants["grant"] = Grant{ID: "grant", UserID: "user", ClientID: "client", ExpiresAt: now.Add(time.Hour)}
		store.RefreshTokens["refresh-digest"] = RefreshToken{GrantID: "grant", UserID: "user", ClientID: "client", ExpiresAt: now.Add(time.Hour)}
		if err := store.Save(); err != nil {
			t.Fatalf("Save: %v", err)
		}
		store.io = failingRenameStoreIO{storeIO: osStoreIO{}}
		err = store.transact(func(next *storeFile) bool {
			delete(next.Codes, "code-digest")
			delete(next.Clients, "client")
			delete(next.Grants, "grant")
			refresh := next.RefreshTokens["refresh-digest"]
			refresh.UsedAt = now
			next.RefreshTokens["refresh-digest"] = refresh
			return true
		})
		if err == nil {
			t.Fatal("transact succeeded, want replacement failure")
		}
		if _, ok := store.Codes["code-digest"]; !ok {
			t.Fatal("authorization code was consumed in memory after failed transaction")
		}
		if !store.RefreshTokens["refresh-digest"].UsedAt.IsZero() {
			t.Fatal("refresh token was rotated in memory after failed transaction")
		}
		if _, ok := store.Clients["client"]; !ok {
			t.Fatal("client was deleted in memory after failed transaction")
		}
		reloaded, err := LoadStore(path)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if _, ok := reloaded.Codes["code-digest"]; !ok || !reloaded.RefreshTokens["refresh-digest"].UsedAt.IsZero() {
			t.Fatalf("failed transaction changed restart state: %+v", reloaded)
		}
		if _, ok := reloaded.Clients["client"]; !ok {
			t.Fatal("failed transaction deleted client after restart")
		}
	})

	t.Run("post-rename failures install committed state", func(t *testing.T) {
		t.Parallel()
		for _, fault := range []string{"open", "sync", "close"} {
			t.Run(fault, func(t *testing.T) {
				t.Parallel()
				path := filepath.Join(t.TempDir(), "oauth.json")
				store, err := LoadStore(path)
				if err != nil {
					t.Fatalf("LoadStore: %v", err)
				}
				store.io = postRenameStoreIO{storeIO: osStoreIO{}, fault: fault}
				err = store.transact(func(next *storeFile) bool {
					next.Clients["committed"] = Client{ID: "committed"}
					return true
				})
				if err == nil {
					t.Fatal("transact succeeded, want post-rename durability error")
				}
				if _, ok := store.Clients["committed"]; !ok {
					t.Fatal("committed state was not installed in memory")
				}
				reloaded, err := LoadStore(path)
				if err != nil {
					t.Fatalf("reload: %v", err)
				}
				if _, ok := reloaded.Clients["committed"]; !ok {
					t.Fatal("committed state was not visible after restart")
				}
			})
		}
	})

	t.Run("concurrent refresh redemption has one durable winner", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "oauth.json")
		store, err := LoadStore(path)
		if err != nil {
			t.Fatalf("LoadStore: %v", err)
		}
		now := time.Now().UTC()
		store.Clients["client"] = Client{ID: "client", RedirectURIs: []string{"https://client.example/callback"}}
		store.Grants["grant"] = Grant{ID: "grant", UserID: "user", ClientID: "client", ExpiresAt: now.Add(time.Hour)}
		store.RefreshTokens[oauth.RefreshTokenKey("refresh-secret")] = RefreshToken{GrantID: "grant", UserID: "user", ClientID: "client", ExpiresAt: now.Add(time.Hour)}
		if err := store.Save(); err != nil {
			t.Fatalf("Save: %v", err)
		}
		server := &Server{state: store, refreshTokenTTL: time.Hour}
		const attempts = 32
		var wg sync.WaitGroup
		winners := make(chan string, attempts)
		for range attempts {
			wg.Go(func() {
				next, err := randomToken()
				if err != nil {
					winners <- "error: " + err.Error()
					return
				}
				result, _, err := server.exchangeRefreshToken("refresh-secret", "client", "user", next, dpopBinding{})
				if err != nil {
					winners <- "error: " + err.Error()
					return
				}
				if result == refreshExchangeRotated {
					winners <- next
				}
			})
		}
		wg.Wait()
		close(winners)
		got := make([]string, 0, attempts)
		for winner := range winners {
			got = append(got, winner)
		}
		if len(got) != 1 || strings.HasPrefix(got[0], "error: ") {
			t.Fatalf("winners = %v, want one successful rotation", got)
		}
		reloaded, err := LoadStore(path)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if reloaded.RefreshTokens[oauth.RefreshTokenKey("refresh-secret")].UsedAt.IsZero() {
			t.Fatal("original refresh replay record was not durable")
		}
		if _, ok := reloaded.RefreshTokens[oauth.RefreshTokenKey(got[0])]; !ok {
			t.Fatal("winning refresh token was not durable")
		}
	})
}

type failingRenameStoreIO struct {
	storeIO
}

func (failingRenameStoreIO) Rename(string, string) error {
	return errors.New("injected rename failure")
}

type postRenameStoreIO struct {
	storeIO

	fault string
}

func (i postRenameStoreIO) Open(name string) (storeSyncCloser, error) {
	if i.fault == "open" {
		return nil, errors.New("injected open failure")
	}
	dir, err := i.storeIO.Open(name)
	if err != nil {
		return nil, err
	}
	return &faultStoreDir{storeSyncCloser: dir, fault: i.fault}, nil
}

type faultStoreDir struct {
	storeSyncCloser

	fault string
}

func (d *faultStoreDir) Sync() error {
	if d.fault == "sync" {
		return errors.New("injected sync failure")
	}
	return d.storeSyncCloser.Sync()
}

func (d *faultStoreDir) Close() error {
	if d.fault == "close" {
		_ = d.storeSyncCloser.Close()
		return errors.New("injected close failure")
	}
	return d.storeSyncCloser.Close()
}

func BenchmarkBearerGrantCheck(b *testing.B) {
	path := filepath.Join(b.TempDir(), "oauth.json")
	store, err := LoadStore(path)
	if err != nil {
		b.Fatalf("LoadStore: %v", err)
	}
	now := time.Now()
	for i := range 1_000 {
		id := fmt.Sprintf("grant-%04d", i)
		store.Grants[id] = Grant{ID: id, ClientID: "client", ExpiresAt: now.Add(time.Hour)}
	}
	if err := store.Save(); err != nil {
		b.Fatalf("Save: %v", err)
	}
	server := &Server{state: store}

	b.Run("read_only", func(b *testing.B) {
		for b.Loop() {
			active, clientID, err := server.touchGrant("grant-0500", now)
			if err != nil || !active || clientID != "client" {
				b.Fatalf("touchGrant = %t, %q, %v", active, clientID, err)
			}
		}
	})

	b.Run("legacy_full_snapshot", func(b *testing.B) {
		for b.Loop() {
			server.mu.Lock()
			grant := server.state.Grants["grant-0500"]
			grant.LastUsedAt = now
			server.state.Grants[grant.ID] = grant
			err := server.state.Save()
			server.mu.Unlock()
			if err != nil {
				b.Fatalf("Save: %v", err)
			}
		}
	})
}
