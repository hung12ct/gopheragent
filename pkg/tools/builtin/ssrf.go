package builtin

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// privateRanges is the set of IP blocks that an agent-controlled HTTP client
// must never reach: loopback, RFC-1918 private networks, link-local
// (169.254.0.0/16 includes every cloud provider's instance-metadata endpoint),
// and various reserved ranges.
var privateRanges []*net.IPNet

func init() {
	blocks := []string{
		"127.0.0.0/8",     // loopback (IPv4)
		"10.0.0.0/8",      // RFC 1918 private
		"172.16.0.0/12",   // RFC 1918 private
		"192.168.0.0/16",  // RFC 1918 private
		"169.254.0.0/16",  // link-local; 169.254.169.254 = AWS/Azure/GCP/DO metadata
		"0.0.0.0/8",       // "this" network
		"100.64.0.0/10",   // shared address space (RFC 6598)
		"192.0.2.0/24",    // TEST-NET-1 (RFC 5737)
		"198.51.100.0/24", // TEST-NET-2 (RFC 5737)
		"203.0.113.0/24",  // TEST-NET-3 (RFC 5737)
		"224.0.0.0/4",     // multicast
		"240.0.0.0/4",     // reserved (RFC 1112)
		"::1/128",         // IPv6 loopback
		"fc00::/7",        // IPv6 unique-local (ULA)
		"fe80::/10",       // IPv6 link-local
	}
	for _, cidr := range blocks {
		_, network, err := net.ParseCIDR(cidr)
		if err == nil {
			privateRanges = append(privateRanges, network)
		}
	}
}

// isPrivateIP reports whether ip falls within any private or reserved range.
func isPrivateIP(ip net.IP) bool {
	// The unspecified address (0.0.0.0, ::) dials the local machine.
	if ip.IsUnspecified() {
		return true
	}
	for _, r := range privateRanges {
		if r.Contains(ip) {
			return true
		}
	}
	return false
}

// newSSRFSafeTransport returns an *http.Transport whose DialContext rejects
// connections to private, loopback, link-local, or reserved IP addresses.
//
// Hosts in skipHosts (lowercase, exact-match) bypass the check — use this for
// explicitly trusted endpoints such as local test servers or known internal
// services. The allowedHosts list in HTTPRequestTool feeds directly into
// skipHosts so that allowlisted destinations are not double-blocked.
//
// Protection is post-DNS: the dialer resolves the hostname once, validates
// every returned address, and connects to those validated IPs directly. The
// name is never resolved a second time, so a DNS-rebinding host cannot answer
// public for the check and private for the connection.
//
// The environment proxy is disabled: through a proxy the dialer only ever sees
// the proxy's address, so the real target would go unchecked.
func newSSRFSafeTransport(skipHosts map[string]bool) *http.Transport {
	base := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}

		// Explicitly trusted hosts skip the private-IP check.
		if skipHosts[strings.ToLower(host)] {
			return base.DialContext(ctx, network, addr)
		}

		// Handle literal IP addresses first (no DNS needed).
		if ip := net.ParseIP(host); ip != nil {
			if isPrivateIP(ip) {
				return nil, fmt.Errorf("tools: SSRF blocked: %s is a private/reserved address", host)
			}
			return base.DialContext(ctx, network, addr)
		}

		// Resolve via DNS, then check every returned address.
		resolved, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("tools: dns lookup %s: %w", host, err)
		}
		for _, a := range resolved {
			if isPrivateIP(a.IP) {
				return nil, fmt.Errorf("tools: SSRF blocked: %s resolves to private/reserved address %s", host, a.IP)
			}
		}
		return dialValidated(ctx, base, network, host, port, resolved)
	}
	return t
}

// dialValidated connects to the first reachable address in addrs, which the
// caller has already checked. Dialing the IP rather than host is what keeps
// the connection pinned to the addresses that passed validation.
func dialValidated(ctx context.Context, d *net.Dialer, network, host, port string, addrs []net.IPAddr) (net.Conn, error) {
	err := fmt.Errorf("tools: dns lookup %s: no addresses", host)
	for _, a := range addrs {
		var conn net.Conn
		if conn, err = d.DialContext(ctx, network, net.JoinHostPort(a.IP.String(), port)); err == nil {
			return conn, nil
		}
	}
	return nil, err
}
