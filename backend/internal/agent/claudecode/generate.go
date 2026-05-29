// Generate directives for recording golden-file test traces.
//
// Run go generate to record traces for any harness/scenario combinations
// whose golden file is missing. To regenerate all traces from scratch,
// remove the testdata/ directory first.
//
// Requires: podman, ANTHROPIC_API_KEY env var.

package claudecode

//go:generate go run ../../cmd/record-trace --harness claude --scenario read-edit-bash --model haiku --api-key-env ANTHROPIC_API_KEY
//go:generate go run ../../cmd/record-trace --harness claude --scenario tool-error --model haiku --api-key-env ANTHROPIC_API_KEY
