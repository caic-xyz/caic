# `oauth/oauthclient` vs `golang.org/x/oauth2` — Contrast Analysis

Compares `oauth/oauthclient` against the Go team's `golang.org/x/oauth2`
(read at commit depth=1, 2025-era). Identifies divergent trade-offs.

## Scope & Maturity

|                          | `oauth/oauthclient`                        | `golang.org/x/oauth2`                             |
| ------------------------ | ------------------------------------------ | ------------------------------------------------- |
| **Code volume**          | ~410 lines (client.go + provider.go)       | ~3,500 lines core + 25+ provider endpoint packages |
| **Age / maintenance**    | Project-internal, ~1 year                  | Since 2014, Go team, widely used                   |
| **Dependencies**         | stdlib only                                | stdlib + `cloud.google.com/go/compute/metadata`    |
| **License**              | Apache 2.0                                 | BSD-style                                          |
| **Implemented RFCs**     | 6749 (auth code + refresh), 7636 (PKCE)    | 6749, 6750, 7636, 7009, 7662, 8628, 8693           |
| **OAuth grants**         | Authorization code + refresh               | Auth code, password, refresh, device, JWT, client credentials |

## Architecture

### `oauthclient` — free functions + Token struct

```go
// Authorization URL: a free function.
url := AuthorizationURL(endpoint, clientID, redirectURI, scopes, state, codeChallenge)

// Token exchange and refresh: both return *oauth.Token.
token, err := ExchangeCode(ctx, cfg, code, codeVerifier)
token, err := RefreshAccessToken(ctx, cfg, refreshToken)
```

`oauth.Token` bundles `AccessToken`, `TokenType`, `RefreshToken`, and `Expiry`.
Token refresh is wired into `auth.Middleware` via a `TokenRefresher` function
that renews expired tokens on each request. No `http.Client` or
`Transport` wrapper — callers manage headers manually.

`ProviderConfig` bundles provider-specific parameters (endpoints, scopes, userinfo
URL, userinfo parser) into a struct with GitHub/GitLab presets. A `Provider`
interface abstracts over provider configs.

### `x/oauth2` — rich types, token lifecycle, HTTP integration

```go
// Config describes a 3-legged OAuth2 flow.
type Config struct {
    ClientID, ClientSecret string
    Endpoint               Endpoint   // AuthURL, DeviceAuthURL, TokenURL, AuthStyle
    RedirectURL            string
    Scopes                 []string
}

// Token is the credential holder.
type Token struct {
    AccessToken, TokenType, RefreshToken string
    Expiry                               time.Time
    ExpiresIn                            int64
    raw                                  any  // arbitrary extra server metadata
}

// TokenSource is the core abstraction for token lifecycle.
type TokenSource interface {
    Token() (*Token, error)
}

// Config.Client returns an *http.Client that auto-injects Authorization
// headers and auto-refreshes expired tokens.
client := config.Client(ctx, token)
```

The `TokenSource` interface enables a refresh chain: `tokenRefresher` calls
the token endpoint with `grant_type=refresh_token` when `Token.Valid()` returns
false, wrapped by `reuseTokenSource` for mutex-guarded caching. The `Transport`
type injects Authorization headers via `Token.SetAuthHeader()`.

## Detailed Comparison

### 1. Token Type and Refresh

**`oauthclient`**: `ExchangeCode` and `RefreshAccessToken` return `*oauth.Token`.
Refresh is driven by a synchronous `TokenRefresher` function called by the
`auth.Middleware` before each request. Expired tokens with a refresh token are
renewed and persisted immediately via `Store.UpsertUser`.

**`x/oauth2`**: the `TokenSource` interface + `reuseTokenSource` enable
lazy refresh: the token is checked on each use and refreshed only when expired.
The `Transport` and `Config.Client()` hide refresh from the caller entirely.

**Key difference**: `oauthclient` uses eager, middleware-driven refresh (single
point in the request pipeline); `x/oauth2` uses lazy, transport-driven refresh
(on first use after expiry). `oauthclient` has no `Transport` or `http.Client`
wrapper.

### 2. Auth Style (client authentication method)

**`oauthclient`**: always sends `client_id` and `client_secret` in the POST body
as `application/x-www-form-urlencoded` parameters. This works for GitHub and
GitLab (both accept params) but is fragile for arbitrary providers.

**`x/oauth2`**: has `AuthStyle` with three modes:
- `AuthStyleAutoDetect` — tries `AuthStyleInHeader` (HTTP Basic) first, falls back to `AuthStyleInParams`
- `AuthStyleInParams` — POST body parameters (same as oauthclient)
- `AuthStyleInHeader` — HTTP Basic Authorization header

The result is cached per `(tokenURL, clientID)` pair in `AuthStyleCache`.
Callers can override via `Endpoint.AuthStyle`. This was built to handle real-world
provider quirks: Reddit requires `AuthStyleInHeader`, Google prefers
`AuthStyleInParams`, Dropbox gets confused with both.

### 3. Error Handling

**`oauthclient`**: returns `*RetrieveError` on non-2xx responses or JSON
responses with an `error` field. `RetrieveError` has `StatusCode`, `ErrorCode`,
`ErrorDescription`, `ErrorURI`, and `Body`. The `Error()` method includes
`StatusCode`, `ErrorCode`, and `ErrorDescription` but excludes the raw body.
A debug log with the full body is emitted for tracing.

**`x/oauth2`**: returns `*RetrieveError` with the same fields plus the full
`*http.Response`. The `Error()` method includes `ErrorCode`, `ErrorDescription`,
and `ErrorURI` but not the raw body.

### 4. Content-Type Response Handling

**`oauthclient`**: sets `Accept: application/json` but falls back to parsing
`application/x-www-form-urlencoded` when JSON parsing fails.

**`x/oauth2`**: reads the `Content-Type` header and handles both
`application/json` and `application/x-www-form-urlencoded` (and `text/plain`).
Mechanically different (Content-Type inspection vs JSON-first-then-fallback)
but same result.

### 5. PKCE API Design

Both implement S256 PKCE per RFC 7636.

**`oauthclient`**: single `NewPKCEChallenge()` returns a struct:

```go
type PKCEChallenge struct {
    Verifier  string
    Challenge string
}
```

The caller passes `Challenge` to `AuthorizationURL()` and `Verifier` to
`ExchangeCode()`.

**`x/oauth2`**: splits into multiple composable functions:

```go
verifier := GenerateVerifier()                         // panics on crypto failure
S256ChallengeOption(verifier)   // for AuthCodeURL()
VerifierOption(verifier)        // for Exchange()
S256ChallengeFromVerifier(v)    // raw conversion for custom use
```

The `x/oauth2` approach is more composable (fits the extensible `AuthCodeOption`
pattern) but requires the caller to thread the verifier string between two call
sites. The `oauthclient` approach is simpler — one call, one struct — but
less extensible (no way to pass extra auth URL parameters without changing the
function signature).

### 6. Transport / HTTP Client Integration

**`oauthclient`**: none. Callers manually attach the `Bearer` Authorization
header to `http.Client` requests.

**`x/oauth2`**: the `Transport` type is an `http.RoundTripper` wrapper that
calls `Source.Token()` before each request and injects the Authorization
header on a cloned request. `Config.Client(ctx, token)` returns an
`*http.Client` pre-configured with this transport.

### 7. Extensibility

**`oauthclient`**: `AuthorizationURL()` has a fixed parameter list. Adding a
new parameter (e.g., `access_type=offline`, `prompt=consent`) requires a
signature change.

**`x/oauth2`**: the `AuthCodeOption` interface allows arbitrary key-value pairs:

```go
type AuthCodeOption interface {
    setValue(url.Values)
}

// Built-in options:
AccessTypeOffline  = SetAuthURLParam("access_type", "offline")
ApprovalForce      = SetAuthURLParam("prompt", "consent")
S256ChallengeOption(verifier)
VerifierOption(verifier)
```

Callers can define their own `AuthCodeOption` implementations without modifying
the library.

### 8. Token Persistence

**`oauthclient`**: none. The caller stores tokens however they want.

**`x/oauth2`**: `ReuseTokenSource(t *Token, src TokenSource)` wraps a
`TokenSource` to reuse a cached token (e.g., from a file on disk) across
program restarts. `ReuseTokenSourceWithExpiry` allows configuring the
expiry buffer. This is a convenience, not a storage mechanism — the caller
still owns serialization.

## Summary of Trade-offs

| Trade-off                | `oauthclient` chose                         | `x/oauth2` chose                                   |
| ------------------------ | ------------------------------------------- | --------------------------------------------------- |
| **Simplicity vs power**  | Simplicity: free functions, no abstractions  | Power: interfaces, wrappers, lifecycle management   |
| **Token lifecycle**      | Refresh via `TokenRefresher` in middleware   | Library-managed: auto-refresh, caching, expiry delta |
| **Dependencies**         | Zero (stdlib only)                          | One external dep (Google metadata)                   |
| **Error visibility**     | Structured `RetrieveError`                  | Structured `RetrieveError`                           |
| **Provider coverage**    | GitHub + GitLab with userinfo parsing       | 25+ endpoint constants, no userinfo                 |
| **HTTP integration**     | None                                        | `http.Client` with auto-refresh transport            |
| **Extensibility**        | Fixed function signatures                   | `AuthCodeOption` interface                           |
| **Content negotiation**  | JSON + form-encoded fallback                | JSON + form-encoded                                 |

## What `oauthclient` omits at the moment

- Full `Config`/`Endpoint` split
- **`AuthCodeOption` interface**: `AuthorizationURL()` has a fixed set of
  parameters. If extensibility is needed, add variadic `url.Values` or a
  functional option pattern.
- **Auth style probing**: GitHub and GitLab both accept `client_id`/`client_secret`
  in POST body params.
- **`TokenSource` interface + `Transport`**: caic only calls userinfo via
  `ProviderConfig.FetchUser()`; no multi-endpoint API usage yet.
- **Google-specific flows** (JWT, external account, impersonation): not
  relevant to caic's current use case.
- **Device authorization grant**: not implemented.
- **`NoContext` / `RegisterBrokenAuthHeaderProvider`**: legacy cruft from
  `x/oauth2`'s long history, don't replicate.
- **The internal/external `Token` mirror**: `x/oauth2` has `internal.Token`
  and `oauth2.Token` to break a circular dependency between the `internal`
  and `oauth2` packages. `oauthclient` has no such dependency cycle.
