// Package tailscale gates Tailscale support on a Tailscale API access token
// that can mint the pre-authorized auth keys runtime instances join with.
package tailscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ExpiryHeadroom is how long before the API access token expires that new
// Tailscale-enabled instances stop being offered. It covers launches queued
// behind host capacity and clock skew between caic and its clients.
const ExpiryHeadroom = time.Hour

// ErrNoAPIKey reports that no Tailscale API access token is configured.
var ErrNoAPIKey = errors.New("tailscale requires TAILSCALE_API_KEY, create an API access token at https://login.tailscale.com/admin/settings/keys")

// APIKey is a Tailscale API access token. Its expiry, read from the Tailscale
// API and refreshed to catch revocation, bounds when new instances may enable
// Tailscale.
type APIKey struct {
	// Secret is the token the runtime passes to the Tailscale API.
	Secret string // Immutable

	log    *slog.Logger
	client *http.Client
	id     string

	mu       sync.Mutex
	state    keyState
	until    time.Time     // availability deadline when state is keyValid; zero when the token never expires
	reason   error         // why state is keyInvalid or keyUnknown
	lookedUp time.Time     // start of the last lookup
	inflight chan struct{} // closed when the running lookup ends; nil when idle
}

// NewAPIKey parses secret. It returns nil when secret is empty. The first
// Until call looks up the token's expiry.
func NewAPIKey(ctx context.Context, log *slog.Logger, secret string) *APIKey {
	if secret == "" {
		return nil
	}
	k := &APIKey{Secret: secret, log: log, client: &http.Client{Timeout: 10 * time.Second}}
	// Access tokens embed their key ID: tskey-api-<id>-<secret>.
	rest, ok := strings.CutPrefix(secret, "tskey-api-")
	id, _, ok2 := strings.Cut(rest, "-")
	if !ok || !ok2 || id == "" {
		k.state = keyInvalid
		k.reason = errors.New("TAILSCALE_API_KEY is not a Tailscale API access token (tskey-api-...)")
		log.WarnContext(ctx, "tailscale unavailable", "err", k.reason)
		return k
	}
	k.id = id
	k.reason = errors.New("tailscale API access token expiry is not known yet")
	return k
}

// Until reports the deadline after which new instances may no longer enable
// Tailscale. A zero deadline means the token never expires. It returns an error
// when Tailscale is unavailable now.
//
// The caller that finds a lookup due performs it. The first lookup and a retry
// after a transient failure (at most once a minute) answer every caller that
// waits on them. A refresh of a valid token (every refreshInterval) detects
// revocation; other callers keep the cached answer meanwhile.
func (k *APIKey) Until(ctx context.Context) (time.Time, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for k.inflight != nil && k.state == keyUnknown {
		done := k.inflight
		k.mu.Unlock()
		select {
		case <-done:
			k.mu.Lock()
		case <-ctx.Done():
			k.mu.Lock()
			return time.Time{}, ctx.Err()
		}
	}
	if k.inflight == nil && k.lookupDueLocked() {
		k.lookupLocked(ctx)
	}
	switch k.state {
	case keyValid:
		if !k.until.IsZero() && !time.Now().Before(k.until) {
			return time.Time{}, fmt.Errorf("tailscale API access token expires at %s, replace TAILSCALE_API_KEY", k.until.Add(ExpiryHeadroom).Format(time.RFC3339))
		}
		return k.until, nil
	default:
		return time.Time{}, k.reason
	}
}

// keysURL is the Tailscale "Get key" endpoint; append the key ID.
const keysURL = "https://api.tailscale.com/api/v2/tailnet/-/keys/"

// retryInterval bounds how often Until retries a lookup that failed
// transiently.
const retryInterval = time.Minute

// refreshInterval bounds how long a revoked token stays offered.
const refreshInterval = 5 * time.Minute

type keyState int

const (
	keyUnknown keyState = iota // the lookup failed transiently
	keyValid
	keyInvalid // the token is malformed, rejected, revoked, or expired
)

// keyResponse is the subset of the Tailscale "Get key" response caic reads.
type keyResponse struct {
	Expires time.Time `json:"expires"`
	Invalid bool      `json:"invalid"`
}

func (k *APIKey) lookupDueLocked() bool {
	switch k.state {
	case keyUnknown:
		return k.lookedUp.IsZero() || time.Since(k.lookedUp) >= retryInterval
	case keyValid:
		return time.Since(k.lookedUp) >= refreshInterval
	default:
		return false
	}
}

// lookupLocked reads the token's expiry without holding k.mu during the
// request. A canceled caller does not abort it, so the outcome reflects the
// token rather than the caller; the client timeout bounds it instead. A
// transient failure keeps a previously valid answer.
func (k *APIKey) lookupLocked(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	done := make(chan struct{})
	k.inflight = done
	k.lookedUp = time.Now()
	k.mu.Unlock()
	resp, err := k.get(ctx)
	k.mu.Lock()
	k.inflight = nil
	close(done)
	switch {
	case errors.Is(err, errRejected):
		k.state = keyInvalid
		k.reason = fmt.Errorf("tailscale API %w, replace TAILSCALE_API_KEY", err)
	case err != nil && k.state == keyValid:
		k.log.WarnContext(ctx, "tailscale API access token refresh failed", "err", err)
		return
	case err != nil:
		k.state = keyUnknown
		k.reason = fmt.Errorf("tailscale API access token expiry is unknown: %w", err)
	case resp.Invalid:
		k.state = keyInvalid
		k.reason = errors.New("tailscale API access token is revoked or expired, replace TAILSCALE_API_KEY")
	default:
		until := time.Time{}
		if !resp.Expires.IsZero() {
			until = resp.Expires.Add(-ExpiryHeadroom)
		}
		if k.state != keyValid || !until.Equal(k.until) {
			k.log.InfoContext(ctx, "tailscale API access token", "expires", resp.Expires)
		}
		k.state = keyValid
		k.reason = nil
		k.until = until
		return
	}
	k.log.WarnContext(ctx, "tailscale unavailable", "err", k.reason)
}

// errRejected reports that the Tailscale API refused the token for good.
var errRejected = errors.New("rejected the access token")

func (k *APIKey) get(ctx context.Context) (*keyResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, keysURL+k.id, http.NoBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+k.Secret)
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return nil, fmt.Errorf("%w (%d: %s)", errRejected, resp.StatusCode, strings.TrimSpace(string(body)))
	default:
		return nil, fmt.Errorf("tailscale API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var r keyResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("decode tailscale key: %w", err)
	}
	return &r, nil
}
