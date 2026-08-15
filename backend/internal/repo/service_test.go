// Tests for repository service notifications.

package repo

import "testing"

func TestServiceChanged(t *testing.T) {
	t.Parallel()

	t.Run("registerCheckout notifies", func(t *testing.T) {
		t.Parallel()

		repositories := NewRegistry()
		s, err := NewService("", repositories)
		if err != nil {
			t.Fatal(err)
		}
		checkout := &Checkout{
			BaseBranch: "main",
			Dir:        "/repo",
			RelPath:    "repo",
		}
		ch := s.Changed()
		if err := s.RegisterCheckout(checkout); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ch:
		default:
			t.Fatal("Changed channel was not closed")
		}
	})
}

func TestNewServiceRequiresDependencies(t *testing.T) {
	t.Parallel()

	if _, err := NewService("", nil); err == nil || err.Error() != "repository registry is required" {
		t.Fatalf("NewService() error = %v, want repository registry is required", err)
	}
}
