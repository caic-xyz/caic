// Tests for runtime inventory metadata normalization.

package mdruntime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInspectOSArch(t *testing.T) {
	t.Parallel()

	t.Run("valid fills missing architecture", func(t *testing.T) {
		t.Parallel()
		runtimePath := writeFakeRuntime(t, t.TempDir(), `#!/bin/sh
if [ "$1" = "inspect" ] && [ "$2" = "md-test" ] && [ "$3" = "--format" ]; then
	echo linux/amd64
	exit 0
fi
exit 1
`)

		osName, cpuArchitecture := inspectOSArch(t.Context(), runtimePath, "md-test", "", "", "linux")
		if osName != "linux" || cpuArchitecture != "amd64" {
			t.Fatalf("inspectOSArch = (%q, %q), want (linux, amd64)", osName, cpuArchitecture)
		}
	})

	t.Run("valid keeps complete OS and CPU architecture", func(t *testing.T) {
		t.Parallel()
		osName, cpuArchitecture := inspectOSArch(t.Context(), "docker", "md-test", "", "", "linux/arm64")
		if osName != "linux" || cpuArchitecture != "arm64" {
			t.Fatalf("inspectOSArch = (%q, %q), want (linux, arm64)", osName, cpuArchitecture)
		}
	})

	t.Run("valid fills architecture from image inspect", func(t *testing.T) {
		t.Parallel()
		runtimePath := writeFakeRuntime(t, t.TempDir(), `#!/bin/sh
if [ "$1" = "inspect" ] && [ "$2" = "md-test" ] && [ "$3" = "--format" ]; then
	echo linux/'<no value>'
	exit 0
fi
if [ "$1" = "image" ] && [ "$2" = "inspect" ] && [ "$3" = "sha256:image" ] && [ "$4" = "--format" ]; then
	echo linux/amd64
	exit 0
fi
exit 1
`)

		osName, cpuArchitecture := inspectOSArch(t.Context(), runtimePath, "md-test", "sha256:image", "image:ref", "linux")
		if osName != "linux" || cpuArchitecture != "amd64" {
			t.Fatalf("inspectOSArch = (%q, %q), want (linux, amd64)", osName, cpuArchitecture)
		}
	})

	t.Run("error keeps observed OS", func(t *testing.T) {
		t.Parallel()
		runtimePath := writeFakeRuntime(t, t.TempDir(), "#!/bin/sh\nexit 1\n")

		osName, cpuArchitecture := inspectOSArch(t.Context(), runtimePath, "md-test", "sha256:image", "image:ref", "linux")
		if osName != "linux" || cpuArchitecture != "" {
			t.Fatalf("inspectOSArch = (%q, %q), want (linux, empty)", osName, cpuArchitecture)
		}
	})
}

func writeFakeRuntime(t *testing.T, dir, script string) string {
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // Test creates a fake runtime executable.
		t.Fatal(err)
	}
	return path
}
