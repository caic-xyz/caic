// Benchmarks cached CIMD resolution and private-key client assertion verification.

package oauthserver

import (
	"testing"
	"time"

	"github.com/caic-xyz/caic/oauth"
)

func BenchmarkClientMetadataResolution(b *testing.B) {
	clientID := "https://client.example/client.json"
	resolver := newClientMetadataResolver(nil, nil)
	resolver.clients[clientID] = clientMetadataCacheEntry{
		client: Client{
			ID:                      clientID,
			Name:                    "Benchmark Client",
			RedirectURIs:            []string{"https://client.example/callback"},
			TokenEndpointAuthMethod: oauth.TokenEndpointAuthNone,
			GrantTypes:              []string{oauth.GrantAuthorizationCode},
			Provenance:              ClientProvenanceMetadata,
		},
		expiresAt: time.Now().Add(time.Hour),
	}
	b.ReportAllocs()
	for b.Loop() {
		client, err := resolver.resolve(b.Context(), clientID)
		if err != nil || client.ID != clientID {
			b.Fatalf("resolve = %#v, %v", client, err)
		}
	}
}

func BenchmarkPrivateKeyJWTVerification(b *testing.B) {
	clientID := "https://client.example/client.json"
	key, jwk := testDPoPRSAKeyPair(b)
	jwk.Kid = "benchmark-key"
	jwk.Alg = oauth.JWTAlgRS256
	jwk.Use = "sig"
	now := time.Now()
	assertion := makeClientAssertion(b, key, jwk.Kid, clientID, "benchmark-jti", now)
	keys := []oauth.JWK{*jwk}
	b.ReportAllocs()
	for b.Loop() {
		claims, err := verifyClientAssertion(assertion, clientID, testBaseURL+"/oauth/token", keys, now)
		if err != nil || claims.JWTID != "benchmark-jti" {
			b.Fatalf("verify = %#v, %v", claims, err)
		}
	}
}
