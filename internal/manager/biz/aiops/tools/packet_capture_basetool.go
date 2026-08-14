package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ongridio/ongrid/internal/manager/biz/aiops/tools/basetool"
	pcapbiz "github.com/ongridio/ongrid/internal/manager/biz/packetcapture"
	pcapmodel "github.com/ongridio/ongrid/internal/manager/model/packetcapture"
)

const (
	ToolNameCapturePCAP    = "capture_pcap"
	ToolNameGetPCAPSession = "get_packet_capture_session"
)

const capturePCAPDescription = "Start a bounded packet capture session on one or more host devices. Use repeat_count to create multiple capture members for each target in the same session. The tool returns as soon as every member is accepted by its edge; inspect the returned session for progress and artifacts. Captures are audited and must be explicitly requested by the operator."

const capturePCAPWhenToUse = "Use only when the user explicitly asks to capture packets or diagnose live network traffic on a specific host device/interface. Do not use for normal metric/log/trace questions. Prefer query_logql/query_traceql/query_promql first unless the user asks for raw packets."

var CapturePCAPSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "device_id": {"type": "integer", "description": "Host device id to capture on."},
    "interface": {"type": "string", "description": "Network interface name on the host, for example eth0."},
	"targets": {"type": "array", "minItems": 1, "description": "Capture targets. Use one item for one host; multiple items may be on the same or different edges.", "items": {"type": "object", "properties": {"device_id": {"type": "integer"}, "interface": {"type": "string"}}, "required": ["device_id", "interface"]}},
	"repeat_count": {"type": "integer", "minimum": 1, "maximum": 10, "default": 1, "description": "Number of sequential capture rounds per target in the same session. A round captures all targets concurrently; later rounds start after the prior round duration."},
    "filter": {"type": "string", "description": "Simple filter grammar: tcp, udp, icmp, icmp6, host <IP>, port <N>, joined by 'and'. Example: tcp and port 443."},
    "duration_seconds": {"type": "integer", "minimum": 1, "maximum": 300, "default": 30},
    "max_bytes": {"type": "integer", "minimum": 1, "maximum": 268435456, "default": 67108864},
    "max_packets": {"type": "integer", "minimum": 1, "maximum": 500000, "default": 100000},
    "snaplen": {"type": "integer", "minimum": 64, "maximum": 65535, "default": 1514},
    "promiscuous": {"type": "boolean", "default": false},
	"title": {"type": "string", "description": "Short investigation name shown on the packet capture session."},
	"reason": {"type": "string", "description": "Why this capture is requested. Stored on the capture record."}
  },
	"anyOf": [{"required": ["device_id", "interface"]}, {"required": ["targets"]}]
}`)

type PacketCaptureCreator interface {
	Create(ctx context.Context, in pcapbiz.CreateInput) (*pcapbiz.CreateOutput, error)
	Refresh(ctx context.Context, id uint64) (*pcapmodel.Capture, error)
	GetSession(ctx context.Context, publicID string) (*pcapbiz.SessionDetail, error)
}

var GetPacketCaptureSessionSchema = json.RawMessage(`{
  "type":"object",
  "properties":{"session_id":{"type":"string","description":"Opaque packet capture session id, pcap-session-..."}},
  "required":["session_id"]
}`)

type GetPacketCaptureSessionTool struct{ uc PacketCaptureCreator }

func NewGetPacketCaptureSessionTool(uc PacketCaptureCreator) *GetPacketCaptureSessionTool {
	return &GetPacketCaptureSessionTool{uc: uc}
}

func (t *GetPacketCaptureSessionTool) Info(context.Context) (*basetool.ToolInfo, error) {
	return &basetool.ToolInfo{Name: ToolNameGetPCAPSession, Description: "Read a coordinated multi-edge packet capture session for diagnosis. Returns member capture status, normalized cross-edge flows, and merged packet metadata timeline; never returns raw PCAP payloads.", WhenToUse: "Use when the user asks to analyze, compare, or explain a packet capture session. Call this before making network-loss or latency claims.", Parameters: GetPacketCaptureSessionSchema, Class: "read"}, nil
}

func (t *GetPacketCaptureSessionTool) InvokableRun(ctx context.Context, argsJSON string, _ ...basetool.InvokeOption) (string, error) {
	if t.uc == nil {
		return "", fmt.Errorf("%s: packet capture usecase not configured", ToolNameGetPCAPSession)
	}
	var in struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return "", fmt.Errorf("%s: bad args: %w", ToolNameGetPCAPSession, err)
	}
	detail, err := t.uc.GetSession(ctx, strings.TrimSpace(in.SessionID))
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string]any{"session": detail.Session, "captures": detail.Captures, "analysis": detail.Analysis})
	if err != nil {
		return "", fmt.Errorf("%s: marshal response: %w", ToolNameGetPCAPSession, err)
	}
	return string(body), nil
}

type PacketCaptureSessionCreator interface {
	CreateSession(ctx context.Context, in pcapbiz.CreateSessionInput) (*pcapbiz.SessionOutput, error)
}

type CapturePCAPTool struct {
	uc  PacketCaptureCreator
	log *slog.Logger
}

type capturePCAPArgs struct {
	DeviceID        uint64 `json:"device_id"`
	Interface       string `json:"interface"`
	Filter          string `json:"filter"`
	DurationSeconds int    `json:"duration_seconds"`
	MaxBytes        int64  `json:"max_bytes"`
	MaxPackets      int    `json:"max_packets"`
	Snaplen         int    `json:"snaplen"`
	Promiscuous     bool   `json:"promiscuous"`
	RepeatCount     int    `json:"repeat_count"`
	Title           string `json:"title"`
	Reason          string `json:"reason"`
	Targets         []struct {
		DeviceID  uint64 `json:"device_id"`
		Interface string `json:"interface"`
	} `json:"targets"`
}

func NewCapturePCAPTool(uc PacketCaptureCreator, log *slog.Logger) *CapturePCAPTool {
	if log == nil {
		log = slog.Default()
	}
	return &CapturePCAPTool{uc: uc, log: log}
}

func (t *CapturePCAPTool) Info(_ context.Context) (*basetool.ToolInfo, error) {
	return &basetool.ToolInfo{
		Name:        ToolNameCapturePCAP,
		Description: capturePCAPDescription,
		WhenToUse:   capturePCAPWhenToUse,
		Parameters:  CapturePCAPSchema,
		// Capturing observes network traffic and creates a bounded evidence
		// artifact; it does not change the managed host configuration. Keeping
		// it read-class avoids applying the SOP gate intended for restart and
		// configuration changes to an explicit, time-bounded diagnostic request.
		Class: "read",
	}, nil
}

func (t *CapturePCAPTool) InvokableRun(ctx context.Context, argsJSON string, opts ...basetool.InvokeOption) (string, error) {
	if t.uc == nil {
		return "", fmt.Errorf("%s: packet capture usecase not configured", ToolNameCapturePCAP)
	}
	var in capturePCAPArgs
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return "", fmt.Errorf("%s: bad args: %w", ToolNameCapturePCAP, err)
	}
	resolved := basetool.ResolveOptions(opts)
	source := pcapbiz.SourceChat
	switch basetool.ArtifactSourceFromContext(ctx) {
	case basetool.ArtifactSourceWorkflow:
		source = pcapbiz.SourceWorkflow
	case basetool.ArtifactSourceChat:
		source = pcapbiz.SourceChat
	}
	sessionCreator, ok := t.uc.(PacketCaptureSessionCreator)
	if !ok {
		return "", fmt.Errorf("%s: packet capture sessions are not configured", ToolNameCapturePCAP)
	}
	targets := make([]pcapbiz.SessionTarget, 0, len(in.Targets)+1)
	for _, target := range in.Targets {
		targets = append(targets, pcapbiz.SessionTarget{DeviceID: target.DeviceID, Interface: strings.TrimSpace(target.Interface)})
	}
	if len(targets) == 0 {
		targets = append(targets, pcapbiz.SessionTarget{DeviceID: in.DeviceID, Interface: strings.TrimSpace(in.Interface)})
	}
	repeatCount := in.RepeatCount
	if repeatCount == 0 {
		repeatCount = 1
	}
	if repeatCount < 1 || repeatCount > 10 {
		return "", fmt.Errorf("%s: repeat_count must be between 1 and 10", ToolNameCapturePCAP)
	}
	if repeatCount > 1 {
		original := targets
		targets = make([]pcapbiz.SessionTarget, 0, len(original)*repeatCount)
		interval := in.DurationSeconds
		if interval == 0 {
			interval = 30
		}
		for round := range repeatCount {
			for _, target := range original {
				target.StartAfterSeconds = round * interval
				targets = append(targets, target)
			}
		}
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = "Packet capture"
	}
	out, err := sessionCreator.CreateSession(ctx, pcapbiz.CreateSessionInput{Targets: targets, Filter: strings.TrimSpace(in.Filter), DurationSeconds: in.DurationSeconds, MaxBytes: in.MaxBytes, MaxPackets: in.MaxPackets, Snaplen: in.Snaplen, Promiscuous: in.Promiscuous, Title: title, Description: strings.TrimSpace(in.Reason), Source: source, CreatedBy: resolved.UserID})
	if err != nil {
		return "", err
	}
	if len(out.Captures) == 0 {
		return "", fmt.Errorf("%s: packet capture session has no created members", ToolNameCapturePCAP)
	}
	operation := basetool.Operation{
		Kind:    "packet_capture_session",
		ID:      out.Session.PublicID,
		State:   string(out.Session.State),
		Title:   out.Session.Title,
		Summary: fmt.Sprintf("%d capture member(s)", len(out.Captures)),
		Links:   map[string]string{"detail": "/artifacts/packet-sessions/" + out.Session.PublicID},
	}
	body, err := json.Marshal(map[string]any{"operation": operation, "session": out.Session, "captures": out.Captures, "member_errors": out.MemberErrors, "result": capturePCAPResult(out.Captures[0], nil, false)})
	if err != nil {
		return "", fmt.Errorf("%s: marshal response: %w", ToolNameCapturePCAP, err)
	}
	return string(body), nil
}

const (
	capturePollInterval = time.Second
	maxCaptureWait      = 45 * time.Second
)

// waitForCapture keeps a chat turn coherent for ordinary short captures. The
// bounded wait prevents a long forensic capture from occupying an agent turn.
func (t *CapturePCAPTool) waitForCapture(ctx context.Context, capture *pcapmodel.Capture) (*pcapmodel.Capture, bool) {
	if capture == nil || capture.ID == 0 {
		return capture, false
	}
	waitFor := time.Duration(capture.DurationSecs)*time.Second + 20*time.Second
	if waitFor < 20*time.Second {
		waitFor = 20 * time.Second
	}
	if waitFor > maxCaptureWait {
		waitFor = maxCaptureWait
	}
	waitCtx, cancel := context.WithTimeout(ctx, waitFor)
	defer cancel()

	current := capture
	for {
		if current.State == pcapmodel.StateReady && current.ParsedJSON != "" {
			return current, true
		}
		if current.State == pcapmodel.StateFailed || current.State == pcapmodel.StateCancelled {
			return current, true
		}
		select {
		case <-waitCtx.Done():
			return current, false
		case <-time.After(capturePollInterval):
		}
		refreshed, err := t.uc.Refresh(waitCtx, current.ID)
		if err != nil {
			t.log.Warn("packet capture: wait refresh failed", slog.Uint64("capture_id", current.ID), slog.Any("err", err))
			continue
		}
		current = refreshed
	}
}

func capturePCAPResult(capture *pcapmodel.Capture, edge any, waited bool) map[string]any {
	result := map[string]any{
		"capture": capture,
		"edge":    edge,
		"waited":  waited,
	}
	if capture == nil || capture.State != pcapmodel.StateReady || capture.ParsedJSON == "" {
		return result
	}
	var parsed struct {
		ArtifactID string `json:"artifact_id"`
		Packets    []struct {
			Number      any    `json:"number"`
			Source      string `json:"source"`
			Destination string `json:"destination"`
			Protocol    string `json:"protocol"`
		} `json:"packets"`
	}
	if err := json.Unmarshal([]byte(capture.ParsedJSON), &parsed); err != nil {
		return result
	}
	preview := parsed.Packets
	if len(preview) > 3 {
		preview = preview[:3]
	}
	result["artifact"] = map[string]any{
		"id":               capture.ArtifactID,
		"captured_packets": capture.CapturedPackets,
		"captured_bytes":   capture.CapturedBytes,
		"first_packets":    preview,
	}
	return result
}

var _ basetool.BaseTool = (*CapturePCAPTool)(nil)
var _ basetool.BaseTool = (*GetPacketCaptureSessionTool)(nil)
