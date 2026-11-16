package builtin

import (
	"context"
	"net"
	"strings"
	"testing"
)

func TestIsPrivateIP_BlocksUnspecified(t *testing.T) {
	for _, s := range []string{"::", "0.0.0.0", "::ffff:127.0.0.1", "169.254.169.254"} {
		if !isPrivateIP(net.ParseIP(s)) {
			t.Errorf("isPrivateIP(%s) = false, want true", s)
		}
	}
	if isPrivateIP(net.ParseIP("93.184.216.34")) {
		t.Error("public address reported as private")
	}
}

func TestSSRFSafeTransport_IgnoresEnvironmentProxy(t *testing.T) {
	if newSSRFSafeTransport(nil).Proxy != nil {
		t.Fatal("transport must not inherit a proxy: the dialer would only see the proxy address")
	}
}

func TestSSRFSafeTransport_BlocksUnspecifiedLiteral(t *testing.T) {
	_, err := newSSRFSafeTransport(nil).DialContext(context.Background(), "tcp", "[::]:80")
	if err == nil || !strings.Contains(err.Error(), "SSRF blocked") {
		t.Fatalf("expected SSRF block for [::], got err=%v", err)
	}
}

// The hostname passed to dialValidated does not resolve, so a successful
// connection proves the dialer used the validated IP and never re-resolved.
func TestDialValidated_DialsIPNotHostname(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	addrs := []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}
	conn, err := dialValidated(context.Background(), &net.Dialer{}, "tcp", "rebind.invalid", port, addrs)
	if err != nil {
		t.Fatalf("dialValidated: %v", err)
	}
	_ = conn.Close()

	if _, err := dialValidated(context.Background(), &net.Dialer{}, "tcp", "rebind.invalid", port, nil); err == nil {
		t.Fatal("expected error for empty address list")
	}
}
