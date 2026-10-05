// Preferences file decoding and explicit projections between disk and runtime values.

package preferences

import (
	"encoding/json"
	"time"

	v1 "github.com/caic-xyz/caic/backend/internal/preferences/data/v1"
	"github.com/caic-xyz/md"
)

func decodeUsersFile(raw []byte) (*usersFile, error) {
	var f v1.UsersFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	var users map[string]Preferences
	if f.Users != nil {
		users = make(map[string]Preferences, len(f.Users))
	}
	for id := range f.Users {
		p := f.Users[id]
		users[id] = preferencesFromData(&p)
	}
	return &usersFile{Users: users}, nil
}

func preferencesFromData(p *v1.Preferences) Preferences {
	var repos []RepoPrefs
	if p.Repositories != nil {
		repos = make([]RepoPrefs, len(p.Repositories))
	}
	for i := range p.Repositories {
		r := &p.Repositories[i]
		repos[i] = RepoPrefs{Path: r.Path, BaseBranch: r.BaseBranch, Harness: r.Harness, Model: r.Model, LastUsed: r.LastUsed}
	}
	var caches []CacheMapping
	if p.Settings.CacheMappings != nil {
		caches = make([]CacheMapping, len(p.Settings.CacheMappings))
	}
	for i := range p.Settings.CacheMappings {
		r := &p.Settings.CacheMappings[i]
		caches[i] = CacheMapping{HostPath: r.HostPath, ContainerPath: r.ContainerPath, Enabled: r.Enabled}
	}
	var mounts []MountMapping
	if p.Settings.CustomMounts != nil {
		mounts = make([]MountMapping, len(p.Settings.CustomMounts))
	}
	for i := range p.Settings.CustomMounts {
		r := &p.Settings.CustomMounts[i]
		mounts[i] = MountMapping{HostPath: r.HostPath, ContainerPath: r.ContainerPath, Enabled: r.Enabled, ReadOnly: r.ReadOnly}
	}
	var runtimes map[string]RuntimeSettings
	if p.Settings.RuntimeSettings != nil {
		runtimes = make(map[string]RuntimeSettings, len(p.Settings.RuntimeSettings))
	}
	for name, r := range p.Settings.RuntimeSettings {
		runtimes[name] = RuntimeSettings{ContainerPlatform: md.Platform(r.ContainerPlatform), MaxCPUs: r.MaxCPUs}
	}
	// Settings absent leaves zero values; a present object or null applies defaults.
	// Explicit purgeDelay zero is retained and validated by the behavioral owner.
	var delay time.Duration
	if p.Settings.Present {
		delay = defaultSettings().PurgeDelay
	}
	if p.Settings.PurgeDelay != nil {
		delay = *p.Settings.PurgeDelay
	}
	return Preferences{
		Repositories: repos, Version: p.Version, Harness: p.Harness,
		Models: p.Models, Efforts: EffortPreferences(p.Efforts),
		Settings: Settings{
			AutoFixOnCIFailure: p.Settings.AutoFixOnCIFailure,
			AutoFixOnPROpen:    p.Settings.AutoFixOnPROpen,
			BaseImage:          p.Settings.BaseImage,
			WellKnownCaches:    p.Settings.WellKnownCaches,
			RuntimeName:        p.Settings.RuntimeName,
			CacheMappings:      caches, CustomMounts: mounts,
			RuntimeSettings: runtimes, PurgeDelay: delay,
		},
	}
}

func preferencesToData(p *Preferences) v1.Preferences {
	var repos []v1.RepoPrefs
	if p.Repositories != nil {
		repos = make([]v1.RepoPrefs, len(p.Repositories))
	}
	for i := range p.Repositories {
		r := &p.Repositories[i]
		repos[i] = v1.RepoPrefs{Path: r.Path, BaseBranch: r.BaseBranch, Harness: r.Harness, Model: r.Model, LastUsed: r.LastUsed}
	}
	var caches []v1.CacheMapping
	if p.Settings.CacheMappings != nil {
		caches = make([]v1.CacheMapping, len(p.Settings.CacheMappings))
	}
	for i := range p.Settings.CacheMappings {
		r := &p.Settings.CacheMappings[i]
		caches[i] = v1.CacheMapping{HostPath: r.HostPath, ContainerPath: r.ContainerPath, Enabled: r.Enabled}
	}
	var mounts []v1.MountMapping
	if p.Settings.CustomMounts != nil {
		mounts = make([]v1.MountMapping, len(p.Settings.CustomMounts))
	}
	for i := range p.Settings.CustomMounts {
		r := &p.Settings.CustomMounts[i]
		mounts[i] = v1.MountMapping{HostPath: r.HostPath, ContainerPath: r.ContainerPath, Enabled: r.Enabled, ReadOnly: r.ReadOnly}
	}
	var runtimes map[string]v1.RuntimeSettings
	if p.Settings.RuntimeSettings != nil {
		runtimes = make(map[string]v1.RuntimeSettings, len(p.Settings.RuntimeSettings))
	}
	for name, r := range p.Settings.RuntimeSettings {
		runtimes[name] = v1.RuntimeSettings{ContainerPlatform: v1.Platform(r.ContainerPlatform), MaxCPUs: r.MaxCPUs}
	}
	delay := p.Settings.PurgeDelay
	return v1.Preferences{
		Repositories: repos, Version: p.Version, Harness: p.Harness,
		Models: p.Models, Efforts: v1.EffortPreferences(p.Efforts),
		Settings: v1.Settings{
			AutoFixOnCIFailure: p.Settings.AutoFixOnCIFailure,
			AutoFixOnPROpen:    p.Settings.AutoFixOnPROpen,
			BaseImage:          p.Settings.BaseImage,
			WellKnownCaches:    p.Settings.WellKnownCaches,
			RuntimeName:        p.Settings.RuntimeName,
			CacheMappings:      caches, CustomMounts: mounts,
			RuntimeSettings: runtimes, PurgeDelay: &delay,
		},
	}
}

func usersFileToData(users map[string]Preferences) v1.UsersFile {
	var stored map[string]v1.Preferences
	if users != nil {
		stored = make(map[string]v1.Preferences, len(users))
	}
	for id := range users {
		p := users[id]
		stored[id] = preferencesToData(&p)
	}
	return v1.UsersFile{Users: stored}
}
