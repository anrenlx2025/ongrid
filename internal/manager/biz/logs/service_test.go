package logs_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	bizlogs "github.com/ongridio/ongrid/internal/manager/biz/logs"
	logsstore "github.com/ongridio/ongrid/internal/manager/data/logs/store"
	logsmodel "github.com/ongridio/ongrid/internal/manager/model/logs"
	"github.com/ongridio/ongrid/internal/pkg/errs"
	"github.com/ongridio/ongrid/internal/pkg/logquery"
)

type mapSecrets map[string]map[string]string

func (m mapSecrets) ResolveFields(_ context.Context, name string) (map[string]string, error) {
	fields := m[name]
	out := make(map[string]string, len(fields))
	for key, value := range fields {
		out[key] = value
	}
	return out, nil
}

type managedSecrets struct {
	mu           sync.Mutex
	values       map[string]map[string]string
	creates      []string
	deletes      []string
	failCreateAt int
}

func newManagedSecrets() *managedSecrets {
	return &managedSecrets{values: map[string]map[string]string{}}
}

func (m *managedSecrets) ResolveFields(_ context.Context, name string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fields := m.values[name]
	out := make(map[string]string, len(fields))
	for key, value := range fields {
		out[key] = value
	}
	return out, nil
}

func (m *managedSecrets) CreateManaged(_ context.Context, name, credType, _ string, fields map[string]string) error {
	if credType != "elasticsearch" {
		return errors.New("unexpected credential type")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failCreateAt > 0 && len(m.creates)+1 == m.failCreateAt {
		return errors.New("injected managed credential create failure")
	}
	if _, exists := m.values[name]; exists {
		return errors.New("managed credential already exists")
	}
	stored := make(map[string]string, len(fields))
	for key, value := range fields {
		stored[key] = value
	}
	m.values[name] = stored
	m.creates = append(m.creates, name)
	return nil
}

func (m *managedSecrets) DeleteManaged(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.values[name]; !exists {
		return errs.ErrNotFound
	}
	delete(m.values, name)
	m.deletes = append(m.deletes, name)
	return nil
}

func (m *managedSecrets) apiKey(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.values[name]["api_key"]
}

func (m *managedSecrets) createCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.creates)
}

func (m *managedSecrets) storedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.values)
}

func (m *managedSecrets) deleteCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.deletes)
}

type mapHostDevices map[uint64]uint64

func (m mapHostDevices) LookupHostDevice(_ context.Context, edgeID uint64) (uint64, error) {
	deviceID := m[edgeID]
	if deviceID == 0 {
		return 0, errs.ErrNotFound
	}
	return deviceID, nil
}

type fixedEdgeInventory []bizlogs.RolloutEdge

func (i fixedEdgeInventory) ListRolloutEdges(context.Context) ([]bizlogs.RolloutEdge, error) {
	return append([]bizlogs.RolloutEdge(nil), i...), nil
}

type histogramCountingSearcher struct {
	mu                sync.Mutex
	countRequests     []logquery.SearchRequest
	histogramRequests []logquery.SearchRequest
	intervals         []time.Duration
}

type failingSaveRepo struct {
	bizlogs.Repo
}

func (failingSaveRepo) SaveBackend(context.Context, *logsmodel.Backend) error {
	return errors.New("injected backend save failure")
}

type echoProbeSearcher struct {
	mu       sync.Mutex
	requests []logquery.SearchRequest
}

func (s *echoProbeSearcher) Search(_ context.Context, req logquery.SearchRequest) (*logquery.SearchResult, error) {
	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.mu.Unlock()
	message := ""
	if len(req.Keywords.Include) > 0 {
		message = req.Keywords.Include[0]
	}
	return &logquery.SearchResult{Records: []logquery.Record{{ID: "loki-probe", Timestamp: req.End, Message: message, Backend: "loki"}}}, nil
}

func (*echoProbeSearcher) Count(context.Context, logquery.SearchRequest) (uint64, error) {
	return 0, nil
}

func (*echoProbeSearcher) Fields(context.Context, time.Time, time.Time, logquery.Scope) ([]logquery.Field, error) {
	return logquery.AllowedFields(), nil
}

func (*echoProbeSearcher) FieldValues(context.Context, logquery.FieldValuesRequest) ([]string, error) {
	return nil, nil
}

func (*echoProbeSearcher) Histogram(context.Context, logquery.SearchRequest, time.Duration) ([]logquery.HistogramBucket, error) {
	return nil, nil
}

func (s *histogramCountingSearcher) Search(context.Context, logquery.SearchRequest) (*logquery.SearchResult, error) {
	return &logquery.SearchResult{}, nil
}

func (s *histogramCountingSearcher) Count(_ context.Context, req logquery.SearchRequest) (uint64, error) {
	s.mu.Lock()
	s.countRequests = append(s.countRequests, req)
	s.mu.Unlock()
	return 0, errors.New("backend Count must not be used for a histogram")
}

func (s *histogramCountingSearcher) Fields(context.Context, time.Time, time.Time, logquery.Scope) ([]logquery.Field, error) {
	return logquery.AllowedFields(), nil
}

func (s *histogramCountingSearcher) FieldValues(context.Context, logquery.FieldValuesRequest) ([]string, error) {
	return nil, nil
}

func (s *histogramCountingSearcher) Histogram(_ context.Context, req logquery.SearchRequest, interval time.Duration) ([]logquery.HistogramBucket, error) {
	s.mu.Lock()
	s.histogramRequests = append(s.histogramRequests, req)
	s.intervals = append(s.intervals, interval)
	s.mu.Unlock()
	return []logquery.HistogramBucket{
		{Start: req.Start, Count: 120},
		{Start: req.Start.Add(interval), Count: 120},
		{Start: req.Start.Add(2 * interval), Count: 90},
	}, nil
}

func (s *histogramCountingSearcher) snapshot() ([]logquery.SearchRequest, []logquery.SearchRequest, []time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]logquery.SearchRequest(nil), s.countRequests...),
		append([]logquery.SearchRequest(nil), s.histogramRequests...),
		append([]time.Duration(nil), s.intervals...)
}

type countingNotifier struct {
	mu    sync.Mutex
	calls int
}

func (n *countingNotifier) NotifyLogsBackendChanged(context.Context) error {
	n.mu.Lock()
	n.calls++
	n.mu.Unlock()
	return nil
}

func (n *countingNotifier) count() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.calls
}

func TestServiceRevisionActivationSecretAndRollback(t *testing.T) {
	db := openTestDB(t)
	repo := logsstore.NewRepo(db)

	var authMu sync.Mutex
	seenAuth := map[string]int{}
	probePattern := regexp.MustCompile(`ongrid-log-probe-[A-Za-z0-9_-]+`)
	es := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authMu.Lock()
		seenAuth[r.Header.Get("Authorization")]++
		authMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/":
			_ = json.NewEncoder(w).Encode(map[string]any{"version": map[string]string{"number": "8.16.3"}})
		case r.Method == http.MethodPost && r.URL.Path == "/_security/user/_has_privileges":
			_ = json.NewEncoder(w).Encode(map[string]any{"has_all_requested": true})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/_pit"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "probe-pit"})
		case r.Method == http.MethodPost && r.URL.Path == "/_search":
			body, _ := io.ReadAll(r.Body)
			deviceID := "9001"
			if strings.Contains(string(body), `"resource.attributes.device_id":["9002"]`) {
				deviceID = "9002"
			} else if !strings.Contains(string(body), `"resource.attributes.device_id":["9001"]`) {
				t.Errorf("probe query does not use resolved device_id: %s", body)
			}
			probeID := probePattern.FindString(string(body))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"pit_id": "probe-pit",
				"hits": map[string]any{"hits": []any{map[string]any{
					"_id": "probe-record", "sort": []any{"2026-08-18T12:00:00Z", 1},
					"_source": map[string]any{
						"@timestamp": "2026-08-18T12:00:00Z", "body": map[string]any{"text": probeID},
						"resource": map[string]any{"attributes": map[string]any{"device_id": deviceID}},
					},
				}}},
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/_pit":
			_ = json.NewEncoder(w).Encode(map[string]any{"succeeded": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer es.Close()

	secrets := mapSecrets{
		"es-write": {"api_key": "write-key"},
		"es-query": {"api_key": "query-key"},
	}
	loki := &echoProbeSearcher{}
	svc := bizlogs.NewService(repo, secrets, loki)
	svc.SetHostDeviceResolver(mapHostDevices{42: 9001, 43: 9002})
	svc.SetRolloutEdgeInventory(fixedEdgeInventory{{EdgeID: 42, Online: true}, {EdgeID: 43, Online: true}})
	notifier := &countingNotifier{}
	svc.SetRolloutNotifier(notifier)

	first, err := svc.SaveDraft(context.Background(), bizlogs.SaveInput{
		WriteEndpoints:     []string{es.URL},
		QueryEndpoint:      es.URL,
		Dataset:            "ongrid.system",
		Namespace:          "prod",
		WriteCredentialRef: "es-write",
		QueryCredentialRef: "es-query",
		TLSInsecure:        true,
	})
	if err != nil {
		t.Fatalf("SaveDraft(first): %v", err)
	}
	if first.Generation != 1 || first.Status != logsmodel.BackendStatusDraft {
		t.Fatalf("first draft = generation %d status %q", first.Generation, first.Status)
	}

	distributing, err := svc.Activate(context.Background(), first.ID, bizlogs.ActivationInput{EdgeIDs: []uint64{42}, Canary: true})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if distributing.Status != logsmodel.BackendStatusDistributing || distributing.DetectedVersion != "8.16.3" || distributing.CutoverAt != nil {
		t.Fatalf("distributing view = %+v", distributing)
	}
	if notifier.count() != 1 {
		t.Fatalf("notifications after distribution = %d, want 1", notifier.count())
	}
	authMu.Lock()
	if seenAuth["ApiKey query-key"] == 0 || seenAuth["ApiKey write-key"] == 0 {
		t.Fatalf("probe auth headers = %#v", seenAuth)
	}
	authMu.Unlock()
	if len(distributing.Assignments) != 1 || distributing.Assignments[0].ProbeID == "" {
		t.Fatalf("rollout assignments = %+v", distributing.Assignments)
	}
	overlay, err := svc.PluginRuntimeOverlay(context.Background(), 42, "logs")
	if err != nil {
		t.Fatalf("PluginRuntimeOverlay: %v", err)
	}
	if overlay["rollout_shadow"] != true || overlay["baseline_backend"] != "builtin_loki" {
		t.Fatalf("initial rollout overlay = %#v", overlay)
	}
	probeID := distributing.Assignments[0].ProbeID

	secret, err := svc.PluginSecretForEdge(context.Background(), 42, "logs", bizlogs.SecretSlotESAPIKey, 1)
	if err != nil {
		t.Fatalf("PluginSecretForEdge: %v", err)
	}
	wantHash := sha256.Sum256([]byte("write-key"))
	if secret.Content != "write-key" || secret.SHA256 != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("plugin secret metadata = %+v", secret)
	}
	if err := svc.MarkApplied(context.Background(), 42, 1, probeID, ""); err != nil {
		t.Fatalf("MarkApplied: %v", err)
	}
	canary, err := svc.Get(context.Background())
	if err != nil {
		t.Fatalf("Get(canary): %v", err)
	}
	if canary.Status != logsmodel.BackendStatusVerifying || canary.CutoverAt != nil || notifier.count() != 1 {
		t.Fatalf("canary after real probe = %+v notifications=%d", canary, notifier.count())
	}

	// Fleet promotion first expands the shadow rollout to every currently
	// online Edge. The global cutover remains unset until the second real-path
	// probe succeeds; the already-verified canary assignment is preserved.
	fleet, err := svc.Activate(context.Background(), first.ID, bizlogs.ActivationInput{})
	if err != nil {
		t.Fatalf("Activate(fleet): %v", err)
	}
	if fleet.Status != logsmodel.BackendStatusDistributing || fleet.CutoverAt != nil || !fleet.RolloutAutoActivate || len(fleet.Assignments) != 2 || notifier.count() != 2 {
		t.Fatalf("fleet pre-cutover = %+v notifications=%d", fleet, notifier.count())
	}
	var secondProbeID string
	for _, item := range fleet.Assignments {
		if item.EdgeID == 42 && (item.Status != logsmodel.AssignmentStatusVerified || item.LastWriteSuccessAt == nil) {
			t.Fatalf("verified canary was not preserved: %+v", item)
		}
		if item.EdgeID == 43 {
			secondProbeID = item.ProbeID
		}
	}
	if secondProbeID == "" {
		t.Fatalf("fleet assignments missing Edge 43: %+v", fleet.Assignments)
	}
	if _, err := svc.PluginSecretForEdge(context.Background(), 43, "logs", bizlogs.SecretSlotESAPIKey, 1); err != nil {
		t.Fatalf("PluginSecretForEdge(fleet): %v", err)
	}
	if err := svc.MarkApplied(context.Background(), 43, 1, secondProbeID, ""); err != nil {
		t.Fatalf("MarkApplied(fleet): %v", err)
	}
	active, err := svc.Get(context.Background())
	if err != nil {
		t.Fatalf("Get(active): %v", err)
	}
	if active.Status != logsmodel.BackendStatusActive || active.CutoverAt == nil || notifier.count() != 3 {
		t.Fatalf("active after fleet real probes = %+v notifications=%d", active, notifier.count())
	}
	if err := repo.SetRolloutBackendState(context.Background(), first.ID, logsmodel.BackendStatusDegraded, "8.16.3", "late probe", time.Now().UTC()); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("late rollout state update error = %v, want conflict", err)
	}
	stillActive, err := repo.GetBackend(context.Background(), first.ID)
	if err != nil || stillActive.Status != logsmodel.BackendStatusActive {
		t.Fatalf("late acknowledgement changed active backend: %+v, %v", stillActive, err)
	}

	second, err := svc.SaveDraft(context.Background(), bizlogs.SaveInput{
		WriteEndpoints:     []string{es.URL},
		QueryEndpoint:      es.URL,
		Dataset:            "ongrid.application",
		Namespace:          "prod",
		WriteCredentialRef: "es-write",
		QueryCredentialRef: "es-query",
		TLSInsecure:        true,
	})
	if err != nil {
		t.Fatalf("SaveDraft(second): %v", err)
	}
	if second.ID == first.ID || second.Generation != 2 || second.Status != logsmodel.BackendStatusDraft {
		t.Fatalf("second draft = id %d generation %d status %q; first id=%d", second.ID, second.Generation, second.Status, first.ID)
	}
	runtime, err := svc.ActiveRuntime(context.Background())
	if err != nil {
		t.Fatalf("ActiveRuntime while editing next revision: %v", err)
	}
	if runtime == nil || runtime.BackendID != first.ID || runtime.Generation != 1 || runtime.Dataset != "ongrid.system" {
		t.Fatalf("active runtime was overwritten by draft: %+v", runtime)
	}

	// Re-fetching after a restart must not regress an already-applied row.
	if _, err := svc.PluginSecretForEdge(context.Background(), 42, "logs", bizlogs.SecretSlotESAPIKey, 1); err != nil {
		t.Fatalf("PluginSecretForEdge(second): %v", err)
	}
	assignment, err := repo.GetAssignment(context.Background(), first.ID, 42)
	if err != nil {
		t.Fatalf("GetAssignment: %v", err)
	}
	if assignment.Status != logsmodel.AssignmentStatusVerified || assignment.AppliedGeneration != 1 {
		t.Fatalf("assignment regressed after secret refetch: %+v", assignment)
	}
	if assignment.LastWriteSuccessAt == nil {
		t.Fatalf("verified assignment must record a successful real log write")
	}

	rollingBack, err := svc.Rollback(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rollingBack.Status != logsmodel.BackendStatusRollingBack || rollingBack.EndedAt != nil || len(rollingBack.Assignments) != 2 || notifier.count() != 4 {
		t.Fatalf("rollback prewarm = %+v notifications %d", rollingBack, notifier.count())
	}
	rollbackOverlay, err := svc.PluginRuntimeOverlay(context.Background(), 42, "logs")
	if err != nil {
		t.Fatalf("PluginRuntimeOverlay(rollback): %v", err)
	}
	if rollbackOverlay["backend"] != "builtin_loki" || rollbackOverlay["baseline_backend"] != "external_elasticsearch" || rollbackOverlay["rollout_shadow"] != true {
		t.Fatalf("rollback overlay = %#v", rollbackOverlay)
	}
	runtime, err = svc.ActiveRuntime(context.Background())
	if err != nil || runtime == nil || runtime.BackendID != first.ID {
		t.Fatalf("authoritative runtime during rollback = %+v, %v", runtime, err)
	}
	rollbackProbeIDs := map[uint64]string{}
	for _, item := range rollingBack.Assignments {
		rollbackProbeIDs[item.EdgeID] = item.ProbeID
	}
	failedRollback, err := repo.GetAssignment(context.Background(), first.ID, 42)
	if err != nil {
		t.Fatalf("GetAssignment(failed rollback): %v", err)
	}
	failedRollback.Status = logsmodel.AssignmentStatusFailed
	failedRollback.AppliedGeneration = 0
	failedRollback.LastError = "probe log is not visible yet"
	if err := repo.UpsertAssignment(context.Background(), failedRollback); err != nil {
		t.Fatalf("UpsertAssignment(failed rollback): %v", err)
	}
	if _, err := svc.PluginSecretForEdge(context.Background(), 42, "logs", bizlogs.SecretSlotESAPIKey, first.Generation); err != nil {
		t.Fatalf("authoritative secret after failed rollback probe: %v", err)
	}
	failedRollback.Status = logsmodel.AssignmentStatusPending
	failedRollback.LastError = ""
	if err := repo.UpsertAssignment(context.Background(), failedRollback); err != nil {
		t.Fatalf("restore rollback assignment: %v", err)
	}
	for _, edgeID := range []uint64{42, 43} {
		if _, err := svc.PluginSecretForEdge(context.Background(), edgeID, "logs", bizlogs.SecretSlotESAPIKey, first.Generation); err != nil {
			t.Fatalf("PluginSecretForEdge(rollback edge %d): %v", edgeID, err)
		}
		if err := svc.MarkApplied(context.Background(), edgeID, first.Generation, rollbackProbeIDs[edgeID], ""); err != nil {
			t.Fatalf("MarkApplied(rollback edge %d): %v", edgeID, err)
		}
	}
	rolledBack, err := repo.GetBackend(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("GetBackend(rolled back): %v", err)
	}
	if rolledBack.Status != logsmodel.BackendStatusRolledBack || rolledBack.EndedAt == nil || notifier.count() != 5 {
		t.Fatalf("completed rollback = status %q notifications %d", rolledBack.Status, notifier.count())
	}
	runtime, err = svc.ActiveRuntime(context.Background())
	if err != nil {
		t.Fatalf("ActiveRuntime after rollback: %v", err)
	}
	if runtime != nil {
		t.Fatalf("runtime after rollback = %+v, want built-in Loki", runtime)
	}
	idempotent, err := svc.Rollback(context.Background(), first.ID)
	if err != nil {
		t.Fatalf("Rollback(already rolled back): %v", err)
	}
	if idempotent.Status != logsmodel.BackendStatusRolledBack || notifier.count() != 5 {
		t.Fatalf("idempotent rollback = %+v notifications=%d", idempotent, notifier.count())
	}
}

func TestServiceHistogramUsesOneGlobalExactBucketGrid(t *testing.T) {
	repo := logsstore.NewRepo(openTestDB(t))
	searcher := &histogramCountingSearcher{}
	svc := bizlogs.NewService(repo, nil, searcher)
	start := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	end := start.Add(5*time.Minute + 30*time.Second)

	buckets, err := svc.Histogram(context.Background(), logquery.SearchRequest{
		Start: start, End: end, Limit: 1,
	}, 2*time.Minute)
	if err != nil {
		t.Fatalf("Histogram: %v", err)
	}
	if len(buckets) != 3 {
		t.Fatalf("bucket count = %d, want 3: %#v", len(buckets), buckets)
	}
	wantCounts := []uint64{120, 120, 90}
	for i, bucket := range buckets {
		wantStart := start.Add(time.Duration(i) * 2 * time.Minute)
		if !bucket.Start.Equal(wantStart) || bucket.Count != wantCounts[i] {
			t.Fatalf("bucket[%d] = %+v, want start=%s count=%d", i, bucket, wantStart, wantCounts[i])
		}
	}
	countRequests, histogramRequests, intervals := searcher.snapshot()
	if len(countRequests) != 0 {
		t.Fatalf("exact count requests = %d, want 0", len(countRequests))
	}
	if len(histogramRequests) != 1 || len(intervals) != 1 || intervals[0] != 2*time.Minute {
		t.Fatalf("native histogram calls = %d intervals=%v, want one 2m call", len(histogramRequests), intervals)
	}
	if histogramRequests[0].Cursor != "" || !histogramRequests[0].Start.Equal(start) || !histogramRequests[0].End.Equal(end) {
		t.Fatalf("native histogram request = %+v", histogramRequests[0])
	}
	if _, err := svc.Histogram(context.Background(), logquery.SearchRequest{
		Start: start, End: start.Add(501 * time.Second), Limit: 1,
	}, time.Second); err == nil || !strings.Contains(err.Error(), "exceeds 500 buckets") {
		t.Fatalf("oversized histogram error = %v", err)
	}
}

func TestServiceReplacementRolloutShadowsPreviousElasticsearch(t *testing.T) {
	db := openTestDB(t)
	repo := logsstore.NewRepo(db)
	es := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/_security/user/_has_privileges" {
			_ = json.NewEncoder(w).Encode(map[string]any{"has_all_requested": true})
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			_ = json.NewEncoder(w).Encode(map[string]any{"version": map[string]string{"number": "8.16.3"}})
			return
		}
		http.NotFound(w, r)
	}))
	defer es.Close()

	svc := bizlogs.NewService(repo, mapSecrets{
		"write-v1": {"api_key": "write-key-v1"},
		"query-v1": {"api_key": "query-key-v1"},
		"write-v2": {"api_key": "write-key-v2"},
		"query-v2": {"api_key": "query-key-v2"},
	}, nil)
	svc.SetHostDeviceResolver(mapHostDevices{42: 9001})
	first, err := svc.SaveDraft(context.Background(), bizlogs.SaveInput{
		WriteEndpoints: []string{es.URL}, QueryEndpoint: es.URL,
		Dataset: "ongrid.host", Namespace: "old",
		WriteCredentialRef: "write-v1", QueryCredentialRef: "query-v1", TLSInsecure: true,
	})
	if err != nil {
		t.Fatalf("SaveDraft(first): %v", err)
	}
	if err := repo.SetBackendState(context.Background(), first.ID, logsmodel.BackendStatusVerifying, "8.16.3", "", time.Now().UTC()); err != nil {
		t.Fatalf("SetBackendState(first): %v", err)
	}
	if err := repo.ActivateBackend(context.Background(), first.ID, "8.16.3", time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatalf("ActivateBackend(first): %v", err)
	}
	second, err := svc.SaveDraft(context.Background(), bizlogs.SaveInput{
		WriteEndpoints: []string{es.URL}, QueryEndpoint: es.URL,
		Dataset: "ongrid.host", Namespace: "new",
		WriteCredentialRef: "write-v2", QueryCredentialRef: "query-v2", TLSInsecure: true,
	})
	if err != nil {
		t.Fatalf("SaveDraft(second): %v", err)
	}
	if _, err := svc.Activate(context.Background(), second.ID, bizlogs.ActivationInput{EdgeIDs: []uint64{42}, Canary: true}); err != nil {
		t.Fatalf("Activate(second canary): %v", err)
	}
	overlay, err := svc.PluginRuntimeOverlay(context.Background(), 42, "logs")
	if err != nil {
		t.Fatalf("PluginRuntimeOverlay: %v", err)
	}
	if overlay["rollout_shadow"] != true || overlay["baseline_backend"] != "external_elasticsearch" {
		t.Fatalf("replacement rollout overlay = %#v", overlay)
	}
	if overlay["backend_generation"] != uint64(2) || overlay["baseline_backend_generation"] != uint64(1) {
		t.Fatalf("replacement generations = %#v", overlay)
	}
	if overlay["baseline_elasticsearch_namespace"] != "old" || overlay["elasticsearch_namespace"] != "new" {
		t.Fatalf("replacement routing = %#v", overlay)
	}
	if _, err := svc.PluginSecretForEdge(context.Background(), 42, "logs", bizlogs.SecretSlotESAPIKey, 2); err != nil {
		t.Fatalf("candidate secret: %v", err)
	}
	if _, err := svc.PluginSecretForEdge(context.Background(), 42, "logs", bizlogs.SecretSlotESAPIKey, 1); err != nil {
		t.Fatalf("authoritative baseline secret: %v", err)
	}
}

func TestServiceTestChecksEveryWriteEndpointWithoutBroadeningWriteKey(t *testing.T) {
	var requestMu sync.Mutex
	newEndpoint := func(requests *[]string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestMu.Lock()
			*requests = append(*requests, r.Header.Get("Authorization")+" "+r.Method+" "+r.URL.Path)
			requestMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/_security/user/_has_privileges":
				_ = json.NewEncoder(w).Encode(map[string]any{"has_all_requested": true})
			case r.Method == http.MethodGet && r.URL.Path == "/":
				_ = json.NewEncoder(w).Encode(map[string]any{"version": map[string]string{"number": "8.16.3"}})
			default:
				http.NotFound(w, r)
			}
		}))
	}
	var firstRequests, secondRequests []string
	firstEndpoint := newEndpoint(&firstRequests)
	defer firstEndpoint.Close()
	secondEndpoint := newEndpoint(&secondRequests)
	defer secondEndpoint.Close()

	svc := bizlogs.NewService(logsstore.NewRepo(openTestDB(t)), mapSecrets{
		"write": {"api_key": "write-key"},
		"query": {"api_key": "query-key"},
	}, nil)
	backend, err := svc.SaveDraft(context.Background(), bizlogs.SaveInput{
		WriteEndpoints: []string{firstEndpoint.URL, secondEndpoint.URL}, QueryEndpoint: firstEndpoint.URL,
		Dataset: "ongrid.host", Namespace: "prod",
		WriteCredentialRef: "write", QueryCredentialRef: "query", TLSInsecure: true,
	})
	if err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	tested, err := svc.Test(context.Background(), backend.ID)
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if tested.Status != logsmodel.BackendStatusDraft || tested.DetectedVersion != "8.16.3" {
		t.Fatalf("tested backend = %+v", tested)
	}

	requestMu.Lock()
	defer requestMu.Unlock()
	wantQueryProbe := false
	wantWritePrivileges := false
	for _, request := range secondRequests {
		switch request {
		case "ApiKey query-key GET /":
			wantQueryProbe = true
		case "ApiKey write-key POST /_security/user/_has_privileges":
			wantWritePrivileges = true
		case "ApiKey write-key GET /":
			t.Fatalf("runtime write key was used for cluster version probe: %#v", secondRequests)
		}
	}
	if !wantQueryProbe || !wantWritePrivileges {
		t.Fatalf("second write endpoint was not fully checked: %#v", secondRequests)
	}
}

func TestServiceStoresDirectAPIKeysAsManagedWriteOnlyCredentials(t *testing.T) {
	secrets := newManagedSecrets()
	svc := bizlogs.NewService(logsstore.NewRepo(openTestDB(t)), secrets, nil)

	first, err := svc.SaveDraft(context.Background(), bizlogs.SaveInput{
		WriteEndpoints: []string{"https://es.example.com"}, QueryEndpoint: "https://es.example.com",
		Dataset: "ongrid.system", Namespace: "prod",
		WriteAPIKey: "ApiKey encoded-write-v1", QueryAPIKey: "encoded-query-v1",
	})
	if err != nil {
		t.Fatalf("SaveDraft(direct keys): %v", err)
	}
	if first.WriteCredentialRef == first.QueryCredentialRef ||
		!strings.HasPrefix(first.WriteCredentialRef, "ongrid-managed-logs-es-write-") ||
		!strings.HasPrefix(first.QueryCredentialRef, "ongrid-managed-logs-es-query-") {
		t.Fatalf("managed refs = write %q query %q", first.WriteCredentialRef, first.QueryCredentialRef)
	}
	if got := secrets.apiKey(first.WriteCredentialRef); got != "encoded-write-v1" {
		t.Fatalf("stored write key = %q", got)
	}
	if got := secrets.apiKey(first.QueryCredentialRef); got != "encoded-query-v1" {
		t.Fatalf("stored query key = %q", got)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal backend view: %v", err)
	}
	if strings.Contains(string(encoded), "encoded-write-v1") || strings.Contains(string(encoded), "encoded-query-v1") {
		t.Fatalf("backend response leaked direct API key: %s", encoded)
	}

	unchanged, err := svc.SaveDraft(context.Background(), bizlogs.SaveInput{
		WriteEndpoints: []string{"https://es.example.com"}, QueryEndpoint: "https://es.example.com",
		Dataset: "ongrid.application", Namespace: "prod",
	})
	if err != nil {
		t.Fatalf("SaveDraft(blank write-only fields): %v", err)
	}
	if unchanged.WriteCredentialRef != first.WriteCredentialRef || unchanged.QueryCredentialRef != first.QueryCredentialRef {
		t.Fatalf("blank write-only fields replaced refs: first=%+v unchanged=%+v", first, unchanged)
	}
	if secrets.createCount() != 2 {
		t.Fatalf("blank write-only fields performed %d creates, want 2 total", secrets.createCount())
	}

	rotated, err := svc.SaveDraft(context.Background(), bizlogs.SaveInput{
		WriteEndpoints: []string{"https://es.example.com"}, QueryEndpoint: "https://es.example.com",
		Dataset: "ongrid.application", Namespace: "prod", WriteAPIKey: "encoded-write-v2",
	})
	if err != nil {
		t.Fatalf("SaveDraft(rotate write key): %v", err)
	}
	if rotated.WriteCredentialRef == first.WriteCredentialRef || rotated.QueryCredentialRef != first.QueryCredentialRef {
		t.Fatalf("draft rotation did not isolate the new write ref: first=%+v rotated=%+v", first, rotated)
	}
	if got := secrets.apiKey(rotated.WriteCredentialRef); got != "encoded-write-v2" {
		t.Fatalf("rotated write key = %q", got)
	}
	if got := secrets.apiKey(first.WriteCredentialRef); got != "encoded-write-v1" {
		t.Fatalf("rotation mutated the previously referenced write key = %q", got)
	}
}

func TestServiceCanReuseWriteAPIKeyWithoutSharingCredentialReference(t *testing.T) {
	secrets := newManagedSecrets()
	svc := bizlogs.NewService(logsstore.NewRepo(openTestDB(t)), secrets, nil)

	backend, err := svc.SaveDraft(context.Background(), bizlogs.SaveInput{
		WriteEndpoints: []string{"https://es.example.com"}, QueryEndpoint: "https://es.example.com",
		Dataset: "ongrid.system", WriteAPIKey: "encoded-shared", ReuseWriteAPIKey: true,
	})
	if err != nil {
		t.Fatalf("SaveDraft(reuse write key): %v", err)
	}
	if backend.WriteCredentialRef == backend.QueryCredentialRef {
		t.Fatalf("reuse mode shared a credential reference: %+v", backend)
	}
	if write, query := secrets.apiKey(backend.WriteCredentialRef), secrets.apiKey(backend.QueryCredentialRef); write != "encoded-shared" || query != write {
		t.Fatalf("stored reuse keys = write %q query %q", write, query)
	}
}

func TestServiceRejectsSharedDirectKeyBeforeWritingManagedCredentials(t *testing.T) {
	secrets := newManagedSecrets()
	svc := bizlogs.NewService(logsstore.NewRepo(openTestDB(t)), secrets, nil)

	_, err := svc.SaveDraft(context.Background(), bizlogs.SaveInput{
		WriteEndpoints: []string{"https://es.example.com"}, QueryEndpoint: "https://es.example.com",
		Dataset: "ongrid.system", WriteAPIKey: "encoded-shared", QueryAPIKey: "encoded-shared",
	})
	if err == nil || !strings.Contains(err.Error(), "reuse_write_api_key") {
		t.Fatalf("SaveDraft error = %v, want explicit reuse-mode validation", err)
	}
	if secrets.createCount() != 0 {
		t.Fatalf("rejected request created %d managed credentials", secrets.createCount())
	}
}

func TestServiceDirectAPIKeyRequiresEncryptedManagedStore(t *testing.T) {
	svc := bizlogs.NewService(logsstore.NewRepo(openTestDB(t)), mapSecrets{}, nil)
	_, err := svc.SaveDraft(context.Background(), bizlogs.SaveInput{
		WriteEndpoints: []string{"https://es.example.com"}, QueryEndpoint: "https://es.example.com",
		Dataset: "ongrid.system", WriteAPIKey: "encoded-write", QueryAPIKey: "encoded-query",
	})
	if err == nil || !strings.Contains(err.Error(), "credential storage is unavailable") {
		t.Fatalf("SaveDraft error = %v, want managed storage failure", err)
	}
}

func TestServiceSaveDraftCleansManagedCredentialsWhenCreateFails(t *testing.T) {
	secrets := newManagedSecrets()
	secrets.failCreateAt = 2
	svc := bizlogs.NewService(logsstore.NewRepo(openTestDB(t)), secrets, nil)

	_, err := svc.SaveDraft(t.Context(), bizlogs.SaveInput{
		WriteEndpoints: []string{"https://es.example.com"}, QueryEndpoint: "https://es.example.com",
		Dataset: "ongrid.system", WriteAPIKey: "encoded-write", QueryAPIKey: "encoded-query",
	})
	if err == nil || !strings.Contains(err.Error(), "injected managed credential create failure") {
		t.Fatalf("SaveDraft error = %v, want injected create failure", err)
	}
	if secrets.storedCount() != 0 || secrets.deleteCount() != 1 {
		t.Fatalf("managed credentials after failed create: stored=%d deleted=%d", secrets.storedCount(), secrets.deleteCount())
	}
}

func TestServiceSaveDraftCleansManagedCredentialsWhenBackendSaveFails(t *testing.T) {
	secrets := newManagedSecrets()
	repo := failingSaveRepo{Repo: logsstore.NewRepo(openTestDB(t))}
	svc := bizlogs.NewService(repo, secrets, nil)

	_, err := svc.SaveDraft(t.Context(), bizlogs.SaveInput{
		WriteEndpoints: []string{"https://es.example.com"}, QueryEndpoint: "https://es.example.com",
		Dataset: "ongrid.system", WriteAPIKey: "encoded-write", QueryAPIKey: "encoded-query",
	})
	if err == nil || !strings.Contains(err.Error(), "injected backend save failure") {
		t.Fatalf("SaveDraft error = %v, want injected save failure", err)
	}
	if secrets.storedCount() != 0 || secrets.deleteCount() != 2 {
		t.Fatalf("managed credentials after failed save: stored=%d deleted=%d", secrets.storedCount(), secrets.deleteCount())
	}
}

func TestServiceRejectsUnsafeBackendInput(t *testing.T) {
	db := openTestDB(t)
	svc := bizlogs.NewService(logsstore.NewRepo(db), mapSecrets{
		"shared": {"api_key": "key"},
		"query":  {"api_key": "query"},
	}, nil)

	tests := []struct {
		name string
		in   bizlogs.SaveInput
	}{
		{
			name: "plain HTTP without explicit compatibility switch",
			in: bizlogs.SaveInput{WriteEndpoints: []string{"http://es.example"}, QueryEndpoint: "http://es.example",
				Dataset: "ongrid.system", WriteCredentialRef: "shared", QueryCredentialRef: "query"},
		},
		{
			name: "same write and query credential",
			in: bizlogs.SaveInput{WriteEndpoints: []string{"https://es.example"}, QueryEndpoint: "https://es.example",
				Dataset: "ongrid.system", WriteCredentialRef: "shared", QueryCredentialRef: "shared"},
		},
		{
			name: "dataset outside product namespace",
			in: bizlogs.SaveInput{WriteEndpoints: []string{"https://es.example"}, QueryEndpoint: "https://es.example",
				Dataset: "customer-arbitrary", WriteCredentialRef: "shared", QueryCredentialRef: "query"},
		},
		{
			name: "direct API key containing whitespace",
			in: bizlogs.SaveInput{WriteEndpoints: []string{"https://es.example"}, QueryEndpoint: "https://es.example",
				Dataset: "ongrid.system", WriteAPIKey: "encoded write", QueryAPIKey: "encoded-query"},
		},
		{
			name: "query API key supplied in reuse mode",
			in: bizlogs.SaveInput{WriteEndpoints: []string{"https://es.example"}, QueryEndpoint: "https://es.example",
				Dataset: "ongrid.system", WriteAPIKey: "encoded-write", QueryAPIKey: "encoded-query", ReuseWriteAPIKey: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := svc.SaveDraft(context.Background(), tt.in); err == nil {
				t.Fatal("SaveDraft succeeded, want validation error")
			}
		})
	}
}

func TestServiceActivationGuardBlocksFullCutover(t *testing.T) {
	db := openTestDB(t)
	svc := bizlogs.NewService(logsstore.NewRepo(db), mapSecrets{
		"write": {"api_key": "write-key"},
		"query": {"api_key": "query-key"},
	}, nil)
	backend, err := svc.SaveDraft(context.Background(), bizlogs.SaveInput{
		WriteEndpoints: []string{"https://es.example.com"}, QueryEndpoint: "https://es.example.com",
		Dataset: "ongrid.system", WriteCredentialRef: "write", QueryCredentialRef: "query",
	})
	if err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	svc.SetActivationGuard(func(context.Context) error { return errors.New("legacy-log-rule") })
	if _, err := svc.Activate(context.Background(), backend.ID, bizlogs.ActivationInput{EdgeIDs: []uint64{42}}); !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("Activate error = %v, want conflict", err)
	}
	latest, err := svc.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if latest.Status != logsmodel.BackendStatusDraft {
		t.Fatalf("status = %q, want draft", latest.Status)
	}
}

func TestServiceFleetCutoverRejectsOfflineLogEnabledEdge(t *testing.T) {
	repo := logsstore.NewRepo(openTestDB(t))
	svc := bizlogs.NewService(repo, mapSecrets{
		"write": {"api_key": "write-key"},
		"query": {"api_key": "query-key"},
	}, nil)
	svc.SetRolloutEdgeInventory(fixedEdgeInventory{
		{EdgeID: 42, Online: true},
		{EdgeID: 43, Online: false},
	})
	backend, err := svc.SaveDraft(context.Background(), bizlogs.SaveInput{
		WriteEndpoints: []string{"https://es.example.com"}, QueryEndpoint: "https://es.example.com",
		Dataset: "ongrid.system", WriteCredentialRef: "write", QueryCredentialRef: "query",
	})
	if err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if _, err := svc.Activate(context.Background(), backend.ID, bizlogs.ActivationInput{}); !errors.Is(err, errs.ErrConflict) || !strings.Contains(err.Error(), "43") {
		t.Fatalf("Activate error = %v, want offline Edge conflict", err)
	}
	latest, err := repo.GetBackend(context.Background(), backend.ID)
	if err != nil || latest.Status != logsmodel.BackendStatusDraft {
		t.Fatalf("offline fleet check mutated backend: %+v, %v", latest, err)
	}
}

func TestServiceCancelledDraftDoesNotHideActiveBackend(t *testing.T) {
	repo := logsstore.NewRepo(openTestDB(t))
	svc := bizlogs.NewService(repo, mapSecrets{
		"write": {"api_key": "write-key"},
		"query": {"api_key": "query-key"},
	}, nil)
	first, err := svc.SaveDraft(context.Background(), bizlogs.SaveInput{
		WriteEndpoints: []string{"https://es.example.com"}, QueryEndpoint: "https://es.example.com",
		Dataset: "ongrid.system", Namespace: "old", WriteCredentialRef: "write", QueryCredentialRef: "query",
	})
	if err != nil {
		t.Fatalf("SaveDraft(first): %v", err)
	}
	if err := repo.SetBackendState(context.Background(), first.ID, logsmodel.BackendStatusVerifying, "8.16.3", "", time.Now().UTC()); err != nil {
		t.Fatalf("SetBackendState(first): %v", err)
	}
	if err := repo.ActivateBackend(context.Background(), first.ID, "8.16.3", time.Now().UTC()); err != nil {
		t.Fatalf("ActivateBackend(first): %v", err)
	}
	second, err := svc.SaveDraft(context.Background(), bizlogs.SaveInput{
		WriteEndpoints: []string{"https://es.example.com"}, QueryEndpoint: "https://es.example.com",
		Dataset: "ongrid.system", Namespace: "new", WriteCredentialRef: "write", QueryCredentialRef: "query",
	})
	if err != nil {
		t.Fatalf("SaveDraft(second): %v", err)
	}
	if second.CurrentBackend != "elasticsearch" || second.CurrentBackendID != first.ID {
		t.Fatalf("SaveDraft(second) current backend = %q #%d, want elasticsearch #%d", second.CurrentBackend, second.CurrentBackendID, first.ID)
	}
	if _, err := svc.Rollback(context.Background(), second.ID); err != nil {
		t.Fatalf("Rollback(cancel draft): %v", err)
	}
	visible, err := svc.Get(context.Background())
	if err != nil || visible.ID != first.ID || visible.Status != logsmodel.BackendStatusActive || visible.CurrentBackend != "elasticsearch" || visible.CurrentBackendID != first.ID {
		t.Fatalf("Get after draft cancellation = %+v, %v", visible, err)
	}
}

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("sqlite handle: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := logsstore.Migrate(db); err != nil {
		t.Fatalf("migrate logs store: %v", err)
	}
	return db
}
