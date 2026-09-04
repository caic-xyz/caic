// Benchmarks dynamic OAuth client registration metadata validation.

package oauthserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func BenchmarkOAuthRegistrationValidation(b *testing.B) {
	server, err := NewServer(ServerConfig{
		KeyPEM:          testSigningKeyPEM,
		KeyID:           "benchmark-key",
		Issuer:          testBaseURL,
		ResourceURLPath: "/resource",
		Session:         &testSessionManager{},
		UI:              &testAuthorizationUI{},
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(server.Close)
	handler := server.Routes()
	const body = `{"client_name":"Benchmark","redirect_uris":["https://example.com/callback"],"token_endpoint_auth_method":"private_key_jwt"}`

	b.ReportAllocs()
	b.StopTimer()
	requests := make([]*http.Request, b.N)
	responses := make([]*httptest.ResponseRecorder, b.N)
	for i := range b.N {
		requests[i] = httptest.NewRequestWithContext(b.Context(), http.MethodPost, "/oauth/register", strings.NewReader(body))
		requests[i].Header.Set("Content-Type", "application/json")
		responses[i] = httptest.NewRecorder()
	}
	b.StartTimer()
	for i := range b.N {
		handler.ServeHTTP(responses[i], requests[i])
		if responses[i].Code != http.StatusBadRequest {
			b.Fatalf("status = %d: %s", responses[i].Code, responses[i].Body.String())
		}
	}
}
