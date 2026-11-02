// Package gittest isolates tests from the Git configuration of the host.
package gittest

import "os"

// Isolate points Git at empty user and system configuration for the rest of the
// test binary, so a developer's settings cannot change what a test observes.
// Settings that break tests today are commit.gpgsign, init.defaultBranch,
// branch.autoSetupMerge, and core.hooksPath.
//
// A test that needs an identity or a setting sets it on the repository under
// test, which Isolate leaves untouched.
func Isolate() {
	for _, key := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM"} {
		if err := os.Setenv(key, os.DevNull); err != nil {
			panic("gittest: " + err.Error())
		}
	}
}
