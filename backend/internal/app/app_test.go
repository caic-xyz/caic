// Tests that App closes the genai providers it owns at startup failure and shutdown.

package app

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/maruel/genai"
	"github.com/maruel/genai/providers"

	"github.com/caic-xyz/caic/backend/internal/runtime/runtimetest"
	"github.com/caic-xyz/caic/backend/internal/server"
	"github.com/caic-xyz/caic/backend/internal/usage"
)

// closeSpyProvider is a genai.Provider stub recording whether Close was called.
type closeSpyProvider struct {
	genai.Provider

	closed bool
}

// Name and ModelID stand in for the provider methods initProvider logs.
func (p *closeSpyProvider) Name() string    { return "close-spy" }
func (p *closeSpyProvider) ModelID() string { return "close-spy-model" }

func (p *closeSpyProvider) Close() error {
	p.closed = true
	return nil
}

// registerCloseSpyProvider registers a provider whose factory returns spy. The
// factory requires the API key option that initProvider reads from the
// environment, so the caller must pin TEST_KEY.
func registerCloseSpyProvider(t *testing.T, spy *closeSpyProvider) string {
	const name = "test-provider-close"
	providers.All[name] = providers.Config{
		APIKeyEnvVar: "TEST_KEY",
		Factory: func(_ context.Context, opts ...genai.ProviderOption) (genai.Provider, error) {
			if !slices.ContainsFunc(opts, func(o genai.ProviderOption) bool {
				return o == genai.ProviderOptionAPIKey("test-key")
			}) {
				return nil, errors.New("API key is required")
			}
			return spy, nil
		},
	}
	t.Cleanup(func() { delete(providers.All, name) })
	return name
}

// closeSpyConfig builds a hermetic server config whose LLM is the spy provider.
func closeSpyConfig(t *testing.T, providerName string) *server.Config {
	// A fake runtime and a CIDR-only ipgeo allowlist keep New hermetic: no
	// containers, no database, and no named-origin network fetches.
	return &server.Config{
		Dirs:          server.DirsConfig{ConfigDir: t.TempDir(), CacheDir: t.TempDir()},
		Runtime:       server.RuntimeConfig{System: &runtimetest.FakeSystem{}, SkipWarmup: true},
		LLM:           server.LLMConfig{Provider: providerName},
		IPGeo:         server.IPGeoConfig{Allowlist: "0.0.0.0/0,::/0"},
		UsageFetchers: []usage.ProviderFetcher{},
	}
}

func TestApp(t *testing.T) {
	// Registry mutation requires a non-parallel test.
	t.Run("NewClosesProviderOnStartupFailure", func(t *testing.T) {
		t.Setenv("TEST_KEY", "test-key")
		spy := &closeSpyProvider{}
		cfg := closeSpyConfig(t, registerCloseSpyProvider(t, spy))
		// An unreadable IPGeo database makes New return an error after it has
		// registered the provider, exercising the startup-failure close path.
		cfg.IPGeo.DB = filepath.Join(t.TempDir(), "missing.mmdb")

		_, err := New(t.Context(), slog.New(slog.DiscardHandler), t.TempDir(), cfg)
		if err == nil || !strings.Contains(err.Error(), "ipgeo") {
			t.Fatalf("New() = %v, want ipgeo startup failure", err)
		}
		if !spy.closed {
			t.Error("provider was not closed after startup failure")
		}
	})

	t.Run("ServeClosesProviderAtShutdown", func(t *testing.T) {
		t.Setenv("TEST_KEY", "test-key")
		spy := &closeSpyProvider{}
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		srv, err := New(ctx, slog.New(slog.DiscardHandler), t.TempDir(), closeSpyConfig(t, registerCloseSpyProvider(t, spy)))
		if err != nil {
			t.Fatalf("New() = %v", err)
		}
		var lc net.ListenConfig
		ln, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- srv.Serve(ctx, ln) }()
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Serve() = %v", err)
		}
		if !spy.closed {
			t.Error("provider was not closed when serving stopped")
		}
	})
}
