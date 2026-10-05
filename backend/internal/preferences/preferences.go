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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/md"
)

const (
	// MinPurgeDelay is the shortest supported task purge recovery window.
	MinPurgeDelay time.Duration = 10 * time.Second
	// MaxPurgeDelay is the longest supported task purge recovery window.
	MaxPurgeDelay time.Duration = 24 * time.Hour
)

// CacheMapping maps a host directory to a container path for cache/state sharing.
type CacheMapping struct {
	// HostPath is the path on the host filesystem to mount.
	HostPath string
	// ContainerPath is the path inside the container where HostPath will be mounted.
	// Empty uses the matching home-relative HostPath; md owns the resolution.
	ContainerPath string
	// Enabled controls whether this mapping is passed to new containers.
	Enabled bool
}

// MountMapping maps a host directory to a container path as a general mount.
type MountMapping struct {
	// HostPath is the path on the host filesystem to mount.
	HostPath string
	// ContainerPath is the path inside the container where HostPath will be mounted.
	// Empty uses the matching home-relative HostPath; md owns the resolution.
	ContainerPath string
	// Enabled controls whether this mapping is passed to new containers.
	Enabled bool
	// ReadOnly controls whether the container sees this mount as read-only.
	ReadOnly bool
}

// ContainerImage identifies a base image and optional platform pair.
type ContainerImage struct {
	BaseImage string
	Platform  string
}

// Preferences holds persistent user preferences.
type Preferences struct {
	// Version is the preferences file format version.
	Version int
	// Repositories is an ordered list of recently used repositories (most
	// recent first), each with optional per-repo overrides.
	Repositories []RepoPrefs
	// Harness is the last used agent harness (e.g. "claude", "codex").
	Harness string
	// Models maps harness name to the last used model for that harness.
	Models map[string]string
	// Efforts maps harness name to model name to the last used thinking effort.
	// The empty model key stores the default-model preference.
	Efforts EffortPreferences
	// Settings holds user-configurable behavioral settings.
	Settings Settings
}

func newPreferences() *Preferences {
	return &Preferences{Version: currentVersion, Settings: defaultSettings()}
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
	return p.Settings.Validate()
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
	c.Efforts = cloneEffortPreferences(p.Efforts)
	c.Settings.CacheMappings = slices.Clone(p.Settings.CacheMappings)
	c.Settings.CustomMounts = slices.Clone(p.Settings.CustomMounts)
	c.Settings.WellKnownCaches = maps.Clone(p.Settings.WellKnownCaches)
	c.Settings.RuntimeSettings = maps.Clone(p.Settings.RuntimeSettings)
	return c
}

// EffortPreferences stores thinking-effort preferences by harness and model.
type EffortPreferences map[string]map[string]string

func cloneEffortPreferences(e EffortPreferences) EffortPreferences {
	if e == nil {
		return nil
	}
	c := make(EffortPreferences, len(e))
	for harness, efforts := range e {
		c[harness] = maps.Clone(efforts)
	}
	return c
}

// Settings holds user-configurable behavioral settings.
type Settings struct {
	// AutoFixOnCIFailure automatically starts a new task to fix CI when a
	// task's PR CI fails and the original task can no longer receive input.
	AutoFixOnCIFailure bool
	// AutoFixOnPROpen automatically creates a task to review and fix a pull
	// request when it is opened or reopened via a forge webhook.
	AutoFixOnPROpen bool
	// BaseImage overrides the default container base image. Empty means use
	// the default.
	BaseImage string
	// RuntimeSettings stores CPU architecture and limits by runtime name.
	RuntimeSettings map[string]RuntimeSettings
	// PurgeDelay is the recovery window before a stopped task is deleted.
	PurgeDelay time.Duration
	// WellKnownCaches maps cache name to enabled state. Absent or false means
	// disabled, true means enabled. Caches are opt-in.
	WellKnownCaches map[string]bool
	// CacheMappings are custom directory mappings to mount into the container.
	CacheMappings []CacheMapping
	// CustomMounts are custom non-cache directory mappings to mount into the container.
	CustomMounts []MountMapping
	// RuntimeName is the last selected runtime backend for new tasks.
	RuntimeName string
}

func defaultSettings() Settings {
	return Settings{PurgeDelay: 15 * time.Second}
}

// Validate checks that the settings are well-formed.
func (s *Settings) Validate() error {
	for i, m := range s.CacheMappings {
		if _, err := md.ResolveMountTarget(m.HostPath, m.ContainerPath); err != nil {
			return fmt.Errorf("cacheMappings[%d]: %w", i, err)
		}
	}
	for i, m := range s.CustomMounts {
		if _, err := md.ResolveMountTarget(m.HostPath, m.ContainerPath); err != nil {
			return fmt.Errorf("customMounts[%d]: %w", i, err)
		}
	}
	for name, settings := range s.RuntimeSettings {
		if name == "" {
			return errors.New("runtimeSettings requires a non-empty runtime name")
		}
		if err := settings.Validate(); err != nil {
			return fmt.Errorf("runtimeSettings[%q]: %w", name, err)
		}
	}
	if s.PurgeDelay < MinPurgeDelay || s.PurgeDelay > MaxPurgeDelay {
		return fmt.Errorf("purgeDelay must be between %s and %s", MinPurgeDelay, MaxPurgeDelay)
	}
	return nil
}

// RuntimeSettings holds CPU configuration for a single runtime.
type RuntimeSettings struct {
	// ContainerPlatform selects the CPU architecture. Empty means native.
	ContainerPlatform md.Platform
	// MaxCPUs limits CPU cores. Zero uses md's automatic runtime default.
	MaxCPUs int
}

// Validate checks the runtime's CPU configuration.
func (s *RuntimeSettings) Validate() error {
	if err := s.ContainerPlatform.Validate(); err != nil {
		return fmt.Errorf("unsupported containerPlatform %q", s.ContainerPlatform)
	}
	if s.MaxCPUs < 0 {
		return errors.New("maxCPUs must be non-negative")
	}
	return nil
}

// RepoPrefs stores per-repository user preferences. Fields override the
// global defaults in Preferences when set.
type RepoPrefs struct {
	// Path is the repository identifier (e.g. "github/caic").
	Path string
	// BaseBranch overrides the repository's default branch when creating tasks.
	BaseBranch string
	// Harness is the preferred agent harness for this repo.
	Harness string
	// Model is the preferred model for this repo's harness.
	Model string
	// LastUsed is the Unix timestamp (seconds) of the last task created for
	// this repo.
	LastUsed int64
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
	mf, err := decodeUsersFile(data)
	if err != nil {
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
	data, err := json.MarshalIndent(usersFileToData(s.cached), "", "  ")
	if err != nil {
		return fmt.Errorf("marshal preferences: %w", err)
	}
	data = append(data, '\n')
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

// BaseImages returns all distinct non-empty base images configured across all
// users' global preferences.
func (s *Store) BaseImages() []ContainerImage {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[string]struct{})
	for k := range s.cached {
		settings := s.cached[k].Settings
		if settings.BaseImage != "" {
			seen[settings.BaseImage+"\x00"] = struct{}{}
			for _, config := range settings.RuntimeSettings {
				seen[settings.BaseImage+"\x00"+config.ContainerPlatform.String()] = struct{}{}
			}
		}
	}
	keys := slices.Sorted(maps.Keys(seen))
	images := make([]ContainerImage, len(keys))
	for i, key := range keys {
		baseImage, platform, _ := strings.Cut(key, "\x00")
		images[i] = ContainerImage{BaseImage: baseImage, Platform: platform}
	}
	return images
}

// currentVersion is the preferences file format version.
const currentVersion = 1

// recentWindow is how far back we consider a repo "recent".
const recentWindow = 7 * 24 * time.Hour

// minRecentRepos is the minimum number of repos always shown as recent,
// regardless of last-used time.
const minRecentRepos = 10

// usersFile groups runtime preferences by user for validation.
type usersFile struct {
	Users map[string]Preferences
}

// Validate checks all runtime preferences and user keys.
func (f *usersFile) Validate() error {
	for id := range f.Users {
		if id == "" {
			return errors.New("users: empty user ID key")
		}
		p := f.Users[id]
		if err := p.Validate(); err != nil {
			return fmt.Errorf("users[%q]: %w", id, err)
		}
	}
	return nil
}
