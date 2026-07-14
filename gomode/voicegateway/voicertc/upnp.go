// UPnP port mapping for the voice gateway WebRTC UDP listener.

package voicertc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

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
		var clientErr error
		clients, clientErr = appendUPnPClientsByURL(ctx, clients, loc)
		err = errors.Join(err, clientErr)
	}
	return clients, err
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

func appendUPnPClientsByURL(ctx context.Context, clients []upnpWANConnection, loc *url.URL) ([]upnpWANConnection, error) {
	var err error
	ip2, ip2Err := internetgateway2.NewWANIPConnection2ClientsByURLCtx(ctx, loc)
	err = errors.Join(err, ip2Err)
	for _, client := range ip2 {
		clients = append(clients, client)
	}

	ip2v1, ip2v1Err := internetgateway2.NewWANIPConnection1ClientsByURLCtx(ctx, loc)
	err = errors.Join(err, ip2v1Err)
	for _, client := range ip2v1 {
		clients = append(clients, client)
	}

	ip1, ip1Err := internetgateway1.NewWANIPConnection1ClientsByURLCtx(ctx, loc)
	err = errors.Join(err, ip1Err)
	for _, client := range ip1 {
		clients = append(clients, client)
	}

	ppp2, ppp2Err := internetgateway2.NewWANPPPConnection1ClientsByURLCtx(ctx, loc)
	err = errors.Join(err, ppp2Err)
	for _, client := range ppp2 {
		clients = append(clients, client)
	}

	ppp1, ppp1Err := internetgateway1.NewWANPPPConnection1ClientsByURLCtx(ctx, loc)
	err = errors.Join(err, ppp1Err)
	for _, client := range ppp1 {
		clients = append(clients, client)
	}
	return clients, err
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
	externalPort, err := addInitialUPnPPortMapping(ctx, client, internalIP, internalPort)
	if err != nil {
		return nil, err
	}
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

func addInitialUPnPPortMapping(ctx context.Context, client upnpWANConnection, internalIP string, internalPort uint16) (uint16, error) {
	leaseSeconds := uint32(upnpLeaseDuration / time.Second)
	requestCtx, cancel := context.WithTimeout(ctx, upnpTimeout)
	defer cancel()
	anyClient, ok := client.(upnpWANAnyPortConnection)
	if !ok {
		return addExactUPnPPortMapping(requestCtx, client, internalIP, internalPort)
	}
	externalPort, err := anyClient.AddAnyPortMappingCtx(requestCtx, "", internalPort, upnpProtocolUDP, internalPort, internalIP, true, upnpMappingDescription, leaseSeconds)
	if err != nil {
		exactPort, exactErr := addExactUPnPPortMapping(requestCtx, client, internalIP, internalPort)
		if exactErr == nil {
			return exactPort, nil
		}
		return 0, errors.Join(fmt.Errorf("add any UPnP UDP mapping: %w", err), exactErr)
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
