// Tests for voice gateway configuration validation.

package voicegateway

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	t.Parallel()
	t.Run("missing file returns defaults", func(t *testing.T) {
		t.Parallel()
		cfg, err := LoadConfig(filepath.Join(t.TempDir(), "config.toml"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Server.HTTP != ":3479" {
			t.Errorf("Server.HTTP = %q, want :3479", cfg.Server.HTTP)
		}
		if cfg.Server.WebRTCUDPPort != 0 {
			t.Errorf("Server.WebRTCUDPPort = %d, want 0", cfg.Server.WebRTCUDPPort)
		}
		if cfg.Model != "gemini-3.1-flash-live-preview" {
			t.Errorf("Model = %q, want gemini-3.1-flash-live-preview", cfg.Model)
		}
	})

	t.Run("parses static config", func(t *testing.T) {
		t.Parallel()
		publicKey := newTestPublicKey(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "config.toml")
		content := fmt.Sprintf(`
model = "gemini-live-test"

[server]
http = ":4444"
webrtc_udp_port = 4445

[[trusted_issuers]]
service = "caic"
issuer = "https://caic.example.com"
public_key = %q
`, publicKey)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		if cfg.Server.HTTP != ":4444" {
			t.Errorf("Server.HTTP = %q, want :4444", cfg.Server.HTTP)
		}
		if cfg.Model != "gemini-live-test" {
			t.Errorf("Model = %q, want gemini-live-test", cfg.Model)
		}
		if len(cfg.TrustedIssuers) != 1 || cfg.TrustedIssuers[0].Service != "caic" {
			t.Errorf("TrustedIssuers = %+v, want one caic issuer", cfg.TrustedIssuers)
		}
	})

	t.Run("parses custom backend", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "config.toml")
		content := `
backend = "local-stack"

[local_stack.asr]
provider = "llamacpp"
remote = "http://localhost:8090"
model = "asr-local"

[local_stack.llm]
provider = "llamacpp"
remote = "http://localhost:8080"
model = "gemma-local"
`
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		if cfg.Backend != BackendLocalStack {
			t.Errorf("Backend = %q, want %q", cfg.Backend, BackendLocalStack)
		}
		if cfg.LocalStack.ASR.Provider != "llamacpp" {
			t.Errorf("LocalStack.ASR.Provider = %q, want llamacpp", cfg.LocalStack.ASR.Provider)
		}
		if cfg.LocalStack.ASR.Remote != "http://localhost:8090" {
			t.Errorf("LocalStack.ASR.Remote = %q, want http://localhost:8090", cfg.LocalStack.ASR.Remote)
		}
		if cfg.LocalStack.ASR.Model != "asr-local" {
			t.Errorf("LocalStack.ASR.Model = %q, want asr-local", cfg.LocalStack.ASR.Model)
		}
		if cfg.LocalStack.LLM.Provider != "llamacpp" {
			t.Errorf("LocalStack.LLM.Provider = %q, want llamacpp", cfg.LocalStack.LLM.Provider)
		}
		if cfg.LocalStack.LLM.Remote != "http://localhost:8080" {
			t.Errorf("LocalStack.LLM.Remote = %q, want http://localhost:8080", cfg.LocalStack.LLM.Remote)
		}
		if cfg.LocalStack.LLM.Model != "gemma-local" {
			t.Errorf("LocalStack.LLM.Model = %q, want gemma-local", cfg.LocalStack.LLM.Model)
		}
	})

	t.Run("missing file defaults to gemini live", func(t *testing.T) {
		t.Parallel()
		cfg, err := LoadConfig(filepath.Join(t.TempDir(), "config.toml"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Backend != BackendGeminiLive {
			t.Errorf("Backend = %q, want %q", cfg.Backend, BackendGeminiLive)
		}
	})

	t.Run("unknown field error", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte("bogus = 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LoadConfig(path)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "missing in the target struct") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("removed local stack options are rejected", func(t *testing.T) {
		t.Parallel()
		for _, option := range []string{
			`cache_dir = "/tmp/caic-llama"`,
			`host_port = "127.0.0.1:9090"`,
			`threads = 4`,
			`build = 1234`,
			`extra_args = ["--jinja"]`,
		} {
			t.Run(option, func(t *testing.T) {
				t.Parallel()
				path := filepath.Join(t.TempDir(), "config.toml")
				content := fmt.Sprintf("[local_stack.llm]\n%s\n", option)
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
				_, err := LoadConfig(path)
				if err == nil {
					t.Fatal("expected error")
				}
				if !strings.Contains(err.Error(), "missing in the target struct") {
					t.Fatalf("unexpected error: %v", err)
				}
			})
		}
	})
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()
	t.Run("valid default", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("rejects invalid issuer URL", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.TrustedIssuers = []TrustedIssuerConfig{{
			Service:   "caic",
			Issuer:    "not a url",
			PublicKey: newTestPublicKey(t),
		}}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "trusted_issuers[0].issuer") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("allows disabled webrtc port", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.Server.WebRTCUDPPort = -1
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("embedded allows missing http", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.Server.HTTP = ""
		if err := cfg.ValidateEmbedded(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("rejects invalid webrtc port", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.Server.WebRTCUDPPort = -2
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "server.webrtc_udp_port") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("rejects invalid issuer public key", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.TrustedIssuers = []TrustedIssuerConfig{{
			Service:   "caic",
			Issuer:    "https://caic.example.com",
			PublicKey: "bogus",
		}}
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "trusted_issuers[0].public_key") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("rejects unknown backend", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.Backend = "made-up"
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), `backend "made-up"`) {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("rejects empty backend", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.Backend = ""
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "backend is required") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("allows local stack backend", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.Backend = BackendLocalStack
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("rejects local stack asr remote without provider", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.LocalStack.ASR.Remote = "http://localhost:8090"
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "local_stack.asr.provider") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("allows local stack asr remote with explicit provider", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.LocalStack.ASR.Provider = "llamacpp"
		cfg.LocalStack.ASR.Remote = "http://localhost:8090"
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("rejects invalid local stack asr remote", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.LocalStack.ASR.Provider = "llamacpp"
		cfg.LocalStack.ASR.Remote = "not a url"
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "local_stack.asr.remote") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("rejects unsupported local stack asr provider", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.LocalStack.ASR.Provider = "unknown"
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "local_stack.asr.provider") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("rejects local stack llm remote without provider", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.LocalStack.LLM.Remote = "http://localhost:8080"
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "local_stack.llm.provider") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("allows local stack llm remote with explicit provider", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.LocalStack.LLM.Provider = "llamacpp"
		cfg.LocalStack.LLM.Remote = "http://localhost:8080"
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("rejects invalid local stack llm remote", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.LocalStack.LLM.Provider = "llamacpp"
		cfg.LocalStack.LLM.Remote = "not a url"
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "local_stack.llm.remote") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("rejects unsupported local stack llm provider", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.LocalStack.LLM.Provider = "unknown"
		err := cfg.Validate()
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "local_stack.llm.provider") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("allows local stack provider from the genai providers registry", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.LocalStack.LLM.Provider = "openaicompatible"
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("allows trusted public key issuer", func(t *testing.T) {
		t.Parallel()
		cfg := DefaultConfig()
		cfg.TrustedIssuers = []TrustedIssuerConfig{{
			Service:   "caic",
			Issuer:    "https://caic.example.com",
			PublicKey: newTestPublicKey(t),
		}}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
	})
}
