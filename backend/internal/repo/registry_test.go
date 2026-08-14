// Tests repository identity and checkout registration.

package repo

import (
	"testing"

	"github.com/caic-xyz/caic/backend/internal/logtest"
)

func TestRegistry(t *testing.T) {
	t.Parallel()
	r := NewRegistry(t.Context(), nil)
	c := &Checkout{Dir: "/repos/app", Log: logtest.Logger(t)}
	r.Register(&Repository{RelPath: "app", AbsPath: "/repos/app", ForgeOwner: "org", ForgeRepo: "app"}, c)
	if got, ok := r.Repository("app"); !ok || got.AbsPath != "/repos/app" {
		t.Fatalf("Repository() = %#v, %v", got, ok)
	}
	if got, ok := r.Checkout("app"); !ok || got != c {
		t.Fatalf("Checkout() = %p, %v", got, ok)
	}
	if c.RepoName != "app" {
		t.Fatalf("RepoName = %q, want app", c.RepoName)
	}
	seen := false
	for checkout := range r.Checkouts() {
		seen = true
		if checkout != c {
			t.Fatalf("Checkouts() yielded %p, want %p", checkout, c)
		}
	}
	if !seen {
		t.Fatal("Checkouts() yielded no checkout")
	}
	if got, ok := r.RepositoryByForge("ORG", "APP"); !ok || got.RelPath != "app" {
		t.Fatalf("RepositoryByForge() = %#v, %v", got, ok)
	}
	if !r.Remove("app") {
		t.Fatal("Remove() = false")
	}
	if _, ok := r.Checkout("app"); ok {
		t.Fatal("checkout remains after Remove")
	}
}
