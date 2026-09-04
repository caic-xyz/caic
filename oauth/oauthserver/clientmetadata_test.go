// Tests for secure client metadata discovery, caching, and JWT client authentication.

package oauthserver

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caic-xyz/caic/oauth"
)

func TestClientMetadata(t *testing.T) {
	t.Parallel()

	t.Run("fetch pins a public address and preserves host and SNI", func(t *testing.T) {
		t.Parallel()
		var baseURL string
		var requests atomic.Int64
		network, roots := newClientMetadataTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			if r.Host != strings.TrimPrefix(baseURL, "https://") {
				t.Errorf("Host = %q, want %q", r.Host, strings.TrimPrefix(baseURL, "https://"))
			}
			serverName := ""
			if r.TLS != nil {
				serverName = r.TLS.ServerName
			}
			if serverName != "metadata.example" {
				t.Errorf("TLS SNI = %q, want metadata.example", serverName)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "public, max-age=60")
			_, _ = w.Write([]byte(`{"client_id":"` + baseURL + `/client.json","client_name":"Verified Client","redirect_uris":["https://client.example/callback"]}`))
		}))
		baseURL = network.baseURL
		resolver := newClientMetadataResolver(network, roots)
		clientID := baseURL + "/client.json"
		first, err := resolver.resolve(t.Context(), clientID)
		if err != nil {
			t.Fatalf("resolve first: %v", err)
		}
		second, err := resolver.resolve(t.Context(), clientID)
		if err != nil {
			t.Fatalf("resolve cached: %v", err)
		}
		if first.ID != clientID || first.Name != "Verified Client" || first.Provenance != ClientProvenanceMetadata || second.ID != first.ID {
			t.Fatalf("resolved clients = %#v, %#v", first, second)
		}
		if requests.Load() != 1 || network.lookupCount.Load() != 1 || network.dialCount.Load() != 1 {
			t.Fatalf("requests/lookups/dials = %d/%d/%d, want 1/1/1", requests.Load(), network.lookupCount.Load(), network.dialCount.Load())
		}
		network.mu.Lock()
		dialed := network.lastDial
		network.mu.Unlock()
		if strings.Contains(dialed, "metadata.example") {
			t.Fatalf("dial address %q was not pinned to validated IP", dialed)
		}
	})

	t.Run("redirects failures and stale entries fail closed without negative caching", func(t *testing.T) {
		t.Parallel()
		var baseURL string
		var good atomic.Bool
		var clientRequests atomic.Int64
		var targetRequests atomic.Int64
		network, roots := newClientMetadataTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/redirect.json":
				http.Redirect(w, r, baseURL+"/target.json", http.StatusFound)
			case "/target.json":
				targetRequests.Add(1)
			case "/client.json":
				clientRequests.Add(1)
				if !good.Load() {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				_, _ = w.Write([]byte(`{"client_id":"` + baseURL + `/client.json","client_name":"Recovered","redirect_uris":["https://client.example/callback"]}`))
			}
		}))
		baseURL = network.baseURL
		resolver := newClientMetadataResolver(network, roots)
		if _, err := resolver.resolve(t.Context(), baseURL+"/redirect.json"); err == nil {
			t.Fatal("redirecting metadata response unexpectedly succeeded")
		}
		if targetRequests.Load() != 0 {
			t.Fatalf("redirect target requests = %d, want 0", targetRequests.Load())
		}
		clientID := baseURL + "/client.json"
		resolver.clients[clientID] = clientMetadataCacheEntry{
			client:    Client{ID: clientID, Name: "Stale", Provenance: ClientProvenanceMetadata},
			expiresAt: time.Now().Add(-time.Second),
		}
		if _, err := resolver.resolve(t.Context(), clientID); err == nil {
			t.Fatal("expired metadata was served after refresh failure")
		}
		good.Store(true)
		if client, err := resolver.resolve(t.Context(), clientID); err != nil || client.Name != "Recovered" {
			t.Fatalf("uncached retry = %#v, %v", client, err)
		}
		if client, err := resolver.resolve(t.Context(), clientID); err != nil || client.Name != "Recovered" {
			t.Fatalf("no-store retry = %#v, %v", client, err)
		}
		if clientRequests.Load() != 3 {
			t.Fatalf("client requests = %d, want 3 (failure plus two uncached successes)", clientRequests.Load())
		}
	})

	t.Run("special-use result rejects the complete DNS answer before dialing", func(t *testing.T) {
		t.Parallel()
		network := &clientMetadataTestNetwork{
			addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr("127.0.0.1")},
		}
		resolver := newClientMetadataResolver(network, nil)
		if _, err := resolver.dialPublicAddress(t.Context(), "tcp", "metadata.example:443"); err == nil {
			t.Fatal("mixed public/private DNS answer unexpectedly dialed")
		}
		if network.dialCount.Load() != 0 {
			t.Fatalf("dial count = %d, want 0", network.dialCount.Load())
		}
	})

	t.Run("metadata response size is bounded after transfer decoding", func(t *testing.T) {
		t.Parallel()
		network, roots := newClientMetadataTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"padding":"` + strings.Repeat("x", clientMetadataMaxBytes) + `"}`))
		}))
		resolver := newClientMetadataResolver(network, roots)
		if _, err := resolver.resolve(t.Context(), network.baseURL+"/client.json"); err == nil || !strings.Contains(err.Error(), "too large") {
			t.Fatalf("oversized metadata error = %v", err)
		}
	})

	t.Run("identifier and address policy rejects unstable or special-use identities", func(t *testing.T) {
		t.Parallel()
		for _, raw := range []string{
			"http://client.example/client.json",
			"https://client.example/",
			"https://client.example/client.json?version=1",
			"https://user@client.example/client.json",
			"https://client.example/a/../client.json",
			"https://client.example/client.json#fragment",
		} {
			if err := validateClientIdentifierURL(raw); err == nil {
				t.Errorf("validateClientIdentifierURL(%q) unexpectedly succeeded", raw)
			}
		}
		for _, raw := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "192.0.2.1", "::1", "fc00::1", "2001:db8::1"} {
			if isPublicInternetAddress(netip.MustParseAddr(raw)) {
				t.Errorf("isPublicInternetAddress(%s) = true", raw)
			}
		}
		if !isPublicInternetAddress(netip.MustParseAddr("93.184.216.34")) || !isPublicInternetAddress(netip.MustParseAddr("2606:4700:4700::1111")) {
			t.Fatal("public example addresses were rejected")
		}
	})

	t.Run("cache freshness honors restrictive origin policy", func(t *testing.T) {
		t.Parallel()
		now := time.Now().Truncate(time.Second)
		cases := []struct {
			header http.Header
			want   time.Duration
		}{
			{header: http.Header{}, want: time.Minute},
			{header: http.Header{"Cache-Control": {"max-age=7"}}, want: 7 * time.Second},
			{header: http.Header{"Cache-Control": {"no-store, max-age=60"}}, want: 0},
			{header: http.Header{"Cache-Control": {"max-age=20"}, "Age": {"15"}}, want: 5 * time.Second},
			{header: http.Header{"Cache-Control": {"max-age=20"}, "Age": {"9223372036854775807"}}, want: 0},
			{header: http.Header{"Expires": {now.Add(3 * time.Second).UTC().Format(http.TimeFormat)}}, want: 3 * time.Second},
		}
		for _, tc := range cases {
			if got := cacheFreshness(tc.header, now); got != tc.want {
				t.Errorf("cacheFreshness(%v) = %s, want %s", tc.header, got, tc.want)
			}
		}
	})

	t.Run("pending retrievals are bounded and share identical work", func(t *testing.T) {
		t.Parallel()
		resolver := newClientMetadataResolver(nil, nil)
		sharedKey := strconv.FormatInt(clientMetadataMaxBytes, 10) + "\x00https://client.example/shared.json"
		shared := &clientMetadataFetch{
			done:   make(chan struct{}),
			body:   []byte(`{"shared":true}`),
			header: http.Header{"Cache-Control": {"max-age=1"}},
		}
		close(shared.done)
		resolver.fetches[sharedKey] = shared
		body, _, err := resolver.fetchOnce(t.Context(), "https://client.example/shared.json", clientMetadataMaxBytes)
		if err != nil || string(body) != `{"shared":true}` {
			t.Fatalf("joined fetch = %s, %v", body, err)
		}
		delete(resolver.fetches, sharedKey)
		for i := range clientMetadataFetchLimit {
			key := strconv.FormatInt(clientMetadataMaxBytes, 10) + "\x00https://client.example/" + strconv.Itoa(i)
			resolver.fetches[key] = &clientMetadataFetch{done: make(chan struct{})}
		}
		if _, _, err := resolver.fetchOnce(t.Context(), "https://client.example/overflow", clientMetadataMaxBytes); err == nil || !strings.Contains(err.Error(), "capacity") {
			t.Fatalf("overflow fetch error = %v", err)
		}
		if len(resolver.fetches) != clientMetadataFetchLimit {
			t.Fatalf("pending fetches = %d, want %d", len(resolver.fetches), clientMetadataFetchLimit)
		}
	})

	t.Run("remote jwks is validated and cached within bounded storage", func(t *testing.T) {
		t.Parallel()
		_, jwk := testDPoPRSAKeyPair(t)
		jwk.Kid = "remote-key"
		jwk.Alg = oauth.JWTAlgRS256
		jwk.Use = "sig"
		jwksBody, err := json.Marshal(oauth.JWKSet{Keys: []oauth.JWK{*jwk}})
		if err != nil {
			t.Fatalf("marshal jwks: %v", err)
		}
		var requests atomic.Int64
		network, roots := newClientMetadataTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			w.Header().Set("Content-Type", "application/jwk-set+json")
			w.Header().Set("Cache-Control", "max-age=60")
			_, _ = w.Write(jwksBody)
		}))
		resolver := newClientMetadataResolver(network, roots)
		client := Client{JWKSURI: network.baseURL + "/keys.json"}
		for range 2 {
			keys, err := resolver.keys(t.Context(), &client)
			if err != nil || len(keys) != 1 || keys[0].Kid != jwk.Kid {
				t.Fatalf("resolved keys = %+v, %v", keys, err)
			}
		}
		if requests.Load() != 1 {
			t.Fatalf("jwks requests = %d, want 1", requests.Load())
		}
		for i := range clientJWKSCacheLimit + 1 {
			key := "https://keys.example/" + strconv.Itoa(i)
			storeClientJWKSCache(resolver.jwks, key, clientJWKSCacheEntry{keys: []oauth.JWK{*jwk}, expiresAt: time.Now().Add(time.Minute), used: uint64(i + 10)}, time.Now())
		}
		if len(resolver.jwks) != clientJWKSCacheLimit {
			t.Fatalf("jwks cache entries = %d, want %d", len(resolver.jwks), clientJWKSCacheLimit)
		}
		for i := range clientMetadataCacheLimit + 1 {
			key := "https://client.example/" + strconv.Itoa(i)
			storeClientMetadataCache(resolver.clients, key, &clientMetadataCacheEntry{client: Client{ID: key}, expiresAt: time.Now().Add(time.Minute), used: uint64(i + 10)}, time.Now())
		}
		if len(resolver.clients) != clientMetadataCacheLimit {
			t.Fatalf("metadata cache entries = %d, want %d", len(resolver.clients), clientMetadataCacheLimit)
		}
	})

	t.Run("cimd redirects require exact matching while dynamic registrations retain loopback ports", func(t *testing.T) {
		t.Parallel()
		registered := []string{"http://127.0.0.1:1000/callback"}
		metadataClient := Client{RedirectURIs: registered, Provenance: ClientProvenanceMetadata}
		dynamicClient := Client{RedirectURIs: registered, Provenance: ClientProvenanceDynamic}
		if clientRedirectURIRegistered(&metadataClient, "http://127.0.0.1:2000/callback") {
			t.Fatal("CIMD redirect accepted an unregistered dynamic port")
		}
		if !clientRedirectURIRegistered(&dynamicClient, "http://127.0.0.1:2000/callback") {
			t.Fatal("dynamic registration lost native loopback port matching")
		}
	})

	t.Run("metadata and jwks reject ambiguous or unsafe key material", func(t *testing.T) {
		t.Parallel()
		clientID := "https://client.example/client.json"
		for _, body := range []string{
			`{"client_id":"` + clientID + `","client_id":"` + clientID + `","client_name":"Duplicate","redirect_uris":["https://client.example/callback"]}`,
			`{"client_id":"` + clientID + `","client_name":"Secret","redirect_uris":["https://client.example/callback"],"client_secret":"bad"}`,
			`{"client_id":"` + clientID + `","client_name":"Private","redirect_uris":["https://client.example/callback"],"token_endpoint_auth_method":"private_key_jwt"}`,
			`{"client_id":"` + clientID + `","client_name":"Public Keys","redirect_uris":["https://client.example/callback"],"jwks":{"keys":[]}}`,
		} {
			if _, err := parseClientMetadata(clientID, []byte(body)); err == nil {
				t.Fatalf("unsafe metadata unexpectedly accepted: %s", body)
			}
		}
		for _, body := range []string{
			`{"keys":[{"kty":"RSA","kid":"key","alg":"RS256","n":"AQAB","e":"Aw","d":"secret"}]}`,
			`{"keys":[{"kty":"RSA","kid":"duplicate","alg":"RS256","n":"AQAB","e":"Aw"},{"kty":"RSA","kid":"duplicate","alg":"RS256","n":"AQAB","e":"Aw"}]}`,
		} {
			if _, err := parseClientJWKS([]byte(body)); err == nil {
				t.Fatalf("unsafe JWKS unexpectedly accepted: %s", body)
			}
		}
	})

	t.Run("private client accepts a valid inline public jwks", func(t *testing.T) {
		t.Parallel()
		_, jwk := testDPoPRSAKeyPair(t)
		jwk.Kid = "inline-key"
		jwk.Alg = oauth.JWTAlgRS256
		jwk.Use = "sig"
		clientID := "https://client.example/client.json"
		document, err := json.Marshal(oauth.ClientIDMetadataDocument{
			ClientID:                clientID,
			ClientName:              "Private Client",
			RedirectURIs:            []string{"https://client.example/callback"},
			TokenEndpointAuthMethod: oauth.TokenEndpointAuthPrivateKeyJWT,
			JWKS:                    &oauth.JWKSet{Keys: []oauth.JWK{*jwk}},
		})
		if err != nil {
			t.Fatalf("marshal metadata: %v", err)
		}
		client, err := parseClientMetadata(clientID, document)
		if err != nil {
			t.Fatalf("parse private metadata: %v", err)
		}
		if client.TokenEndpointAuthMethod != oauth.TokenEndpointAuthPrivateKeyJWT || len(client.JWKS) != 1 || client.JWKS[0].Kid != jwk.Kid {
			t.Fatalf("private client = %#v", client)
		}
	})
}

func TestPrivateKeyJWTClientAuthentication(t *testing.T) {
	t.Parallel()

	clientID := "https://client.example/client.json"
	key, jwk := testDPoPRSAKeyPair(t)
	jwk.Kid = "client-signing-key"
	jwk.Alg = oauth.JWTAlgRS256
	jwk.Use = "sig"
	client := Client{
		ID:                      clientID,
		Name:                    "Confidential Client",
		RedirectURIs:            []string{"https://client.example/callback"},
		TokenEndpointAuthMethod: oauth.TokenEndpointAuthPrivateKeyJWT,
		GrantTypes:              []string{oauth.GrantAuthorizationCode},
		Provenance:              ClientProvenanceMetadata,
		JWKS:                    []oauth.JWK{*jwk},
	}
	statePath := t.TempDir() + "/oauth.json"
	user := testUser()
	ui := &captureAuthorizationUI{}
	cfg := &ServerConfig{
		RefreshTokenStorePath: statePath,
		Session:               &testSessionManager{users: map[string]oauth.User{user.ID: user}},
		UI:                    ui,
	}
	server := newTestServer(t, cfg)
	seedClientMetadataCache(server, &client)
	now := time.Now()
	handler := newTestServerHandler(server)
	registered := oauth.RegisterResponse{ClientID: clientID, RedirectURIs: slices.Clone(client.RedirectURIs)}
	code := authorizeOAuthTestCode(t, handler, user, &registered, client.RedirectURIs[0], []string{"read"}, "")
	if !ui.last.ClientMetadataDocument || ui.last.ClientHostname != "client.example" || ui.last.ClientID != clientID {
		t.Fatalf("consent client identity = %+v", ui.last)
	}
	validAssertion := makeClientAssertion(t, key, jwk.Kid, clientID, "successful-jti", now)
	validForm := url.Values{
		"grant_type":            {oauth.GrantAuthorizationCode},
		"client_id":             {clientID},
		"code":                  {code},
		"code_verifier":         {testVerifier},
		"redirect_uri":          {client.RedirectURIs[0]},
		"resource":              {testResourceURL},
		"client_assertion_type": {clientAssertionType},
		"client_assertion":      {validAssertion},
	}
	validRequest := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(validForm.Encode()))
	validRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	validResponse := httptest.NewRecorder()
	handler.ServeHTTP(validResponse, validRequest)
	if validResponse.Code != http.StatusOK {
		t.Fatalf("private_key_jwt token exchange = %d %s", validResponse.Code, validResponse.Body.String())
	}

	assertion := makeClientAssertion(t, key, jwk.Kid, clientID, "durable-jti", now)
	request := func(s *Server, assertion string) *httptest.ResponseRecorder {
		form := url.Values{
			"grant_type":            {oauth.GrantAuthorizationCode},
			"client_id":             {clientID},
			"code":                  {"invalid-code"},
			"client_assertion_type": {clientAssertionType},
			"client_assertion":      {assertion},
		}
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		newTestServerHandler(s).ServeHTTP(response, req)
		return response
	}
	if response := request(server, assertion); response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_grant") {
		t.Fatalf("first authenticated request = %d %s", response.Code, response.Body.String())
	}
	if response := request(server, assertion); response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), "invalid_client") {
		t.Fatalf("replayed request = %d %s", response.Code, response.Body.String())
	}
	if len(server.state.ClientAssertionJTIs) != 2 {
		t.Fatalf("durable assertion jtis = %d, want 2", len(server.state.ClientAssertionJTIs))
	}
	server.Close()
	restarted := newTestServer(t, cfg)
	seedClientMetadataCache(restarted, &client)
	if response := request(restarted, assertion); response.Code != http.StatusUnauthorized {
		t.Fatalf("restart replay request = %d %s", response.Code, response.Body.String())
	}
	if len(restarted.state.DPoPProofs) != 0 {
		t.Fatalf("client assertion replay state leaked into DPoP namespace: %d entries", len(restarted.state.DPoPProofs))
	}
	unknownKeyAssertion := makeClientAssertion(t, key, "unknown-key", clientID, "unknown-key-jti", now)
	if response := request(restarted, unknownKeyAssertion); response.Code != http.StatusUnauthorized {
		t.Fatalf("unknown key request = %d %s", response.Code, response.Body.String())
	}
	ambiguousClient := client
	ambiguousClient.JWKS = append(slices.Clone(client.JWKS), client.JWKS[0])
	seedClientMetadataCache(restarted, &ambiguousClient)
	ambiguousAssertion := makeClientAssertion(t, key, jwk.Kid, clientID, "ambiguous-key-jti", now)
	if response := request(restarted, ambiguousAssertion); response.Code != http.StatusUnauthorized {
		t.Fatalf("ambiguous key request = %d %s", response.Code, response.Body.String())
	}
	seedClientMetadataCache(restarted, &client)
	restarted.state.ClientAssertionJTIs = make(map[string]time.Time, clientAssertionJTILimit)
	for i := range clientAssertionJTILimit {
		restarted.state.ClientAssertionJTIs[strconv.Itoa(i)] = now.Add(time.Minute)
	}
	if err := restarted.reserveClientAssertion(clientID, "over-capacity", now.Add(time.Minute)); err == nil || len(restarted.state.ClientAssertionJTIs) != clientAssertionJTILimit {
		t.Fatalf("capacity reservation error = %v, entries = %d", err, len(restarted.state.ClientAssertionJTIs))
	}
	restarted.state.ClientAssertionJTIs["0"] = now.Add(-time.Minute)
	if err := restarted.reserveClientAssertion(clientID, "after-prune", now.Add(time.Minute)); err != nil || len(restarted.state.ClientAssertionJTIs) != clientAssertionJTILimit {
		t.Fatalf("pruned reservation error = %v, entries = %d", err, len(restarted.state.ClientAssertionJTIs))
	}

	for name, claims := range map[string]clientAssertionClaims{
		"wrong issuer":   {Issuer: "other", Subject: clientID, Audience: testBaseURL + "/oauth/token", JWTID: "a", IssuedAt: now.Unix(), Expiry: now.Add(time.Minute).Unix()},
		"wrong subject":  {Issuer: clientID, Subject: "other", Audience: testBaseURL + "/oauth/token", JWTID: "b", IssuedAt: now.Unix(), Expiry: now.Add(time.Minute).Unix()},
		"wrong audience": {Issuer: clientID, Subject: clientID, Audience: testBaseURL + "/other", JWTID: "c", IssuedAt: now.Unix(), Expiry: now.Add(time.Minute).Unix()},
		"long lifetime":  {Issuer: clientID, Subject: clientID, Audience: testBaseURL + "/oauth/token", JWTID: "d", IssuedAt: now.Unix(), Expiry: now.Add(10 * time.Minute).Unix()},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			raw := makeClientAssertionClaims(t, key, jwk.Kid, &claims)
			if _, err := verifyClientAssertion(raw, clientID, testBaseURL+"/oauth/token", client.JWKS, now); err == nil {
				t.Fatal("invalid assertion unexpectedly verified")
			}
		})
	}
}

func TestClientMetadataRequiresDurableStore(t *testing.T) {
	t.Parallel()
	server, err := NewServer(ServerConfig{
		KeyPEM:                  testSigningKeyPEM,
		KeyID:                   "test-key",
		Issuer:                  testBaseURL,
		ResourceURLPath:         "/resource",
		ResourceMetadataURLPath: "/.well-known/oauth-protected-resource/resource",
		ClientIDPrefix:          "test_",
		SupportedScopes:         []string{"read"},
		DefaultScopes:           []string{"read"},
		Session:                 &testSessionManager{},
		UI:                      &testAuthorizationUI{},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(server.Close)
	if _, err := server.resolveOAuthClient(t.Context(), "https://client.example/client.json"); err == nil || !strings.Contains(err.Error(), "durable") {
		t.Fatalf("metadata resolution error = %v", err)
	}
	w := httptest.NewRecorder()
	server.handleOAuthMetadata(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/.well-known/oauth-authorization-server", http.NoBody))
	var metadata oauth.AuthorizationServerMetadata
	if err := json.NewDecoder(w.Body).Decode(&metadata); err != nil {
		t.Fatalf("decode discovery: %v", err)
	}
	if metadata.ClientIDMetadataDocumentSupported || slices.Contains(metadata.TokenEndpointAuthMethodsSupported, oauth.TokenEndpointAuthPrivateKeyJWT) || slices.Contains(metadata.RevocationEndpointAuthMethodsSupported, oauth.TokenEndpointAuthPrivateKeyJWT) {
		t.Fatalf("in-memory discovery advertises durable client authentication: %+v", metadata)
	}
}

type clientMetadataTestNetwork struct {
	baseURL   string
	dialAddr  string
	addresses []netip.Addr

	lookupCount atomic.Int64
	dialCount   atomic.Int64
	mu          sync.Mutex
	lastDial    string
}

func (n *clientMetadataTestNetwork) LookupNetIP(_ context.Context, _, _ string) ([]netip.Addr, error) {
	n.lookupCount.Add(1)
	return slices.Clone(n.addresses), nil
}

func (n *clientMetadataTestNetwork) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	n.dialCount.Add(1)
	n.mu.Lock()
	n.lastDial = address
	n.mu.Unlock()
	return (&net.Dialer{}).DialContext(ctx, network, n.dialAddr)
}

func newClientMetadataTestServer(t *testing.T, handler http.Handler) (*clientMetadataTestNetwork, *x509.CertPool) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate TLS key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "metadata.example"},
		DNSNames:     []string{"metadata.example"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create TLS certificate: %v", err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{certificateDER}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	certificate, err := x509.ParseCertificate(certificateDER)
	if err != nil {
		t.Fatalf("parse TLS certificate: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	_, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split test server address: %v", err)
	}
	network := &clientMetadataTestNetwork{
		baseURL:   "https://metadata.example:" + port,
		dialAddr:  server.Listener.Addr().String(),
		addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")},
	}
	return network, roots
}

func seedClientMetadataCache(server *Server, client *Client) {
	server.clientMetadata.clients[client.ID] = clientMetadataCacheEntry{client: cloneClient(client), expiresAt: time.Now().Add(time.Minute)}
}

func makeClientAssertion(t testing.TB, key *rsa.PrivateKey, kid, clientID, jti string, now time.Time) string {
	claims := &clientAssertionClaims{
		Issuer: clientID, Subject: clientID, Audience: testBaseURL + "/oauth/token", JWTID: jti,
		IssuedAt: now.Unix(), Expiry: now.Add(time.Minute).Unix(),
	}
	return makeClientAssertionClaims(t, key, kid, claims)
}

func makeClientAssertionClaims(t testing.TB, key *rsa.PrivateKey, kid string, claims *clientAssertionClaims) string {
	headerJSON, err := json.Marshal(clientAssertionHeader{Alg: oauth.JWTAlgRS256, Kid: kid, Typ: "JWT"})
	if err != nil {
		t.Fatalf("marshal assertion header: %v", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal assertion claims: %v", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	signature, err := signJWS(oauth.JWTAlgRS256, key, []byte(signingInput))
	if err != nil {
		t.Fatalf("sign client assertion: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}
