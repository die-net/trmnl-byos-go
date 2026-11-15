package guard

import (
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestPublic(t *testing.T) {
	t.Parallel()
	for addr, want := range map[string]bool{
		"93.184.215.14":          true,
		"2606:4700:4700::1111":   true,
		"127.0.0.1":              false,
		"10.1.2.3":               false,
		"172.16.0.1":             false,
		"192.168.1.1":            false,
		"169.254.169.254":        false,
		"100.100.100.100":        false,
		"0.0.0.0":                false,
		"255.255.255.255":        false,
		"224.0.0.1":              false,
		"::1":                    false,
		"::":                     false,
		"fe80::1":                false,
		"fd00::1":                false,
		"::ffff:127.0.0.1":       false,
		"::ffff:93.184.215.14":   true,
		"64:ff9b::a00:1":         false,
		"2002:a00:1::1":          false,
		"2001:db8::1":            false,
		"198.18.0.1":             false,
		"240.0.0.1":              false,
		"192.0.0.170":            false,
		"203.0.113.9":            false,
		"2001:4860:4860::8888":   true,
		"::ffff:169.254.169.254": false,
	} {
		if got := Public(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Public(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestHostAddress(t *testing.T) {
	t.Parallel()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) == 0 {
		t.Fatal("this host has no addresses to check")
	}
	for _, ia := range addrs {
		n, ok := ia.(*net.IPNet)
		if !ok {
			continue
		}
		a, _ := netip.AddrFromSlice(n.IP)
		if own, err := hostAddress(a); err != nil || !own {
			t.Errorf("hostAddress(%s) = %v, %v, want this host's own", a, own, err)
		}
	}
	// TEST-NET-1 is never assigned to a host.
	if own, err := hostAddress(netip.MustParseAddr("192.0.2.1")); err != nil || own {
		t.Errorf("hostAddress(192.0.2.1) = %v, %v, want not this host's", own, err)
	}
}

// TestDialerRefusesLoopback checks that the dialer refuses a listener on
// loopback, by address and by name, on any port.
func TestDialerRefusesLoopback(t *testing.T) {
	t.Parallel()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	d := NewDialer(5 * time.Second)
	for _, addr := range []string{ln.Addr().String(), net.JoinHostPort("localhost", port)} {
		conn, err := d.DialContext(t.Context(), "tcp", addr)
		if err == nil {
			_ = conn.Close()
			t.Errorf("dialed %s", addr)
			continue
		}
		if !errors.Is(err, ErrNotPublic) {
			t.Errorf("dial %s: %v, want ErrNotPublic", addr, err)
		}
	}
}
