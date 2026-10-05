// Package v1 defines version 1 of the persisted user preferences schema.
package v1

import (
	"encoding/json"
	"time"
)

// Platform is the stored container CPU architecture.
type Platform string

// EffortPreferences stores effort choices by harness and model.
type EffortPreferences map[string]map[string]string

// UsersFile holds preferences keyed by user ID.
type UsersFile struct {
	Users map[string]Preferences `json:"users,omitempty"`
}

// CacheMapping is the persisted CacheMapping representation.
type CacheMapping struct {
	// HostPath is the path on the host filesystem to mount.
	HostPath string `json:"hostPath"`
	// ContainerPath is the path inside the container where HostPath will be mounted.
	// Empty uses the matching home-relative HostPath; md owns the resolution.
	ContainerPath string `json:"containerPath"`
	// Enabled controls whether this mapping is passed to new containers.
	Enabled bool `json:"enabled"`
}

// MountMapping is the persisted MountMapping representation.
type MountMapping struct {
	// HostPath is the path on the host filesystem to mount.
	HostPath string `json:"hostPath"`
	// ContainerPath is the path inside the container where HostPath will be mounted.
	// Empty uses the matching home-relative HostPath; md owns the resolution.
	ContainerPath string `json:"containerPath"`
	// Enabled controls whether this mapping is passed to new containers.
	Enabled bool `json:"enabled"`
	// ReadOnly controls whether the container sees this mount as read-only.
	ReadOnly bool `json:"readOnly"`
}

// Preferences is the persisted Preferences representation.
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
	// Efforts maps harness name to model name to the last used thinking effort.
	// The empty model key stores the default-model preference.
	Efforts EffortPreferences `json:"efforts,omitempty"`
	// Settings holds user-configurable behavioral settings.
	Settings Settings `json:"settings"`
}

// Settings is the persisted Settings representation.
type Settings struct {
	// Present records whether settings was decoded, including an explicit null.
	// It is serialization metadata and is never written to the file.
	Present bool `json:"-"`
	// AutoFixOnCIFailure automatically starts a new task to fix CI when a
	// task's PR CI fails and the original task can no longer receive input.
	AutoFixOnCIFailure bool `json:"autoFixOnCIFailure"`
	// AutoFixOnPROpen automatically creates a task to review and fix a pull
	// request when it is opened or reopened via a forge webhook.
	AutoFixOnPROpen bool `json:"autoFixOnPROpen"`
	// BaseImage overrides the default container base image. Empty means use
	// the default.
	BaseImage string `json:"baseImage,omitempty"`
	// RuntimeSettings stores CPU architecture and limits by runtime name.
	RuntimeSettings map[string]RuntimeSettings `json:"runtimeSettings,omitempty"`
	// PurgeDelay is the recovery window before a stopped task is deleted.
	PurgeDelay *time.Duration `json:"purgeDelay"`
	// WellKnownCaches maps cache name to enabled state. Absent or false means
	// disabled, true means enabled. Caches are opt-in.
	WellKnownCaches map[string]bool `json:"wellKnownCaches,omitempty"`
	// CacheMappings are custom directory mappings to mount into the container.
	CacheMappings []CacheMapping `json:"cacheMappings,omitempty"`
	// CustomMounts are custom non-cache directory mappings to mount into the container.
	CustomMounts []MountMapping `json:"customMounts,omitempty"`
	// RuntimeName is the last selected runtime backend for new tasks.
	RuntimeName string `json:"runtimeName,omitempty"`
}

// UnmarshalJSON records settings presence without applying runtime defaults.
func (s *Settings) UnmarshalJSON(raw []byte) error {
	type plain Settings
	var v plain
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	*s = Settings(v)
	s.Present = true
	return nil
}

// RuntimeSettings is the persisted RuntimeSettings representation.
type RuntimeSettings struct {
	// ContainerPlatform selects the CPU architecture. Empty means native.
	ContainerPlatform Platform `json:"containerPlatform,omitempty"`
	// MaxCPUs limits CPU cores. Zero uses md's automatic runtime default.
	MaxCPUs int `json:"maxCPUs,omitempty"`
}

// RepoPrefs is the persisted RepoPrefs representation.
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
