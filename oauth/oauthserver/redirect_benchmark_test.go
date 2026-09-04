// Benchmarks native loopback OAuth redirect matching.

package oauthserver

import "testing"

func BenchmarkLoopbackRedirectMatch(b *testing.B) {
	registered := []string{"http://127.0.0.1:1000/callback?flow=1"}
	const requested = "http://127.0.0.1:2000/callback?flow=1"
	for b.Loop() {
		if !redirectURIRegistered(registered, requested) {
			b.Fatal("dynamic loopback redirect did not match")
		}
	}
}
