package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ongridio/ongrid/internal/manager/biz/aiops/tools/basetool"
	pcapbiz "github.com/ongridio/ongrid/internal/manager/biz/packetcapture"
	pcapmodel "github.com/ongridio/ongrid/internal/manager/model/packetcapture"
	"github.com/ongridio/ongrid/internal/pkg/tunnel"
)

type fakePacketCaptureCreator struct {
	in      pcapbiz.CreateInput
	refresh func(*pcapmodel.Capture) *pcapmodel.Capture
}

func (f *fakePacketCaptureCreator) Create(_ context.Context, in pcapbiz.CreateInput) (*pcapbiz.CreateOutput, error) {
	f.in = in
	return &pcapbiz.CreateOutput{
		Capture: &pcapmodel.Capture{
			ID:            12,
			DeviceID:      in.DeviceID,
			InterfaceName: in.Interface,
			State:         pcapmodel.StateCapturing,
		},
		Edge: tunnel.PacketCaptureTask{ID: "pcap-12", State: "running"},
	}, nil
}

func (f *fakePacketCaptureCreator) Refresh(_ context.Context, id uint64) (*pcapmodel.Capture, error) {
	capture := &pcapmodel.Capture{ID: id, State: pcapmodel.StateReady, ArtifactID: "pcap-11111111-1111-1111-1111-111111111111", ParsedJSON: `{"packets":[{"number":1,"source":"10.0.0.1","destination":"10.0.0.2","protocol":"TCP"}]}`}
	if f.refresh != nil {
		capture = f.refresh(capture)
	}
	return capture, nil
}

func (f *fakePacketCaptureCreator) GetSession(_ context.Context, id string) (*pcapbiz.SessionDetail, error) {
	return &pcapbiz.SessionDetail{Session: &pcapmodel.Session{PublicID: id}, Analysis: pcapbiz.SessionAnalysis{}}, nil
}

func TestGetPacketCaptureSessionToolReturnsSessionAnalysis(t *testing.T) {
	tool := NewGetPacketCaptureSessionTool(&fakePacketCaptureCreator{})
	out, err := tool.InvokableRun(context.Background(), `{"session_id":"pcap-session-123"}`)
	if err != nil || !strings.Contains(out, "pcap-session-123") {
		t.Fatalf("out=%s err=%v", out, err)
	}
}

func TestCapturePCAPToolInfoIsReadSpecialty(t *testing.T) {
	tool := NewCapturePCAPTool(&fakePacketCaptureCreator{}, nil)
	info, err := tool.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Name != ToolNameCapturePCAP || info.Class != "read" {
		t.Fatalf("info = name:%s class:%s", info.Name, info.Class)
	}
	if toolTier(tool) != "specialty" {
		t.Fatalf("%s should be specialty", ToolNameCapturePCAP)
	}
}

func TestCapturePCAPToolInvokesUsecase(t *testing.T) {
	creator := &fakePacketCaptureCreator{}
	tool := NewCapturePCAPTool(creator, nil)

	out, err := tool.InvokableRun(context.Background(), `{
		"device_id": 24,
		"interface": "eth0",
		"filter": "tcp and port 443",
		"duration_seconds": 20,
		"reason": "debug checkout timeout"
	}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if creator.in.Source != pcapbiz.SourceChat || creator.in.DeviceID != 24 || creator.in.Interface != "eth0" {
		t.Fatalf("input = %+v", creator.in)
	}
	var decoded struct {
		Capture struct {
			ID uint64 `json:"id"`
		} `json:"capture"`
		Edge struct {
			State string `json:"state"`
		} `json:"edge"`
		Waited   bool `json:"waited"`
		Artifact struct {
			ID           string `json:"id"`
			FirstPackets []struct {
				Source string `json:"source"`
			} `json:"first_packets"`
		} `json:"artifact"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if decoded.Capture.ID != 12 || decoded.Edge.State != "running" || !decoded.Waited || decoded.Artifact.ID == "" || len(decoded.Artifact.FirstPackets) != 1 {
		t.Fatalf("output = %s", out)
	}
}

func TestCapturePCAPToolUsesWorkflowSourceFromContext(t *testing.T) {
	creator := &fakePacketCaptureCreator{}
	tool := NewCapturePCAPTool(creator, nil)
	ctx := basetool.WithArtifactSource(context.Background(), basetool.ArtifactSourceWorkflow)

	_, err := tool.InvokableRun(ctx, `{"device_id":24,"interface":"eth0"}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if creator.in.Source != pcapbiz.SourceWorkflow {
		t.Fatalf("source = %q, want workflow", creator.in.Source)
	}
}
