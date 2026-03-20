// Package preferences manages persistent user preferences with in-memory
// caching and atomic file persistence. All users' preferences are stored in a
// single JSON file keyed by user ID ("default" for unauthenticated access).
package preferences

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// CacheMapping maps a host directory to a container path for cache/state sharing.
type CacheMapping struct {
	// HostPath is the path on the host filesystem to mount.
	HostPath string `json:"hostPath"`
	// ContainerPath is the path inside the container where hostPath will be mounted.
	ContainerPath string `json:"containerPath"`
}

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
	// Settings holds user-configurable behavioral settings.
	Settings Settings `json:"settings,omitempty"`
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
	for i, m := range p.Settings.CacheMappings {
		if m.HostPath == "" {
			return fmt.Errorf("cacheMappings[%d]: empty hostPath", i)
		}
		if m.ContainerPath == "" {
			return fmt.Errorf("cacheMappings[%d]: empty containerPath", i)
		}
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
	r.LastUsed = time.Now().Unix()
	if overrides.BaseBranch != "" {
		r.BaseBranch = overrides.BaseBranch
	}
	if overrides.Harness != "" {
		r.Harness = overrides.Harness
	}
	if overrides.Model != "" {
		r.Model = overrides.Model
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
}

// RecentRepos returns the subset of Repositories that should appear in the
// "Recent" section: the first minRecentRepos entries plus any beyond that
// used within recentWindow.
func (p *Preferences) RecentRepos(now time.Time) []RepoPrefs {
	cutoff := now.Add(-recentWindow).Unix()
	result := make([]RepoPrefs, 0, len(p.Repositories))
	for i, r := range p.Repositories {
		if i < minRecentRepos || r.LastUsed >= cutoff {
			result = append(result, r)
		}
	}
	return result
}

func (p *Preferences) clone() Preferences {
	c := *p
	c.Repositories = slices.Clone(p.Repositories)
	c.Models = maps.Clone(p.Models)
	c.Settings.CacheMappings = slices.Clone(p.Settings.CacheMappings)
	c.Settings.WellKnownCaches = maps.Clone(p.Settings.WellKnownCaches)
	return c
}

// Settings holds user-configurable behavioral settings.
type Settings struct {
	// AutoFixOnCIFailure automatically starts a new task to fix CI when a
	// task's PR CI fails and the original task can no longer receive input.
	AutoFixOnCIFailure bool `json:"autoFixOnCIFailure,omitempty"`
	// AutoFixOnPROpen automatically creates a task to review and fix a pull
	// request when it is opened or reopened via a forge webhook.
	AutoFixOnPROpen bool `json:"autoFixOnPROpen,omitempty"`
	// BaseImage overrides the default container base image. Empty means use
	// the default.
	BaseImage string `json:"baseImage,omitempty"`
	// UseDefaultCaches controls whether default harness caches are mounted.
	// When false, only custom CacheMappings are used.
	UseDefaultCaches bool `json:"useDefaultCaches,omitempty"`
	// WellKnownCaches maps cache name to enabled state. nil means use default
	// (all true), true means explicitly enabled, false means explicitly disabled.
	WellKnownCaches map[string]bool `json:"wellKnownCaches,omitempty"`
	// CacheMappings are custom directory mappings to mount into the container.
	CacheMappings []CacheMapping `json:"cacheMappings,omitempty"`
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
	// LastUsed is the Unix timestamp (seconds) of the last task created for
	// this repo.
	LastUsed int64 `json:"lastUsed,omitempty"`
}

// Store manages all users' preferences in a single JSON file.
// All methods are safe for concurrent use.
type Store struct {
	mu     sync.Mutex
	path   string
	cached map[string]Preferences // keyed by userID
}

// Open opens (or creates) a multi-user preferences file at path.
// If the file does not exist, an empty store is returned.
func Open(path string) (*Store, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is caller-provided
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Store{path: path, cached: map[string]Preferences{}}, nil
		}
		return nil, fmt.Errorf("read preferences: %w", err)
	}
	var mf usersFile
	if err := json.Unmarshal(data, &mf); err != nil {
		return nil, fmt.Errorf("parse preferences: %w", err)
	}
	if err := mf.Validate(); err != nil {
		return nil, fmt.Errorf("invalid preferences: %w", err)
	}
	if mf.Users == nil {
		mf.Users = map[string]Preferences{}
	}
	return &Store{path: path, cached: mf.Users}, nil
}

// Get returns a copy of preferences for userID. Returns defaults when userID has no stored prefs.
func (s *Store) Get(userID string) Preferences {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.cached[userID]
	if !ok {
		return *newPreferences()
	}
	return p.clone()
}

// Update applies fn to userID's preferences and atomically saves the file.
func (s *Store) Update(userID string, fn func(*Preferences)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.cached[userID]
	if !ok {
		p = *newPreferences()
	}
	fn(&p)
	if err := p.Validate(); err != nil {
		return fmt.Errorf("validate preferences: %w", err)
	}
	s.cached[userID] = p
	data, err := json.MarshalIndent(usersFile{Users: s.cached}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal preferences: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create preferences dir: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write preferences: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename preferences: %w", err)
	}
	return nil
}

// currentVersion is the preferences file format version.
const currentVersion = 1

// recentWindow is how far back we consider a repo "recent".
const recentWindow = 7 * 24 * time.Hour

// minRecentRepos is the minimum number of repos always shown as recent,
// regardless of last-used time.
const minRecentRepos = 10

func newPreferences() *Preferences {
	return &Preferences{Version: currentVersion}
}

// usersFile is the on-disk JSON format for the Store.
type usersFile struct {
	Users map[string]Preferences `json:"users,omitempty"`
}

// BaseImages returns all distinct non-empty base images configured across all
// users' global preferences.
func (s *Store) BaseImages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[string]struct{})
	for _, p := range s.cached {
		if p.Settings.BaseImage != "" {
			seen[p.Settings.BaseImage] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// Validate checks that the on-disk format is well-formed.
func (f *usersFile) Validate() error {
	for id, p := range f.Users {
		if id == "" {
			return errors.New("users: empty user ID key")
		}
		if err := p.Validate(); err != nil {
			return fmt.Errorf("users[%q]: %w", id, err)
		}
	}
	return nil
}
