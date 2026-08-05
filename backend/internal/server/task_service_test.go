// Tests for task metadata diagnostics.

package server

import (
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
)

func TestTaskInfoCompareWarnings(t *testing.T) {
	t.Parallel()

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("valid", func(t *testing.T) {
		t.Parallel()

		recorded := v1.TaskInfoRecorded{
			Mounts: []v1.TaskInfoMount{{
				HostPath:      "~/.config/caic",
				ContainerPath: "~/.config/caic",
			}},
		}
		observed := v1.TaskInfoObservedRuntime{
			Mounts: []v1.TaskInfoMount{{
				HostPath:      filepath.ToSlash(filepath.Join(home, ".config", "caic")),
				ContainerPath: "/home/user/.config/caic",
			}},
		}

		warnings := taskInfoCompareWarnings(&recorded, &observed)
		if len(warnings) != 0 {
			t.Fatalf("warnings = %+v, want none", warnings)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()

		recorded := v1.TaskInfoRecorded{
			Mounts: []v1.TaskInfoMount{{
				HostPath:      "~/.config/caic",
				ContainerPath: "~/.config/caic",
			}},
		}
		observed := v1.TaskInfoObservedRuntime{
			Mounts: []v1.TaskInfoMount{{
				HostPath:      filepath.ToSlash(filepath.Join(home, ".config", "other")),
				ContainerPath: "/home/user/.config/caic",
			}},
		}

		warnings := taskInfoCompareWarnings(&recorded, &observed)
		if len(warnings) != 1 {
			t.Fatalf("warnings = %+v, want one warning", warnings)
		}
	})
}
