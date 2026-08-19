// Tests for State validation, decoding, and Result JSON round-tripping.

package taskslog

import (
	"encoding/json"
	"testing"
)

func TestState(t *testing.T) {
	t.Parallel()
	t.Run("String", func(t *testing.T) {
		t.Parallel()
		for _, tt := range []struct {
			state State
			want  string
		}{
			{StatePending, "pending"},
			{StateBranching, "branching"},
			{StateProvisioning, "provisioning"},
			{StateStarting, "starting"},
			{StateRunning, "running"},
			{StateWaiting, "waiting"},
			{StateAsking, "asking"},
			{StateHasPlan, "has_plan"},
			{StatePulling, "pulling"},
			{StatePushing, "pushing"},
			{StatePurging, "purging"},
			{StateCrashed, "crashed"},
			{StateFailed, "failed"},
			{StateStopping, "stopping"},
			{StateStopped, "stopped"},
			{StatePurged, "purged"},
			{State("bogus"), "unknown"},
		} {
			if got := tt.state.String(); got != tt.want {
				t.Errorf("State(%q).String() = %q, want %q", string(tt.state), got, tt.want)
			}
		}
	})
	t.Run("IsTerminal", func(t *testing.T) {
		t.Parallel()
		for _, tt := range []struct {
			state State
			want  bool
		}{
			{StateFailed, true},
			{StatePurged, true},
			{StateCrashed, false},
			{StateStopped, false},
			{StateRunning, false},
		} {
			if got := tt.state.IsTerminal(); got != tt.want {
				t.Errorf("State(%q).IsTerminal() = %t, want %t", string(tt.state), got, tt.want)
			}
		}
	})
	t.Run("Validate", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			for _, s := range []State{
				StatePending, StateBranching, StateProvisioning, StateStarting, StateRunning,
				StateWaiting, StateAsking, StateHasPlan, StatePulling, StatePushing,
				StateStopping, StateStopped, StatePurging, StateCrashed, StateFailed, StatePurged,
			} {
				if err := s.Validate(); err != nil {
					t.Errorf("Validate(%q) = %v, want nil", string(s), err)
				}
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			for _, s := range []State{"", "bogus", "Pending", "terminated"} {
				if err := s.Validate(); err == nil {
					t.Errorf("Validate(%q) = nil, want error", string(s))
				}
			}
		})
	})
	t.Run("UnmarshalJSON", func(t *testing.T) {
		t.Parallel()
		t.Run("valid", func(t *testing.T) {
			t.Parallel()
			var s State
			if err := json.Unmarshal([]byte(`"crashed"`), &s); err != nil {
				t.Fatal(err)
			}
			if s != StateCrashed {
				t.Errorf("s = %q, want %q", string(s), string(StateCrashed))
			}
		})
		t.Run("error", func(t *testing.T) {
			t.Parallel()
			var s State
			if err := json.Unmarshal([]byte(`"bogus"`), &s); err == nil {
				t.Error("Unmarshal(bogus) error = nil, want error")
			}
			if err := json.Unmarshal([]byte(`5`), &s); err == nil {
				t.Error("Unmarshal(5) error = nil, want error (not a string)")
			}
		})
	})
}
