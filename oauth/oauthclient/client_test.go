// Tests for OAuth authorization-code client helpers.

package oauthclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/caic-xyz/caic/oauth"
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
		got := AuthorizationURL(endpoint, "client-1", "https://app/cb", []string{"a", "b"}, "state-1", "", nil)
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("parse url: %v", err)
		}
		q := u.Query()
		if q.Has("code_challenge") || q.Has("code_challenge_method") {
			t.Fatalf("PKCE params present without challenge: %s", got)
		}
		if q.Get("response_type") != oauth.ResponseTypeCode {
			t.Fatalf("response_type = %q", q.Get("response_type"))
		}
	})

	t.Run("with pkce", func(t *testing.T) {
		t.Parallel()
		got := AuthorizationURL(endpoint, "client-1", "https://app/cb", []string{"a"}, "state-1", "chal-xyz", nil)
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

	t.Run("extra params", func(t *testing.T) {
		t.Parallel()
		extra := url.Values{"access_type": {"offline"}, "prompt": {"consent"}}
		got := AuthorizationURL(endpoint, "client-1", "https://app/cb", []string{"a"}, "state-1", "", extra)
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("parse url: %v", err)
		}
		q := u.Query()
		if q.Get("access_type") != "offline" {
			t.Fatalf("access_type = %q, want offline", q.Get("access_type"))
		}
		if q.Get("prompt") != "consent" {
			t.Fatalf("prompt = %q, want consent", q.Get("prompt"))
		}
		if q.Get("client_id") != "client-1" {
			t.Fatalf("client_id = %q, want client-1", q.Get("client_id"))
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

		cfg := oauth.ClientConfig{ClientID: "c", ClientSecret: "s", TokenURL: srv.URL, RedirectURI: "https://app/cb"}
		tok, err := ExchangeCode(t.Context(), cfg, "code-1", "")
		if err != nil {
			t.Fatalf("ExchangeCode: %v", err)
		}
		if tok.AccessToken != "at" || tok.RefreshToken != "rt" {
			t.Fatalf("tokens = %q/%q", tok.AccessToken, tok.RefreshToken)
		}
		if tok.Expiry.IsZero() {
			t.Fatal("expiry is zero")
		}
		if tok.TokenType != "Bearer" {
			t.Fatalf("TokenType = %q, want Bearer", tok.TokenType)
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

		cfg := oauth.ClientConfig{ClientID: "c", ClientSecret: "s", TokenURL: srv.URL, RedirectURI: "https://app/cb"}
		if _, err := ExchangeCode(t.Context(), cfg, "code-1", "verifier-abc"); err != nil {
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

		cfg := oauth.ClientConfig{ClientID: "c", ClientSecret: "s", TokenURL: srv.URL, RedirectURI: "https://app/cb"}
		_, err := ExchangeCode(t.Context(), cfg, "code-1", "")
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

		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := ExchangeCode(ctx, oauth.ClientConfig{TokenURL: srv.URL}, "code", "")
		if err == nil {
			t.Fatal("ExchangeCode succeeded against a hung provider, want timeout error")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("ExchangeCode took %v, expected to be bounded near the context deadline", elapsed)
		}
	})
}

// TestRetrieveError verifies structured error parsing from RFC 6749 error responses.
func TestRetrieveError(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":             "invalid_grant",
				"error_description": "The authorization code has expired",
				"error_uri":         "https://provider.example/errors#invalid_grant",
			})
		}))
		t.Cleanup(srv.Close)

		cfg := oauth.ClientConfig{ClientID: "c", ClientSecret: "s", TokenURL: srv.URL, RedirectURI: "https://app/cb"}
		_, err := ExchangeCode(t.Context(), cfg, "code-1", "")
		if err == nil {
			t.Fatal("ExchangeCode succeeded, want error")
		}
		rerr, ok := errors.AsType[*RetrieveError](err)
		if !ok {
			t.Fatalf("error is not *RetrieveError: %T %v", err, err)
		}
		if rerr.StatusCode != http.StatusBadRequest {
			t.Fatalf("StatusCode = %d, want %d", rerr.StatusCode, http.StatusBadRequest)
		}
		if rerr.ErrorCode != "invalid_grant" {
			t.Fatalf("ErrorCode = %q, want invalid_grant", rerr.ErrorCode)
		}
		if rerr.ErrorDescription != "The authorization code has expired" {
			t.Fatalf("ErrorDescription = %q", rerr.ErrorDescription)
		}
		if rerr.ErrorURI != "https://provider.example/errors#invalid_grant" {
			t.Fatalf("ErrorURI = %q", rerr.ErrorURI)
		}
		// Body is accessible for programmatic inspection.
		if len(rerr.Body) == 0 {
			t.Fatal("Body is empty")
		}
		// Error() message must not contain the raw body.
		if strings.Contains(rerr.Error(), `"error"`) || strings.Contains(rerr.Error(), `error_description`) {
			t.Fatalf("Error() must not contain raw JSON body, got: %s", rerr.Error())
		}
		// Error() must contain the key fields.
		if !strings.Contains(rerr.Error(), "400") || !strings.Contains(rerr.Error(), "invalid_grant") {
			t.Fatalf("Error() is missing status or error code: %s", rerr.Error())
		}
	})

	t.Run("error non-json body preserves raw body", func(t *testing.T) {
		t.Parallel()
		const htmlBody = "<html><body>500 Internal Server Error</body></html>"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(htmlBody))
		}))
		t.Cleanup(srv.Close)

		cfg := oauth.ClientConfig{ClientID: "c", ClientSecret: "s", TokenURL: srv.URL, RedirectURI: "https://app/cb"}
		_, err := ExchangeCode(t.Context(), cfg, "code-1", "")
		rerr, ok := errors.AsType[*RetrieveError](err)
		if !ok {
			t.Fatalf("error is not *RetrieveError: %T %v", err, err)
		}
		if rerr.StatusCode != http.StatusInternalServerError {
			t.Fatalf("StatusCode = %d", rerr.StatusCode)
		}
		if string(rerr.Body) != htmlBody {
			t.Fatalf("Body = %q, want %q", string(rerr.Body), htmlBody)
		}
		if rerr.ErrorCode != "" {
			t.Fatalf("ErrorCode = %q, want empty (non-JSON body)", rerr.ErrorCode)
		}
	})
}

// TestExchangeCodeFormEncodedFallback verifies that a form-urlencoded token
// response is parsed correctly when JSON parsing fails.
func TestExchangeCodeFormEncodedFallback(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/x-www-form-urlencoded")
			_, _ = w.Write([]byte("access_token=at-form&refresh_token=rt-form&expires_in=1800&token_type=Bearer"))
		}))
		t.Cleanup(srv.Close)

		cfg := oauth.ClientConfig{ClientID: "c", ClientSecret: "s", TokenURL: srv.URL, RedirectURI: "https://app/cb"}
		tok, err := ExchangeCode(t.Context(), cfg, "code-1", "")
		if err != nil {
			t.Fatalf("ExchangeCode: %v", err)
		}
		if tok.AccessToken != "at-form" {
			t.Fatalf("AccessToken = %q, want at-form", tok.AccessToken)
		}
		if tok.RefreshToken != "rt-form" {
			t.Fatalf("RefreshToken = %q, want rt-form", tok.RefreshToken)
		}
		if tok.TokenType != "Bearer" {
			t.Fatalf("TokenType = %q, want Bearer", tok.TokenType)
		}
		if tok.Expiry.IsZero() {
			t.Fatal("Expiry is zero")
		}
	})

	t.Run("error form-encoded fallback returns form error", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/x-www-form-urlencoded")
			_, _ = w.Write([]byte("error=invalid_client&error_description=Client+not+found"))
		}))
		t.Cleanup(srv.Close)

		cfg := oauth.ClientConfig{ClientID: "c", ClientSecret: "s", TokenURL: srv.URL, RedirectURI: "https://app/cb"}
		_, err := ExchangeCode(t.Context(), cfg, "code-1", "")
		if err == nil {
			t.Fatal("ExchangeCode succeeded, want error")
		}
		rerr, ok := errors.AsType[*RetrieveError](err)
		if !ok {
			t.Fatalf("error is not *RetrieveError: %T %v", err, err)
		}
		if rerr.ErrorCode != "invalid_client" {
			t.Fatalf("ErrorCode = %q, want invalid_client", rerr.ErrorCode)
		}
		if rerr.ErrorDescription != "Client not found" {
			t.Fatalf("ErrorDescription = %q, want 'Client not found'", rerr.ErrorDescription)
		}
	})
}

// TestRefreshAccessToken tests token refresh via grant_type=refresh_token.
func TestRefreshAccessToken(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		var grantType string
		var refreshToken string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			grantType = r.PostForm.Get("grant_type")
			refreshToken = r.PostForm.Get("refresh_token")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"new-at","refresh_token":"new-rt","expires_in":3600}`))
		}))
		t.Cleanup(srv.Close)

		cfg := oauth.ClientConfig{ClientID: "c", ClientSecret: "s", TokenURL: srv.URL}
		tok, err := RefreshAccessToken(t.Context(), cfg, "old-rt")
		if err != nil {
			t.Fatalf("RefreshAccessToken: %v", err)
		}
		if grantType != oauth.GrantRefreshToken {
			t.Fatalf("grant_type = %q, want %q", grantType, oauth.GrantRefreshToken)
		}
		if refreshToken != "old-rt" {
			t.Fatalf("refresh_token = %q, want old-rt", refreshToken)
		}
		if tok.AccessToken != "new-at" || tok.RefreshToken != "new-rt" {
			t.Fatalf("tokens = %q/%q, want new-at/new-rt", tok.AccessToken, tok.RefreshToken)
		}
		if tok.TokenType != "Bearer" {
			t.Fatalf("TokenType = %q, want Bearer", tok.TokenType)
		}
	})

	t.Run("error returns RetrieveError", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":             "invalid_grant",
				"error_description": "Refresh token has expired",
			})
		}))
		t.Cleanup(srv.Close)

		cfg := oauth.ClientConfig{ClientID: "c", ClientSecret: "s", TokenURL: srv.URL}
		_, err := RefreshAccessToken(t.Context(), cfg, "expired-rt")
		rerr, ok := errors.AsType[*RetrieveError](err)
		if !ok {
			t.Fatalf("error is not *RetrieveError: %T %v", err, err)
		}
		if rerr.ErrorCode != "invalid_grant" {
			t.Fatalf("ErrorCode = %q", rerr.ErrorCode)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		t.Parallel()
		done := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			<-done
		}))
		defer srv.Close()
		defer close(done)

		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := RefreshAccessToken(ctx, oauth.ClientConfig{TokenURL: srv.URL}, "some-rt")
		if err == nil {
			t.Fatal("RefreshAccessToken succeeded against hung server, want timeout error")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("RefreshAccessToken took %v, expected bounded near 100ms", elapsed)
		}
	})
}

// TestTokenExpired verifies Token.Expired with the built-in expiry delta.
func TestTokenExpired(t *testing.T) {
	t.Parallel()

	t.Run("zero expiry never expires", func(t *testing.T) {
		t.Parallel()
		tok := oauth.Token{}
		if tok.Expired() {
			t.Fatal("zero Expiry should not be expired")
		}
	})

	t.Run("far future not expired", func(t *testing.T) {
		t.Parallel()
		tok := oauth.Token{Expiry: time.Now().Add(time.Hour)}
		if tok.Expired() {
			t.Fatal("1h-away Expiry should not be expired")
		}
	})

	t.Run("past expiry is expired", func(t *testing.T) {
		t.Parallel()
		tok := oauth.Token{Expiry: time.Now().Add(-time.Hour)}
		if !tok.Expired() {
			t.Fatal("past Expiry should be expired")
		}
	})

	t.Run("default delta triggers early expiry", func(t *testing.T) {
		t.Parallel()
		// Expiry is 30s away; DefaultExpiryDelta is 10s.
		// Effective expiry = 30s - 10s = 20s from now → not expired.
		tok := oauth.Token{Expiry: time.Now().Add(30 * time.Second)}
		if tok.Expired() {
			t.Fatal("30s-away expiry should not be expired with 10s default delta")
		}
		// Expiry is 5s away → effective expiry is 5s ago → expired.
		tok = oauth.Token{Expiry: time.Now().Add(5 * time.Second)}
		if !tok.Expired() {
			t.Fatal("5s-away expiry should be expired with 10s default delta")
		}
	})
}
