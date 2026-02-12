package container

import "testing"

func TestNewLib(t *testing.T) {
	lib, err := NewLib("")
	if err != nil {
		t.Fatal(err)
	}
	if lib.Client == nil {
		t.Fatal("NewLib returned Lib with nil Client")
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
