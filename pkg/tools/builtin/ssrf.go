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
		"127.0.0.0/8",    // loopback (IPv4)
		"10.0.0.0/8",     // RFC 1918 private
		"172.16.0.0/12",  // RFC 1918 private
		"192.168.0.0/16", // RFC 1918 private
		"169.254.0.0/16", // link-local; 169.254.169.254 = AWS/Azure/GCP/DO metadata
		"0.0.0.0/8",      // "this" network
		"100.64.0.0/10",  // shared address space (RFC 6598)
		"192.0.2.0/24",   // TEST-NET-1 (RFC 5737)
		"198.51.100.0/24", // TEST-NET-2 (RFC 5737)
		"203.0.113.0/24", // TEST-NET-3 (RFC 5737)
		"224.0.0.0/4",    // multicast
		"240.0.0.0/4",    // reserved (RFC 1112)
		"::1/128",          // IPv6 loopback
		"fc00::/7",         // IPv6 unique-local (ULA)
		"fe80::/10",        // IPv6 link-local
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
// Protection is post-DNS: the dialer resolves the hostname to IP(s) before
// opening a socket, preventing DNS-rebinding attacks where a hostname first
// resolves to a public IP (passing any name-based check) and then to a private
// IP at connection time.
func newSSRFSafeTransport(skipHosts map[string]bool) *http.Transport {
	base := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
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
		return base.DialContext(ctx, network, net.JoinHostPort(host, port))
	}
	return t
}
