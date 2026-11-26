// Credential picks the saved or configured Tailscale API access token and verifies administrator changes.

package tailscale

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Source identifies where the active Tailscale API access token comes from.
type Source string

// Token sources, in precedence order.
const (
	SourceSaved  Source = "saved"  // saved by an administrator
	SourceConfig Source = "config" // TAILSCALE_API_KEY from config.toml or the environment
	SourceNone   Source = "none"
)

// ErrUnusableKey reports a token that Credential.Save refuses: malformed,
// rejected, revoked, expiring within ExpiryHeadroom, or not verifiable now.
var ErrUnusableKey = errors.New("unusable tailscale API access token")

// unusableKeyError matches ErrUnusableKey and reads as its cause alone.
type unusableKeyError struct{ err error }

func (e unusableKeyError) Error() string { return e.err.Error() }

func (e unusableKeyError) Is(target error) bool { return target == ErrUnusableKey }

func (e unusableKeyError) Unwrap() error { return e.err }

// KeyStore persists the token an administrator saves; an empty secret removes
// it. Credential serializes calls.
type KeyStore interface {
	SaveTailscaleAPIKey(secret string) error
}

// Status describes the active token without revealing it.
type Status struct {
	Source Source
	KeyID  string    // empty when Source is SourceNone or the token is malformed
	Expiry time.Time // zero when the token never expires or its expiry is unknown
	// Until is when new tasks stop being able to enable Tailscale; zero when
	// Err is set or the token never expires.
	Until time.Time
	Err   error // why Tailscale is unavailable now
	// ConfigKeyID identifies the configured token that removing a saved token
	// falls back to; empty when none is configured.
	ConfigKeyID string
}

// Credential owns the active Tailscale API access token: the one an
// administrator saved, otherwise the configured one.
type Credential struct {
	log    *slog.Logger
	client *http.Client // shared by the tokens' lookups
	store  KeyStore
	config *apiKey // nil when none is configured; Immutable

	mu    sync.Mutex // serializes Save and Remove
	saved *apiKey    // nil when no administrator-saved token
}

// NewCredential selects the active token among saved and configured.
func NewCredential(ctx context.Context, log *slog.Logger, store KeyStore, saved, configured string) *Credential {
	client := &http.Client{Timeout: 10 * time.Second}
	c := &Credential{
		log:    log,
		client: client,
		store:  store,
		config: newAPIKey(log, client, configured),
		saved:  newAPIKey(log, client, saved),
	}
	active, _ := c.activeLocked()
	if active != nil && active.KeyID() == "" {
		_, err := active.Until(ctx)
		log.WarnContext(ctx, "tailscale unavailable", "err", err)
	}
	return c
}

// APIKey returns the active token, empty when none. Runtimes pass it to md on
// each launch, which mints the instance's ephemeral auth key with it; callers
// check Until first.
func (c *Credential) APIKey() string {
	k, _ := c.active()
	if k == nil {
		return ""
	}
	return k.secret
}

// Until reports when new tasks stop being able to enable Tailscale, as
// apiKey.Until does for the active token.
func (c *Credential) Until(ctx context.Context) (time.Time, error) {
	k, _ := c.active()
	if k == nil {
		return time.Time{}, ErrNoAPIKey
	}
	return k.Until(ctx)
}

// Status describes the active token, looking it up as Until does.
func (c *Credential) Status(ctx context.Context) Status {
	k, src := c.active()
	st := Status{Source: src, Err: ErrNoAPIKey}
	if c.config != nil {
		st.ConfigKeyID = c.config.KeyID()
	}
	if k != nil {
		st.KeyID = k.KeyID()
		st.Expiry, st.Until, st.Err = k.check(ctx)
	}
	return st
}

// Save verifies secret with the Tailscale API, persists it, and makes it the
// active token. A secret that fails verification wraps ErrUnusableKey and
// leaves the active token unchanged.
func (c *Credential) Save(ctx context.Context, secret string) error {
	k := newAPIKey(c.log, c.client, secret)
	if k == nil {
		return unusableKeyError{errors.New("empty tailscale API access token")}
	}
	if _, err := k.Until(ctx); err != nil {
		return unusableKeyError{err}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.store.SaveTailscaleAPIKey(secret); err != nil {
		return fmt.Errorf("save tailscale API access token: %w", err)
	}
	c.saved = k
	c.log.InfoContext(ctx, "tailscale API access token saved", "key_id", k.KeyID())
	return nil
}

// Remove deletes the saved token, falling back to the configured one.
func (c *Credential) Remove(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.saved == nil {
		return nil
	}
	if err := c.store.SaveTailscaleAPIKey(""); err != nil {
		return fmt.Errorf("remove tailscale API access token: %w", err)
	}
	c.saved = nil
	c.log.InfoContext(ctx, "tailscale API access token removed")
	return nil
}

func (c *Credential) active() (*apiKey, Source) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeLocked()
}

func (c *Credential) activeLocked() (*apiKey, Source) {
	switch {
	case c.saved != nil:
		return c.saved, SourceSaved
	case c.config != nil:
		return c.config, SourceConfig
	default:
		return nil, SourceNone
	}
}
