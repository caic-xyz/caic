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
	c := &Checkout{Dir: "/repos/app", RelPath: "app", Repository: repository}
	if err := r.RegisterCheckout(c); err != nil {
		t.Fatal(err)
	}
	secondRepository := r.RegisterRepository(Repository{ForgeKind: forge.KindGitHub, ForgeOwner: "ORG", ForgeRepo: "APP"})
	second := &Checkout{Dir: "/repos/app-copy", RelPath: "app-copy", Repository: secondRepository}
	if err := r.RegisterCheckout(second); err != nil {
		t.Fatal(err)
	}
	if second.Repository != repository {
		t.Fatal("RegisterRepository did not canonicalize Repository")
	}
	if got, ok := r.Checkout("app"); !ok || got != c {
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
	if got := slices.Collect(r.Checkouts()); len(got) != 1 || got[0] != second {
		t.Fatalf("Checkouts() after removal = %#v", got)
	}
	if err := r.RegisterCheckout(&Checkout{Dir: "/repos/other", RelPath: "app-copy"}); err == nil || err.Error() != `checkout already registered at "app-copy"` {
		t.Fatalf("duplicate path error = %v", err)
	}
	if err := r.RegisterCheckout(&Checkout{Dir: "/repos/app-copy", RelPath: "other"}); err == nil || err.Error() != `checkout directory already registered at "/repos/app-copy"` {
		t.Fatalf("duplicate directory error = %v", err)
	}
	if err := r.RegisterCheckout(&Checkout{Dir: "/repos/unregistered", RelPath: "unregistered", Repository: &Repository{ForgeKind: forge.KindGitHub, ForgeOwner: "org", ForgeRepo: "unregistered"}}); err == nil || err.Error() != "checkout repository is not registered" {
		t.Fatalf("unregistered repository error = %v", err)
	}
}
