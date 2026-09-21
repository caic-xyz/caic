// Tests OpenRouter live per-model pricing and its pricer wiring over HTTP stubs.

package usage

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/maruel/genai"
	"github.com/maruel/genai/providers/openrouter"

	"github.com/caic-xyz/caic/backend/internal/agent"
)

const openRouterModelsPayload = `{"data":[
	{"id":"z-ai/glm-5.3-flash","pricing":{"prompt":"0.00000015","completion":"0.0000005","input_cache_read":"0.00000003","input_cache_write":"0.00000015"}},
	{"id":"mistralai/mistral-small","pricing":{"prompt":"0.0000001","completion":"0.0000003"}}
]}`

// redirectTransport rewrites every request to the stub server, so the genai
// client's fixed OpenRouter URL can be served by httptest.
type redirectTransport struct{ url string }

func (t redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = "http"
	req.URL.Host = t.url[len("http://"):]
	req.Host = req.URL.Host
	return http.DefaultTransport.RoundTrip(req)
}

// stubOpenRouterFetcher builds a fetcher whose pricing requests are
// redirected to server.
func stubOpenRouterFetcher(t *testing.T, server *httptest.Server) *OpenRouterFetcher {
	models, err := openrouter.New(t.Context(),
		genai.ProviderOptionAPIKey("test-key"),
		genai.ProviderOptionTransportWrapper(func(rt http.RoundTripper) http.RoundTripper {
			return redirectTransport{server.URL}
		}))
	if err != nil {
		t.Fatal(err)
	}
	return &OpenRouterFetcher{
		baseFetcher: newBaseFetcher(agent.QuotaProviderOpenRouter, AuthKindAPIKey, ""),
		models:      models,
	}
}

func TestOpenRouterFetcherModelPrice(t *testing.T) {
	t.Parallel()
	t.Run("FetchAndLookup", func(t *testing.T) {
		t.Parallel()
		var hits int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits++
			_, _ = w.Write([]byte(openRouterModelsPayload))
		}))
		t.Cleanup(server.Close)
		f := stubOpenRouterFetcher(t, server)

		price, ok := f.ModelPrice(agent.QuotaProviderOpenRouter, "z-ai/glm-5.3-flash", time.Now())
		if !ok {
			t.Fatal("priced model not found")
		}
		want := ModelPrice{InputPerMTok: 0.15, CachedInputPerMTok: 0.03, CacheWritePerMTok: 0.15, OutputPerMTok: 0.50}
		if price != want {
			t.Errorf("price = %+v, want %+v", price, want)
		}

		// Variant suffixes resolve to the base model; lookups stay cached.
		if _, ok := f.ModelPrice(agent.QuotaProviderOpenRouter, "z-ai/glm-5.3-flash:free", time.Now()); !ok {
			t.Error("variant-suffixed model not priced")
		}
		if hits != 1 {
			t.Errorf("models endpoint hit %d times, want 1 (cached)", hits)
		}
	})
	t.Run("FetchErrorBackoff", func(t *testing.T) {
		t.Parallel()
		var hits int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits++
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(server.Close)
		f := stubOpenRouterFetcher(t, server)

		if _, ok := f.ModelPrice(agent.QuotaProviderOpenRouter, "z-ai/glm-5.3-flash", time.Now()); ok {
			t.Error("priced a model despite fetch failure")
		}
		if _, ok := f.ModelPrice(agent.QuotaProviderOpenRouter, "z-ai/glm-5.3-flash", time.Now()); ok {
			t.Error("priced a model on retry within backoff window")
		}
		if hits != 1 {
			t.Errorf("models endpoint hit %d times, want 1 (backed off)", hits)
		}
	})
}
