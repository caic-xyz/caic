// IPv4-only transport.Net implementation and offline-safe host address discovery.

package voicertc

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/pion/transport/v4"
)

// ipv4Net implements transport.Net using only IPv4 via Go's stdlib net
// package, bypassing pion/stdnet's netlinkrib dependency.
type ipv4Net struct {
	ctx        context.Context
	interfaces []*transport.Interface
}

// defaultIPv4 discovers the host's default IPv4 address without requiring
// Internet reachability. It first asks the routing table for the preferred
// source address. If the host is offline, it falls back to local interface
// addresses and finally loopback so the bridge can still start for local use.
func defaultIPv4(ctx context.Context) (net.IP, error) {
	ip, err := defaultRouteIPv4(ctx)
	if err == nil {
		return ip, nil
	}
	fallback, fallbackErr := interfaceIPv4()
	if fallbackErr != nil {
		return nil, errors.Join(err, fallbackErr)
	}
	return fallback, nil
}

// defaultRouteIPv4 discovers the preferred routed IPv4 source address by
// dialing a UDP socket. No data is sent; the OS routing table selects the
// source address.
func defaultRouteIPv4(ctx context.Context) (net.IP, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "udp4", "8.8.8.8:80")
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	addr, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil, fmt.Errorf("defaultRouteIPv4: unexpected address type: %T", c.LocalAddr())
	}
	return addr.IP, nil
}

func interfaceIPv4() (net.IP, error) {
	ifcs, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list interfaces: %w", err)
	}
	candidates := make([]net.IP, 0)
	for _, ifc := range ifcs {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			return nil, fmt.Errorf("list addresses for %s: %w", ifc.Name, err)
		}
		for _, addr := range addrs {
			ip := addrIPv4(addr)
			if ip == nil {
				continue
			}
			candidates = append(candidates, ip)
		}
	}
	if ip, ok := bestIPv4Candidate(candidates); ok {
		return ip, nil
	}
	return net.IPv4(127, 0, 0, 1), nil
}

func addrIPv4(addr net.Addr) net.IP {
	var ip net.IP
	switch v := addr.(type) {
	case *net.IPNet:
		ip = v.IP
	case *net.IPAddr:
		ip = v.IP
	default:
		return nil
	}
	v4 := ip.To4()
	if v4 == nil {
		return nil
	}
	return append(net.IP(nil), v4...)
}

func bestIPv4Candidate(candidates []net.IP) (net.IP, bool) {
	for _, ip := range candidates {
		if ip.IsGlobalUnicast() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
			return ip, true
		}
	}
	for _, ip := range candidates {
		if !ip.IsLoopback() && ip.IsLinkLocalUnicast() {
			return ip, true
		}
	}
	for _, ip := range candidates {
		if ip.IsLoopback() {
			return ip, true
		}
	}
	return nil, false
}

// newIPv4Net returns an ipv4Net with a synthetic interface carrying hostIP.
//
// This avoids netlink enumeration while giving pion a routable address
// for ICE candidate gathering.
func newIPv4Net(ctx context.Context, hostIP net.IP) *ipv4Net {
	ifc := transport.NewInterface(net.Interface{
		Index: 1,
		Name:  "eth0",
		Flags: net.FlagUp | net.FlagMulticast,
	})
	ifc.AddAddress(&net.IPNet{IP: hostIP, Mask: net.CIDRMask(32, 32)})
	return &ipv4Net{ctx: ctx, interfaces: []*transport.Interface{ifc}}
}

func (n *ipv4Net) Interfaces() ([]*transport.Interface, error) {
	return n.interfaces, nil
}

func (n *ipv4Net) InterfaceByIndex(index int) (*transport.Interface, error) {
	for _, ifc := range n.interfaces {
		if ifc.Index == index {
			return ifc, nil
		}
	}
	return nil, fmt.Errorf("%w: index=%d", transport.ErrInterfaceNotFound, index)
}

func (n *ipv4Net) InterfaceByName(name string) (*transport.Interface, error) {
	for _, ifc := range n.interfaces {
		if ifc.Name == name {
			return ifc, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", transport.ErrInterfaceNotFound, name)
}

func (n *ipv4Net) ListenPacket(network, address string) (net.PacketConn, error) {
	var lc net.ListenConfig
	return lc.ListenPacket(n.ctx, network, address)
}

func (n *ipv4Net) ListenUDP(network string, locAddr *net.UDPAddr) (transport.UDPConn, error) {
	return net.ListenUDP(network, locAddr)
}

func (n *ipv4Net) ListenTCP(network string, laddr *net.TCPAddr) (transport.TCPListener, error) {
	l, err := net.ListenTCP(network, laddr)
	if err != nil {
		return nil, err
	}
	return &tcpListenerWrapper{l}, nil
}

func (n *ipv4Net) Dial(network, address string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(n.ctx, network, address)
}

func (n *ipv4Net) DialUDP(network string, laddr, raddr *net.UDPAddr) (transport.UDPConn, error) {
	return net.DialUDP(network, laddr, raddr)
}

func (n *ipv4Net) DialTCP(network string, laddr, raddr *net.TCPAddr) (transport.TCPConn, error) {
	c, err := net.DialTCP(network, laddr, raddr)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (n *ipv4Net) ResolveIPAddr(network, address string) (*net.IPAddr, error) {
	return net.ResolveIPAddr(network, address)
}

func (n *ipv4Net) ResolveUDPAddr(network, address string) (*net.UDPAddr, error) {
	return net.ResolveUDPAddr(network, address)
}

func (n *ipv4Net) ResolveTCPAddr(network, address string) (*net.TCPAddr, error) {
	return net.ResolveTCPAddr(network, address)
}

func (n *ipv4Net) CreateDialer(d *net.Dialer) transport.Dialer {
	return d
}

func (n *ipv4Net) CreateListenConfig(lc *net.ListenConfig) transport.ListenConfig {
	return &listenConfigWrapper{lc: lc}
}

// tcpListenerWrapper adapts *net.TCPListener to transport.TCPListener by
// wrapping AcceptTCP to return transport.TCPConn instead of *net.TCPConn.
type tcpListenerWrapper struct {
	*net.TCPListener
}

func (w *tcpListenerWrapper) AcceptTCP() (transport.TCPConn, error) {
	c, err := w.TCPListener.AcceptTCP()
	if err != nil {
		return nil, err
	}
	return c, nil
}

// listenConfigWrapper adapts *net.ListenConfig to transport.ListenConfig.
type listenConfigWrapper struct {
	lc *net.ListenConfig
}

func (w *listenConfigWrapper) Listen(ctx context.Context, network, address string) (net.Listener, error) {
	return w.lc.Listen(ctx, network, address)
}

func (w *listenConfigWrapper) ListenPacket(ctx context.Context, network, address string) (net.PacketConn, error) {
	return w.lc.ListenPacket(ctx, network, address)
}
