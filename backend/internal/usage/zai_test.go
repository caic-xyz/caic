// Tests Z.ai credit grants parsing and the cached fetcher over an HTTP stub.

package usage

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func swapZaiURL(t *testing.T, url string) {
	t.Helper()
	orig := zaiCreditGrantsURL
	zaiCreditGrantsURL = url
	t.Cleanup(func() { zaiCreditGrantsURL = orig })
}

func TestParseZaiCreditGrants(t *testing.T) {
	t.Parallel()
	t.Run("OpenAI-style payload", func(t *testing.T) {
		t.Parallel()
		payload, err := parseZaiCreditGrants([]byte(`{"total_granted":18.0,"total_used":0.63,"total_available":17.37}`))
		if err != nil {
			t.Fatalf("parseZaiCreditGrants() err = %v", err)
		}
		if payload.TotalGranted != 18 || payload.TotalUsed != 0.63 || payload.TotalAvailable != 17.37 {
			t.Fatalf("payload = %#v, want granted 18 used 0.63 available 17.37", payload)
		}
	})

	t.Run("zero balance keeps payload", func(t *testing.T) {
		t.Parallel()
		payload, err := parseZaiCreditGrants([]byte(`{"total_granted":0,"total_used":0,"total_available":0}`))
		if err != nil {
			t.Fatalf("parseZaiCreditGrants() err = %v", err)
		}
		if payload.TotalAvailable != 0 {
			t.Fatalf("payload = %#v, want zero balance", payload)
		}
	})

	t.Run("monitor envelope", func(t *testing.T) {
		t.Parallel()
		payload, err := parseZaiCreditGrants([]byte(`{"code":200,"msg":"ok","data":{"total_granted":5,"total_used":1,"total_available":4}}`))
		if err != nil {
			t.Fatalf("parseZaiCreditGrants() err = %v", err)
		}
		if payload.TotalAvailable != 4 || payload.TotalGranted != 5 || payload.TotalUsed != 1 {
			t.Fatalf("payload = %#v, want available 4", payload)
		}
	})

	t.Run("unrecognized shape", func(t *testing.T) {
		t.Parallel()
		if _, err := parseZaiCreditGrants([]byte(`{"unexpected":true}`)); err == nil {
			t.Fatal("parseZaiCreditGrants() err = nil, want unrecognized-shape error")
		}
	})
}

func TestZaiFetcherGet(t *testing.T) {
	t.Parallel()
	t.Run("balance from stub server", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"total_granted":18.0,"total_used":0.63,"total_available":17.37}`))
		}))
		defer server.Close()
		swapZaiURL(t, server.URL)

		f := NewZaiFetcher("zai-key")
		if f == nil {
			t.Fatal("NewZaiFetcher(key) = nil")
		}
		quota := f.Get(t.Context())
		if quota == nil || quota.Provider != agent.QuotaProviderZai {
			t.Fatalf("Get() = %#v, want zai provider quota", quota)
		}
		if quota.Balance.Currency != "USD" || quota.Balance.Total != 17.37 || quota.Balance.Granted != 18 {
			t.Fatalf("balance = %#v, want USD 17.37 granted 18", quota.Balance)
		}
	})

	t.Run("empty key returns no fetcher", func(t *testing.T) {
		t.Parallel()
		if f := NewZaiFetcher(""); f != nil {
			t.Fatalf("NewZaiFetcher(\"\") = %v, want nil", f)
		}
	})
}
