// Benchmarks bounded OAuth request parsing on token request paths.

package oauthserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func BenchmarkOAuthTokenInputValidation(b *testing.B) {
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
	const body = "grant_type=authorization_code&code=missing&client_id=unknown"

	b.ReportAllocs()
	b.StopTimer()
	requests := make([]*http.Request, b.N)
	responses := make([]*httptest.ResponseRecorder, b.N)
	for i := range b.N {
		requests[i] = httptest.NewRequestWithContext(b.Context(), http.MethodPost, "/oauth/token", strings.NewReader(body))
		requests[i].Header.Set("Content-Type", "application/x-www-form-urlencoded")
		responses[i] = httptest.NewRecorder()
	}
	b.StartTimer()
	for i := range b.N {
		request := requests[i]
		response := responses[i]
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			b.Fatalf("status = %d: %s", response.Code, response.Body.String())
		}
	}
}
