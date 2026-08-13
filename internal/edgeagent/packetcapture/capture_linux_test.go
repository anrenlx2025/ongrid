//go:build linux

package packetcapture

import (
	"encoding/binary"
	"net/netip"
	"os"
	"testing"
	"time"
)

func TestNormalizeRequestAppliesBoundedDefaults(t *testing.T) {
	got, _, err := normalizeRequest(Request{CaptureID: "capture-123", Interface: "eth0"})
	if err != nil {
		t.Fatalf("normalizeRequest: %v", err)
	}
	if got.Duration != defaultDuration || got.MaxBytes != defaultMaxBytes || got.MaxPackets != defaultMaxPackets || got.Snaplen != defaultSnaplen {
		t.Fatalf("defaults = %+v", got)
	}
}

func TestNormalizeRequestRejectsUnsafeInput(t *testing.T) {
	tests := []Request{
		{CaptureID: "../escape", Interface: "eth0"},
		{CaptureID: "capture-123", Interface: "../eth0"},
		{CaptureID: "capture-123", Interface: "eth0", Duration: maxDuration + time.Second},
		{CaptureID: "capture-123", Interface: "eth0", Filter: "tcp or udp"},
	}
	for _, in := range tests {
		if _, _, err := normalizeRequest(in); err == nil {
			t.Fatalf("normalizeRequest(%+v) error = nil", in)
		}
	}
}

func TestPacketMatcher(t *testing.T) {
	matcher, err := parseFilter("tcp and host 10.0.0.2 and port 443")
	if err != nil {
		t.Fatalf("parseFilter: %v", err)
	}
	frame := ipv4TCPFrame(t, netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.2"), 51515, 443)
	if !matcher.matches(frame) {
		t.Fatal("matcher did not match expected frame")
	}
	if matcher.matches(ipv4TCPFrame(t, netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.3"), 51515, 443)) {
		t.Fatal("matcher accepted wrong host")
	}
}

func TestWritePcapHeaders(t *testing.T) {
	dir := t.TempDir()
	if _, err := New(dir); err != nil {
		t.Fatalf("New: %v", err)
	}
	// Header helpers are tested independently so CI does not need CAP_NET_RAW.
	f, err := os.CreateTemp(dir, "pcap-*.pcap")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer f.Close()
	if err := writeGlobalHeader(f, 128); err != nil {
		t.Fatalf("writeGlobalHeader: %v", err)
	}
	if err := writePacket(f, time.Unix(7, 123_000).UTC(), []byte{1, 2, 3}, 8); err != nil {
		t.Fatalf("writePacket: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(b) != pcapHeaderLen+16+3 || binary.LittleEndian.Uint32(b[:4]) != 0xa1b2c3d4 {
		t.Fatalf("invalid pcap output: %x", b)
	}
}

func ipv4TCPFrame(t *testing.T, src, dst netip.Addr, srcPort, dstPort uint16) []byte {
	t.Helper()
	frame := make([]byte, 14+20+20)
	binary.BigEndian.PutUint16(frame[12:14], 0x0800)
	frame[14] = 0x45
	frame[23] = 6
	s := src.As4()
	d := dst.As4()
	copy(frame[26:30], s[:])
	copy(frame[30:34], d[:])
	binary.BigEndian.PutUint16(frame[34:36], srcPort)
	binary.BigEndian.PutUint16(frame[36:38], dstPort)
	return frame
}
