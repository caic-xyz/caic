// UPnP port mapping for the voice gateway WebRTC UDP listener.

package voicertc

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/huin/goupnp"
	internetgateway1 "github.com/huin/goupnp/dcps/internetgateway1"
	internetgateway2 "github.com/huin/goupnp/dcps/internetgateway2"
	"github.com/huin/goupnp/httpu"
	"github.com/huin/goupnp/ssdp"
)

const (
	upnpMappingDescription = "caic voice WebRTC"
	upnpProtocolUDP        = "UDP"
	upnpLeaseDuration      = 30 * time.Minute
	upnpRefreshInterval    = upnpLeaseDuration / 2
	upnpTimeout            = 5 * time.Second
	upnpSSDPTimeout        = 2 * time.Second
)

type upnpWANConnection interface {
	AddPortMappingCtx(ctx context.Context, remoteHost string, externalPort uint16, protocol string, internalPort uint16, internalClient string, enabled bool, description string, leaseDuration uint32) error
	DeletePortMappingCtx(ctx context.Context, remoteHost string, externalPort uint16, protocol string) error
	GetExternalIPAddressCtx(ctx context.Context) (string, error)
}

type upnpWANAnyPortConnection interface {
	AddAnyPortMappingCtx(ctx context.Context, remoteHost string, externalPort uint16, protocol string, internalPort uint16, internalClient string, enabled bool, description string, leaseDuration uint32) (uint16, error)
}

type upnpWANGenericPortMappingConnection interface {
	GetGenericPortMappingEntryCtx(ctx context.Context, index uint16) (remoteHost string, externalPort uint16, protocol string, internalPort uint16, internalClient string, enabled bool, description string, leaseDuration uint32, err error)
}

type upnpWANSpecificPortMappingConnection interface {
	GetSpecificPortMappingEntryCtx(ctx context.Context, remoteHost string, externalPort uint16, protocol string) (internalPort uint16, internalClient string, enabled bool, description string, leaseDuration uint32, err error)
}

type upnpServiceClient interface {
	GetServiceClient() *goupnp.ServiceClient
}

type upnpMapping struct {
	client       upnpWANConnection
	ip           net.IP
	externalPort uint16
	internalPort uint16

	refreshCancel context.CancelFunc
	refreshDone   chan struct{}

	refreshMu  sync.Mutex
	refreshErr error
}

func (m *upnpMapping) close(ctx context.Context) error {
	m.stopRefresh()
	ctx, cancel := context.WithTimeout(ctx, upnpTimeout)
	defer cancel()
	if err := m.client.DeletePortMappingCtx(ctx, "", m.externalPort, upnpProtocolUDP); err != nil {
		return fmt.Errorf("delete UPnP UDP mapping %d: %w", m.externalPort, err)
	}
	return nil
}

func mapUPnPUDP(ctx context.Context, internalIP net.IP, port int) (*upnpMapping, error) {
	v4 := internalIP.To4()
	if v4 == nil {
		return nil, fmt.Errorf("UPnP requires an IPv4 internal address, got %s", internalIP)
	}
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("UPnP requires a UDP port between 1 and 65535, got %d", port)
	}

	ctx, cancel := context.WithTimeout(ctx, upnpTimeout)
	defer cancel()

	clients, discoveryErr := discoverUPnPConnections(ctx, v4)
	if len(clients) == 0 {
		return nil, upnpDiscoveryError(discoveryErr)
	}

	var attempts []error
	for _, client := range clients {
		mapping, err := tryMapUPnPUDP(ctx, client, v4.String(), uint16(port))
		if err == nil {
			return mapping, nil
		}
		attempts = append(attempts, err)
	}
	return nil, fmt.Errorf("add UPnP UDP mapping %d: %w", port, errors.Join(attempts...))
}

func discoverUPnPConnections(ctx context.Context, hostIP net.IP) ([]upnpWANConnection, error) {
	locations, err := discoverUPnPLocations(ctx, hostIP)
	clients := make([]upnpWANConnection, 0, len(locations))
	for _, loc := range locations {
		if routeErr := validateUPnPGatewayRoute(ctx, hostIP, loc); routeErr != nil {
			slog.WarnContext(ctx, "voicertc: reject UPnP gateway with mismatched route source", "location", loc.String(), "hostIP", hostIP, "err", routeErr)
			err = errors.Join(err, routeErr)
			continue
		}
		var clientErr error
		clients, clientErr = appendUPnPClientsByURL(ctx, clients, loc, hostIP)
		err = errors.Join(err, clientErr)
	}
	return clients, err
}

// validateUPnPGatewayRoute verifies that routing to the UPnP gateway selects
// hostIP as the source address used for NewInternalClient.
func validateUPnPGatewayRoute(ctx context.Context, hostIP net.IP, loc *url.URL) error {
	gatewayIP, err := urlIPv4(loc)
	if err != nil {
		return err
	}
	sourceIP, err := gatewayRouteSourceIPv4(ctx, gatewayIP)
	if err != nil {
		return err
	}
	return sameIPv4Address(hostIP, sourceIP)
}

// gatewayRouteSourceIPv4 returns the source address the OS selects for gatewayIP.
// Dialing UDP establishes no network traffic.
func gatewayRouteSourceIPv4(ctx context.Context, gatewayIP net.IP) (sourceIP net.IP, retErr error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "udp4", net.JoinHostPort(gatewayIP.String(), "9"))
	if err != nil {
		return nil, fmt.Errorf("route to UPnP gateway %s: %w", gatewayIP, err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close UPnP gateway route probe: %w", err))
		}
	}()
	address, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil, fmt.Errorf("UPnP gateway route has unexpected local address type %T", conn.LocalAddr())
	}
	return address.IP, nil
}

// sameIPv4Address verifies that expected and actual are the same IPv4 address.
func sameIPv4Address(expected, actual net.IP) error {
	expected = expected.To4()
	actual = actual.To4()
	if expected == nil || actual == nil {
		return fmt.Errorf("UPnP requires IPv4 internal and route source addresses, got internal %s and route source %s", expected, actual)
	}
	if !expected.Equal(actual) {
		return fmt.Errorf("UPnP gateway route selects source IP %s, want internal client IP %s", actual, expected)
	}
	return nil
}

// urlIPv4 returns the literal IPv4 address in a URL hostname.
func urlIPv4(loc *url.URL) (net.IP, error) {
	host := loc.Hostname()
	if host == "" {
		return nil, fmt.Errorf("UPnP gateway location %q has no hostname", loc)
	}
	ip := net.ParseIP(host).To4()
	if ip == nil {
		return nil, fmt.Errorf("UPnP gateway location %q must use a literal IPv4 address", loc)
	}
	return ip, nil
}

func discoverUPnPLocations(ctx context.Context, hostIP net.IP) ([]*url.URL, error) {
	client, err := httpu.NewHTTPUClientAddr(hostIP.String())
	if err != nil {
		return nil, fmt.Errorf("create SSDP client on %s: %w", hostIP, err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			slog.WarnContext(ctx, "voicertc: close SSDP client", "err", err)
		}
	}()
	searchCtx, cancel := context.WithTimeout(ctx, upnpSSDPTimeout)
	defer cancel()
	responses, err := ssdp.RawSearch(searchCtx, client, ssdp.SSDPAll, 3) //nolint:bodyclose // closed below by closeSSDPResponses.
	if err != nil {
		return nil, fmt.Errorf("SSDP search: %w", err)
	}
	defer closeSSDPResponses(ctx, responses)
	return upnpLocationsFromResponses(responses), nil
}

func upnpLocationsFromResponses(responses []*http.Response) []*url.URL {
	seen := make(map[string]struct{})
	locations := make([]*url.URL, 0, len(responses))
	for _, response := range responses {
		loc, err := response.Location()
		if err != nil {
			continue
		}
		key := loc.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		locations = append(locations, loc)
	}
	return locations
}

func appendUPnPClientsByURL(ctx context.Context, clients []upnpWANConnection, loc *url.URL, hostIP net.IP) ([]upnpWANConnection, error) {
	// Build clients from the verified root description. The goupnp v1 ByURL
	// constructors would fetch it again using their package-global HTTP client,
	// losing this gateway's source-address and redirect policy.
	root, err := upnpRootDeviceByURL(ctx, loc, hostIP)
	if err != nil {
		return clients, err
	}
	base := loc
	if root.URLBaseStr != "" {
		base, err = url.Parse(root.URLBaseStr)
		if err != nil {
			return clients, fmt.Errorf("parse UPnP URL base %q: %w", root.URLBaseStr, err)
		}
		if err := validateUPnPGatewayRoute(ctx, hostIP, base); err != nil {
			return clients, fmt.Errorf("validate UPnP URL base: %w", err)
		}
	}
	root.SetURLBase(base)

	var connectionErr error
	var clientErr error
	ip2, ip2Err := internetgateway2.NewWANIPConnection2ClientsFromRootDevice(root, loc)
	connectionErr = errors.Join(connectionErr, ip2Err)
	for _, client := range ip2 {
		clients, clientErr = appendUPnPClient(ctx, clients, client, hostIP)
		connectionErr = errors.Join(connectionErr, clientErr)
	}

	ip2v1, ip2v1Err := internetgateway2.NewWANIPConnection1ClientsFromRootDevice(root, loc)
	connectionErr = errors.Join(connectionErr, ip2v1Err)
	for _, client := range ip2v1 {
		clients, clientErr = appendUPnPClient(ctx, clients, client, hostIP)
		connectionErr = errors.Join(connectionErr, clientErr)
	}

	ip1, ip1Err := internetgateway1.NewWANIPConnection1ClientsFromRootDevice(root, loc)
	connectionErr = errors.Join(connectionErr, ip1Err)
	for _, client := range ip1 {
		clients, clientErr = appendUPnPClient(ctx, clients, client, hostIP)
		connectionErr = errors.Join(connectionErr, clientErr)
	}

	ppp2, ppp2Err := internetgateway2.NewWANPPPConnection1ClientsFromRootDevice(root, loc)
	connectionErr = errors.Join(connectionErr, ppp2Err)
	for _, client := range ppp2 {
		clients, clientErr = appendUPnPClient(ctx, clients, client, hostIP)
		connectionErr = errors.Join(connectionErr, clientErr)
	}

	ppp1, ppp1Err := internetgateway1.NewWANPPPConnection1ClientsFromRootDevice(root, loc)
	connectionErr = errors.Join(connectionErr, ppp1Err)
	for _, client := range ppp1 {
		clients, clientErr = appendUPnPClient(ctx, clients, client, hostIP)
		connectionErr = errors.Join(connectionErr, clientErr)
	}
	return clients, connectionErr
}

// upnpRootDeviceByURL fetches a UPnP device description from hostIP without
// following redirects.
//
// goupnp v1's DeviceByURLCtx uses a package-global HTTPClientDefault, so it
// cannot enforce our per-gateway source-address and redirect policy. This is a
// small copy of its root-description parsing path until the dependency offers
// a per-call HTTP client for discovery.
func upnpRootDeviceByURL(ctx context.Context, loc *url.URL, hostIP net.IP) (root *goupnp.RootDevice, retErr error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loc.String(), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("create UPnP device description request: %w", err)
	}
	client, err := newUPnPHTTPClient(hostIP)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get UPnP device description: %w", err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close UPnP device description response: %w", err))
		}
	}()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get UPnP device description: HTTP %s", response.Status)
	}
	root = new(goupnp.RootDevice)
	decoder := xml.NewDecoder(response.Body)
	decoder.DefaultSpace = goupnp.DeviceXMLNamespace
	decoder.CharsetReader = goupnp.CharsetReaderDefault
	if err := decoder.Decode(root); err != nil {
		return nil, fmt.Errorf("decode UPnP device description: %w", err)
	}
	return root, nil
}

// appendUPnPClient adds a UPnP service client after validating and securing its
// SOAP control endpoint.
func appendUPnPClient(ctx context.Context, clients []upnpWANConnection, client upnpWANConnection, hostIP net.IP) ([]upnpWANConnection, error) {
	serviceClient, ok := client.(upnpServiceClient)
	if !ok {
		return clients, fmt.Errorf("UPnP service client %T does not expose its control endpoint", client)
	}
	service := serviceClient.GetServiceClient()
	if service == nil || service.SOAPClient == nil {
		return clients, fmt.Errorf("UPnP service client %T has no SOAP control endpoint", client)
	}
	endpoint := service.SOAPClient.EndpointURL
	if err := validateUPnPGatewayRoute(ctx, hostIP, &endpoint); err != nil {
		return clients, fmt.Errorf("validate UPnP SOAP control endpoint: %w", err)
	}
	httpClient, err := newUPnPHTTPClient(hostIP)
	if err != nil {
		return clients, err
	}
	// goupnp v1 exposes the SOAP client on each generated service, unlike its
	// root-description fetcher. Preserve the same source-address and redirect
	// policy for every mapping and refresh request.
	service.SOAPClient.HTTPClient = *httpClient
	return append(clients, client), nil
}

// newUPnPHTTPClient returns an HTTP client that uses hostIP as its source
// address and rejects redirects.
func newUPnPHTTPClient(hostIP net.IP) (*http.Client, error) {
	v4 := hostIP.To4()
	if v4 == nil {
		return nil, fmt.Errorf("UPnP HTTP client requires an IPv4 source address, got %s", hostIP)
	}
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("UPnP HTTP client requires *http.Transport, got %T", http.DefaultTransport)
	}
	transport = transport.Clone()
	dialer := &net.Dialer{LocalAddr: &net.TCPAddr{IP: v4}}
	transport.DialContext = dialer.DialContext
	return &http.Client{Transport: transport, CheckRedirect: rejectUPnPRedirect}, nil
}

// rejectUPnPRedirect prevents UPnP requests from leaving their validated endpoint.
func rejectUPnPRedirect(_ *http.Request, _ []*http.Request) error {
	return errors.New("UPnP HTTP redirects are not permitted")
}

func closeSSDPResponses(ctx context.Context, responses []*http.Response) {
	for _, response := range responses {
		if response.Body == nil {
			continue
		}
		if err := response.Body.Close(); err != nil {
			slog.WarnContext(ctx, "voicertc: close SSDP response body", "err", err)
		}
	}
}

func upnpDiscoveryError(err error) error {
	if err == nil {
		return errors.New("discover UPnP internet gateway: no WANIPConnection or WANPPPConnection services found")
	}
	return fmt.Errorf("discover UPnP internet gateway: %w", err)
}

func tryMapUPnPUDP(ctx context.Context, client upnpWANConnection, internalIP string, internalPort uint16) (*upnpMapping, error) {
	logUPnPService(ctx, client)
	externalPort, err := addInitialUPnPPortMapping(ctx, client, internalIP, internalPort)
	if err != nil {
		return nil, err
	}
	logUPnPPortMappings(ctx, client)
	logUPnPSpecificPortMapping(ctx, client, externalPort)
	external, err := client.GetExternalIPAddressCtx(ctx)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("get UPnP external IP: %w", err),
			deleteUPnPPortMapping(ctx, client, externalPort),
		)
	}
	ip := net.ParseIP(external).To4()
	if ip == nil {
		return nil, errors.Join(
			fmt.Errorf("UPnP gateway returned invalid external IPv4 address %q", external),
			deleteUPnPPortMapping(ctx, client, externalPort),
		)
	}
	if ip.IsPrivate() {
		slog.WarnContext(ctx, "UPnP gateway external address is private; voice may still fail behind double NAT", "ip", ip.String())
	}
	mapping := &upnpMapping{client: client, ip: append(net.IP(nil), ip...), externalPort: externalPort, internalPort: internalPort}
	mapping.startRefresh(ctx, internalIP)
	return mapping, nil
}

func logUPnPService(ctx context.Context, client upnpWANConnection) {
	serviceClient, ok := client.(upnpServiceClient)
	if !ok {
		return
	}
	service := serviceClient.GetServiceClient()
	if service == nil || service.Service == nil || service.SOAPClient == nil {
		return
	}
	slog.InfoContext(ctx, "voicertc: UPnP service", "location", service.Location, "serviceID", service.Service.ServiceId, "serviceType", service.Service.ServiceType, "controlURL", service.SOAPClient.EndpointURL)
}

func logUPnPPortMappings(ctx context.Context, client upnpWANConnection) {
	mappingClient, ok := client.(upnpWANGenericPortMappingConnection)
	if !ok {
		slog.InfoContext(ctx, "voicertc: UPnP gateway cannot list port mappings")
		return
	}
	for index := range uint16(64) {
		remoteHost, externalPort, protocol, internalPort, internalClient, enabled, description, leaseDuration, err := mappingClient.GetGenericPortMappingEntryCtx(ctx, index)
		if err != nil {
			return
		}
		slog.DebugContext(ctx, "voicertc: UPnP port mapping", "index", index, "remoteHost", remoteHost, "externalPort", externalPort, "protocol", protocol, "internalPort", internalPort, "internalClient", internalClient, "enabled", enabled, "description", description, "leaseDuration", leaseDuration)
	}
	slog.WarnContext(ctx, "voicertc: UPnP port mapping list reached limit", "limit", 64)
}

func logUPnPSpecificPortMapping(ctx context.Context, client upnpWANConnection, externalPort uint16) {
	mappingClient, ok := client.(upnpWANSpecificPortMappingConnection)
	if !ok {
		slog.InfoContext(ctx, "voicertc: UPnP gateway cannot query port mapping", "externalPort", externalPort)
		return
	}
	internalPort, internalClient, enabled, description, leaseDuration, err := mappingClient.GetSpecificPortMappingEntryCtx(ctx, "", externalPort, upnpProtocolUDP)
	if err != nil {
		slog.WarnContext(ctx, "voicertc: query UPnP port mapping", "externalPort", externalPort, "err", err)
		return
	}
	slog.InfoContext(ctx, "voicertc: UPnP specific port mapping", "externalPort", externalPort, "protocol", upnpProtocolUDP, "internalPort", internalPort, "internalClient", internalClient, "enabled", enabled, "description", description, "leaseDuration", leaseDuration)
}

func addInitialUPnPPortMapping(ctx context.Context, client upnpWANConnection, internalIP string, internalPort uint16) (uint16, error) {
	leaseSeconds := uint32(upnpLeaseDuration / time.Second)
	requestCtx, cancel := context.WithTimeout(ctx, upnpTimeout)
	defer cancel()
	slog.InfoContext(requestCtx, "voicertc: request UPnP exact port mapping", "externalPort", internalPort, "internalPort", internalPort, "internalClient", internalIP)
	exactPort, exactErr := addExactUPnPPortMapping(requestCtx, client, internalIP, internalPort)
	if exactErr == nil {
		return exactPort, nil
	}
	anyClient, ok := client.(upnpWANAnyPortConnection)
	if !ok {
		return 0, exactErr
	}
	slog.WarnContext(requestCtx, "voicertc: exact UPnP port mapping failed; trying any port", "externalPort", internalPort, "internalPort", internalPort, "internalClient", internalIP, "err", exactErr)
	externalPort, anyErr := anyClient.AddAnyPortMappingCtx(requestCtx, "", internalPort, upnpProtocolUDP, internalPort, internalIP, true, upnpMappingDescription, leaseSeconds)
	if anyErr != nil {
		return 0, errors.Join(fmt.Errorf("add exact UPnP UDP mapping: %w", exactErr), fmt.Errorf("add any UPnP UDP mapping: %w", anyErr))
	}
	if externalPort == 0 {
		return 0, errors.New("UPnP gateway returned invalid external UDP port 0")
	}
	return externalPort, nil
}

func addExactUPnPPortMapping(ctx context.Context, client upnpWANConnection, internalIP string, internalPort uint16) (uint16, error) {
	leaseSeconds := uint32(upnpLeaseDuration / time.Second)
	return internalPort, client.AddPortMappingCtx(ctx, "", internalPort, upnpProtocolUDP, internalPort, internalIP, true, upnpMappingDescription, leaseSeconds)
}

func refreshUPnPPortMapping(ctx context.Context, client upnpWANConnection, internalIP string, externalPort, internalPort uint16) error {
	leaseSeconds := uint32(upnpLeaseDuration / time.Second)
	refreshCtx, cancel := context.WithTimeout(ctx, upnpTimeout)
	defer cancel()
	return client.AddPortMappingCtx(refreshCtx, "", externalPort, upnpProtocolUDP, internalPort, internalIP, true, upnpMappingDescription, leaseSeconds)
}

func (m *upnpMapping) startRefresh(ctx context.Context, internalIP string) {
	refreshCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	m.refreshCancel = cancel
	m.refreshDone = make(chan struct{})
	go func() {
		defer close(m.refreshDone)
		timer := time.NewTimer(upnpRefreshInterval)
		defer timer.Stop()
		for {
			select {
			case <-refreshCtx.Done():
				return
			case <-timer.C:
				if err := m.refresh(refreshCtx, internalIP); err != nil {
					slog.ErrorContext(refreshCtx, "voicertc: refresh UPnP UDP mapping", "externalPort", m.externalPort, "internalPort", m.internalPort, "err", err)
				}
				timer.Reset(upnpRefreshInterval)
			}
		}
	}()
}

func (m *upnpMapping) refresh(ctx context.Context, internalIP string) error {
	err := refreshUPnPPortMapping(ctx, m.client, internalIP, m.externalPort, m.internalPort)
	if err != nil {
		err = fmt.Errorf("refresh UPnP UDP mapping %d -> %d: %w", m.externalPort, m.internalPort, err)
	}
	m.setRefreshErr(err)
	return err
}

func (m *upnpMapping) setRefreshErr(err error) {
	m.refreshMu.Lock()
	m.refreshErr = err
	m.refreshMu.Unlock()
}

func (m *upnpMapping) refreshError() string {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	if m.refreshErr == nil {
		return ""
	}
	return m.refreshErr.Error()
}

func (m *upnpMapping) stopRefresh() {
	if m.refreshCancel == nil || m.refreshDone == nil {
		return
	}
	m.refreshCancel()
	<-m.refreshDone
	m.refreshCancel = nil
	m.refreshDone = nil
}

func deleteUPnPPortMapping(ctx context.Context, client upnpWANConnection, port uint16) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), upnpTimeout)
	defer cancel()
	return client.DeletePortMappingCtx(cleanupCtx, "", port, upnpProtocolUDP)
}
