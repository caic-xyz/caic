// Generate directives for recording golden-file test traces.
//
// Run go generate to record traces for any harness/scenario combinations
// whose golden file is missing. To regenerate all traces from scratch,
// remove the testdata/ directory first.
//
// Requires: podman, XIAOMI_API_KEY env var.

package pi

//go:generate go run ../../cmd/record-trace --harness pi --scenario read-edit-bash --model xiaomi/mimo-v2.5
//go:generate go run ../../cmd/record-trace --harness pi --scenario tool-error --model xiaomi/mimo-v2.5
