// IPv4-only transport.Net implementation that avoids pion/stdnet's anet
// package, which uses netlinkrib and fails on hosts without IPv6 support.

package voicertc

import (
	"context"
	"fmt"
	"net"

	"github.com/pion/transport/v4"
)

// ipv4Net implements transport.Net using only IPv4 via Go's stdlib net
// package, bypassing pion/stdnet's netlinkrib dependency.
type ipv4Net struct {
	interfaces []*transport.Interface
}

// defaultIPv4 discovers the host's default IPv4 address by dialing a UDP
// socket. No data is sent; the OS routing table selects the source address.
// This avoids netlink calls that fail on hosts without IPv6.
func defaultIPv4() (net.IP, error) {
	c, err := net.Dial("udp4", "8.8.8.8:80")
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	addr, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil, fmt.Errorf("defaultIPv4: unexpected address type: %T", c.LocalAddr())
	}
	return addr.IP, nil
}

// newIPv4Net returns an ipv4Net with a synthetic interface carrying hostIP.
//
// This avoids netlink enumeration while giving pion a routable address
// for ICE candidate gathering.
func newIPv4Net(hostIP net.IP) *ipv4Net {
	ifc := transport.NewInterface(net.Interface{
		Index: 1,
		Name:  "eth0",
		Flags: net.FlagUp | net.FlagMulticast,
	})
	ifc.AddAddress(&net.IPNet{IP: hostIP, Mask: net.CIDRMask(32, 32)})
	return &ipv4Net{interfaces: []*transport.Interface{ifc}}
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
	return net.ListenPacket(network, address)
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
	return net.Dial(network, address)
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
