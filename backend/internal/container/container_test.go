package container

import (
	"path/filepath"
	"testing"
)

func TestNew(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmpDir, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(tmpDir, ".local", "share"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmpDir, ".local", "state"))
	c, err := New("", "")
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("New returned nil client")
	}
}

func TestBranchFromContainer(t *testing.T) {
	tests := []struct {
		name      string
		container string
		repo      string
		wantBr    string
		wantOK    bool
	}{
		{
			name:      "standard caic branch",
			container: "md-caic-caic-fix-auth",
			repo:      "caic",
			wantBr:    "caic-fix-auth",
			wantOK:    true,
		},
		{
			name:      "non-caic branch",
			container: "md-caic-feature-xyz",
			repo:      "caic",
			wantBr:    "feature-xyz",
			wantOK:    true,
		},
		{
			name:      "wrong repo prefix",
			container: "md-other-caic-fix",
			repo:      "caic",
			wantBr:    "",
			wantOK:    false,
		},
		{
			name:      "no md prefix",
			container: "notmd-caic-fix",
			repo:      "caic",
			wantBr:    "",
			wantOK:    false,
		},
		{
			name:      "different repo",
			container: "md-myrepo-caic-fix",
			repo:      "myrepo",
			wantBr:    "caic-fix",
			wantOK:    true,
		},
		{
			name:      "numeric caic branch",
			container: "md-myrepo-caic-2",
			repo:      "myrepo",
			wantBr:    "caic-2",
			wantOK:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			br, ok := BranchFromContainer(tt.container, tt.repo)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if br != tt.wantBr {
				t.Errorf("branch = %q, want %q", br, tt.wantBr)
			}
		})
	}
}
