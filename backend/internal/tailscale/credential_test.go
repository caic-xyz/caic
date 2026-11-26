// Tests token precedence, verified saves, and fallback on removal over an HTTP stub.

package tailscale

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/backend/internal/tailscale/tailscaletest"
	"github.com/caic-xyz/caic/internal/testlog"
)

// newStubCredential answers lookups for tokens whose key ID starts with "ok"
// and rejects the others.
func newStubCredential(t *testing.T, saved, configured string) (*Credential, *tailscaletest.KeyStore) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v2/tailnet/-/keys/ok") {
			http.Error(w, `{"message":"API token invalid"}`, http.StatusUnauthorized)
			return
		}
		expiresIn(24*time.Hour)(w, r)
	}))
	t.Cleanup(server.Close)
	store := &tailscaletest.KeyStore{}
	if err := store.SaveTailscaleAPIKey(saved); err != nil {
		t.Fatal(err)
	}
	c := NewCredential(t.Context(), testlog.New(t), store, saved, configured)
	c.client.Transport = redirectTransport{server.URL}
	return c, store
}

func TestCredential(t *testing.T) {
	t.Parallel()
	t.Run("NewCredential", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name, saved, configured string
			source                  Source
			keyID                   string
		}{
			{"saved overrides config", "tskey-api-okSaved-s", "tskey-api-okConfig-c", SourceSaved, "okSaved"},
			{"config", "", "tskey-api-okConfig-c", SourceConfig, "okConfig"},
			{"none", "", "", SourceNone, ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				c, _ := newStubCredential(t, tc.saved, tc.configured)
				st := c.Status(t.Context())
				if st.Source != tc.source || st.KeyID != tc.keyID {
					t.Fatalf("Status() = %+v, want source %q key %q", st, tc.source, tc.keyID)
				}
				want := tc.saved
				if want == "" {
					want = tc.configured
				}
				if got := c.APIKey(); got != want {
					t.Fatalf("APIKey() = %q, want %q", got, want)
				}
				if tc.source == SourceNone {
					if _, err := c.Until(t.Context()); !errors.Is(err, ErrNoAPIKey) {
						t.Fatalf("Until() error = %v, want ErrNoAPIKey", err)
					}
				} else if st.Err != nil || st.Expiry.IsZero() || st.Until.IsZero() {
					t.Fatalf("Status() = %+v, want an available token with an expiry", st)
				}
			})
		}
	})
	t.Run("Save", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			c, store := newStubCredential(t, "", "tskey-api-okConfig-c")
			if err := c.Save(t.Context(), "tskey-api-okNew-n"); err != nil {
				t.Fatal(err)
			}
			if store.Secret() != "tskey-api-okNew-n" || c.APIKey() != "tskey-api-okNew-n" {
				t.Fatalf("store = %q, APIKey() = %q, want the new token in both", store.Secret(), c.APIKey())
			}
			if st := c.Status(t.Context()); st.Source != SourceSaved || st.KeyID != "okNew" || st.ConfigKeyID != "okConfig" {
				t.Fatalf("Status() = %+v", st)
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			for _, secret := range []string{"", "not-a-token", "tskey-api-revoked-r"} {
				c, store := newStubCredential(t, "", "tskey-api-okConfig-c")
				if err := c.Save(t.Context(), secret); !errors.Is(err, ErrUnusableKey) {
					t.Errorf("Save(%q) error = %v, want ErrUnusableKey", secret, err)
				}
				if store.Secret() != "" || c.APIKey() != "tskey-api-okConfig-c" {
					t.Errorf("Save(%q) changed store = %q, APIKey() = %q", secret, store.Secret(), c.APIKey())
				}
			}
		})
	})
	t.Run("Remove", func(t *testing.T) {
		t.Parallel()
		c, store := newStubCredential(t, "tskey-api-okSaved-s", "tskey-api-okConfig-c")
		if err := c.Remove(t.Context()); err != nil {
			t.Fatal(err)
		}
		if store.Secret() != "" || c.APIKey() != "tskey-api-okConfig-c" {
			t.Fatalf("store = %q, APIKey() = %q, want the config token active", store.Secret(), c.APIKey())
		}
		if st := c.Status(t.Context()); st.Source != SourceConfig || st.KeyID != "okConfig" {
			t.Fatalf("Status() = %+v", st)
		}
	})
}
