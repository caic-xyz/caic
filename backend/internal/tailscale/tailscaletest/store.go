// Package tailscaletest provides test doubles for the tailscale package.
package tailscaletest

import "sync"

// KeyStore is an in-memory tailscale.KeyStore.
type KeyStore struct {
	mu     sync.Mutex
	secret string
	tags   []string
}

// SaveTailscaleAPIKey implements tailscale.KeyStore.
func (s *KeyStore) SaveTailscaleAPIKey(secret string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.secret = secret
	return nil
}

// SaveTailscaleTags implements tailscale.KeyStore.
func (s *KeyStore) SaveTailscaleTags(tags []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tags = tags
	return nil
}

// Tags returns the last saved tags.
func (s *KeyStore) Tags() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tags
}

// Secret returns the last saved secret.
func (s *KeyStore) Secret() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.secret
}
