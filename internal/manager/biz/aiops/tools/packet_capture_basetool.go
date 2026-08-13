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

const ToolNameCapturePCAP = "capture_pcap"

const capturePCAPDescription = "Start a bounded packet capture on a host device. For short captures, waits for parsing and returns the durable artifact id plus a packet summary. Captures are audited and must be explicitly requested by the operator."

const capturePCAPWhenToUse = "Use only when the user explicitly asks to capture packets or diagnose live network traffic on a specific host device/interface. Do not use for normal metric/log/trace questions. Prefer query_logql/query_traceql/query_promql first unless the user asks for raw packets."

var CapturePCAPSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "device_id": {"type": "integer", "description": "Host device id to capture on."},
    "interface": {"type": "string", "description": "Network interface name on the host, for example eth0."},
    "filter": {"type": "string", "description": "Simple filter grammar: tcp, udp, icmp, icmp6, host <IP>, port <N>, joined by 'and'. Example: tcp and port 443."},
    "duration_seconds": {"type": "integer", "minimum": 1, "maximum": 300, "default": 30},
    "max_bytes": {"type": "integer", "minimum": 1, "maximum": 268435456, "default": 67108864},
    "max_packets": {"type": "integer", "minimum": 1, "maximum": 500000, "default": 100000},
    "snaplen": {"type": "integer", "minimum": 64, "maximum": 65535, "default": 1514},
    "promiscuous": {"type": "boolean", "default": false},
    "reason": {"type": "string", "description": "Why this capture is requested. Stored on the capture record."}
  },
  "required": ["device_id", "interface"]
}`)

type PacketCaptureCreator interface {
	Create(ctx context.Context, in pcapbiz.CreateInput) (*pcapbiz.CreateOutput, error)
	Refresh(ctx context.Context, id uint64) (*pcapmodel.Capture, error)
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
	Reason          string `json:"reason"`
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
		Class:       "read",
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
	out, err := t.uc.Create(ctx, pcapbiz.CreateInput{
		DeviceID:        in.DeviceID,
		Interface:       strings.TrimSpace(in.Interface),
		Filter:          strings.TrimSpace(in.Filter),
		DurationSeconds: in.DurationSeconds,
		MaxBytes:        in.MaxBytes,
		MaxPackets:      in.MaxPackets,
		Snaplen:         in.Snaplen,
		Promiscuous:     in.Promiscuous,
		Description:     strings.TrimSpace(in.Reason),
		Source:          source,
		CreatedBy:       resolved.UserID,
	})
	if err != nil {
		return "", err
	}
	capture, waited := t.waitForCapture(ctx, out.Capture)
	body, err := json.Marshal(capturePCAPResult(capture, out.Edge, waited))
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
