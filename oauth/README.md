# oauth

Standalone Go OAuth 2.0 authorization server and client library.

Implements: RFC 6749 (Authorization Framework), 6750 (Bearer Token Usage),
7636 (PKCE), 7009 (Token Revocation), 7662 (Token Introspection),
8693 (Token Exchange), 9068 (JWT Profile), 8414 (AS Metadata),
7591/7592 (Dynamic Client Registration), 8628 (Device Authorization),
9126 (Pushed Authorization Requests), 9207 (Issuer Identification),
9449 (DPoP), 9700 (Security BCP).

Zero external dependencies — stdlib only.

## Quick Start

```go
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/caic-xyz/oauth"
)

type stubSessionManager struct{}
func (stubSessionManager) CurrentUser(ctx context.Context) (oauth.User, bool) {
	return oauth.User{ID: "usr_1", Username: "alice"}, true
}
func (stubSessionManager) AttachUser(ctx context.Context, u oauth.User) context.Context { return ctx }
func (stubSessionManager) FindUser(id string) (oauth.User, bool) {
	return oauth.User{ID: id, Username: id}, true
}
func (stubSessionManager) EndSession(ctx context.Context, r *http.Request, u oauth.User) (string, error) {
	return "/bye", nil
}

type stubUI struct{}
func (stubUI) LoginStartURL(r *http.Request) string      { return "/login" }
func (stubUI) ProviderLabel(provider string) string       { return provider }
func (stubUI) RenderOAuthConsent(w http.ResponseWriter, d *oauth.ConsentPageData) error {
	fmt.Fprintf(w, "Hi %s, grant %s access to %s?", d.Username, d.ClientName, d.ScopeItems)
	return nil
}

func main() {
	keyPEM, _ := os.ReadFile("key.pem")
	srv, err := oauth.NewServer(oauth.ServerConfig{
		KeyPEM:            keyPEM,
		KeyID:             "key-1",
		RefreshTokenStorePath: "oauth_store.json",
		SupportedScopes:   []string{"api:read", "api:write"},
		DefaultScopes:     []string{"api:read"},
		ScopeLabels:       map[string]string{"api:read": "Read access", "api:write": "Write access"},
		BaseURL:           func(r *http.Request) string { return "https://example.com" },
		Session:           stubSessionManager{},
		UI:                stubUI{},
	})
	if err != nil {
		log.Fatal(err)
	}

	// Register a client
	client, err := srv.RegisterClient("My App", []string{"https://example.com/callback"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Client ID: %s\n", client.ID)

	http.ListenAndServe(":8080", srv.Routes())
}
```

## Interfaces

The package delegates product-specific concerns to two interfaces:

- **SessionManager** — user login state, session attachment, user lookup, and end-session redirect
- **AuthorizationUI** — login redirect URL, consent page rendering, OIDC provider labels

Optional seams: `BaseURLFunc`, `AuditRecorder`, `RateLimiter`.

## Client Helpers

The `oauth` package also provides generic client helpers for
Authorization Code with PKCE:

```go
cfg := oauth.ClientConfig{ClientID: "…", ClientSecret: "…", TokenURL: "…", RedirectURI: "…"}
url := oauth.AuthorizationURL("https://example.com/oauth/authorize", cfg.ClientID, cfg.RedirectURI, []string{"api:read"}, state)
access, refresh, expiry, err := oauth.ExchangeCode(ctx, cfg, code)
```

## License

Apache 2.0
