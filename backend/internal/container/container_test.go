package container

import "testing"

func TestParseList(t *testing.T) {
	raw := `md-wmao-wmao-fix-auth   running   5 minutes ago
md-wmao-wmao-add-tests  stopped   1 hour ago
something-else          running   2 minutes ago`

	entries := parseList(raw)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].Name != "md-wmao-wmao-fix-auth" {
		t.Errorf("entries[0].Name = %q", entries[0].Name)
	}
	if entries[0].Status != "running" {
		t.Errorf("entries[0].Status = %q", entries[0].Status)
	}
	if entries[1].Name != "md-wmao-wmao-add-tests" {
		t.Errorf("entries[1].Name = %q", entries[1].Name)
	}
}

func TestParseListEmpty(t *testing.T) {
	entries := parseList("")
	if len(entries) != 0 {
		t.Fatalf("got %d entries, want 0", len(entries))
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
			name:      "standard wmao branch",
			container: "md-wmao-wmao-fix-auth",
			repo:      "wmao",
			wantBr:    "wmao/fix-auth",
			wantOK:    true,
		},
		{
			name:      "non-wmao branch",
			container: "md-wmao-feature-xyz",
			repo:      "wmao",
			wantBr:    "feature-xyz",
			wantOK:    true,
		},
		{
			name:      "wrong repo prefix",
			container: "md-other-wmao-fix",
			repo:      "wmao",
			wantBr:    "",
			wantOK:    false,
		},
		{
			name:      "no md prefix",
			container: "notmd-wmao-fix",
			repo:      "wmao",
			wantBr:    "",
			wantOK:    false,
		},
		{
			name:      "different repo",
			container: "md-myrepo-wmao-fix",
			repo:      "myrepo",
			wantBr:    "wmao/fix",
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
