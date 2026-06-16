// OAuth durable authorization state storage.

package oauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const storeVersion = 1

// Client is a dynamically-registered OAuth client.
type Client struct {
	ID                      string    `json:"id"`
	Name                    string    `json:"name"`
	RedirectURIs            []string  `json:"redirectURIs"`
	TokenEndpointAuthMethod string    `json:"tokenEndpointAuthMethod"`
	CreatedAt               time.Time `json:"createdAt"`
}

// Code is an issued authorization code with PKCE binding.
type Code struct {
	UserID        string    `json:"userID"`
	ClientID      string    `json:"clientID"`
	RedirectURI   string    `json:"redirectURI"`
	CodeChallenge string    `json:"codeChallenge"`
	Resource      string    `json:"resource"`
	Scope         string    `json:"scope"`
	ExpiresAt     time.Time `json:"expiresAt"`
}

// ConsentParams holds in-progress OAuth authorization consent state.
type ConsentParams struct {
	UserID    string            `json:"userID"`
	Params    map[string]string `json:"params"`
	ExpiresAt time.Time         `json:"expiresAt"`
}

// RefreshToken is an opaque refresh token persisted by hash.
type RefreshToken struct {
	GrantID   string    `json:"grantID"`
	UserID    string    `json:"userID"`
	ClientID  string    `json:"clientID"`
	Resource  string    `json:"resource"`
	Scope     string    `json:"scope"`
	DPoPJKT   string    `json:"dpopJKT,omitempty"`
	ExpiresAt time.Time `json:"expiresAt"`
	UsedAt    time.Time `json:"usedAt,omitzero"`
	RevokedAt time.Time `json:"revokedAt,omitzero"`
}

// Grant ties a user authorization grant to a client and token.
type Grant struct {
	ID         string    `json:"id"`
	UserID     string    `json:"userID"`
	ClientID   string    `json:"clientID"`
	ClientName string    `json:"clientName"`
	Resource   string    `json:"resource"`
	Scope      string    `json:"scope"`
	CreatedAt  time.Time `json:"createdAt"`
	LastUsedAt time.Time `json:"lastUsedAt,omitzero"`
	ExpiresAt  time.Time `json:"expiresAt"`
	RevokedAt  time.Time `json:"revokedAt,omitzero"`
}

// Store holds durable OAuth clients, refresh tokens, grants, authorization codes, and consents.
type Store struct {
	Clients       map[string]Client        `json:"clients,omitempty"`
	RefreshTokens map[string]RefreshToken  `json:"refreshTokens,omitempty"`
	Grants        map[string]Grant         `json:"grants,omitempty"`
	Codes         map[string]Code          `json:"codes,omitempty"`
	Consents      map[string]ConsentParams `json:"consents,omitempty"`

	path string
}

type storeFile struct {
	Version       int                      `json:"version"`
	Clients       map[string]Client        `json:"clients,omitempty"`
	RefreshTokens map[string]RefreshToken  `json:"refreshTokens,omitempty"`
	Grants        map[string]Grant         `json:"grants,omitempty"`
	Codes         map[string]Code          `json:"codes,omitempty"`
	Consents      map[string]ConsentParams `json:"consents,omitempty"`
}

// LoadStore loads durable OAuth state from path.
func LoadStore(path string) (*Store, error) {
	store := newEmptyStore(path)
	if path == "" {
		return store, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // path is app-controlled persistent state.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return store, nil
		}
		return nil, fmt.Errorf("read oauth state: %w", err)
	}
	var file storeFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse oauth state: %w", err)
	}
	store.Clients = file.Clients
	store.RefreshTokens = file.RefreshTokens
	store.Grants = file.Grants
	store.Codes = file.Codes
	store.Consents = file.Consents
	store.ensureMaps()
	store.pruneExpired(time.Now())
	return store, nil
}

// Save writes the durable OAuth state to its configured path.
func (s *Store) Save() error {
	if s.path == "" {
		return nil
	}
	s.ensureMaps()
	file := storeFile{Version: storeVersion, Clients: s.Clients, RefreshTokens: s.RefreshTokens, Grants: s.Grants, Codes: s.Codes, Consents: s.Consents}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal oauth state: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create oauth state dir: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write oauth state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename oauth state: %w", err)
	}
	return nil
}

// Path returns the durable OAuth state path.
func (s *Store) Path() string {
	return s.path
}

// ListUserGrants returns a user's grants, newest first.
func (s *Store) ListUserGrants(userID string) []Grant {
	grants := make([]Grant, 0, len(s.Grants))
	for id := range s.Grants {
		grant := s.Grants[id]
		if grant.UserID == userID {
			grants = append(grants, grant)
		}
	}
	slices.SortFunc(grants, func(a, b Grant) int {
		if cmp := b.CreatedAt.Compare(a.CreatedAt); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.ID, b.ID)
	})
	return grants
}

// RevokeUserGrant revokes one user's grant and all refresh tokens for it.
func (s *Store) RevokeUserGrant(userID, grantID string, now time.Time) bool {
	grant, ok := s.Grants[grantID]
	if !ok || grant.UserID != userID {
		return false
	}
	if grant.RevokedAt.IsZero() {
		grant.RevokedAt = now
		s.Grants[grantID] = grant
		for tokenHash := range s.RefreshTokens {
			entry := s.RefreshTokens[tokenHash]
			if entry.GrantID == grantID && entry.RevokedAt.IsZero() {
				entry.RevokedAt = now
				s.RefreshTokens[tokenHash] = entry
			}
		}
	}
	return true
}

// PruneExpiredRefreshTokens removes expired refresh tokens.
func (s *Store) PruneExpiredRefreshTokens(now time.Time) bool {
	return s.pruneExpired(now)
}

func newEmptyStore(path string) *Store {
	store := &Store{path: path}
	store.ensureMaps()
	return store
}

func (s *Store) ensureMaps() {
	if s.Clients == nil {
		s.Clients = map[string]Client{}
	}
	if s.RefreshTokens == nil {
		s.RefreshTokens = map[string]RefreshToken{}
	}
	if s.Grants == nil {
		s.Grants = map[string]Grant{}
	}
	if s.Codes == nil {
		s.Codes = map[string]Code{}
	}
	if s.Consents == nil {
		s.Consents = map[string]ConsentParams{}
	}
}

func (s *Store) pruneExpired(now time.Time) bool {
	changed := false
	for token := range s.RefreshTokens {
		if now.After(s.RefreshTokens[token].ExpiresAt) {
			delete(s.RefreshTokens, token)
			changed = true
		}
	}
	for id := range s.Grants {
		if now.After(s.Grants[id].ExpiresAt) {
			delete(s.Grants, id)
			changed = true
		}
	}
	for code := range s.Codes {
		if now.After(s.Codes[code].ExpiresAt) {
			delete(s.Codes, code)
			changed = true
		}
	}
	for consent := range s.Consents {
		if now.After(s.Consents[consent].ExpiresAt) {
			delete(s.Consents, consent)
			changed = true
		}
	}
	return changed
}
