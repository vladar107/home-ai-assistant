package simkit

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
)

// Router gives loopback fakes public-looking host names for a client that refuses private
// addresses: it resolves each registered name to its own public address and dials that address
// at the fake's loopback listener. A name that was not registered does not resolve, so nothing
// else can be reached. Its LookupNetIP and DialContext fit a connector's resolver and dial
// settings without widening the connector's address guard.
type Router struct {
	mu      sync.Mutex
	hosts   map[string]netip.Addr
	targets map[netip.Addr]string
}

func NewRouter() *Router {
	return &Router{hosts: map[string]netip.Addr{}, targets: map[netip.Addr]string{}}
}

// Add routes host, on whatever standard port a client uses, to the loopback server at target, a
// host:port address. Adding a host again points it at the new target.
func (r *Router) Add(host, target string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	address, known := r.hosts[host]
	if !known {
		// 93.184.216.0/24 is ordinary public address space.
		address = netip.AddrFrom4([4]byte{93, 184, 216, byte(len(r.hosts) + 1)})
		r.hosts[host] = address
	}
	r.targets[address] = target
}

func (r *Router) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	address, known := r.hosts[host]
	if !known {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return []netip.Addr{address}, nil
}

func (r *Router) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	parsed, err := netip.ParseAddr(host)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	target, known := r.targets[parsed]
	r.mu.Unlock()
	if !known {
		return nil, fmt.Errorf("no fake listens at %s", address)
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, network, target)
}
