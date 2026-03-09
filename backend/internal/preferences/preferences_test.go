package preferences

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	t.Run("valid_empty", func(t *testing.T) {
		p := newPreferences()
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
			Harness:   "claude",
			Models:    map[string]string{"claude": "opus", "codex": "o3"},
			BaseImage: "custom:latest",
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
}

func TestStore(t *testing.T) {
	t.Run("round_trip", func(t *testing.T) {
		fp := filepath.Join(t.TempDir(), "preferences.json")

		want := &Preferences{
			Version: 1,
			Repositories: []RepoPrefs{
				{Path: "github/caic", BaseBranch: "develop"},
				{Path: "github/other"},
			},
			Harness:   "claude",
			Models:    map[string]string{"claude": "opus"},
			BaseImage: "custom:latest",
		}
		if err := save(want, fp); err != nil {
			t.Fatal(err)
		}
		s, err := Open(fp)
		if err != nil {
			t.Fatal(err)
		}
		got := s.Get()
		if got.Version != want.Version {
			t.Errorf("version = %d, want %d", got.Version, want.Version)
		}
		if got.Harness != want.Harness {
			t.Errorf("harness = %q, want %q", got.Harness, want.Harness)
		}
		if got.BaseImage != want.BaseImage {
			t.Errorf("baseImage = %q, want %q", got.BaseImage, want.BaseImage)
		}
		if len(got.Repositories) != len(want.Repositories) {
			t.Fatalf("repos len = %d, want %d", len(got.Repositories), len(want.Repositories))
		}
		for i, r := range got.Repositories {
			if r.Path != want.Repositories[i].Path {
				t.Errorf("repos[%d].path = %q, want %q", i, r.Path, want.Repositories[i].Path)
			}
			if r.BaseBranch != want.Repositories[i].BaseBranch {
				t.Errorf("repos[%d].baseBranch = %q, want %q", i, r.BaseBranch, want.Repositories[i].BaseBranch)
			}
		}
		if m, ok := got.Models["claude"]; !ok || m != "opus" {
			t.Errorf("models[claude] = %q, want %q", m, "opus")
		}
	})

	t.Run("open_missing", func(t *testing.T) {
		fp := filepath.Join(t.TempDir(), "nonexistent", "preferences.json")
		s, err := Open(fp)
		if err != nil {
			t.Fatal(err)
		}
		got := s.Get()
		if got.Version != currentVersion {
			t.Errorf("version = %d, want %d", got.Version, currentVersion)
		}
	})

	t.Run("update_persists", func(t *testing.T) {
		fp := filepath.Join(t.TempDir(), "sub", "deep", "preferences.json")
		s, err := Open(fp)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Update(func(p *Preferences) {
			p.Harness = "claude"
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(fp); err != nil {
			t.Fatal(err)
		}
		// Reopen and verify persistence.
		s2, err := Open(fp)
		if err != nil {
			t.Fatal(err)
		}
		if got := s2.Get(); got.Harness != "claude" {
			t.Errorf("harness = %q, want %q", got.Harness, "claude")
		}
	})

	t.Run("get_returns_deep_copy", func(t *testing.T) {
		fp := filepath.Join(t.TempDir(), "preferences.json")
		s, err := Open(fp)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Update(func(p *Preferences) {
			p.TouchRepo("github/foo", &RepoPrefs{Harness: "claude", Model: "opus"})
		}); err != nil {
			t.Fatal(err)
		}

		snapshot := s.Get()

		// Mutate scalar.
		snapshot.Harness = "mutated"
		if got := s.Get(); got.Harness == "mutated" {
			t.Error("scalar field aliased")
		}

		// Mutate slice element.
		snapshot.Repositories[0].Harness = "mutated"
		if got := s.Get(); got.Repositories[0].Harness == "mutated" {
			t.Error("slice element aliased")
		}

		// Mutate map.
		snapshot.Models["claude"] = "mutated"
		if got := s.Get(); got.Models["claude"] == "mutated" {
			t.Error("map aliased")
		}
	})

	t.Run("save_rejects_invalid", func(t *testing.T) {
		fp := filepath.Join(t.TempDir(), "preferences.json")
		p := &Preferences{Version: 0}
		if err := save(p, fp); err == nil {
			t.Fatal("expected error saving invalid preferences")
		}
	})
}

func TestTouchRepo(t *testing.T) {
	t.Run("new_repo_with_overrides", func(t *testing.T) {
		before := time.Now().Unix()
		p := newPreferences()
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
