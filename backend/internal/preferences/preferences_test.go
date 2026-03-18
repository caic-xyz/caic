package preferences

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	t.Run("valid_empty", func(t *testing.T) {
		p := &Preferences{Version: currentVersion}
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("valid_full", func(t *testing.T) {
		p := &Preferences{
			Version: 1,
			Repositories: []RepoPrefs{
				{Path: "github/foo", BaseBranch: "develop"},
				{Path: "github/bar"},
			},
			Harness:  "claude",
			Models:   map[string]string{"claude": "opus", "codex": "o3"},
			Settings: Settings{BaseImage: "custom:latest"},
		}
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("wrong_version", func(t *testing.T) {
		p := &Preferences{Version: 99}
		if err := p.Validate(); err == nil {
			t.Fatal("expected error for wrong version")
		}
	})
	t.Run("empty_repo_path", func(t *testing.T) {
		p := &Preferences{Version: 1, Repositories: []RepoPrefs{{Path: ""}}}
		if err := p.Validate(); err == nil {
			t.Fatal("expected error for empty repo path")
		}
	})
	t.Run("duplicate_repo_path", func(t *testing.T) {
		p := &Preferences{
			Version: 1,
			Repositories: []RepoPrefs{
				{Path: "github/foo"},
				{Path: "github/foo"},
			},
		}
		if err := p.Validate(); err == nil {
			t.Fatal("expected error for duplicate repo path")
		}
	})
	t.Run("valid_cache_mappings", func(t *testing.T) {
		p := &Preferences{
			Version: 1,
			Settings: Settings{
				CacheMappings: []CacheMapping{
					{HostPath: "/host/a", ContainerPath: "/container/a"},
				},
			},
		}
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("cache_mapping_empty_host", func(t *testing.T) {
		p := &Preferences{
			Version: 1,
			Settings: Settings{
				CacheMappings: []CacheMapping{
					{HostPath: "", ContainerPath: "/container/a"},
				},
			},
		}
		if err := p.Validate(); err == nil {
			t.Fatal("expected error for empty hostPath")
		}
	})
	t.Run("cache_mapping_empty_container", func(t *testing.T) {
		p := &Preferences{
			Version: 1,
			Settings: Settings{
				CacheMappings: []CacheMapping{
					{HostPath: "/host/a", ContainerPath: ""},
				},
			},
		}
		if err := p.Validate(); err == nil {
			t.Fatal("expected error for empty containerPath")
		}
	})
}

func TestUsersFileValidate(t *testing.T) {
	t.Run("valid_empty_map", func(t *testing.T) {
		f := &usersFile{Users: map[string]Preferences{}}
		if err := f.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("valid_nil_map", func(t *testing.T) {
		f := &usersFile{}
		if err := f.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("valid_users", func(t *testing.T) {
		f := &usersFile{Users: map[string]Preferences{
			"alice": {Version: currentVersion},
			"bob":   {Version: currentVersion},
		}}
		if err := f.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("empty_user_id", func(t *testing.T) {
		f := &usersFile{Users: map[string]Preferences{
			"": {Version: currentVersion},
		}}
		if err := f.Validate(); err == nil {
			t.Fatal("expected error for empty user ID key")
		}
	})
	t.Run("invalid_prefs_propagate", func(t *testing.T) {
		f := &usersFile{Users: map[string]Preferences{
			"alice": {Version: 99},
		}}
		if err := f.Validate(); err == nil {
			t.Fatal("expected error for invalid prefs")
		}
	})
}

func TestUsers(t *testing.T) {
	t.Run("round_trip", func(t *testing.T) {
		fp := filepath.Join(t.TempDir(), "preferences.json")
		s, err := Open(fp)
		if err != nil {
			t.Fatal(err)
		}
		want := Preferences{
			Version: 1,
			Repositories: []RepoPrefs{
				{Path: "github/caic", BaseBranch: "develop"},
				{Path: "github/other"},
			},
			Harness: "claude",
			Models:  map[string]string{"claude": "opus"},
			Settings: Settings{
				BaseImage:        "custom:latest",
				UseDefaultCaches: true,
				WellKnownCaches:  map[string]bool{"go-mod": true, "npm": false},
				CacheMappings: []CacheMapping{
					{HostPath: "/host/cache", ContainerPath: "/container/cache"},
				},
			},
		}
		if err := s.Update("alice", func(p *Preferences) { *p = want }); err != nil {
			t.Fatal(err)
		}
		// Reopen to verify persistence.
		s2, err := Open(fp)
		if err != nil {
			t.Fatal(err)
		}
		got := s2.Get("alice")
		if got.Harness != want.Harness {
			t.Errorf("harness = %q, want %q", got.Harness, want.Harness)
		}
		if got.Settings.BaseImage != want.Settings.BaseImage {
			t.Errorf("baseImage = %q, want %q", got.Settings.BaseImage, want.Settings.BaseImage)
		}
		if len(got.Repositories) != len(want.Repositories) {
			t.Fatalf("repos len = %d, want %d", len(got.Repositories), len(want.Repositories))
		}
		for i, r := range got.Repositories {
			if r.Path != want.Repositories[i].Path {
				t.Errorf("repos[%d].path = %q, want %q", i, r.Path, want.Repositories[i].Path)
			}
		}
		if m := got.Models["claude"]; m != "opus" {
			t.Errorf("models[claude] = %q, want %q", m, "opus")
		}
		if !got.Settings.UseDefaultCaches {
			t.Error("useDefaultCaches = false, want true")
		}
		if len(got.Settings.WellKnownCaches) != 2 {
			t.Fatalf("wellKnownCaches len = %d, want 2", len(got.Settings.WellKnownCaches))
		}
		if !got.Settings.WellKnownCaches["go-mod"] {
			t.Error("wellKnownCaches[go-mod] = false, want true")
		}
		if got.Settings.WellKnownCaches["npm"] {
			t.Error("wellKnownCaches[npm] = true, want false")
		}
		if len(got.Settings.CacheMappings) != 1 {
			t.Fatalf("cacheMappings len = %d, want 1", len(got.Settings.CacheMappings))
		}
		if got.Settings.CacheMappings[0].HostPath != "/host/cache" {
			t.Errorf("cacheMappings[0].hostPath = %q, want %q", got.Settings.CacheMappings[0].HostPath, "/host/cache")
		}
	})

	t.Run("missing_file_returns_defaults", func(t *testing.T) {
		fp := filepath.Join(t.TempDir(), "nonexistent", "preferences.json")
		s, err := Open(fp)
		if err != nil {
			t.Fatal(err)
		}
		got := s.Get("anyuser")
		if got.Version != currentVersion {
			t.Errorf("version = %d, want %d", got.Version, currentVersion)
		}
	})

	t.Run("update_persists_and_creates_dirs", func(t *testing.T) {
		fp := filepath.Join(t.TempDir(), "sub", "deep", "preferences.json")
		s, err := Open(fp)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Update("u1", func(p *Preferences) { p.Harness = "claude" }); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(fp); err != nil {
			t.Fatal(err)
		}
		s2, err := Open(fp)
		if err != nil {
			t.Fatal(err)
		}
		if got := s2.Get("u1"); got.Harness != "claude" {
			t.Errorf("harness = %q, want %q", got.Harness, "claude")
		}
	})

	t.Run("users_are_isolated", func(t *testing.T) {
		fp := filepath.Join(t.TempDir(), "preferences.json")
		s, err := Open(fp)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Update("alice", func(p *Preferences) { p.Harness = "claude" }); err != nil {
			t.Fatal(err)
		}
		if got := s.Get("bob"); got.Harness != "" {
			t.Errorf("bob should have empty harness, got %q", got.Harness)
		}
	})

	t.Run("update_rejects_invalid_prefs", func(t *testing.T) {
		fp := filepath.Join(t.TempDir(), "preferences.json")
		s, err := Open(fp)
		if err != nil {
			t.Fatal(err)
		}
		err = s.Update("u", func(p *Preferences) {
			p.Repositories = []RepoPrefs{{Path: ""}}
		})
		if err == nil {
			t.Fatal("expected error for invalid prefs")
		}
		// Cached state must not be updated on failure.
		if got := s.Get("u"); len(got.Repositories) != 0 {
			t.Errorf("cached state was mutated despite validation failure")
		}
	})

	t.Run("get_returns_deep_copy", func(t *testing.T) {
		fp := filepath.Join(t.TempDir(), "preferences.json")
		s, err := Open(fp)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Update("u", func(p *Preferences) {
			p.TouchRepo("github/foo", &RepoPrefs{Harness: "claude", Model: "opus"})
			p.Settings.WellKnownCaches = map[string]bool{"go-mod": true}
			p.Settings.CacheMappings = []CacheMapping{{HostPath: "/a", ContainerPath: "/b"}}
		}); err != nil {
			t.Fatal(err)
		}

		snapshot := s.Get("u")

		snapshot.Harness = "mutated"
		if got := s.Get("u"); got.Harness == "mutated" {
			t.Error("scalar field aliased")
		}

		snapshot.Repositories[0].Harness = "mutated"
		if got := s.Get("u"); got.Repositories[0].Harness == "mutated" {
			t.Error("slice element aliased")
		}

		snapshot.Models["claude"] = "mutated"
		if got := s.Get("u"); got.Models["claude"] == "mutated" {
			t.Error("map aliased")
		}

		snapshot.Settings.WellKnownCaches["go-mod"] = false
		if got := s.Get("u"); !got.Settings.WellKnownCaches["go-mod"] {
			t.Error("wellKnownCaches map aliased")
		}

		snapshot.Settings.CacheMappings[0].HostPath = "mutated"
		if got := s.Get("u"); got.Settings.CacheMappings[0].HostPath == "mutated" {
			t.Error("cacheMappings slice aliased")
		}
	})
}

func TestTouchRepo(t *testing.T) {
	t.Run("new_repo_with_overrides", func(t *testing.T) {
		before := time.Now().Unix()
		p := &Preferences{Version: currentVersion}
		p.TouchRepo("github/foo", &RepoPrefs{Harness: "claude", Model: "opus"})
		if len(p.Repositories) != 1 {
			t.Fatalf("got %d repos", len(p.Repositories))
		}
		r := p.Repositories[0]
		if r.Path != "github/foo" || r.Harness != "claude" || r.Model != "opus" {
			t.Fatalf("got %+v", r)
		}
		if r.LastUsed < before {
			t.Errorf("lastUsed = %d, want >= %d", r.LastUsed, before)
		}
		// Global defaults updated.
		if p.Harness != "claude" {
			t.Errorf("global harness = %q, want %q", p.Harness, "claude")
		}
		if p.Models["claude"] != "opus" {
			t.Errorf("global models[claude] = %q, want %q", p.Models["claude"], "opus")
		}
	})
	t.Run("move_to_front_preserves_existing", func(t *testing.T) {
		p := &Preferences{
			Version: 1,
			Repositories: []RepoPrefs{
				{Path: "github/a"},
				{Path: "github/b", BaseBranch: "develop", Harness: "codex", Model: "o3"},
				{Path: "github/c"},
			},
		}
		// Touch with only harness override; model and baseBranch preserved.
		p.TouchRepo("github/b", &RepoPrefs{Harness: "claude"})
		want := []string{"github/b", "github/a", "github/c"}
		for i, r := range p.Repositories {
			if r.Path != want[i] {
				t.Errorf("repos[%d] = %q, want %q", i, r.Path, want[i])
			}
		}
		r := p.Repositories[0]
		if r.BaseBranch != "develop" {
			t.Errorf("baseBranch = %q, want %q", r.BaseBranch, "develop")
		}
		if r.Harness != "claude" {
			t.Errorf("harness = %q, want %q", r.Harness, "claude")
		}
		// Model preserved from before (override was empty).
		if r.Model != "o3" {
			t.Errorf("model = %q, want %q", r.Model, "o3")
		}
	})
	t.Run("already_first", func(t *testing.T) {
		p := &Preferences{
			Version: 1,
			Repositories: []RepoPrefs{
				{Path: "github/a", Harness: "claude"},
				{Path: "github/b"},
			},
		}
		p.TouchRepo("github/a", &RepoPrefs{Model: "sonnet"})
		if p.Repositories[0].Path != "github/a" || p.Repositories[1].Path != "github/b" {
			t.Fatalf("got %v", p.Repositories)
		}
		if p.Repositories[0].Model != "sonnet" {
			t.Errorf("model = %q, want %q", p.Repositories[0].Model, "sonnet")
		}
		// Harness preserved.
		if p.Repositories[0].Harness != "claude" {
			t.Errorf("harness = %q, want %q", p.Repositories[0].Harness, "claude")
		}
	})
	t.Run("clear_model_after_touch", func(t *testing.T) {
		// Simulate the server clearing model when the user selects "default".
		// TouchRepo alone can't clear model (empty = don't override), so the
		// caller must clear it explicitly after TouchRepo.
		p := &Preferences{
			Version: 1,
			Models:  map[string]string{"claude": "opus"},
			Repositories: []RepoPrefs{
				{Path: "github/a", Harness: "claude", Model: "opus"},
			},
		}
		p.TouchRepo("github/a", &RepoPrefs{Harness: "claude"})
		// Caller clears model explicitly (mirrors server.go logic).
		p.Repositories[0].Model = ""
		delete(p.Models, "claude")
		if p.Repositories[0].Model != "" {
			t.Errorf("repo model = %q, want empty", p.Repositories[0].Model)
		}
		if _, ok := p.Models["claude"]; ok {
			t.Errorf("global models[claude] should be deleted")
		}
	})
	t.Run("empty_overrides_preserve_all", func(t *testing.T) {
		p := &Preferences{
			Version: 1,
			Repositories: []RepoPrefs{
				{Path: "github/a", Harness: "codex", Model: "o3", BaseImage: "custom:v1", BaseBranch: "dev"},
			},
		}
		p.TouchRepo("github/a", &RepoPrefs{})
		r := p.Repositories[0]
		if r.Harness != "codex" || r.Model != "o3" || r.BaseImage != "custom:v1" || r.BaseBranch != "dev" {
			t.Fatalf("fields clobbered: %+v", r)
		}
	})
}

func TestRecentRepos(t *testing.T) {
	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-3 * 24 * time.Hour).Unix() // 3 days ago — within window
	old := now.Add(-10 * 24 * time.Hour).Unix()   // 10 days ago — outside window

	t.Run("all_recent_under_min", func(t *testing.T) {
		// Fewer than minRecentRepos repos, all old: all returned.
		p := &Preferences{
			Version: 1,
			Repositories: []RepoPrefs{
				{Path: "a", LastUsed: old},
				{Path: "b", LastUsed: old},
			},
		}
		got := p.RecentRepos(now)
		if len(got) != 2 {
			t.Fatalf("got %d repos, want 2", len(got))
		}
	})

	t.Run("keeps_min_10_regardless_of_age", func(t *testing.T) {
		repos := make([]RepoPrefs, 15)
		for i := range repos {
			repos[i] = RepoPrefs{Path: string(rune('a' + i)), LastUsed: old}
		}
		p := &Preferences{Version: 1, Repositories: repos}
		got := p.RecentRepos(now)
		if len(got) != 10 {
			t.Fatalf("got %d repos, want 10", len(got))
		}
		// First 10 preserved in order.
		for i, r := range got {
			if r.Path != repos[i].Path {
				t.Errorf("repos[%d] = %q, want %q", i, r.Path, repos[i].Path)
			}
		}
	})

	t.Run("recent_beyond_min_included", func(t *testing.T) {
		repos := make([]RepoPrefs, 12)
		for i := range repos {
			repos[i] = RepoPrefs{Path: string(rune('a' + i)), LastUsed: old}
		}
		// Make repo at index 11 (beyond min 10) recently used.
		repos[11].LastUsed = recent
		p := &Preferences{Version: 1, Repositories: repos}
		got := p.RecentRepos(now)
		if len(got) != 11 {
			t.Fatalf("got %d repos, want 11", len(got))
		}
		if got[10].Path != repos[11].Path {
			t.Errorf("repos[10] = %q, want %q", got[10].Path, repos[11].Path)
		}
	})

	t.Run("no_timestamp_falls_back_to_min", func(t *testing.T) {
		// Repos with no LastUsed (zero) beyond index 10 are excluded.
		repos := make([]RepoPrefs, 15)
		for i := range repos {
			repos[i] = RepoPrefs{Path: string(rune('a' + i))}
		}
		p := &Preferences{Version: 1, Repositories: repos}
		got := p.RecentRepos(now)
		if len(got) != 10 {
			t.Fatalf("got %d repos, want 10", len(got))
		}
	})
}
