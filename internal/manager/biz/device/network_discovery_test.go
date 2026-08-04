package device

import (
	"context"
	"errors"
	"testing"

	model "github.com/ongridio/ongrid/internal/manager/model/device"
	"github.com/ongridio/ongrid/internal/pkg/errs"
	"github.com/ongridio/ongrid/internal/pkg/tunnel"
)

type fakeNetworkDiscoveryRepo struct {
	rows []*model.NetworkDiscoveryCandidate
}

type fakeNetworkSNMPCaller struct {
	response []byte
	calls    int
}

func (f *fakeNetworkSNMPCaller) Call(context.Context, uint64, string, []byte) ([]byte, error) {
	f.calls++
	return f.response, nil
}

func (f *fakeNetworkDiscoveryRepo) UpsertCandidates(_ context.Context, rows []*model.NetworkDiscoveryCandidate) error {
	f.rows = append(f.rows, rows...)
	return nil
}

func (f *fakeNetworkDiscoveryRepo) ListCandidates(context.Context, NetworkCandidateFilter) ([]*model.NetworkDiscoveryCandidate, int64, error) {
	return f.rows, int64(len(f.rows)), nil
}

func (f *fakeNetworkDiscoveryRepo) UpdateCandidate(_ context.Context, candidate *model.NetworkDiscoveryCandidate) error {
	for i, row := range f.rows {
		if row.ID == candidate.ID {
			f.rows[i] = candidate
			return nil
		}
	}
	return errs.ErrNotFound
}

func (f *fakeNetworkDiscoveryRepo) GetCandidate(_ context.Context, id uint64) (*model.NetworkDiscoveryCandidate, error) {
	for _, row := range f.rows {
		if row.ID == id {
			return row, nil
		}
	}
	return nil, errs.ErrNotFound
}

func (f *fakeNetworkDiscoveryRepo) MarkCandidatePromoted(_ context.Context, id, deviceID uint64) error {
	for _, row := range f.rows {
		if row.ID == id {
			row.Status = NetworkCandidateStatusPromoted
			row.PromotedDeviceID = &deviceID
			return nil
		}
	}
	return errs.ErrNotFound
}

func (f *fakeNetworkDiscoveryRepo) UpsertDeviceNetwork(context.Context, *model.DeviceNetwork) error {
	return nil
}

func (f *fakeNetworkDiscoveryRepo) GetNetworkDeviceDetail(_ context.Context, deviceID uint64) (*NetworkDeviceDetail, error) {
	for _, row := range f.rows {
		if row.PromotedDeviceID != nil && *row.PromotedDeviceID == deviceID {
			return &NetworkDeviceDetail{
				Profile:   &model.DeviceNetwork{DeviceID: deviceID, DeviceKind: "network"},
				Candidate: row,
			}, nil
		}
	}
	return nil, errs.ErrNotFound
}

func TestNetworkDiscoveryUsecaseKeepsARPAsCandidate(t *testing.T) {
	repo := &fakeNetworkDiscoveryRepo{}
	uc := NewNetworkDiscoveryUsecase(repo)

	accepted, err := uc.IngestNetworkDiscovery(context.Background(), 7, tunnel.NetworkDiscoveryRequest{
		Candidates: []tunnel.NetworkDiscoveryCandidateReport{{
			IPAddress: "192.0.2.1", MAC: "AA-BB-CC-DD-EE-FF", Source: "arp",
		}},
	})
	if err != nil || accepted != 1 || len(repo.rows) != 1 {
		t.Fatalf("accepted=%d rows=%d err=%v", accepted, len(repo.rows), err)
	}
	if repo.rows[0].Status != NetworkCandidateStatusUnknown {
		t.Fatalf("ARP candidate status=%q", repo.rows[0].Status)
	}
	if repo.rows[0].MAC != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("normalized MAC=%q", repo.rows[0].MAC)
	}
}

func TestNetworkDiscoveryUsecaseDeduplicatesBatch(t *testing.T) {
	repo := &fakeNetworkDiscoveryRepo{}
	uc := NewNetworkDiscoveryUsecase(repo)
	report := tunnel.NetworkDiscoveryCandidateReport{IPAddress: "192.0.2.10", InterfaceName: "eth0", Source: "gateway"}
	accepted, err := uc.IngestNetworkDiscovery(context.Background(), 7, tunnel.NetworkDiscoveryRequest{
		Candidates: []tunnel.NetworkDiscoveryCandidateReport{report, report},
	})
	if err != nil || accepted != 1 || len(repo.rows) != 1 {
		t.Fatalf("accepted=%d rows=%d err=%v", accepted, len(repo.rows), err)
	}
}

func TestNetworkDiscoveryUsecaseAcceptsLLDPWithoutIPAddress(t *testing.T) {
	repo := &fakeNetworkDiscoveryRepo{}
	uc := NewNetworkDiscoveryUsecase(repo)
	accepted, err := uc.IngestNetworkDiscovery(context.Background(), 7, tunnel.NetworkDiscoveryRequest{
		Candidates: []tunnel.NetworkDiscoveryCandidateReport{{
			InterfaceName: "eth0", Source: "lldp", LLDPChassisID: "00:11:22:33:44:55",
		}},
	})
	if err != nil || accepted != 1 || len(repo.rows) != 1 {
		t.Fatalf("accepted=%d rows=%d err=%v", accepted, len(repo.rows), err)
	}
	if repo.rows[0].Status != NetworkCandidateStatusIdentified {
		t.Fatalf("LLDP candidate status=%q", repo.rows[0].Status)
	}
}

func TestPromoteCandidateIsExplicitAndIdempotent(t *testing.T) {
	candidateRepo := &fakeNetworkDiscoveryRepo{rows: []*model.NetworkDiscoveryCandidate{{
		ID: 9, ObserverEdgeID: 7, IPAddress: "192.0.2.10", Status: NetworkCandidateStatusSNMPVerified,
		SourceDataJSON: `{"sys_name":"sw-a","vendor":"acme","model":"x1"}`,
	}}}
	deviceRepo := &fakeRepo{byID: map[uint64]*model.Device{}}
	links := &fakeLinks{}
	uc := NewNetworkDiscoveryUsecase(candidateRepo)
	uc.SetPromotionDependencies(candidateRepo, deviceRepo, links)

	device, err := uc.PromoteCandidate(context.Background(), 9, "edge-switch")
	if err != nil || device == nil || device.Roles != model.RoleBitNetwork {
		t.Fatalf("device=%+v err=%v", device, err)
	}
	if len(links.linked) != 1 || links.linked[0] != [2]uint64{7, device.ID} {
		t.Fatalf("links=%v", links.linked)
	}
	again, err := uc.PromoteCandidate(context.Background(), 9, "ignored-name")
	if err != nil || again.ID != device.ID {
		t.Fatalf("idempotent device=%+v err=%v", again, err)
	}
}

func TestPromoteCandidateRequiresSNMPVerification(t *testing.T) {
	candidateRepo := &fakeNetworkDiscoveryRepo{rows: []*model.NetworkDiscoveryCandidate{{
		ID: 11, ObserverEdgeID: 7, IPAddress: "192.0.2.11", Status: NetworkCandidateStatusIdentified,
	}}}
	uc := NewNetworkDiscoveryUsecase(candidateRepo)
	uc.SetPromotionDependencies(candidateRepo, &fakeRepo{byID: map[uint64]*model.Device{}}, &fakeLinks{})
	if _, err := uc.PromoteCandidate(context.Background(), 11, ""); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("err=%v, want conflict", err)
	}
}

func TestPromoteCandidateRepairsStalePromotedStatus(t *testing.T) {
	deviceID := uint64(42)
	candidateRepo := &fakeNetworkDiscoveryRepo{rows: []*model.NetworkDiscoveryCandidate{{
		ID: 10, Status: NetworkCandidateStatusIdentified, PromotedDeviceID: &deviceID,
	}}}
	deviceRepo := &fakeRepo{byID: map[uint64]*model.Device{
		deviceID: {ID: deviceID, Name: "existing-network-device", Roles: model.RoleBitNetwork},
	}}
	uc := NewNetworkDiscoveryUsecase(candidateRepo)
	uc.SetPromotionDependencies(candidateRepo, deviceRepo, &fakeLinks{})

	device, err := uc.PromoteCandidate(context.Background(), 10, "")
	if err != nil || device.ID != deviceID {
		t.Fatalf("device=%+v err=%v", device, err)
	}
	if candidateRepo.rows[0].Status != NetworkCandidateStatusPromoted {
		t.Fatalf("repaired status=%q, want %q", candidateRepo.rows[0].Status, NetworkCandidateStatusPromoted)
	}
}

func TestScanAndPromoteCandidateRefreshesExistingNetworkDevice(t *testing.T) {
	deviceID := uint64(42)
	candidateRepo := &fakeNetworkDiscoveryRepo{rows: []*model.NetworkDiscoveryCandidate{{
		ID: 12, ObserverEdgeID: 7, IPAddress: "192.0.2.12", Source: "lldp",
		Status: NetworkCandidateStatusPromoted, PromotedDeviceID: &deviceID,
	}}}
	deviceRepo := &fakeRepo{byID: map[uint64]*model.Device{
		deviceID: {ID: deviceID, Name: "existing-network-device", Roles: model.RoleBitNetwork},
	}}
	caller := &fakeNetworkSNMPCaller{response: []byte(`{
		"ok":true,"ip_address":"192.0.2.12","sys_name":"switch-a",
		"interfaces":[{"if_index":1,"name":"eth0","oper_status":"up"}]
	}`)}
	uc := NewNetworkDiscoveryUsecase(candidateRepo)
	uc.SetPromotionDependencies(candidateRepo, deviceRepo, &fakeLinks{})
	uc.SetEdgeCaller(caller)

	device, err := uc.ScanAndPromoteCandidate(context.Background(), 12, tunnel.ProbeNetworkSNMPRequest{
		Version: "v2c", Community: "readonly",
	}, "")
	if err != nil || device.ID != deviceID {
		t.Fatalf("device=%+v err=%v", device, err)
	}
	if caller.calls != 1 || candidateRepo.rows[0].Source != "snmp" || candidateRepo.rows[0].InterfacesJSON == "[]" {
		t.Fatalf("calls=%d candidate=%+v", caller.calls, candidateRepo.rows[0])
	}
	if candidateRepo.rows[0].Status != NetworkCandidateStatusPromoted {
		t.Fatalf("status=%q", candidateRepo.rows[0].Status)
	}
}
