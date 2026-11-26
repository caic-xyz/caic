// Tests Tailscale API access token expiry lookups over an HTTP stub.

package tailscale

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tailscaledata "github.com/caic-xyz/caic/backend/internal/tailscale/data"
	"github.com/caic-xyz/caic/internal/testlog"
)

type redirectTransport struct{ url string }

func (t redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = "http"
	req.URL.Host = t.url[len("http://"):]
	req.Host = req.URL.Host
	return http.DefaultTransport.RoundTrip(req)
}

// stubAPIKey routes the token's lookups to handler and counts them.
func stubAPIKey(t *testing.T, secret string, handler http.HandlerFunc) (*apiKey, *atomic.Int32) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	k := newAPIKey(testlog.New(t), &http.Client{Transport: redirectTransport{server.URL}}, secret)
	return k, &calls
}

// writeKey answers a lookup with key, as an API access token with ID kID.
func writeKey(w http.ResponseWriter, key *tailscaledata.Key) {
	key.ID = "kID"
	key.KeyType = "api"
	if err := json.NewEncoder(w).Encode(key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func expiresIn(d time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeKey(w, &tailscaledata.Key{Expires: time.Now().Add(d).Truncate(time.Second)})
	}
}

func TestNewAPIKey(t *testing.T) {
	t.Parallel()
	if k := newAPIKey(testlog.New(t), http.DefaultClient, ""); k != nil {
		t.Fatalf("newAPIKey(\"\") = %v, want nil", k)
	}
}

func TestAPIKey(t *testing.T) {
	t.Parallel()
	t.Run("Until", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			t.Run("expiry minus headroom", func(t *testing.T) {
				t.Parallel()
				k, calls := stubAPIKey(t, "tskey-api-kID-secret", func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/api/v2/tailnet/-/keys/kID" {
						t.Errorf("path = %q, want the key ID lookup", r.URL.Path)
					}
					if got := r.Header.Get("Authorization"); got != "Bearer tskey-api-kID-secret" {
						t.Errorf("Authorization = %q", got)
					}
					writeKey(w, &tailscaledata.Key{Expires: time.Date(2099, 1, 2, 3, 4, 5, 0, time.UTC)})
				})
				for range 2 {
					until, err := k.Until(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					if want := time.Date(2099, 1, 2, 2, 4, 5, 0, time.UTC); !until.Equal(want) {
						t.Fatalf("Until() = %v, want %v", until, want)
					}
				}
				if n := calls.Load(); n != 1 {
					t.Fatalf("lookups = %d, want 1", n)
				}
			})
			t.Run("concurrent callers share the first lookup", func(t *testing.T) {
				t.Parallel()
				release := make(chan struct{})
				started := make(chan struct{})
				k, calls := stubAPIKey(t, "tskey-api-kID-secret", func(w http.ResponseWriter, r *http.Request) {
					close(started)
					<-release
					expiresIn(24*time.Hour)(w, r)
				})
				var wg sync.WaitGroup
				errs := make(chan error, 2)
				wg.Go(func() {
					_, err := k.Until(t.Context())
					errs <- err
				})
				<-started
				wg.Go(func() {
					_, err := k.Until(t.Context())
					errs <- err
				})
				close(release)
				wg.Wait()
				close(errs)
				for err := range errs {
					if err != nil {
						t.Fatal(err)
					}
				}
				if n := calls.Load(); n != 1 {
					t.Fatalf("lookups = %d, want 1", n)
				}
			})
			t.Run("no expiry", func(t *testing.T) {
				t.Parallel()
				k, _ := stubAPIKey(t, "tskey-api-kID-secret", func(w http.ResponseWriter, _ *http.Request) {
					writeKey(w, &tailscaledata.Key{})
				})
				until, err := k.Until(t.Context())
				if err != nil || !until.IsZero() {
					t.Fatalf("Until() = %v, %v, want zero, nil", until, err)
				}
			})
		})
		t.Run("refresh", func(t *testing.T) {
			t.Parallel()
			for _, tc := range []struct {
				name      string
				status    int32 // HTTP status the stub answers after the first lookup
				available bool
			}{
				{"transient failure keeps the token", http.StatusServiceUnavailable, true},
				{"revocation withdraws the token", http.StatusUnauthorized, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					var status atomic.Int32
					status.Store(http.StatusOK)
					k, calls := stubAPIKey(t, "tskey-api-kID-secret", func(w http.ResponseWriter, r *http.Request) {
						if s := int(status.Load()); s != http.StatusOK {
							http.Error(w, "refused", s)
							return
						}
						expiresIn(24*time.Hour)(w, r)
					})
					if _, err := k.Until(t.Context()); err != nil {
						t.Fatal(err)
					}
					status.Store(tc.status)
					if _, err := k.Until(t.Context()); err != nil {
						t.Fatalf("Until() before the refresh interval = %v", err)
					}
					if n := calls.Load(); n != 1 {
						t.Fatalf("lookups = %d, want 1", n)
					}
					k.mu.Lock()
					k.lookedUp = k.lookedUp.Add(-refreshInterval)
					k.mu.Unlock()
					if _, err := k.Until(t.Context()); (err == nil) != tc.available {
						t.Fatalf("Until() after the refresh interval = %v, want available=%v", err, tc.available)
					}
					if n := calls.Load(); n != 2 {
						t.Fatalf("lookups = %d, want 2", n)
					}
				})
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			cases := []struct {
				name    string
				handler http.HandlerFunc
			}{
				{"within headroom", expiresIn(ExpiryHeadroom - time.Minute)},
				{"expired", expiresIn(-time.Hour)},
				{"invalid", func(w http.ResponseWriter, _ *http.Request) {
					writeKey(w, &tailscaledata.Key{Invalid: true})
				}},
				{"unauthorized", func(w http.ResponseWriter, _ *http.Request) {
					http.Error(w, `{"message":"API token invalid"}`, http.StatusUnauthorized)
				}},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					t.Parallel()
					k, calls := stubAPIKey(t, "tskey-api-kID-secret", tc.handler)
					for range 2 {
						if until, err := k.Until(t.Context()); err == nil {
							t.Fatalf("Until() = %v, nil, want error", until)
						}
					}
					if n := calls.Load(); n != 1 {
						t.Fatalf("lookups = %d, want 1", n)
					}
				})
			}
			t.Run("malformed token", func(t *testing.T) {
				t.Parallel()
				for _, secret := range []string{"tskey-auth-kID-secret", "tskey-api-", "tskey-api-kID"} {
					k, calls := stubAPIKey(t, secret, expiresIn(24*time.Hour))
					if _, err := k.Until(t.Context()); err == nil {
						t.Errorf("Until() for %q succeeded, want error", secret)
					}
					if n := calls.Load(); n != 0 {
						t.Errorf("lookups for %q = %d, want 0", secret, n)
					}
				}
			})
			t.Run("transient failure retries later", func(t *testing.T) {
				t.Parallel()
				var fail atomic.Bool
				fail.Store(true)
				k, calls := stubAPIKey(t, "tskey-api-kID-secret", func(w http.ResponseWriter, r *http.Request) {
					if fail.Load() {
						http.Error(w, "unavailable", http.StatusServiceUnavailable)
						return
					}
					expiresIn(24*time.Hour)(w, r)
				})
				_, err := k.Until(t.Context())
				if err == nil || errors.Is(err, errRejected) {
					t.Fatalf("Until() error = %v, want transient error", err)
				}
				fail.Store(false)
				if _, err := k.Until(t.Context()); err == nil {
					t.Fatal("Until() succeeded before the retry interval elapsed")
				}
				if n := calls.Load(); n != 1 {
					t.Fatalf("lookups = %d, want 1", n)
				}
				k.mu.Lock()
				k.lookedUp = k.lookedUp.Add(-retryInterval)
				k.mu.Unlock()
				if _, err := k.Until(t.Context()); err != nil {
					t.Fatalf("Until() after the retry interval = %v", err)
				}
			})
		})
	})
}
