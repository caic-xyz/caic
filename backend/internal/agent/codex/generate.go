// Generate directives for recording golden-file test traces.
//
// Run go generate to record traces for any harness/scenario combinations
// whose golden file is missing. To regenerate all traces from scratch,
// remove the testdata/ directory first.
//
// Requires: podman, OPENAI_API_KEY env var.

package codex

//go:generate go run ../../cmd/record-trace --harness codex --scenario read-edit-bash --model gpt-5.2-mini --api-key-env OPENAI_API_KEY
//go:generate go run ../../cmd/record-trace --harness codex --scenario tool-error --model gpt-5.2-mini --api-key-env OPENAI_API_KEY
