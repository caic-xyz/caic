// upnp-dump prints the WAN service and current port mappings reported by a UPnP IGD.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/huin/goupnp"
	internetgateway1 "github.com/huin/goupnp/dcps/internetgateway1"
	internetgateway2 "github.com/huin/goupnp/dcps/internetgateway2"
)

const mappingLimit = 64

type wanConnection interface {
	GetExternalIPAddressCtx(ctx context.Context) (string, error)
	GetStatusInfoCtx(ctx context.Context) (connectionStatus string, lastConnectionError string, uptime uint32, err error)
	GetGenericPortMappingEntryCtx(ctx context.Context, index uint16) (remoteHost string, externalPort uint16, protocol string, internalPort uint16, internalClient string, enabled bool, description string, leaseDuration uint32, err error)
	GetServiceClient() *goupnp.ServiceClient
}

func main() {
	if err := mainImpl(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func mainImpl() error {
	location := flag.String("location", "", "UPnP device-description LOCATION URL")
	timeout := flag.Duration("timeout", 10*time.Second, "overall query timeout")
	flag.Parse()
	if *location == "" {
		flag.Usage()
		return errors.New("-location is required")
	}
	loc, err := url.Parse(*location)
	if err != nil {
		return fmt.Errorf("parse -location: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	connections, err := connectionsByURL(ctx, loc)
	if len(connections) == 0 {
		if err == nil {
			return errors.New("discover WAN services: no WANIPConnection or WANPPPConnection service found")
		}
		return fmt.Errorf("discover WAN services: %w", err)
	}
	for _, connection := range connections {
		dumpConnection(ctx, connection)
	}
	return nil
}

func connectionsByURL(ctx context.Context, loc *url.URL) ([]wanConnection, error) {
	var retErr error
	connections := make([]wanConnection, 0, 5)
	ip2, err := internetgateway2.NewWANIPConnection2ClientsByURLCtx(ctx, loc)
	retErr = errors.Join(retErr, err)
	connections = append(connections, wanIPConnection2Slice(ip2)...)
	ip2v1, err := internetgateway2.NewWANIPConnection1ClientsByURLCtx(ctx, loc)
	retErr = errors.Join(retErr, err)
	connections = append(connections, wanIPConnection2V1Slice(ip2v1)...)
	ip1, err := internetgateway1.NewWANIPConnection1ClientsByURLCtx(ctx, loc)
	retErr = errors.Join(retErr, err)
	connections = append(connections, wanIPConnection1Slice(ip1)...)
	ppp2, err := internetgateway2.NewWANPPPConnection1ClientsByURLCtx(ctx, loc)
	retErr = errors.Join(retErr, err)
	connections = append(connections, wanPPPConnection2Slice(ppp2)...)
	ppp1, err := internetgateway1.NewWANPPPConnection1ClientsByURLCtx(ctx, loc)
	retErr = errors.Join(retErr, err)
	connections = append(connections, wanPPPConnection1Slice(ppp1)...)
	return connections, retErr
}

func wanIPConnection2Slice(clients []*internetgateway2.WANIPConnection2) []wanConnection {
	return wanConnectionSlice(clients)
}

func wanIPConnection2V1Slice(clients []*internetgateway2.WANIPConnection1) []wanConnection {
	return wanConnectionSlice(clients)
}

func wanIPConnection1Slice(clients []*internetgateway1.WANIPConnection1) []wanConnection {
	return wanConnectionSlice(clients)
}

func wanPPPConnection2Slice(clients []*internetgateway2.WANPPPConnection1) []wanConnection {
	return wanConnectionSlice(clients)
}

func wanPPPConnection1Slice(clients []*internetgateway1.WANPPPConnection1) []wanConnection {
	return wanConnectionSlice(clients)
}

func wanConnectionSlice[T wanConnection](clients []T) []wanConnection {
	connections := make([]wanConnection, 0, len(clients))
	for _, client := range clients {
		connections = append(connections, client)
	}
	return connections
}

func dumpConnection(ctx context.Context, connection wanConnection) {
	service := connection.GetServiceClient()
	fmt.Printf("service_id=%s service_type=%s control_url=%s\n", service.Service.ServiceId, service.Service.ServiceType, service.SOAPClient.EndpointURL.String())
	externalIP, err := connection.GetExternalIPAddressCtx(ctx)
	if err != nil {
		fmt.Printf("external_ip_error=%v\n", err)
	} else {
		fmt.Printf("external_ip=%s\n", externalIP)
	}
	status, lastError, uptime, err := connection.GetStatusInfoCtx(ctx)
	if err != nil {
		fmt.Printf("status_error=%v\n", err)
	} else {
		fmt.Printf("connection_status=%s last_error=%s uptime=%d\n", status, lastError, uptime)
	}
	for index := range uint16(mappingLimit) {
		remoteHost, externalPort, protocol, internalPort, internalClient, enabled, description, leaseDuration, err := connection.GetGenericPortMappingEntryCtx(ctx, index)
		if err != nil {
			break
		}
		fmt.Printf("mapping index=%d remote_host=%q external_port=%d protocol=%s internal_client=%s internal_port=%d enabled=%t description=%q lease_seconds=%d\n", index, remoteHost, externalPort, protocol, internalClient, internalPort, enabled, description, leaseDuration)
	}
}
