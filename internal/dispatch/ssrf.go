package dispatch

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"time"
)

// BlockedAddressError reports a target rejected by the SSRF guard.
type BlockedAddressError struct {
	Host string
	IP   string
}

func (e *BlockedAddressError) Error() string {
	return fmt.Sprintf(
		"refusing to connect to %s (%s): it resolves to a private or loopback address; set ALLOW_PRIVATE_TARGETS=true if this is intentional",
		e.Host, e.IP)
}

// SSRFGuard rejects outbound requests to addresses that a webhook target has
// no business being on — loopback, link-local, and RFC1918 ranges — so a
// service URL cannot be used to reach the gateway's own network.
//
// The check happens in the dialer rather than only on the URL, which closes
// the DNS-rebinding gap: a hostname that passes a pre-flight lookup can still
// resolve to a private address by the time the connection is made.
type SSRFGuard struct {
	AllowPrivate bool
	dialer       *net.Dialer
}

func NewSSRFGuard(allowPrivate bool) *SSRFGuard {
	return &SSRFGuard{
		AllowPrivate: allowPrivate,
		dialer: &net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		},
	}
}

// DialContext dials only after checking the resolved address.
func (g *SSRFGuard) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if g.AllowPrivate {
		return g.dialer.DialContext(ctx, network, addr)
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}

	// Dial the first permitted address, so a host with a mix of public and
	// private records still works over its public one.
	var lastErr error = &BlockedAddressError{Host: host, IP: "no addresses"}
	for _, ip := range ips {
		if blocked(ip.IP) {
			lastErr = &BlockedAddressError{Host: host, IP: ip.IP.String()}
			continue
		}
		conn, err := g.dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// CheckURL validates a target before it is stored, so an operator learns about
// a bad URL at configuration time rather than on the first delivery.
func (g *SSRFGuard) CheckURL(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("URL scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("URL must include a host")
	}
	if g.AllowPrivate {
		return nil
	}

	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if blocked(ip) {
			return &BlockedAddressError{Host: host, IP: ip.String()}
		}
		return nil
	}

	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
	if err != nil {
		return fmt.Errorf("DNS lookup for %q failed: %w", host, err)
	}
	// Every resolved address must be acceptable at configuration time; the
	// dialer re-checks at connection time anyway.
	for _, ip := range ips {
		if blocked(ip.IP) {
			return &BlockedAddressError{Host: host, IP: ip.IP.String()}
		}
	}
	return nil
}

// blocked reports whether an IP is in a range a webhook target must not use.
func blocked(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast() ||
		isCGNAT(ip)
}

// isCGNAT covers 100.64.0.0/10, which is not private per RFC1918 but is
// carrier-internal and used by several container runtimes.
func isCGNAT(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
}
