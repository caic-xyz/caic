// Tests for OAuth authorization-code client helpers.

package oauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNewPKCEChallenge(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		c, err := NewPKCEChallenge()
		if err != nil {
			t.Fatalf("NewPKCEChallenge: %v", err)
		}
		if c.Verifier == "" || c.Challenge == "" {
			t.Fatalf("empty verifier or challenge: %+v", c)
		}
		if c.Verifier == c.Challenge {
			t.Fatal("challenge equals verifier; S256 not applied")
		}
		// Two calls must not collide.
		c2, err := NewPKCEChallenge()
		if err != nil {
			t.Fatalf("NewPKCEChallenge: %v", err)
		}
		if c.Verifier == c2.Verifier {
			t.Fatal("two challenges share a verifier")
		}
	})
}

func TestAuthorizationURL(t *testing.T) {
	t.Parallel()
	const endpoint = "https://provider.example/authorize"

	t.Run("no pkce", func(t *testing.T) {
		t.Parallel()
		got := AuthorizationURL(endpoint, "client-1", "https://app/cb", []string{"a", "b"}, "state-1", "")
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("parse url: %v", err)
		}
		q := u.Query()
		if q.Has("code_challenge") || q.Has("code_challenge_method") {
			t.Fatalf("PKCE params present without challenge: %s", got)
		}
		if q.Get("response_type") != ResponseTypeCode {
			t.Fatalf("response_type = %q", q.Get("response_type"))
		}
	})

	t.Run("with pkce", func(t *testing.T) {
		t.Parallel()
		got := AuthorizationURL(endpoint, "client-1", "https://app/cb", []string{"a"}, "state-1", "chal-xyz")
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("parse url: %v", err)
		}
		q := u.Query()
		if q.Get("code_challenge") != "chal-xyz" {
			t.Fatalf("code_challenge = %q, want chal-xyz", q.Get("code_challenge"))
		}
		if q.Get("code_challenge_method") != "S256" {
			t.Fatalf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
		}
	})
}

func TestExchangeCode(t *testing.T) {
	t.Parallel()
	t.Run("valid no pkce", func(t *testing.T) {
		t.Parallel()
		var gotVerifier string
		var hadVerifier bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			gotVerifier, hadVerifier = r.PostForm.Get("code_verifier"), r.PostForm.Has("code_verifier")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600}`))
		}))
		t.Cleanup(srv.Close)

		cfg := ClientConfig{ClientID: "c", ClientSecret: "s", TokenURL: srv.URL, RedirectURI: "https://app/cb"}
		access, refresh, expiry, err := ExchangeCode(t.Context(), cfg, "code-1", "")
		if err != nil {
			t.Fatalf("ExchangeCode: %v", err)
		}
		if access != "at" || refresh != "rt" {
			t.Fatalf("tokens = %q/%q", access, refresh)
		}
		if expiry.IsZero() {
			t.Fatal("expiry is zero")
		}
		if hadVerifier {
			t.Fatalf("code_verifier sent without PKCE: %q", gotVerifier)
		}
	})

	t.Run("valid with pkce", func(t *testing.T) {
		t.Parallel()
		var gotVerifier string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			gotVerifier = r.PostForm.Get("code_verifier")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"at"}`))
		}))
		t.Cleanup(srv.Close)

		cfg := ClientConfig{ClientID: "c", ClientSecret: "s", TokenURL: srv.URL, RedirectURI: "https://app/cb"}
		if _, _, _, err := ExchangeCode(t.Context(), cfg, "code-1", "verifier-abc"); err != nil {
			t.Fatalf("ExchangeCode: %v", err)
		}
		if gotVerifier != "verifier-abc" {
			t.Fatalf("code_verifier = %q, want verifier-abc", gotVerifier)
		}
	})

	t.Run("error does not leak provider body", func(t *testing.T) {
		t.Parallel()
		const secretBody = "SENSITIVE-PROVIDER-DETAIL-12345"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(secretBody))
		}))
		t.Cleanup(srv.Close)

		cfg := ClientConfig{ClientID: "c", ClientSecret: "s", TokenURL: srv.URL, RedirectURI: "https://app/cb"}
		_, _, _, err := ExchangeCode(t.Context(), cfg, "code-1", "")
		if err == nil {
			t.Fatal("ExchangeCode on 400 = nil error, want error")
		}
		if strings.Contains(err.Error(), secretBody) {
			t.Fatalf("error leaks provider body: %v", err)
		}
		if !strings.Contains(err.Error(), "400") {
			t.Fatalf("error should mention status: %v", err)
		}
	})
}

func TestExchangeTimeout(t *testing.T) {
	t.Parallel()
	t.Run("hung provider is bounded by the request context", func(t *testing.T) {
		t.Parallel()
		// A server that never responds; the caller's short deadline (smaller
		// than exchangeTimeout) must win and ExchangeCode must return promptly.
		// done is closed before srv.Close (defer LIFO) so the blocked handler
		// returns and Close does not wait on it.
		done := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			<-done
		}))
		defer srv.Close()
		defer close(done)

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, _, _, err := ExchangeCode(ctx, ClientConfig{TokenURL: srv.URL}, "code", "")
		if err == nil {
			t.Fatal("ExchangeCode succeeded against a hung provider, want timeout error")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("ExchangeCode took %v, expected to be bounded near the context deadline", elapsed)
		}
	})
}
