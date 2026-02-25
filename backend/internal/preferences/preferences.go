// Package preferences manages persistent user preferences with in-memory
// caching and atomic file persistence.
package preferences

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// currentVersion is the preferences file format version.
const currentVersion = 1

// Preferences holds persistent user preferences.
type Preferences struct {
	// Version is the preferences file format version.
	Version int `json:"version"`
	// Repositories is an ordered list of recently used repositories (most
	// recent first), each with optional per-repo overrides.
	Repositories []RepoPrefs `json:"repositories,omitempty"`
	// Harness is the last used agent harness (e.g. "claude", "codex").
	Harness string `json:"harness,omitempty"`
	// Models maps harness name to the last used model for that harness.
	Models map[string]string `json:"models,omitempty"`
	// BaseImage overrides the default container base image. Empty means use
	// the default.
	BaseImage string `json:"baseImage,omitempty"`
}

// RepoPrefs stores per-repository user preferences. Fields override the
// global defaults in Preferences when set.
type RepoPrefs struct {
	// Path is the repository identifier (e.g. "github/caic").
	Path string `json:"path"`
	// BaseBranch overrides the repository's default branch when creating tasks.
	BaseBranch string `json:"baseBranch,omitempty"`
	// Harness is the preferred agent harness for this repo.
	Harness string `json:"harness,omitempty"`
	// Model is the preferred model for this repo's harness.
	Model string `json:"model,omitempty"`
	// BaseImage overrides the default container base image for this repo.
	BaseImage string `json:"baseImage,omitempty"`
}

// Validate checks that the preferences are well-formed.
func (p *Preferences) Validate() error {
	if p.Version != currentVersion {
		return fmt.Errorf("unsupported preferences version %d (want %d)", p.Version, currentVersion)
	}
	seen := make(map[string]struct{}, len(p.Repositories))
	for i, r := range p.Repositories {
		if r.Path == "" {
			return fmt.Errorf("repositories[%d]: empty path", i)
		}
		if _, ok := seen[r.Path]; ok {
			return fmt.Errorf("repositories[%d]: duplicate path %q", i, r.Path)
		}
		seen[r.Path] = struct{}{}
	}
	return nil
}

// TouchRepo moves repo to the front of the MRU list and updates its
// per-repo preferences from the given overrides. Only non-empty override
// fields are applied. If the repo is not yet tracked, it is added.
func (p *Preferences) TouchRepo(repoPath string, overrides *RepoPrefs) {
	idx := -1
	for i, r := range p.Repositories {
		if r.Path == repoPath {
			idx = i
			break
		}
	}
	var r RepoPrefs
	if idx >= 0 {
		r = p.Repositories[idx]
		copy(p.Repositories[1:idx+1], p.Repositories[:idx])
	} else {
		p.Repositories = append(p.Repositories, RepoPrefs{})
		copy(p.Repositories[1:], p.Repositories[:len(p.Repositories)-1])
	}
	r.Path = repoPath
	if overrides.BaseBranch != "" {
		r.BaseBranch = overrides.BaseBranch
	}
	if overrides.Harness != "" {
		r.Harness = overrides.Harness
	}
	if overrides.Model != "" {
		r.Model = overrides.Model
	}
	if overrides.BaseImage != "" {
		r.BaseImage = overrides.BaseImage
	}
	p.Repositories[0] = r

	// Update global defaults.
	if overrides.Harness != "" {
		p.Harness = overrides.Harness
	}
	if overrides.Harness != "" && overrides.Model != "" {
		if p.Models == nil {
			p.Models = make(map[string]string)
		}
		p.Models[overrides.Harness] = overrides.Model
	}
	if overrides.BaseImage != "" {
		p.BaseImage = overrides.BaseImage
	}
}

// Store manages persistent user preferences with in-memory caching.
// All methods are safe for concurrent use.
type Store struct {
	mu     sync.Mutex
	path   string
	cached *Preferences
}

// Open creates a Store backed by the given file path. The file is read once;
// subsequent reads are served from cache. Returns default preferences if the
// file does not exist.
func Open(path string) (*Store, error) {
	p, err := load(path)
	if err != nil {
		return nil, err
	}
	return &Store{path: path, cached: p}, nil
}

// Get returns a deep copy of the current preferences.
func (s *Store) Get() Preferences {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cached.clone()
}

func (p *Preferences) clone() Preferences {
	c := *p
	if len(p.Repositories) > 0 {
		c.Repositories = make([]RepoPrefs, len(p.Repositories))
		copy(c.Repositories, p.Repositories)
	}
	if len(p.Models) > 0 {
		c.Models = make(map[string]string, len(p.Models))
		for k, v := range p.Models {
			c.Models[k] = v
		}
	}
	return c
}

// Update applies fn to the current preferences and persists the result.
func (s *Store) Update(fn func(*Preferences)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s.cached)
	return save(s.cached, s.path)
}

func newPreferences() *Preferences {
	return &Preferences{Version: currentVersion}
}

func load(path string) (*Preferences, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is caller-provided, validated at startup
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return newPreferences(), nil
		}
		return nil, fmt.Errorf("read preferences: %w", err)
	}
	p := &Preferences{}
	if err := json.Unmarshal(data, p); err != nil {
		return nil, fmt.Errorf("parse preferences: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("invalid preferences: %w", err)
	}
	return p, nil
}

func save(prefs *Preferences, path string) error {
	if err := prefs.Validate(); err != nil {
		return fmt.Errorf("save preferences: %w", err)
	}
	data, err := json.MarshalIndent(prefs, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal preferences: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create preferences dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write preferences: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename preferences: %w", err)
	}
	return nil
}
