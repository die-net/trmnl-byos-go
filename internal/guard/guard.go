// Package guard dials only public addresses, for connections a recipe asks
// for. The check runs in the dialer's Control hook, on the address each
// connection is about to use, so neither a redirect nor a DNS answer that
// changes between lookups can get around it.
//
// The address check is copied from legwork's internal/guard
// (github.com/die-net/legwork, 89765ca), without its https-only port rule,
// since recipes poll plain HTTP and other ports. The dialer is conduit's
// direct dialer (github.com/die-net/conduit internal/dialer, 9a21684).
package guard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// notPublic are ranges that IsGlobalUnicast and IsPrivate leave in but no
// connection for a recipe should reach.
var notPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // Carrier-grade NAT, and Tailscale.
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64, which can map to a private IPv4 address.
	netip.MustParsePrefix("64:ff9b:1::/48"), // Local-use NAT64.
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"), // 6to4, which embeds an IPv4 address.
}

// Public reports whether a is an address a recipe's connection may use.
func Public(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range notPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// ErrNotPublic is returned for a connection to an address that is not public.
var ErrNotPublic = errors.New("not a public address")

func control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("guard: %w", err)
	}
	if !Public(ap.Addr()) {
		return fmt.Errorf("guard: %s: %w", ap.Addr(), ErrNotPublic)
	}
	own, err := hostAddress(ap.Addr())
	if err != nil {
		return fmt.Errorf("guard: %w", err)
	}
	if own {
		return fmt.Errorf("guard: %s is this host's own: %w", ap.Addr(), ErrNotPublic)
	}
	return nil
}

// hostAddress reports whether a is one of this host's own addresses. A
// public address the host holds reaches its own services, such as a reverse
// proxy listening on every address, which a recipe must not. The interfaces
// are read on each connection, since addresses change.
func hostAddress(a netip.Addr) (bool, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false, fmt.Errorf("list this host's addresses: %w", err)
	}
	a = a.Unmap()
	for _, ia := range addrs {
		if n, ok := ia.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap() == a {
				return true, nil
			}
		}
	}
	return false, nil
}

// Dialer dials public addresses only.
type Dialer struct {
	dialer         net.Dialer
	defaultNetwork string
	keepAlive      net.KeepAliveConfig
}

// NewDialer returns a Dialer that gives up on a connection after timeout.
func NewDialer(timeout time.Duration) *Dialer {
	return &Dialer{
		dialer:         net.Dialer{Timeout: timeout, Control: control},
		defaultNetwork: defaultNetwork(),
		keepAlive:      net.KeepAliveConfig{Enable: true},
	}
}

// DialContext looks up address and connects to it if it is public. For TCP
// it also:
//   - normalizes "tcp" to "tcp4" or "tcp6" when the host has only one
//     address family, which saves looking up the other;
//   - looks names up fully qualified, with a trailing ".", so the DNS
//     search path is not tried;
//   - turns on TCP keepalive.
func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network == "tcp" {
		network = d.defaultNetwork
	}
	if host, port, err := net.SplitHostPort(address); err == nil && host != "" && port != "" {
		if net.ParseIP(host) == nil && host[len(host)-1] != '.' {
			address = net.JoinHostPort(host+".", port)
		}
	}
	conn, err := d.dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetKeepAliveConfig(d.keepAlive)
	}
	return conn, nil
}

// defaultNetwork returns "tcp4" or "tcp6" when the host has addresses in
// only that family, and "tcp" otherwise.
func defaultNetwork() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "tcp"
	}
	var ipv4, ipv6 bool
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err != nil {
			continue
		}
		if ip.To4() != nil {
			// Link-local is sometimes used for IPv4 NAT, so it counts.
			ipv4 = ipv4 || ip.IsGlobalUnicast() || ip.IsLinkLocalUnicast()
		} else {
			ipv6 = ipv6 || ip.IsGlobalUnicast()
		}
	}
	switch {
	case ipv4 && !ipv6:
		return "tcp4"
	case !ipv4 && ipv6:
		return "tcp6"
	default:
		return "tcp"
	}
}
