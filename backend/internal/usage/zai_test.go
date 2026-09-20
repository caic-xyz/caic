// Tests Z.ai account report parsing and the cached fetcher over an HTTP stub.

package usage

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

func swapZaiURL(t *testing.T, url string) {
	t.Helper()
	orig := zaiAccountReportURL
	zaiAccountReportURL = url
	t.Cleanup(func() { zaiAccountReportURL = orig })
}

func TestParseZaiAccountReport(t *testing.T) {
	t.Parallel()
	t.Run("envelope with full report", func(t *testing.T) {
		t.Parallel()
		body := []byte(`{"code":200,"msg":"ok","success":true,"data":{
			"balance":5.17,"rechargeAmount":10.0,"giveAmount":0.0,
			"totalSpendAmount":4.83,"todaySpendAmount":null,
			"availableBalance":5.17,"frozenBalance":0,"creditBalance":null,
			"creditStatus":"NOT_OPEN","modelSpendAmountList":null,"isKA":false}}`)
		payload, err := parseZaiAccountReport(body)
		if err != nil {
			t.Fatalf("parseZaiAccountReport() err = %v", err)
		}
		if payload.Balance == nil || *payload.Balance != 5.17 {
			t.Fatalf("payload.Balance = %v, want 5.17", payload.Balance)
		}
		if payload.RechargeAmount == nil || *payload.RechargeAmount != 10 {
			t.Fatalf("payload.RechargeAmount = %v, want 10", payload.RechargeAmount)
		}
	})

	t.Run("top-level report", func(t *testing.T) {
		t.Parallel()
		payload, err := parseZaiAccountReport([]byte(`{"balance":2.5,"giveAmount":1,"rechargeAmount":1.5}`))
		if err != nil {
			t.Fatalf("parseZaiAccountReport() err = %v", err)
		}
		if payload.Balance == nil || *payload.Balance != 2.5 {
			t.Fatalf("payload.Balance = %v, want 2.5", payload.Balance)
		}
	})

	t.Run("zero balance keeps payload", func(t *testing.T) {
		t.Parallel()
		payload, err := parseZaiAccountReport([]byte(`{"code":200,"data":{"balance":0,"availableBalance":0,"giveAmount":0,"rechargeAmount":0}}`))
		if err != nil {
			t.Fatalf("parseZaiAccountReport() err = %v", err)
		}
		if payload.Balance == nil || *payload.Balance != 0 {
			t.Fatalf("payload.Balance = %v, want 0", payload.Balance)
		}
	})

	t.Run("unrecognized shape", func(t *testing.T) {
		t.Parallel()
		if _, err := parseZaiAccountReport([]byte(`{"unexpected":true}`)); err == nil {
			t.Fatal("parseZaiAccountReport() err = nil, want unrecognized-shape error")
		}
	})
}

func TestZaiFetcherGet(t *testing.T) {
	t.Parallel()
	t.Run("balance from stub server", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":200,"msg":"ok","data":{
				"balance":5.17,"rechargeAmount":10.0,"giveAmount":1.0,
				"totalSpendAmount":5.83,"availableBalance":5.17}}`))
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
		if quota.Balance.Currency != "USD" || quota.Balance.Total != 5.17 || quota.Balance.Granted != 1 || quota.Balance.ToppedUp != 10 {
			t.Fatalf("balance = %#v, want USD 5.17 granted 1 topped up 10", quota.Balance)
		}
	})

	t.Run("empty key returns no fetcher", func(t *testing.T) {
		t.Parallel()
		if f := NewZaiFetcher(""); f != nil {
			t.Fatalf("NewZaiFetcher(\"\") = %v, want nil", f)
		}
	})
}
