// Tests repository identity and checkout registration.

package repo

import (
	"slices"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/forge"
)

func TestRegistry(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	repository := r.RegisterRepository(Repository{ForgeKind: forge.KindGitHub, ForgeOwner: "org", ForgeRepo: "app"})
	if _, ok := r.RepositoryByForge("org", "app"); !ok {
		t.Fatal("repository-only entry was not registered")
	}
	c := &Checkout{Dir: "/repos/app", RelPath: "app", Repository: &Repository{ForgeKind: forge.KindGitHub, ForgeOwner: "ORG", ForgeRepo: "APP"}}
	changed := r.Changed()
	if err := r.RegisterCheckout(c); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("RegisterCheckout did not notify")
	}
	second := &Checkout{Dir: "/repos/app-copy", RelPath: "app-copy", Repository: &Repository{ForgeKind: forge.KindGitHub, ForgeOwner: "ORG", ForgeRepo: "APP"}}
	if err := r.RegisterCheckout(second); err != nil {
		t.Fatal(err)
	}
	if got, ok := r.Checkout("app"); !ok || got != c || got.Repository != repository {
		t.Fatalf("Checkout() = %p, %v", got, ok)
	}
	checkouts := slices.Collect(r.Checkouts())
	if len(checkouts) != 2 {
		t.Fatalf("Checkouts() count = %d, want 2", len(checkouts))
	}
	if got, ok := r.RepositoryByForge("ORG", "APP"); !ok || got != repository {
		t.Fatalf("RepositoryByForge() = %#v, %v", got, ok)
	}
	if !r.UnregisterCheckout("app") {
		t.Fatal("UnregisterCheckout() = false")
	}
	if _, ok := r.Checkout("app"); ok {
		t.Fatal("checkout remains after UnregisterCheckout")
	}
	if got := r.Repositories(); len(got) != 1 || got[0] != repository {
		t.Fatalf("Repositories() = %#v", got)
	}
	if got := slices.Collect(r.Checkouts()); len(got) != 1 || got[0] != second || got[0].Repository != repository {
		t.Fatalf("Checkouts() after removal = %#v", got)
	}
	changed = r.Changed()
	if err := r.RegisterCheckout(&Checkout{Dir: "/repos/other", RelPath: "app-copy"}); err == nil || err.Error() != `checkout already registered at "app-copy"` {
		t.Fatalf("duplicate path error = %v", err)
	}
	if err := r.RegisterCheckout(&Checkout{Dir: "/repos/app-copy", RelPath: "other"}); err == nil || err.Error() != `checkout directory already registered at "/repos/app-copy"` {
		t.Fatalf("duplicate directory error = %v", err)
	}
	select {
	case <-changed:
		t.Fatal("failed registration notified")
	default:
	}
}
