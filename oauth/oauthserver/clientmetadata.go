// Secure Client ID Metadata Document discovery and private-key client authentication.

package oauthserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caic-xyz/caic/oauth"
)

const (
	clientAssertionType      = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
	clientAssertionMaxAge    = 5 * time.Minute
	clientMetadataCacheLimit = 128
	clientMetadataMaxBytes   = 5 * 1024
	clientMetadataMaxTTL     = time.Minute
	clientMetadataTimeout    = 5 * time.Second
	clientAssertionJTILimit  = 4_096
	clientJWKSCacheLimit     = 128
	clientMetadataFetchLimit = 16
	clientJWKSMaxBytes       = 32 * 1024
	clientJWKSMaxKeys        = 20
)

// ClientMetadataNetwork resolves and dials remote Client ID Metadata Document hosts.
// Implementations must not apply an HTTP proxy or perform additional name resolution.
type ClientMetadataNetwork interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

type systemClientMetadataNetwork struct {
	resolver *net.Resolver
	dialer   *net.Dialer
}

func (n systemClientMetadataNetwork) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return n.resolver.LookupNetIP(ctx, network, host)
}

func (n systemClientMetadataNetwork) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return n.dialer.DialContext(ctx, network, address)
}

type clientMetadataCacheEntry struct {
	client    Client
	expiresAt time.Time
	used      uint64
}

type clientJWKSCacheEntry struct {
	keys      []oauth.JWK
	expiresAt time.Time
	used      uint64
}

type clientMetadataResolver struct {
	network ClientMetadataNetwork
	rootCAs *x509.CertPool

	mu       sync.Mutex
	sequence uint64
	clients  map[string]clientMetadataCacheEntry
	jwks     map[string]clientJWKSCacheEntry

	fetchMu sync.Mutex
	fetches map[string]*clientMetadataFetch
}

type clientMetadataFetch struct {
	done   chan struct{}
	body   []byte
	header http.Header
	err    error
}

func newClientMetadataResolver(network ClientMetadataNetwork, rootCAs *x509.CertPool) *clientMetadataResolver {
	if network == nil {
		network = systemClientMetadataNetwork{resolver: net.DefaultResolver, dialer: &net.Dialer{Timeout: clientMetadataTimeout}}
	}
	return &clientMetadataResolver{
		network: network,
		rootCAs: rootCAs,
		clients: map[string]clientMetadataCacheEntry{},
		jwks:    map[string]clientJWKSCacheEntry{},
		fetches: map[string]*clientMetadataFetch{},
	}
}

func (r *clientMetadataResolver) resolve(ctx context.Context, clientID string) (Client, error) {
	if err := validateClientIdentifierURL(clientID); err != nil {
		return Client{}, err
	}
	now := time.Now()
	r.mu.Lock()
	if entry, ok := r.clients[clientID]; ok && now.Before(entry.expiresAt) {
		r.sequence++
		entry.used = r.sequence
		r.clients[clientID] = entry
		client := cloneClient(&entry.client)
		r.mu.Unlock()
		return client, nil
	}
	r.mu.Unlock()

	body, header, err := r.fetchOnce(ctx, clientID, clientMetadataMaxBytes)
	if err != nil {
		return Client{}, fmt.Errorf("fetch client metadata: %w", err)
	}
	client, err := parseClientMetadata(clientID, body)
	if err != nil {
		return Client{}, fmt.Errorf("validate client metadata: %w", err)
	}
	if ttl := cacheFreshness(header, now); ttl > 0 {
		r.mu.Lock()
		r.sequence++
		storeClientMetadataCache(r.clients, clientID, &clientMetadataCacheEntry{client: cloneClient(&client), expiresAt: now.Add(ttl), used: r.sequence}, now)
		r.mu.Unlock()
	}
	return client, nil
}

func (r *clientMetadataResolver) keys(ctx context.Context, client *Client) ([]oauth.JWK, error) {
	if len(client.JWKS) != 0 {
		return slices.Clone(client.JWKS), nil
	}
	if client.JWKSURI == "" {
		return nil, errors.New("client has no verification keys")
	}
	now := time.Now()
	r.mu.Lock()
	if entry, ok := r.jwks[client.JWKSURI]; ok && now.Before(entry.expiresAt) {
		r.sequence++
		entry.used = r.sequence
		r.jwks[client.JWKSURI] = entry
		keys := slices.Clone(entry.keys)
		r.mu.Unlock()
		return keys, nil
	}
	r.mu.Unlock()

	body, header, err := r.fetchOnce(ctx, client.JWKSURI, clientJWKSMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("fetch client jwks: %w", err)
	}
	keys, err := parseClientJWKS(body)
	if err != nil {
		return nil, fmt.Errorf("validate client jwks: %w", err)
	}
	if ttl := cacheFreshness(header, now); ttl > 0 {
		r.mu.Lock()
		r.sequence++
		storeClientJWKSCache(r.jwks, client.JWKSURI, clientJWKSCacheEntry{keys: slices.Clone(keys), expiresAt: now.Add(ttl), used: r.sequence}, now)
		r.mu.Unlock()
	}
	return keys, nil
}

func (r *clientMetadataResolver) fetchOnce(ctx context.Context, rawURL string, maxBytes int64) ([]byte, http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, clientMetadataTimeout)
	defer cancel()
	key := strconv.FormatInt(maxBytes, 10) + "\x00" + rawURL
	r.fetchMu.Lock()
	if pending := r.fetches[key]; pending != nil {
		r.fetchMu.Unlock()
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-pending.done:
			return slices.Clone(pending.body), pending.header.Clone(), pending.err
		}
	}
	if len(r.fetches) >= clientMetadataFetchLimit {
		r.fetchMu.Unlock()
		return nil, nil, errors.New("client metadata retrieval capacity is exhausted")
	}
	pending := &clientMetadataFetch{done: make(chan struct{})}
	r.fetches[key] = pending
	r.fetchMu.Unlock()

	pending.body, pending.header, pending.err = r.fetch(ctx, rawURL, maxBytes)
	r.fetchMu.Lock()
	delete(r.fetches, key)
	close(pending.done)
	r.fetchMu.Unlock()
	return slices.Clone(pending.body), pending.header.Clone(), pending.err
}

func (r *clientMetadataResolver) fetch(ctx context.Context, rawURL string, maxBytes int64) ([]byte, http.Header, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, nil, err
	}
	transport := &http.Transport{
		Proxy:                  nil,
		DisableKeepAlives:      true,
		ForceAttemptHTTP2:      false,
		MaxResponseHeaderBytes: 16 * 1024,
		ResponseHeaderTimeout:  clientMetadataTimeout,
		TLSHandshakeTimeout:    clientMetadataTimeout,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: r.rootCAs},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return r.dialPublicAddress(ctx, network, address)
		},
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   clientMetadataTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody) //nolint:gosec // URL is HTTPS-only and the transport pins a fully validated public address.
	if err != nil {
		return nil, nil, err
	}
	request.Header.Set("Accept", "application/json, application/*+json")
	response, err := client.Do(request) //nolint:gosec // The no-proxy transport rejects special-use DNS answers before dialing.
	if err != nil {
		return nil, nil, err
	}
	body, header, readErr := readClientMetadataResponse(response, maxBytes)
	closeErr := response.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, nil, err
	}
	return body, header, nil
}

func readClientMetadataResponse(response *http.Response, maxBytes int64) ([]byte, http.Header, error) {
	if response.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("unexpected status %d", response.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || (mediaType != "application/json" && (!strings.HasPrefix(mediaType, "application/") || !strings.HasSuffix(mediaType, "+json"))) {
		return nil, nil, errors.New("response content type is not JSON")
	}
	if response.ContentLength > maxBytes {
		return nil, nil, errors.New("response is too large")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, nil, errors.New("response is too large")
	}
	return body, response.Header.Clone(), nil
}

func (r *clientMetadataResolver) dialPublicAddress(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addresses, err := r.network.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(addresses) == 0 {
		return nil, errors.New("metadata host has no addresses")
	}
	for _, address := range addresses {
		if !isPublicInternetAddress(address) {
			return nil, fmt.Errorf("metadata host resolves to special-use address %s", address)
		}
	}
	var errs []error
	for _, address := range addresses {
		conn, dialErr := r.network.DialContext(ctx, network, net.JoinHostPort(address.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		errs = append(errs, dialErr)
	}
	return nil, errors.Join(errs...)
}

func validateClientIdentifierURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.String() != raw || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("client_id must be a canonical credential-free HTTPS URL without query or fragment")
	}
	if u.Path == "" || u.Path == "/" {
		return errors.New("client_id URL must have a non-root path")
	}
	for segment := range strings.SplitSeq(u.Path, "/") {
		if segment == "." || segment == ".." {
			return errors.New("client_id URL must not contain dot path segments")
		}
	}
	return nil
}

func validateFetchedURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.String() != raw || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("referenced URL must be canonical credential-free HTTPS without a fragment")
	}
	return nil
}

func parseClientMetadata(clientID string, body []byte) (Client, error) {
	fields, err := decodeUniqueJSONObject(body)
	if err != nil {
		return Client{}, err
	}
	var document oauth.ClientIDMetadataDocument
	if err := json.Unmarshal(body, &document); err != nil {
		return Client{}, err
	}
	if document.ClientID != clientID {
		return Client{}, errors.New("client_id does not exactly match metadata URL")
	}
	if document.ClientName == "" {
		return Client{}, errors.New("client_name is required")
	}
	for _, forbidden := range []string{"client_secret", "client_secret_expires_at"} {
		if _, present := fields[forbidden]; present {
			return Client{}, fmt.Errorf("client metadata must not contain %s", forbidden)
		}
	}
	method := document.TokenEndpointAuthMethod
	if _, present := fields["token_endpoint_auth_method"]; !present {
		method = oauth.TokenEndpointAuthNone
	}
	if method != oauth.TokenEndpointAuthNone && method != oauth.TokenEndpointAuthPrivateKeyJWT {
		return Client{}, errors.New("unsupported token_endpoint_auth_method")
	}
	grants := document.GrantTypes
	if _, present := fields["grant_types"]; !present {
		grants = []string{oauth.GrantAuthorizationCode}
	}
	validatedGrants, code, err := validateClientMetadata(document.ClientName, document.RedirectURIs, oauth.TokenEndpointAuthNone, grants, false)
	if err != nil {
		return Client{}, fmt.Errorf("%s: %w", code, err)
	}
	if _, present := fields["response_types"]; present && (len(document.ResponseTypes) != 1 || document.ResponseTypes[0] != oauth.ResponseTypeCode) {
		return Client{}, errors.New("response_types must contain only code")
	}
	client := Client{
		ID:                      clientID,
		Name:                    document.ClientName,
		RedirectURIs:            slices.Clone(document.RedirectURIs),
		TokenEndpointAuthMethod: method,
		GrantTypes:              validatedGrants,
		Provenance:              ClientProvenanceMetadata,
		JWKSURI:                 document.JWKSURI,
	}
	_, hasJWKS := fields["jwks"]
	_, hasJWKSURI := fields["jwks_uri"]
	if method == oauth.TokenEndpointAuthPrivateKeyJWT {
		if hasJWKS == hasJWKSURI {
			return Client{}, errors.New("private_key_jwt requires exactly one of jwks or jwks_uri")
		}
		if hasJWKS {
			client.JWKS, err = parseClientJWKS(fields["jwks"])
			if err != nil {
				return Client{}, err
			}
		} else if err := validateFetchedURL(document.JWKSURI); err != nil {
			return Client{}, fmt.Errorf("invalid jwks_uri: %w", err)
		}
	} else if hasJWKS || hasJWKSURI {
		return Client{}, errors.New("public clients must not publish unused authentication keys")
	}
	return client, nil
}

func parseClientJWKS(body []byte) ([]oauth.JWK, error) {
	fields, err := decodeUniqueJSONObject(body)
	if err != nil {
		return nil, err
	}
	rawKeys, ok := fields["keys"]
	if !ok {
		return nil, errors.New("jwks is missing keys")
	}
	var keyDocuments []json.RawMessage
	if err := json.Unmarshal(rawKeys, &keyDocuments); err != nil {
		return nil, errors.New("jwks keys must be an array")
	}
	if len(keyDocuments) == 0 || len(keyDocuments) > clientJWKSMaxKeys {
		return nil, fmt.Errorf("jwks must contain between 1 and %d keys", clientJWKSMaxKeys)
	}
	keys := make([]oauth.JWK, 0, len(keyDocuments))
	kids := make(map[string]struct{}, len(keyDocuments))
	for _, rawKey := range keyDocuments {
		keyFields, err := decodeUniqueJSONObject(rawKey)
		if err != nil {
			return nil, fmt.Errorf("invalid jwk: %w", err)
		}
		for _, privateMember := range []string{"d", "p", "q", "dp", "dq", "qi", "oth", "k"} {
			if _, present := keyFields[privateMember]; present {
				return nil, fmt.Errorf("jwk contains private or symmetric member %q", privateMember)
			}
		}
		var key oauth.JWK
		if err := json.Unmarshal(rawKey, &key); err != nil {
			return nil, err
		}
		if key.Kid == "" || len(key.Kid) > 128 {
			return nil, errors.New("jwk kid is required and must not exceed 128 bytes")
		}
		if _, duplicate := kids[key.Kid]; duplicate {
			return nil, errors.New("jwk kid values must be unique")
		}
		kids[key.Kid] = struct{}{}
		if !isAsymmetricAlg(key.Alg) {
			return nil, errors.New("jwk alg must name a supported asymmetric algorithm")
		}
		if key.Use != "" && key.Use != "sig" {
			return nil, errors.New("jwk use must be sig")
		}
		if len(key.KeyOps) > 1 || (len(key.KeyOps) == 1 && key.KeyOps[0] != "verify") {
			return nil, errors.New("jwk key_ops must contain only verify")
		}
		if _, err := jwkPublicKey(&key, key.Alg); err != nil {
			return nil, fmt.Errorf("invalid jwk %q: %w", key.Kid, err)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

func decodeUniqueJSONObject(body []byte) (map[string]json.RawMessage, error) {
	if !json.Valid(body) || !validJSONSurrogates(body) {
		return nil, errors.New("document is not valid JSON")
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("document must be a JSON object")
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		fieldToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		field, ok := fieldToken.(string)
		if !ok {
			return nil, errors.New("document field name is invalid")
		}
		if _, duplicate := fields[field]; duplicate {
			return nil, fmt.Errorf("duplicate document field %q", field)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[field] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if token, err := decoder.Token(); err != io.EOF || token != nil {
		return nil, errors.New("unexpected trailing JSON")
	}
	return fields, nil
}

type clientAssertionHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ,omitempty"`
}

type clientAssertionClaims struct {
	Issuer    string `json:"iss"`
	Subject   string `json:"sub"`
	Audience  string `json:"aud"`
	JWTID     string `json:"jti"`
	IssuedAt  int64  `json:"iat"`
	NotBefore int64  `json:"nbf,omitempty"`
	Expiry    int64  `json:"exp"`
}

func verifyClientAssertion(raw, clientID, audience string, keys []oauth.JWK, now time.Time) (clientAssertionClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return clientAssertionClaims{}, errors.New("client assertion must be a compact JWT")
	}
	headerJSON, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil {
		return clientAssertionClaims{}, errors.New("client assertion header is invalid")
	}
	var header clientAssertionHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return clientAssertionClaims{}, err
	}
	headerFields, err := decodeUniqueJSONObject(headerJSON)
	if err != nil {
		return clientAssertionClaims{}, err
	}
	for field := range headerFields {
		if field != "alg" && field != "kid" && field != "typ" {
			return clientAssertionClaims{}, fmt.Errorf("unsupported client assertion header %q", field)
		}
	}
	if header.Kid == "" || !isAsymmetricAlg(header.Alg) || (header.Typ != "" && header.Typ != "JWT") {
		return clientAssertionClaims{}, errors.New("client assertion header is unsupported")
	}
	var selected *oauth.JWK
	for i := range keys {
		if keys[i].Kid == header.Kid && keys[i].Alg == header.Alg {
			if selected != nil {
				return clientAssertionClaims{}, errors.New("client assertion key selection is ambiguous")
			}
			selected = &keys[i]
		}
	}
	if selected == nil {
		return clientAssertionClaims{}, errors.New("client assertion key is unknown")
	}
	payloadJSON, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return clientAssertionClaims{}, errors.New("client assertion payload is invalid")
	}
	if _, err := decodeUniqueJSONObject(payloadJSON); err != nil {
		return clientAssertionClaims{}, err
	}
	var claims clientAssertionClaims
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return clientAssertionClaims{}, err
	}
	nowUnix := now.Unix()
	skew := int64(time.Minute / time.Second)
	maxAge := int64(clientAssertionMaxAge / time.Second)
	if claims.Issuer != clientID || claims.Subject != clientID || claims.Audience != audience {
		return clientAssertionClaims{}, errors.New("client assertion identity or audience is invalid")
	}
	if claims.JWTID == "" || len(claims.JWTID) > 256 || claims.IssuedAt == 0 || claims.Expiry == 0 || claims.IssuedAt > nowUnix+skew || claims.IssuedAt < nowUnix-maxAge || claims.NotBefore > nowUnix+skew || claims.Expiry <= nowUnix-skew || claims.Expiry <= claims.IssuedAt || claims.Expiry > claims.IssuedAt+maxAge {
		return clientAssertionClaims{}, errors.New("client assertion time or jti claims are invalid")
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	if err != nil {
		return clientAssertionClaims{}, errors.New("client assertion signature is invalid")
	}
	publicKey, err := jwkPublicKey(selected, header.Alg)
	if err != nil {
		return clientAssertionClaims{}, err
	}
	if err := verifyJWS(publicKey, header.Alg, []byte(parts[0]+"."+parts[1]), signature); err != nil {
		return clientAssertionClaims{}, errors.New("client assertion signature is invalid")
	}
	return claims, nil
}

func cacheFreshness(header http.Header, now time.Time) time.Duration {
	ttl := clientMetadataMaxTTL
	if header.Get("Cache-Control") == "" && strings.EqualFold(strings.TrimSpace(header.Get("Pragma")), "no-cache") {
		return 0
	}
	for directive := range strings.SplitSeq(strings.Join(header.Values("Cache-Control"), ","), ",") {
		name, value, hasValue := strings.Cut(strings.TrimSpace(strings.ToLower(directive)), "=")
		switch name {
		case "no-cache", "no-store":
			return 0
		case "max-age", "s-maxage":
			if !hasValue {
				continue
			}
			seconds, err := strconv.ParseInt(strings.Trim(value, `"`), 10, 64)
			if err == nil {
				if seconds <= 0 {
					return 0
				}
				ttl = min(ttl, time.Duration(min(seconds, int64(clientMetadataMaxTTL/time.Second)))*time.Second)
			}
		}
	}
	if expires, err := http.ParseTime(header.Get("Expires")); err == nil {
		ttl = min(ttl, max(expires.Sub(now), 0))
	}
	if age, err := strconv.ParseInt(header.Get("Age"), 10, 64); err == nil && age > 0 {
		if age >= int64(ttl/time.Second) {
			return 0
		}
		ttl -= time.Duration(age) * time.Second
	}
	return min(ttl, clientMetadataMaxTTL)
}

func storeClientMetadataCache(cache map[string]clientMetadataCacheEntry, key string, entry *clientMetadataCacheEntry, now time.Time) {
	for cachedKey := range cache {
		if !now.Before(cache[cachedKey].expiresAt) {
			delete(cache, cachedKey)
		}
	}
	if len(cache) >= clientMetadataCacheLimit {
		oldestKey := key
		oldestUse := ^uint64(0)
		for cachedKey := range cache {
			if cache[cachedKey].used < oldestUse {
				oldestKey, oldestUse = cachedKey, cache[cachedKey].used
			}
		}
		delete(cache, oldestKey)
	}
	cache[key] = *entry
}

func storeClientJWKSCache(cache map[string]clientJWKSCacheEntry, key string, entry clientJWKSCacheEntry, now time.Time) {
	for cachedKey, cached := range cache {
		if !now.Before(cached.expiresAt) {
			delete(cache, cachedKey)
		}
	}
	if len(cache) >= clientJWKSCacheLimit {
		oldestKey := key
		oldestUse := ^uint64(0)
		for cachedKey, cached := range cache {
			if cached.used < oldestUse {
				oldestKey, oldestUse = cachedKey, cached.used
			}
		}
		delete(cache, oldestKey)
	}
	cache[key] = entry
}

func cloneClient(client *Client) Client {
	cloned := *client
	cloned.RedirectURIs = slices.Clone(client.RedirectURIs)
	cloned.GrantTypes = slices.Clone(client.GrantTypes)
	cloned.JWKS = slices.Clone(client.JWKS)
	for i := range cloned.JWKS {
		cloned.JWKS[i].KeyOps = slices.Clone(cloned.JWKS[i].KeyOps)
	}
	return cloned
}

func isPublicInternetAddress(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsValid() || !address.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range specialUsePrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

var specialUsePrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.31.196.0/24"),
	netip.MustParsePrefix("192.52.193.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("192.175.48.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("2620:4f:8000::/48"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
}
