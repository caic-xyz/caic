// Credential owns the Tailscale API access token and node tags, and verifies administrator changes.

package tailscale

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/md"
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

// DefaultTag is the tag of new tasks' nodes until an administrator saves
// others. It differs from md's own default so a tailnet policy can tell caic's
// agent containers from containers started with md directly.
const DefaultTag = "tag:caic"

// ErrInvalidTags reports node tags that Credential.SaveTags refuses.
var ErrInvalidTags = errors.New("invalid tailscale tags")

// sentinelError matches sentinel and reads as its cause alone.
type sentinelError struct{ sentinel, err error }

func (e sentinelError) Error() string { return e.err.Error() }

func (e sentinelError) Is(target error) bool { return target == e.sentinel }

func (e sentinelError) Unwrap() error { return e.err }

// KeyStore persists what an administrator saves: the token, where an empty
// secret removes it, and the node tags, where none means DefaultTag.
// Credential serializes calls.
type KeyStore interface {
	SaveTailscaleAPIKey(secret string) error
	SaveTailscaleTags(tags []string) error
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
	// Tags are the tags of new tasks' nodes, DefaultTag unless an
	// administrator saved others.
	Tags []string
}

// Credential owns the active Tailscale API access token, the one an
// administrator saved, otherwise the configured one, and the tags md applies
// to the nodes it registers with it.
type Credential struct {
	log    *slog.Logger
	client *http.Client // shared by the tokens' lookups
	store  KeyStore
	config *apiKey // nil when none is configured; Immutable

	mu    sync.Mutex // serializes Save, Remove, and SaveTags
	saved *apiKey    // nil when no administrator-saved token
	tags  []string   // nil means DefaultTag; never mutated in place
}

// NewCredential selects the active token among saved and configured. tags
// are the saved node tags.
func NewCredential(ctx context.Context, log *slog.Logger, store KeyStore, saved, configured string, tags []string) *Credential {
	client := &http.Client{Timeout: 10 * time.Second}
	c := &Credential{
		log:    log,
		client: client,
		store:  store,
		config: newAPIKey(log, client, configured),
		saved:  newAPIKey(log, client, saved),
		tags:   slices.Clone(tags),
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

// Tags returns the node tags of new tasks: the saved ones, otherwise
// DefaultTag. Runtimes pass them to md with APIKey, so md's own default never
// applies.
func (c *Credential) Tags() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.tags) == 0 {
		return []string{DefaultTag}
	}
	return c.tags
}

// SaveTags persists the node tags of new tasks. It trims each tag and drops
// empty and repeated ones; no tag restores DefaultTag. A tag
// Tailscale rejects wraps ErrInvalidTags and leaves the tags unchanged. The
// token's owner must also own each tag, which Tailscale checks only when md
// mints an auth key.
func (c *Credential) SaveTags(ctx context.Context, tags []string) error {
	var next []string
	for _, tag := range tags {
		if tag = strings.TrimSpace(tag); tag != "" && !slices.Contains(next, tag) {
			next = append(next, tag)
		}
	}
	if err := md.ValidateTailscaleTags(next); err != nil {
		return sentinelError{ErrInvalidTags, err}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.store.SaveTailscaleTags(next); err != nil {
		return fmt.Errorf("save tailscale tags: %w", err)
	}
	c.tags = next
	c.log.InfoContext(ctx, "tailscale tags saved", "tags", next)
	return nil
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
	st := Status{Source: src, Err: ErrNoAPIKey, Tags: c.Tags()}
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
		return sentinelError{ErrUnusableKey, errors.New("empty tailscale API access token")}
	}
	if _, err := k.Until(ctx); err != nil {
		return sentinelError{ErrUnusableKey, err}
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
