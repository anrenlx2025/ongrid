package logs

import (
	"sort"
	"testing"
	"time"

	logsmodel "github.com/ongridio/ongrid/internal/manager/model/logs"
	"github.com/ongridio/ongrid/internal/pkg/logquery"
)

func TestSearchPhasesRespectCutoverAndDirection(t *testing.T) {
	start := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	cutover := start.Add(30 * time.Minute)
	end := start.Add(time.Hour)
	backend := &logsmodel.Backend{ID: 7, Generation: 3, CutoverAt: &cutover}

	forward := buildQueryPhases(start, end, []*logsmodel.Backend{backend})
	if len(forward) != 2 || forward[0].backend != nil || forward[1].backend != backend {
		t.Fatalf("forward phases = %#v", forward)
	}
	if !forward[0].end.Equal(cutover) || !forward[1].start.Equal(cutover) {
		t.Fatalf("cutover boundaries = %#v", forward)
	}

	backward := append([]queryPhase(nil), forward...)
	sort.SliceStable(backward, func(i, j int) bool { return backward[i].start.After(backward[j].start) })
	if len(backward) != 2 || backward[0].backend != backend || backward[1].backend != nil {
		t.Fatalf("backward phases = %#v", backward)
	}

	before := buildQueryPhases(start, cutover, []*logsmodel.Backend{backend})
	if len(before) != 1 || before[0].backend != nil {
		t.Fatalf("before-cutover phases = %#v", before)
	}
	afterStart := cutover.Add(time.Nanosecond)
	after := buildQueryPhases(afterStart, end, []*logsmodel.Backend{backend})
	if len(after) != 1 || after[0].backend != backend {
		t.Fatalf("after-cutover phases = %#v", after)
	}
}

func TestBuildQueryPhasesRetainsMultipleRolledBackGenerations(t *testing.T) {
	start := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)
	firstStart, firstEnd := start.Add(20*time.Minute), start.Add(40*time.Minute)
	secondStart, secondEnd := start.Add(time.Hour), start.Add(80*time.Minute)
	first := &logsmodel.Backend{ID: 1, Generation: 1, CutoverAt: &firstStart, EndedAt: &firstEnd}
	second := &logsmodel.Backend{ID: 2, Generation: 2, CutoverAt: &secondStart, EndedAt: &secondEnd}

	phases := buildQueryPhases(start, end, []*logsmodel.Backend{second, first})
	if len(phases) != 5 {
		t.Fatalf("phases = %#v, want Loki/ES1/Loki/ES2/Loki", phases)
	}
	wantBackends := []uint64{0, 1, 0, 2, 0}
	for i, want := range wantBackends {
		var got uint64
		if phases[i].backend != nil {
			got = phases[i].backend.ID
		}
		if got != want {
			t.Fatalf("phase[%d] backend = %d, want %d (%#v)", i, got, want, phases)
		}
	}
	if !phases[1].end.Equal(firstEnd) || !phases[2].start.Equal(firstEnd) {
		t.Fatalf("rollback boundary = %#v", phases)
	}
}

func TestPlannerCursorBindsGenerationAndRequest(t *testing.T) {
	req := logquery.SearchRequest{
		Start:     time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC),
		End:       time.Date(2026, 8, 18, 11, 0, 0, 0, time.UTC),
		Limit:     100,
		Direction: logquery.SortBackward,
	}
	sum, err := plannerRequestSum(req)
	if err != nil {
		t.Fatalf("plannerRequestSum: %v", err)
	}
	raw, err := encodePlannerCursor(plannerCursor{Backend: plannerBackendName, PlanSum: "plan-7", Phase: "elasticsearch:7", Cursor: "opaque", RequestSum: sum})
	if err != nil {
		t.Fatalf("encodePlannerCursor: %v", err)
	}
	var got plannerCursor
	if err := decodePlannerCursor(raw, &got); err != nil {
		t.Fatalf("decodePlannerCursor: %v", err)
	}
	if got.PlanSum != "plan-7" || got.Phase != "elasticsearch:7" || got.Cursor != "opaque" || got.RequestSum != sum {
		t.Fatalf("cursor = %+v", got)
	}

	changed := req
	changed.Limit++
	changedSum, err := plannerRequestSum(changed)
	if err != nil {
		t.Fatalf("changed plannerRequestSum: %v", err)
	}
	if changedSum == sum {
		t.Fatal("request fingerprint did not change")
	}
	if err := decodePlannerCursor("not-base64", &got); err == nil {
		t.Fatal("malformed cursor accepted")
	}
}
