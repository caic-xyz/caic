// Generate directives for recording golden-file test traces.
//
// Run go generate to record traces for any harness/scenario combinations
// whose golden file is missing. To regenerate all traces from scratch,
// remove the testdata/ directory first.
//
// Requires: podman. Uses OpenCode's free model.

package opencode

//go:generate go run ../../cmd/record-trace --harness opencode --scenario read-edit-bash --model opencode/deepseek-v4-flash-free
//go:generate go run ../../cmd/record-trace --harness opencode --scenario tool-error --model opencode/deepseek-v4-flash-free
