package biz

import (
	"context"
	"strings"
	"testing"

	"github.com/ongridio/ongrid/internal/pkg/tunnel"
)

func TestProbeNetworkSNMPValidatesCredentialsBeforeNetworkCall(t *testing.T) {
	tests := []struct {
		name string
		req  tunnel.ProbeNetworkSNMPRequest
		want string
	}{
		{name: "missing address", req: tunnel.ProbeNetworkSNMPRequest{Version: "v2c", Community: "public"}, want: "address"},
		{name: "missing community", req: tunnel.ProbeNetworkSNMPRequest{Address: "192.0.2.1", Version: "v2c"}, want: "community"},
		{name: "invalid version", req: tunnel.ProbeNetworkSNMPRequest{Address: "192.0.2.1", Version: "v1", Community: "public"}, want: "version"},
		{name: "missing v3 username", req: tunnel.ProbeNetworkSNMPRequest{Address: "192.0.2.1", Version: "v3"}, want: "username"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ProbeNetworkSNMP(context.Background(), tt.req)
			if got.OK || !strings.Contains(strings.ToLower(got.Error), tt.want) {
				t.Fatalf("response=%+v, want error containing %q", got, tt.want)
			}
		})
	}
}

func TestProbeNetworkSNMPCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := ProbeNetworkSNMP(ctx, tunnel.ProbeNetworkSNMPRequest{
		Address: "192.0.2.1", Version: "v2c", Community: "public",
	})
	if got.OK || !strings.Contains(got.Error, "canceled") {
		t.Fatalf("response=%+v, want cancelled error", got)
	}
}

func TestSNMPInterfaceHelpers(t *testing.T) {
	index, ok := oidIndex(".1.3.6.1.2.1.2.2.1.2.17", oidIfDescr)
	if !ok || index != 17 {
		t.Fatalf("index=%d ok=%v", index, ok)
	}
	if _, ok := oidIndex(".1.3.6.1.2.1.2.2.1.3.17.1", oidIfType); ok {
		t.Fatal("multi-part suffix must not be treated as an interface index")
	}
	if got := snmpInterfaceKind(6); got != "ethernet" {
		t.Fatalf("kind=%q", got)
	}
	if got := snmpInterfaceStatus(2); got != "down" {
		t.Fatalf("status=%q", got)
	}
}
