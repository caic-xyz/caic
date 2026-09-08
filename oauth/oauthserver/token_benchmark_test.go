// Benchmarks OAuth RFC 9068 access-token issuance and verification.

package oauthserver

import (
	"testing"
	"time"

	"github.com/caic-xyz/caic/oauth"
)

func BenchmarkAccessTokenIssue(b *testing.B) {
	service, err := NewAccessTokenService(testSigningKeyPEM, "benchmark-key", time.Hour)
	if err != nil {
		b.Fatal(err)
	}
	user := oauth.User{ID: "benchmark-user", Username: "alice"}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := service.IssueAccessToken(testBaseURL, user, testResourceURL, "read", "benchmark-grant", "benchmark-client"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAccessTokenVerify(b *testing.B) {
	service, err := NewAccessTokenService(testSigningKeyPEM, "benchmark-key", time.Hour)
	if err != nil {
		b.Fatal(err)
	}
	user := oauth.User{ID: "benchmark-user", Username: "alice"}
	token, err := service.IssueAccessToken(testBaseURL, user, testResourceURL, "read", "benchmark-grant", "benchmark-client")
	if err != nil {
		b.Fatal(err)
	}
	session := &tokenTestSession{user: user}
	now := time.Now()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := service.VerifyAccessToken(token, testBaseURL, testResourceURL, now, activeGrant("benchmark-client"), session); err != nil {
			b.Fatal(err)
		}
	}
}
