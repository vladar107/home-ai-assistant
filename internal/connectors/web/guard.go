package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/httpretry"
)

const (
	userAgent       = "HomeAgent/1.0 (+private household assistant)"
	requestTimeout  = 15 * time.Second
	maximumRedirect = 5
)

var (
	errPrivateAddress   = errors.New("the address is on a private network")
	errUnsupportedURL   = errors.New("the address is not a plain http or https address on a standard port")
	errTooManyRedirects = errors.New("the address redirected too many times")
	errUnresolvable     = errors.New("the address could not be resolved")
	errTooLarge         = errors.New("the response exceeds the download limit")
)

// Resolver looks up the addresses of a host name; *net.Resolver satisfies it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// DialFunc opens a connection to an address the guard has already approved.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// blockedPrefixes are the ranges netip's predicates do not already cover that still lead into a
// local network, to an unroutable destination, or through a translation that can reach either.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("fec0::/10"),
}

// embeddingPrefixes carry an IPv4 address inside an IPv6 one, which a gateway on the path may
// translate back; the embedded address must be public too.
var embeddingPrefixes = []struct {
	prefix netip.Prefix
	offset int
}{
	{netip.MustParsePrefix("64:ff9b::/96"), 12},
	{netip.MustParsePrefix("64:ff9b:1::/48"), 12},
	{netip.MustParsePrefix("2002::/16"), 2},
}

// publicAddress reports whether an address is somewhere on the public internet rather than in
// the home, on the host itself, or in a range that is not routable at all.
func publicAddress(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.Zone() != "" ||
		addr.IsLoopback() || addr.IsPrivate() || addr.IsUnspecified() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() ||
		addr.IsMulticast() {
		return false
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	if addr.Is6() {
		raw := addr.As16()
		for _, embedding := range embeddingPrefixes {
			if embedding.prefix.Contains(addr) {
				embedded := netip.AddrFrom4([4]byte(raw[embedding.offset : embedding.offset+4]))
				return publicAddress(embedded)
			}
		}
	}
	return true
}

// guard is the only way this package reaches the network. It refuses anything but http and https
// on the standard ports, and resolves every host itself so that each connection, including every
// redirect hop, goes only to a public address.
type guard struct {
	client   *http.Client
	resolver Resolver
	dial     DialFunc
}

func newGuard(resolver Resolver, dial DialFunc) *guard {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	if dial == nil {
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		dial = dialer.DialContext
	}
	g := &guard{resolver: resolver, dial: dial}
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           g.dialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: requestTimeout,
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
	}
	g.client = &http.Client{
		Transport: httpretry.New(transport, httpretry.Options{Service: "web"}),
		Timeout:   requestTimeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) > maximumRedirect {
				return errTooManyRedirects
			}
			return checkURL(request.URL)
		},
	}
	return g
}

// checkURL refuses what the dialer cannot see: the scheme, credentials in the address, and a
// port the host name alone would not imply.
func checkURL(target *url.URL) error {
	if target == nil || (target.Scheme != "http" && target.Scheme != "https") || target.User != nil || target.Hostname() == "" {
		return errUnsupportedURL
	}
	switch target.Port() {
	case "", "80", "443":
		return nil
	}
	return errUnsupportedURL
}

func (g *guard) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || (port != "80" && port != "443") {
		return nil, errUnsupportedURL
	}
	addresses, err := g.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	var last error
	for _, addr := range addresses {
		conn, err := g.dial(ctx, network, net.JoinHostPort(addr.String(), port))
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}

// resolve returns the host's addresses only when every one of them is public, so a name that
// mixes public and private answers cannot be steered to the private one.
func (g *guard) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if literal, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		if !publicAddress(literal) {
			return nil, errPrivateAddress
		}
		return []netip.Addr{literal}, nil
	}
	addresses, err := g.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addresses) == 0 {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errUnresolvable
	}
	for _, addr := range addresses {
		if !publicAddress(addr) {
			return nil, errPrivateAddress
		}
	}
	return addresses, nil
}

// response is a fetched body with the address it finally came from.
type response struct {
	finalURL    *url.URL
	status      int
	contentType string
	body        io.ReadCloser
}

// get sends a GET through the guard; the caller closes the body.
func (g *guard) get(ctx context.Context, target *url.URL, accept string) (response, error) {
	if err := checkURL(target); err != nil {
		return response{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return response{}, errUnsupportedURL
	}
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Accept", accept)
	resp, err := g.client.Do(request)
	if err != nil {
		return response{}, err
	}
	return response{finalURL: resp.Request.URL, status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: resp.Body}, nil
}

// readLimited reads a whole body, failing with errTooLarge past limit bytes.
func readLimited(body io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errTooLarge
	}
	return data, nil
}

// failureText turns a guard or transport error into something the model can act on, without the
// raw error, which can carry resolver and socket details.
func failureText(err error) string {
	switch {
	case errors.Is(err, errPrivateAddress):
		return "That address is on a private network, so it cannot be fetched."
	case errors.Is(err, errUnsupportedURL):
		return "Only http and https addresses on the standard ports, without a user name or password, can be fetched."
	case errors.Is(err, errTooManyRedirects):
		return fmt.Sprintf("That address redirected more than %d times, so it was not followed.", maximumRedirect)
	case errors.Is(err, errUnresolvable):
		return "That address's host name could not be found."
	case errors.Is(err, context.DeadlineExceeded), isTimeout(err):
		return "The site did not respond in time."
	default:
		return "The site could not be reached."
	}
}

func isTimeout(err error) bool {
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}
