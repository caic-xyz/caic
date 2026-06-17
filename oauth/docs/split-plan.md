# OAuth Package Split

Split the `oauth` package into three packages: root (shared types), `oauth/client` (client + forge providers), `oauth/server` (authorization server).

## Motivation

1. `oauth` will become a standalone git repository.
2. Multiple projects need GitHub/GitLab OAuth clients without copy-paste.
3. The current package comment in `client.go` ("Provider-specific userinfo parsing belongs in the caller") is stale — forge providers will live in `oauth/client/`.
4. 62k of server code should never be pulled into a client-only import.

## Target Layout

```
oauth/
  dto.go             ← shared types & constants (no change except ClientConfig added)
  sdk.go             ← SDK spec (no change)
  client/
    client.go        ← PKCEChallenge, NewPKCEChallenge, AuthorizationURL, ExchangeCode
    forge_github.go  ← GitHubConfig, FetchGitHubUser (new, extracted from forge_oauth.go)
    forge_gitlab.go  ← GitLabConfig, FetchGitLabUser (new, extracted from forge_oauth.go)
    client_test.go   ← tests
  server/
    server.go        ← Server, NewServer, ServerConfig, SessionManager, AuthorizationUI, ConsentPageData, Grant, handlers
    token.go         ← token signing/verification
    storage.go       ← storage interfaces + filesystem implementation
    state.go         ← GenerateState, SignState, ValidateState
    dpopjti.go       ← DPoP jti replay prevention
    dpopnonce.go     ← DPoP nonce management
    dpopproof.go     ← DPoP proof verification
    server_test.go   ← tests
    token_test.go    ← tests
    storage_test.go  ← tests
    dpopnonce_test.go← tests
```

## Step 1: Move `ClientConfig` to `oauth/dto.go`

`ClientConfig` is currently in `client.go`. Move it to `dto.go` so it stays in the root package. Both `oauth/client` and `oauth/server` (and the backend) need it, and the root package is the shared dependency.

Also move `codeTokenResponse` (unexported, used by ExchangeCode) — keep it with ExchangeCode in `oauth/client/`.

## Step 2: Create `oauth/client/`

Move `client.go` -> `oauth/client/client.go`. Package declaration becomes `package client`.

Types staying with client:
- `PKCEChallenge`, `NewPKCEChallenge` — no root deps
- `AuthorizationURL` — returns string, no root deps
- `ExchangeCode` — takes `oauth.ClientConfig` (import root), `codeTokenResponse` stays unexported

Constants used by client: `ResponseTypeCode`, `GrantAuthorizationCode` — these are in `oauth/dto.go` already. Import `oauth` at the call sites.

## Step 3: Create `oauth/client/forge_github.go` and `forge_gitlab.go`

Extract from `backend/internal/auth/forge_oauth.go`:

```go
// oauth/client/forge_github.go
package client

// GitHubConfig is the OAuth client configuration for github.com.
type GitHubConfig struct {
    ClientID     string
    ClientSecret string
    RedirectURI  string   // plain string, no HostState dependency
    Scopes       []string
}

// NewGitHubConfig returns a GitHubConfig with defaults.
func NewGitHubConfig(clientID, clientSecret, redirectURI string) GitHubConfig {
    return GitHubConfig{
        ClientID:     clientID,
        ClientSecret: clientSecret,
        RedirectURI:  redirectURI,
        Scopes:       []string{"repo", "read:user"},
        AuthEndpoint: "https://github.com/login/oauth/authorize",
        TokenURL:     "https://github.com/login/oauth/access_token",
        UserInfoURL:  "https://api.github.com/user",
    }
}

func (c GitHubConfig) AuthURL(state string) string { ... }
func (c GitHubConfig) ClientConfig() oauth.ClientConfig { ... }

// FetchGitHubUser fetches the authenticated user from api.github.com/user.
func FetchGitHubUser(ctx context.Context, cfg GitHubConfig, accessToken string) (id int64, login, avatarURL string, err error) { ... }
```

Same pattern for GitLab, with `NewGitLabConfig(clientID, clientSecret, gitlabURL, redirectURI string)`.

Key design change: `RedirectURI` is now a plain string passed at construction time. The caller (backend) is responsible for computing it from `HostState.ExternalURL()`. This removes the `HostState` dependency that would prevent the `oauth` package from being standalone.

## Step 4: Create `oauth/server/`

Move all server-side files into `oauth/server/`. Package declaration becomes `package server`.

Files: `server.go`, `token.go`, `storage.go`, `state.go`, `dpopproof.go`, `dpopjti.go`, `dpopnonce.go`, plus all corresponding `_test.go` files.

All public types (`Server`, `NewServer`, `ServerConfig`, `SessionManager`, `AuthorizationUI`, `ConsentPageData`, `Grant`, `GenerateState`, `SignState`, `ValidateState`) become `server.Server`, `server.NewServer`, etc.

## Step 5: Thin adapter in `backend/internal/auth/forge_oauth.go`

Replace the current `ProviderConfig` (which carries `HostState`) with a thin adapter:

```go
func NewGitHubProvider(clientID, secret string, host *HostState) (oauthclient.GitHubConfig, error) {
    uri := host.ExternalURL() + "/api/caic/v1/auth/github/callback"
    if uri == "" { return ... }
    return oauthclient.NewGitHubConfig(clientID, secret, uri), nil
}
```

Or keep `ProviderConfig` as-is but make it delegate to `oauth/client` types. The simplest: keep `ProviderConfig` but remove `AuthEndpoint`/`TokenURL`/`UserInfoURL`/`Scopes`/`Provider`/`Label` from it — those are now in `client.GitHubConfig`. `ProviderConfig` becomes just a wrapper that adds `HostState`:

```go
type ProviderConfig struct {
    client.GitHubConfig   // embedded; or
    Client client.Config   // generic interface
    Host   *HostState
}
```

But this adds complexity. Simpler: kill `ProviderConfig` entirely. The backend stores `*client.GitHubConfig` (or `*client.GitLabConfig`) directly. `RedirectURI()` is computed once in `app.go` when constructing the config. The auth handlers use the config's `AuthURL()` and `ClientConfig()` methods directly.

Update `authHandlers` struct from:
```go
githubOAuth *auth.ProviderConfig
gitlabOAuth *auth.ProviderConfig
```
to:
```go
githubOAuth *oauthclient.GitHubConfig
gitlabOAuth *oauthclient.GitLabConfig
```

`FetchUserInfo` call sites change from:
```go
auth.FetchUserInfo(r.Context(), cfg, accessToken)
```
to:
```go
id, login, avatar, err := oauthclient.FetchGitHubUser(r.Context(), *cfg, accessToken)
```

## Step 6: Update backend imports

| Old import | New import |
|---|---|
| `oauth.NewServer` | `oauthserver.NewServer` |
| `oauth.ServerConfig` | `oauthserver.ServerConfig` |
| `oauth.SessionManager` | `oauthserver.SessionManager` |
| `oauth.AuthorizationUI` | `oauthserver.AuthorizationUI` |
| `oauth.ConsentPageData` | `oauthserver.ConsentPageData` |
| `oauth.Grant` | `oauthserver.Grant` |
| `oauth.GenerateState` | `oauthserver.GenerateState` |
| `oauth.SignState` | `oauthserver.SignState` |
| `oauth.ValidateState` | `oauthserver.ValidateState` |
| `auth.FetchUserInfo` | `oauthclient.FetchGitHubUser` / `oauthclient.FetchGitLabUser` |
| `auth.ProviderConfig` | `*oauthclient.GitHubConfig` / `*oauthclient.GitLabConfig` |

Stays at `oauth.`:
- `oauth.User`, `oauth.BearerClaims`, `oauth.BearerClaimsFromContext`
- `oauth.BearerScopeChallenge`, `oauth.BearerToken`, `oauth.BearerChallenge`
- `oauth.ClientConfig`
- `oauth.ResponseTypeCode`, `oauth.GrantAuthorizationCode` (constants)
- All DTO types

## Step 7: Remove stale comments

The package comment in `oauth/client/client.go` changes from:
```
// This file stops at generic protocol work: authorization URLs and token
// exchange. Provider-specific userinfo parsing belongs in the caller.
```
to:
```
// Authorization-code client helpers and forge provider configurations.
```

## Step 8: Update generated files and lint

Run `make refresh-generated` and `make lint-fix`.

## Non-goals
- Not adding PKCE to the forge flow in this change
- Not changing the `MaskedToken` type (stays in `backend/internal/auth/`)
- Not modifying the OAuth server behavior or tests beyond package renames
- Not creating the standalone git repository yet

## Risks
- `oauth/server/` imports `oauth/` for `User`, `BearerClaims`, etc. — clean, no cycle
- `oauth/client/` imports `oauth/` for `ClientConfig`, constants — clean, no cycle
- Backend now has 3 import paths instead of 1 — acceptable tradeoff
