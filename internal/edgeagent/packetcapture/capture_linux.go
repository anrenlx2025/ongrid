//go:build linux

// Package packetcapture provides the edge-local packet capture primitive.
// It deliberately uses AF_PACKET directly so the edge package does not rely
// on tcpdump, libpcap, or shell command construction.
package packetcapture

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	defaultDuration   = 30 * time.Second
	defaultMaxBytes   = 64 << 20
	defaultMaxPackets = 100_000
	defaultSnaplen    = 1514

	maxDuration   = 5 * time.Minute
	maxBytes      = 256 << 20
	maxPackets    = 500_000
	maxSnaplen    = 65_535
	ethernetMTU   = 65_535
	pcapHeaderLen = 24
)

// Request describes a bounded edge-local capture. OutputPath is not accepted
// from callers: Capturer derives it from BaseDir and CaptureID.
type Request struct {
	CaptureID   string        `json:"capture_id"`
	Interface   string        `json:"interface"`
	Filter      string        `json:"filter,omitempty"`
	Duration    time.Duration `json:"-"`
	MaxBytes    int64         `json:"max_bytes"`
	MaxPackets  int           `json:"max_packets"`
	Snaplen     int           `json:"snaplen"`
	Promiscuous bool          `json:"promiscuous"`
}

// Result contains capture metadata suitable for the manager to persist and
// later turn into a private artifact.
type Result struct {
	Path          string    `json:"-"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at"`
	Packets       int       `json:"packets"`
	PayloadBytes  int64     `json:"payload_bytes"`
	FileBytes     int64     `json:"file_bytes"`
	StopReason    string    `json:"stop_reason"`
	InterfaceName string    `json:"interface"`
}

// Capturer owns only a local output directory. It has no mutable capture
// state, therefore concurrent edge RPCs cannot share or overwrite files.
type Capturer struct {
	baseDir string
}

// New validates the edge-owned pcap directory. Callers must not use a
// user-provided path here; the packet capture service config owns BaseDir.
func New(baseDir string) (*Capturer, error) {
	if strings.TrimSpace(baseDir) == "" {
		return nil, errors.New("packet capture: base directory required")
	}
	clean := filepath.Clean(baseDir)
	if clean == "." || clean == string(filepath.Separator) {
		return nil, fmt.Errorf("packet capture: unsafe base directory %q", baseDir)
	}
	return &Capturer{baseDir: clean}, nil
}

// Capture records packets until the validated duration or a resource limit is
// reached. It requires CAP_NET_RAW; CAP_NET_ADMIN is needed only when
// Promiscuous is requested.
func (c *Capturer) Capture(ctx context.Context, in Request) (Result, error) {
	if c == nil {
		return Result{}, errors.New("packet capture: nil capturer")
	}
	req, matcher, err := normalizeRequest(in)
	if err != nil {
		return Result{}, err
	}
	iface, err := net.InterfaceByName(req.Interface)
	if err != nil {
		return Result{}, fmt.Errorf("packet capture: find interface %q: %w", req.Interface, err)
	}
	if iface.Index <= 0 {
		return Result{}, fmt.Errorf("packet capture: invalid interface %q", req.Interface)
	}
	if err := os.MkdirAll(c.baseDir, 0o700); err != nil {
		return Result{}, fmt.Errorf("packet capture: create output directory: %w", err)
	}
	path := filepath.Join(c.baseDir, req.CaptureID+".pcap")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Result{}, fmt.Errorf("packet capture: create pcap: %w", err)
	}

	if err := writeGlobalHeader(f, req.Snaplen); err != nil {
		return cleanupFailedCapture(f, path, fmt.Errorf("packet capture: write global header: %w", err))
	}
	fd, err := openSocket(iface.Index, req.Promiscuous)
	if err != nil {
		return cleanupFailedCapture(f, path, err)
	}
	defer closeSocket(fd)

	startedAt := time.Now().UTC()
	deadline := startedAt.Add(req.Duration)
	result := Result{Path: path, StartedAt: startedAt, InterfaceName: iface.Name}
	buf := make([]byte, ethernetMTU)

	for {
		if err := ctx.Err(); err != nil {
			result.StopReason = "cancelled"
			break
		}
		if !time.Now().Before(deadline) {
			result.StopReason = "duration_limit"
			break
		}
		if result.Packets >= req.MaxPackets {
			result.StopReason = "packet_limit"
			break
		}
		if result.PayloadBytes >= req.MaxBytes {
			result.StopReason = "byte_limit"
			break
		}

		ready, err := waitReadable(fd, 100*time.Millisecond)
		if err != nil {
			return cleanupFailedCapture(f, path, err)
		}
		if !ready {
			continue
		}
		n, _, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
				continue
			}
			return cleanupFailedCapture(f, path, fmt.Errorf("packet capture: receive packet: %w", err))
		}
		if n == 0 || !matcher.matches(buf[:n]) {
			continue
		}
		capturedLen := min(n, req.Snaplen)
		if result.PayloadBytes+int64(capturedLen) > req.MaxBytes {
			result.StopReason = "byte_limit"
			break
		}
		if err := writePacket(f, time.Now().UTC(), buf[:capturedLen], n); err != nil {
			return cleanupFailedCapture(f, path, fmt.Errorf("packet capture: write packet: %w", err))
		}
		result.Packets++
		result.PayloadBytes += int64(capturedLen)
	}
	if result.StopReason == "" {
		result.StopReason = "completed"
	}
	if err := f.Close(); err != nil {
		return Result{}, fmt.Errorf("packet capture: close pcap: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return Result{}, fmt.Errorf("packet capture: stat pcap: %w", err)
	}
	result.FinishedAt = time.Now().UTC()
	result.FileBytes = info.Size()
	return result, nil
}

func cleanupFailedCapture(f *os.File, path string, cause error) (Result, error) {
	if closeErr := f.Close(); closeErr != nil {
		cause = fmt.Errorf("%w; close failed: %v", cause, closeErr)
	}
	if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return Result{}, fmt.Errorf("%w; remove partial capture: %v", cause, removeErr)
	}
	return Result{}, cause
}

func closeSocket(fd int) {
	_ = unix.Close(fd) // best-effort cleanup; capture already has its terminal result.
}

func normalizeRequest(in Request) (Request, packetMatcher, error) {
	in.CaptureID = strings.TrimSpace(in.CaptureID)
	if !validCaptureID(in.CaptureID) {
		return Request{}, packetMatcher{}, errors.New("packet capture: capture_id must be a UUID or lowercase identifier")
	}
	in.Interface = strings.TrimSpace(in.Interface)
	if in.Interface == "" || len(in.Interface) > 15 || strings.ContainsAny(in.Interface, "/\\\x00") {
		return Request{}, packetMatcher{}, errors.New("packet capture: valid interface required")
	}
	if in.Duration <= 0 {
		in.Duration = defaultDuration
	}
	if in.Duration > maxDuration {
		return Request{}, packetMatcher{}, fmt.Errorf("packet capture: duration exceeds %s", maxDuration)
	}
	if in.MaxBytes <= 0 {
		in.MaxBytes = defaultMaxBytes
	}
	if in.MaxBytes > maxBytes {
		return Request{}, packetMatcher{}, fmt.Errorf("packet capture: max_bytes exceeds %d", maxBytes)
	}
	if in.MaxPackets <= 0 {
		in.MaxPackets = defaultMaxPackets
	}
	if in.MaxPackets > maxPackets {
		return Request{}, packetMatcher{}, fmt.Errorf("packet capture: max_packets exceeds %d", maxPackets)
	}
	if in.Snaplen <= 0 {
		in.Snaplen = defaultSnaplen
	}
	if in.Snaplen > maxSnaplen {
		return Request{}, packetMatcher{}, fmt.Errorf("packet capture: snaplen exceeds %d", maxSnaplen)
	}
	matcher, err := parseFilter(in.Filter)
	if err != nil {
		return Request{}, packetMatcher{}, err
	}
	return in, matcher, nil
}

func validCaptureID(v string) bool {
	if len(v) < 8 || len(v) > 64 {
		return false
	}
	for _, r := range v {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func openSocket(ifindex int, promiscuous bool) (int, error) {
	protocol := htons(unix.ETH_P_ALL)
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(protocol))
	if err != nil {
		return -1, fmt.Errorf("packet capture: open AF_PACKET socket (CAP_NET_RAW required): %w", err)
	}
	fail := func(err error) (int, error) {
		if closeErr := unix.Close(fd); closeErr != nil {
			err = fmt.Errorf("%w; close socket: %v", err, closeErr)
		}
		return -1, err
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: protocol, Ifindex: ifindex}); err != nil {
		return fail(fmt.Errorf("packet capture: bind AF_PACKET socket: %w", err))
	}
	if promiscuous {
		mreq := &unix.PacketMreq{Ifindex: int32(ifindex), Type: unix.PACKET_MR_PROMISC}
		if err := unix.SetsockoptPacketMreq(fd, unix.SOL_PACKET, unix.PACKET_ADD_MEMBERSHIP, mreq); err != nil {
			return fail(fmt.Errorf("packet capture: enable promiscuous mode (CAP_NET_ADMIN required): %w", err))
		}
	}
	return fd, nil
}

func waitReadable(fd int, timeout time.Duration) (bool, error) {
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	ready, err := unix.Poll(poll, int(timeout.Milliseconds()))
	if err != nil {
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
			return false, nil
		}
		return false, fmt.Errorf("packet capture: wait for packet: %w", err)
	}
	return ready > 0, nil
}

func writeGlobalHeader(f *os.File, snaplen int) error {
	var header [pcapHeaderLen]byte
	binary.LittleEndian.PutUint32(header[0:4], 0xa1b2c3d4)
	binary.LittleEndian.PutUint16(header[4:6], 2)
	binary.LittleEndian.PutUint16(header[6:8], 4)
	binary.LittleEndian.PutUint32(header[16:20], uint32(snaplen))
	binary.LittleEndian.PutUint32(header[20:24], 1) // LINKTYPE_ETHERNET
	_, err := f.Write(header[:])
	return err
}

func writePacket(f *os.File, at time.Time, packet []byte, originalLen int) error {
	var header [16]byte
	binary.LittleEndian.PutUint32(header[0:4], uint32(at.Unix()))
	binary.LittleEndian.PutUint32(header[4:8], uint32(at.Nanosecond()/1_000))
	binary.LittleEndian.PutUint32(header[8:12], uint32(len(packet)))
	binary.LittleEndian.PutUint32(header[12:16], uint32(originalLen))
	if _, err := f.Write(header[:]); err != nil {
		return err
	}
	_, err := f.Write(packet)
	return err
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// packetMatcher implements the deliberately small filter subset accepted by
// v1: protocol, one host, and one port joined with "and". Unsupported BPF is
// rejected instead of becoming an accidental unfiltered capture.
type packetMatcher struct {
	protocol uint8
	host     netip.Addr
	port     uint16
}

func parseFilter(raw string) (packetMatcher, error) {
	var matcher packetMatcher
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return matcher, nil
	}
	for _, term := range strings.Split(raw, " and ") {
		term = strings.TrimSpace(term)
		switch {
		case term == "tcp":
			matcher.protocol = 6
		case term == "udp":
			matcher.protocol = 17
		case term == "icmp":
			matcher.protocol = 1
		case term == "icmp6" || term == "icmpv6":
			matcher.protocol = 58
		case strings.HasPrefix(term, "host "):
			if !matcher.host.IsValid() {
				addr, err := netip.ParseAddr(strings.TrimSpace(strings.TrimPrefix(term, "host ")))
				if err != nil {
					return packetMatcher{}, fmt.Errorf("packet capture: invalid host filter: %w", err)
				}
				matcher.host = addr.Unmap()
			} else {
				return packetMatcher{}, errors.New("packet capture: filter accepts one host")
			}
		case strings.HasPrefix(term, "port "):
			if matcher.port != 0 {
				return packetMatcher{}, errors.New("packet capture: filter accepts one port")
			}
			port, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(term, "port ")), 10, 16)
			if err != nil || port == 0 {
				return packetMatcher{}, errors.New("packet capture: invalid port filter")
			}
			matcher.port = uint16(port)
		default:
			return packetMatcher{}, fmt.Errorf("packet capture: unsupported filter %q; use tcp, udp, icmp, host <ip>, port <n>, joined with and", term)
		}
	}
	return matcher, nil
}

func (m packetMatcher) matches(frame []byte) bool {
	proto, src, dst, srcPort, dstPort, ok := parseFrame(frame)
	if !ok {
		return false
	}
	if m.protocol != 0 && m.protocol != proto {
		return false
	}
	if m.host.IsValid() && m.host != src && m.host != dst {
		return false
	}
	if m.port != 0 && m.port != srcPort && m.port != dstPort {
		return false
	}
	return true
}

func parseFrame(frame []byte) (uint8, netip.Addr, netip.Addr, uint16, uint16, bool) {
	if len(frame) < 14 {
		return 0, netip.Addr{}, netip.Addr{}, 0, 0, false
	}
	offset := 14
	etherType := binary.BigEndian.Uint16(frame[12:14])
	for etherType == 0x8100 || etherType == 0x88a8 {
		if len(frame) < offset+4 {
			return 0, netip.Addr{}, netip.Addr{}, 0, 0, false
		}
		etherType = binary.BigEndian.Uint16(frame[offset+2 : offset+4])
		offset += 4
	}
	switch etherType {
	case 0x0800:
		if len(frame) < offset+20 || frame[offset]>>4 != 4 {
			return 0, netip.Addr{}, netip.Addr{}, 0, 0, false
		}
		headerLen := int(frame[offset]&0x0f) * 4
		if headerLen < 20 || len(frame) < offset+headerLen {
			return 0, netip.Addr{}, netip.Addr{}, 0, 0, false
		}
		proto := frame[offset+9]
		src := netip.AddrFrom4([4]byte(frame[offset+12 : offset+16]))
		dst := netip.AddrFrom4([4]byte(frame[offset+16 : offset+20]))
		return parsePorts(frame, offset+headerLen, proto, src, dst)
	case 0x86dd:
		if len(frame) < offset+40 || frame[offset]>>4 != 6 {
			return 0, netip.Addr{}, netip.Addr{}, 0, 0, false
		}
		proto := frame[offset+6]
		src := netip.AddrFrom16([16]byte(frame[offset+8 : offset+24]))
		dst := netip.AddrFrom16([16]byte(frame[offset+24 : offset+40]))
		return parsePorts(frame, offset+40, proto, src, dst)
	default:
		return 0, netip.Addr{}, netip.Addr{}, 0, 0, false
	}
}

func parsePorts(frame []byte, offset int, proto uint8, src, dst netip.Addr) (uint8, netip.Addr, netip.Addr, uint16, uint16, bool) {
	if proto != 6 && proto != 17 {
		return proto, src, dst, 0, 0, true
	}
	if len(frame) < offset+4 {
		return 0, netip.Addr{}, netip.Addr{}, 0, 0, false
	}
	return proto, src, dst, binary.BigEndian.Uint16(frame[offset : offset+2]), binary.BigEndian.Uint16(frame[offset+2 : offset+4]), true
}
