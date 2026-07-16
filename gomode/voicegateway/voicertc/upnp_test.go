// Tests for UPnP port mapping for the WebRTC UDP listener.

package voicertc

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type upnpWANConnectionFake struct {
	externalIP string
	addErr     error
	exactErr   error
	ipErr      error
	deleteErr  error

	added               bool
	deleted             bool
	remoteHost          string
	externalPort        uint16
	protocol            string
	internalPort        uint16
	internalIP          string
	enabled             bool
	description         string
	leaseDuration       uint32
	deleteCtxErr        error
	deletedExternalPort uint16
}

func (f *upnpWANConnectionFake) AddPortMappingCtx(_ context.Context, remoteHost string, externalPort uint16, protocol string, internalPort uint16, internalClient string, enabled bool, description string, leaseDuration uint32) error {
	if f.exactErr != nil {
		return f.exactErr
	}
	if f.addErr != nil {
		return f.addErr
	}
	f.added = true
	f.remoteHost = remoteHost
	f.externalPort = externalPort
	f.protocol = protocol
	f.internalPort = internalPort
	f.internalIP = internalClient
	f.enabled = enabled
	f.description = description
	f.leaseDuration = leaseDuration
	return nil
}

func (f *upnpWANConnectionFake) DeletePortMappingCtx(ctx context.Context, _ string, externalPort uint16, _ string) error {
	f.deleted = true
	f.deleteCtxErr = ctx.Err()
	f.deletedExternalPort = externalPort
	return f.deleteErr
}

func (f *upnpWANConnectionFake) GetExternalIPAddressCtx(_ context.Context) (string, error) {
	if f.ipErr != nil {
		return "", f.ipErr
	}
	return f.externalIP, nil
}

type upnpWANAnyPortConnectionFake struct {
	upnpWANConnectionFake

	reservedPort uint16
}

func (f *upnpWANAnyPortConnectionFake) AddAnyPortMappingCtx(_ context.Context, remoteHost string, externalPort uint16, protocol string, internalPort uint16, internalClient string, enabled bool, description string, leaseDuration uint32) (uint16, error) {
	if f.addErr != nil {
		return 0, f.addErr
	}
	f.added = true
	f.remoteHost = remoteHost
	f.externalPort = externalPort
	f.protocol = protocol
	f.internalPort = internalPort
	f.internalIP = internalClient
	f.enabled = enabled
	f.description = description
	f.leaseDuration = leaseDuration
	return f.reservedPort, nil
}

func TestTryMapUPnPUDP(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		client := &upnpWANConnectionFake{externalIP: "203.0.113.10"}
		mapping, err := tryMapUPnPUDP(t.Context(), client, "192.168.1.20", 3478)
		if err != nil {
			t.Fatal(err)
		}
		if !mapping.ip.Equal(net.ParseIP("203.0.113.10")) {
			t.Errorf("mapping.ip = %s, want 203.0.113.10", mapping.ip)
		}
		if mapping.externalPort != 3478 || mapping.internalPort != 3478 {
			t.Errorf("mapping ports = external %d internal %d, want 3478/3478", mapping.externalPort, mapping.internalPort)
		}
		if !client.added {
			t.Fatal("AddPortMappingCtx was not called")
		}
		if client.remoteHost != "" || client.externalPort != 3478 || client.protocol != upnpProtocolUDP || client.internalPort != 3478 || client.internalIP != "192.168.1.20" || !client.enabled || client.description != upnpMappingDescription || client.leaseDuration != uint32(upnpLeaseDuration/time.Second) {
			t.Fatalf("AddPortMappingCtx args = %+v", client)
		}
		if client.deleted {
			t.Fatal("DeletePortMappingCtx was called")
		}
		if err := mapping.close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("valid add any", func(t *testing.T) {
		t.Parallel()
		client := &upnpWANAnyPortConnectionFake{upnpWANConnectionFake: upnpWANConnectionFake{externalIP: "203.0.113.10", exactErr: errors.New("port in use")}, reservedPort: 40000}
		mapping, err := tryMapUPnPUDP(t.Context(), client, "192.168.1.20", 3478)
		if err != nil {
			t.Fatal(err)
		}
		if mapping.externalPort != 40000 || mapping.internalPort != 3478 {
			t.Errorf("mapping ports = external %d internal %d, want 40000/3478", mapping.externalPort, mapping.internalPort)
		}
		if client.externalPort != 3478 || client.internalPort != 3478 {
			t.Fatalf("AddAnyPortMappingCtx ports = external %d internal %d, want 3478/3478", client.externalPort, client.internalPort)
		}
		if err := mapping.close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if client.deletedExternalPort != 40000 {
			t.Fatalf("deletedExternalPort = %d, want 40000", client.deletedExternalPort)
		}
	})

	t.Run("error", func(t *testing.T) {
		t.Parallel()
		client := &upnpWANConnectionFake{externalIP: "not-an-ip"}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := tryMapUPnPUDP(ctx, client, "192.168.1.20", 3478)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "invalid external IPv4") {
			t.Fatalf("unexpected error: %v", err)
		}
		if !client.deleted {
			t.Fatal("DeletePortMappingCtx was not called")
		}
		if client.deleteCtxErr != nil {
			t.Fatalf("delete context err = %v, want nil", client.deleteCtxErr)
		}
	})
}

func TestUpnpMapping(t *testing.T) {
	t.Parallel()
	t.Run("refresh surfaces error", func(t *testing.T) {
		t.Parallel()
		client := &upnpWANConnectionFake{addErr: errors.New("boom")}
		mapping := &upnpMapping{client: client, externalPort: 40000, internalPort: 3478}
		err := mapping.refresh(t.Context(), "192.168.1.20")
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(mapping.refreshError(), "boom") {
			t.Fatalf("refreshError = %q, want boom", mapping.refreshError())
		}
		client.addErr = nil
		if err := mapping.refresh(t.Context(), "192.168.1.20"); err != nil {
			t.Fatal(err)
		}
		if mapping.refreshError() != "" {
			t.Fatalf("refreshError = %q, want empty", mapping.refreshError())
		}
	})
}

func TestMapUPnPUDP(t *testing.T) {
	t.Parallel()
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		_, err := mapUPnPUDP(t.Context(), net.ParseIP("192.168.1.20"), 0)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "between 1 and 65535") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestUpnpLocationsFromResponses(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		responses := []*http.Response{
			{Header: http.Header{"Location": {"http://192.168.1.1/root.xml"}}},
			{Header: http.Header{"Location": {"http://192.168.1.1/root.xml"}}},
			{Header: http.Header{"Location": {"http://192.168.1.2/root.xml"}}},
			{Header: http.Header{}},
		}
		locations := upnpLocationsFromResponses(responses)
		if len(locations) != 2 {
			t.Fatalf("len(locations) = %d, want 2", len(locations))
		}
		if locations[0].String() != "http://192.168.1.1/root.xml" || locations[1].String() != "http://192.168.1.2/root.xml" {
			t.Fatalf("locations = %v", locations)
		}
	})
}

func TestSameIPv4Address(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		if err := sameIPv4Address(net.ParseIP("192.168.1.123"), net.ParseIP("192.168.1.123")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		err := sameIPv4Address(net.ParseIP("192.168.1.123"), net.ParseIP("100.99.136.28"))
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "route selects source") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestURLIPv4(t *testing.T) {
	t.Parallel()
	t.Run("error hostname", func(t *testing.T) {
		t.Parallel()
		loc, err := url.Parse("http://router.example/root.xml")
		if err != nil {
			t.Fatal(err)
		}
		_, err = urlIPv4(loc)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "literal IPv4 address") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestNewUPnPHTTPClient(t *testing.T) {
	t.Parallel()
	t.Run("valid", func(t *testing.T) {
		t.Parallel()
		var remoteHost string
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			remoteHost, _, _ = net.SplitHostPort(request.RemoteAddr)
			response.WriteHeader(http.StatusNoContent)
		}))
		var listenConfig net.ListenConfig
		listener, err := listenConfig.Listen(t.Context(), "tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		server.Listener = listener
		server.Start()
		t.Cleanup(server.Close)

		client, err := newUPnPHTTPClient(net.ParseIP("127.0.0.2"))
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if remoteHost != "127.0.0.2" {
			t.Fatalf("request source = %s, want 127.0.0.2", remoteHost)
		}
	})
}

func TestUPnPRootDeviceByURL(t *testing.T) {
	t.Parallel()
	t.Run("error redirect", func(t *testing.T) {
		t.Parallel()
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			http.Redirect(response, request, "http://192.0.2.1/root.xml", http.StatusFound)
		}))
		t.Cleanup(server.Close)
		loc, err := url.Parse(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		_, err = upnpRootDeviceByURL(t.Context(), loc, net.ParseIP("127.0.0.2"))
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "redirects are not permitted") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestUpnpDiscoveryError(t *testing.T) {
	t.Parallel()
	t.Run("error", func(t *testing.T) {
		t.Parallel()
		err := upnpDiscoveryError(errors.New("boom"))
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "boom") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}
