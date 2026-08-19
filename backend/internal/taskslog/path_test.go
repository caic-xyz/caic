// Tests synchronized task-log path storage.

package taskslog

import "testing"

func TestPath(t *testing.T) {
	t.Parallel()
	var path Path
	if got := path.Get(); got != "" {
		t.Fatalf("Get() = %q, want empty", got)
	}
	path.Set("/tmp/task.jsonl")
	if got := path.Get(); got != "/tmp/task.jsonl" {
		t.Fatalf("Get() = %q, want /tmp/task.jsonl", got)
	}
}
