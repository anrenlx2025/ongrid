// Package logs owns the external log backend control plane and active-backend
// query routing. It never receives log payloads from Edge collectors.
package logs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	model "github.com/ongridio/ongrid/internal/manager/model/logs"
	"github.com/ongridio/ongrid/internal/pkg/errs"
	pkggrafana "github.com/ongridio/ongrid/internal/pkg/grafana"
	"github.com/ongridio/ongrid/internal/pkg/logquery"
)

const (
	DefaultBackendName      = "external-elasticsearch"
	SecretSlotESAPIKey      = "elasticsearch_api_key"
	managedLogsSecretPrefix = "ongrid-managed-logs-es-"
	elasticsearchCredType   = "elasticsearch"
	maxAPIKeyBytes          = 16 << 10
	maxCAPEMBytes           = 256 << 10
	maxBackendEndpoints     = 8
	managedSecretCleanupTTL = 5 * time.Second
)

var (
	datasetRE   = regexp.MustCompile(`^ongrid\.[a-z0-9][a-z0-9._-]{0,91}$`)
	namespaceRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,99}$`)
)

type Repo interface {
	SaveBackend(ctx context.Context, backend *model.Backend) error
	GetBackend(ctx context.Context, id uint64) (*model.Backend, error)
	LatestBackend(ctx context.Context) (*model.Backend, error)
	ActiveBackend(ctx context.Context) (*model.Backend, error)
	BeginRollout(ctx context.Context, backend *model.Backend, assignments []*model.BackendAssignment) error
	BeginRollback(ctx context.Context, backend *model.Backend, assignments []*model.BackendAssignment) error
	ActivateBackend(ctx context.Context, id uint64, version string, cutover time.Time) error
	CompleteRollback(ctx context.Context, id uint64, endedAt time.Time) error
	CancelBackend(ctx context.Context, id uint64) error
	SetBackendState(ctx context.Context, id uint64, status model.BackendStatus, version, lastError string, testedAt time.Time) error
	SetRolloutBackendState(ctx context.Context, id uint64, status model.BackendStatus, version, lastError string, testedAt time.Time) error
	GetAssignment(ctx context.Context, backendID, edgeID uint64) (*model.BackendAssignment, error)
	UpsertAssignment(ctx context.Context, assignment *model.BackendAssignment) error
	ListAssignments(ctx context.Context, backendID uint64) ([]*model.BackendAssignment, error)
}

// SecretResolver is deliberately read-only. Credential values are decrypted
// only while constructing an Elasticsearch client or answering the dedicated
// authenticated plugin-secret RPC.
type SecretResolver interface {
	ResolveFields(ctx context.Context, name string) (map[string]string, error)
}

// ManagedSecretStore is implemented by Manager's encrypted credential vault.
// It is intentionally an in-process capability rather than an HTTP API: direct
// API keys submitted with a log backend are write-only request fields and are
// converted to ordinary credential references before the backend is persisted.
type ManagedSecretStore interface {
	SecretResolver
	CreateManaged(ctx context.Context, name, credType, description string, fields map[string]string) error
	DeleteManaged(ctx context.Context, name string) error
}

type RolloutNotifier interface {
	NotifyLogsBackendChanged(ctx context.Context) error
}

// GrafanaSyncer is an optional, best-effort observer of authoritative log
// backend changes. Grafana failures must never roll back a verified Edge data
// path cutover.
type GrafanaSyncer interface {
	SyncElasticsearch(ctx context.Context, config pkggrafana.ElasticsearchDatasourceConfig) error
	SyncLoki(ctx context.Context) error
}

type HostDeviceResolver interface {
	LookupHostDevice(ctx context.Context, edgeID uint64) (uint64, error)
}

type RolloutEdge struct {
	EdgeID uint64
	Online bool
}

// RolloutEdgeInventory returns every Edge on which logs are enabled, including
// disconnected identities. A global timeline cannot safely move while one of
// those Edges may still be writing the previous backend.
type RolloutEdgeInventory interface {
	ListRolloutEdges(ctx context.Context) ([]RolloutEdge, error)
}

type SaveInput struct {
	Name               string   `json:"name"`
	WriteEndpoints     []string `json:"write_endpoints"`
	QueryEndpoint      string   `json:"query_endpoint"`
	Dataset            string   `json:"dataset"`
	Namespace          string   `json:"namespace"`
	WriteCredentialRef string   `json:"write_credential_ref,omitempty"`
	QueryCredentialRef string   `json:"query_credential_ref,omitempty"`
	WriteAPIKey        string   `json:"write_api_key,omitempty"`
	QueryAPIKey        string   `json:"query_api_key,omitempty"`
	ReuseWriteAPIKey   bool     `json:"reuse_write_api_key,omitempty"`
	CAPEM              string   `json:"ca_pem,omitempty"`
	PreserveCA         bool     `json:"preserve_ca,omitempty"`
	KibanaURL          string   `json:"kibana_url,omitempty"`
	TLSInsecure        bool     `json:"tls_insecure"`
}

type BackendView struct {
	ID                 uint64                     `json:"id"`
	Name               string                     `json:"name"`
	Type               model.BackendType          `json:"type"`
	CurrentBackend     string                     `json:"current_backend"`
	CurrentBackendID   uint64                     `json:"current_backend_id,omitempty"`
	Status             model.BackendStatus        `json:"status"`
	Generation         uint64                     `json:"generation"`
	WriteEndpoints     []string                   `json:"write_endpoints"`
	QueryEndpoint      string                     `json:"query_endpoint"`
	Dataset            string                     `json:"dataset"`
	Namespace          string                     `json:"namespace"`
	IndexPattern       string                     `json:"index_pattern"`
	WriteCredentialRef string                     `json:"write_credential_ref"`
	QueryCredentialRef string                     `json:"query_credential_ref"`
	HasCustomCA        bool                       `json:"has_custom_ca"`
	KibanaURL          string                     `json:"kibana_url,omitempty"`
	TLSInsecure        bool                       `json:"tls_insecure"`
	DetectedVersion    string                     `json:"detected_version,omitempty"`
	CutoverAt          *time.Time                 `json:"cutover_at,omitempty"`
	EndedAt            *time.Time                 `json:"ended_at,omitempty"`
	LastTestAt         *time.Time                 `json:"last_test_at,omitempty"`
	LastError          string                     `json:"last_error,omitempty"`
	Assignments        []*model.BackendAssignment `json:"assignments,omitempty"`
	CreatedAt          time.Time                  `json:"created_at"`
	UpdatedAt          time.Time                  `json:"updated_at"`
}

type BackendTestResult struct {
	Status          string    `json:"status"`
	DetectedVersion string    `json:"detected_version"`
	TestedAt        time.Time `json:"tested_at"`
}

// RuntimeConfig is the non-sensitive overlay merged into the Edge logs plugin
// snapshot. APIKeyFile is added locally by Edge after secret materialization.
type RuntimeConfig struct {
	BackendID      uint64
	Backend        string
	Generation     uint64
	WriteEndpoints []string
	Dataset        string
	Namespace      string
	CAPEM          string
	TLSInsecure    bool
}

type PluginSecret struct {
	Generation uint64 `json:"generation"`
	Content    string `json:"content"`
	SHA256     string `json:"sha256"`
}

type Service struct {
	repo    Repo
	secrets SecretResolver
	loki    logquery.Searcher
	log     *slog.Logger

	lifecycleMu sync.Mutex
	mu          sync.RWMutex
	notifier    RolloutNotifier
	grafana     GrafanaSyncer
	devices     HostDeviceResolver
	inventory   RolloutEdgeInventory
	applyGuard  func(context.Context) error
	cacheKey    string
	cachedES    *logquery.ElasticsearchClient
}

func NewService(repo Repo, secrets SecretResolver, loki logquery.Searcher, loggers ...*slog.Logger) *Service {
	logger := slog.Default()
	if len(loggers) > 0 && loggers[0] != nil {
		logger = loggers[0]
	}
	return &Service{repo: repo, secrets: secrets, loki: loki, log: logger}
}

func (s *Service) SetRolloutNotifier(notifier RolloutNotifier) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifier = notifier
}

func (s *Service) SetGrafanaSyncer(syncer GrafanaSyncer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grafana = syncer
}

func (s *Service) SetHostDeviceResolver(resolver HostDeviceResolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devices = resolver
}

func (s *Service) SetRolloutEdgeInventory(inventory RolloutEdgeInventory) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inventory = inventory
}

// SetApplyGuard installs product-level preconditions that must pass before
// applying a saved backend configuration to the full Edge fleet.
func (s *Service) SetApplyGuard(guard func(context.Context) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyGuard = guard
}

func (s *Service) Save(ctx context.Context, input SaveInput) (view *BackendView, retErr error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	createdManagedRefs := make([]string, 0, 2)
	backendPersisted := false
	defer func() {
		if retErr == nil || backendPersisted || len(createdManagedRefs) == 0 {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), managedSecretCleanupTTL)
		defer cancel()
		if cleanupErr := s.deleteManagedAPIKeys(cleanupCtx, createdManagedRefs); cleanupErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("cleanup managed Elasticsearch credentials: %w", cleanupErr))
		}
	}()

	normalized, err := normalizeSaveInput(input)
	if err != nil {
		return nil, err
	}
	previous, loadErr := s.repo.LatestBackend(ctx)
	if loadErr != nil && !errors.Is(loadErr, errs.ErrNotFound) {
		return nil, loadErr
	}
	if loadErr == nil && (previous.Status == model.BackendStatusDistributing || previous.Status == model.BackendStatusVerifying) {
		return nil, fmt.Errorf("%w: wait for the current Elasticsearch apply operation to finish", errs.ErrConflict)
	}
	active, activeErr := s.repo.ActiveBackend(ctx)
	if activeErr != nil && !errors.Is(activeErr, errs.ErrNotFound) {
		return nil, activeErr
	}
	if activeErr == nil && active.Status == model.BackendStatusRollingBack {
		return nil, fmt.Errorf("%w: wait for the Loki apply operation to finish", errs.ErrConflict)
	}

	generation := uint64(1)
	editableInPlace := false
	if loadErr == nil {
		generation = previous.Generation
		switch previous.Status {
		case model.BackendStatusSaved, model.BackendStatusDegraded:
			// A saved configuration that has not been applied remains editable
			// in place. The active revision keeps serving reads and writes.
			editableInPlace = true
		default:
			generation++
		}
	}

	writeRef := normalized.WriteCredentialRef
	queryRef := normalized.QueryCredentialRef
	if loadErr == nil {
		// Direct key fields are write-only. An empty value means "keep the
		// current key", so clients never need to fetch plaintext to edit the
		// non-sensitive parts of an existing backend.
		if writeRef == "" {
			writeRef = previous.WriteCredentialRef
		}
		if queryRef == "" {
			queryRef = previous.QueryCredentialRef
		}
	}
	writeKey := normalized.WriteAPIKey
	if writeKey == "" {
		if writeRef == "" {
			return nil, fmt.Errorf("%w: write API key or credential ref is required", errs.ErrInvalid)
		}
		writeKey, err = s.apiKey(ctx, writeRef)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid write credential", errs.ErrInvalid)
		}
	}
	queryKey := normalized.QueryAPIKey
	if normalized.ReuseWriteAPIKey {
		queryKey = writeKey
	} else if queryKey == "" {
		if queryRef == "" {
			return nil, fmt.Errorf("%w: query API key or credential ref is required", errs.ErrInvalid)
		}
		queryKey, err = s.apiKey(ctx, queryRef)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid query credential", errs.ErrInvalid)
		}
	}
	// Resolve and compare every effective key before mutating a managed vault
	// row. A rejected request must not rotate a credential still referenced by
	// the currently saved configuration.
	if !normalized.ReuseWriteAPIKey && subtle.ConstantTimeCompare([]byte(writeKey), []byte(queryKey)) == 1 {
		return nil, fmt.Errorf("%w: write and query API keys must differ unless reuse_write_api_key is enabled", errs.ErrInvalid)
	}

	if normalized.WriteAPIKey != "" {
		writeRef, err = s.storeManagedAPIKey(ctx, "write", writeKey, generation)
		if err != nil {
			return nil, err
		}
		createdManagedRefs = append(createdManagedRefs, writeRef)
	}
	if normalized.ReuseWriteAPIKey || normalized.QueryAPIKey != "" {
		queryRef, err = s.storeManagedAPIKey(ctx, "query", queryKey, generation)
		if err != nil {
			return nil, err
		}
		createdManagedRefs = append(createdManagedRefs, queryRef)
	}
	if writeRef == queryRef {
		return nil, fmt.Errorf("%w: write and query credentials must be different", errs.ErrInvalid)
	}

	endpointsJSON, err := json.Marshal(normalized.WriteEndpoints)
	if err != nil {
		return nil, fmt.Errorf("encode Elasticsearch endpoints: %w", err)
	}
	backend := &model.Backend{
		Name:               normalized.Name,
		Type:               model.BackendTypeElasticsearch,
		Status:             model.BackendStatusSaved,
		Generation:         generation,
		WriteEndpointsJSON: string(endpointsJSON),
		QueryEndpoint:      normalized.QueryEndpoint,
		Dataset:            normalized.Dataset,
		Namespace:          normalized.Namespace,
		IndexPattern:       indexPattern(normalized.Namespace),
		WriteCredentialRef: writeRef,
		QueryCredentialRef: queryRef,
		CAPEM:              normalized.CAPEM,
		KibanaURL:          normalized.KibanaURL,
		TLSInsecure:        normalized.TLSInsecure,
	}
	if normalized.PreserveCA && normalized.CAPEM == "" {
		if loadErr != nil {
			if errors.Is(loadErr, errs.ErrNotFound) {
				return nil, fmt.Errorf("%w: cannot preserve CA without an existing backend", errs.ErrInvalid)
			}
			return nil, loadErr
		}
		backend.CAPEM = previous.CAPEM
	}
	if editableInPlace {
		backend.ID = previous.ID
		backend.CreatedAt = previous.CreatedAt
	}
	if err := s.repo.SaveBackend(ctx, backend); err != nil {
		return nil, err
	}
	backendPersisted = true
	if editableInPlace {
		s.cleanupSupersededManagedAPIKeys(ctx, previous, backend)
	}
	s.invalidateCache()
	return s.view(ctx, backend)
}

func (s *Service) Get(ctx context.Context) (*BackendView, error) {
	backend, err := s.repo.LatestBackend(ctx)
	if err != nil {
		return nil, err
	}
	// A cancelled configuration must not hide the still-authoritative generation
	// in the single-backend settings API. Prefer a live apply or saved config; otherwise
	// surface the active (or rolling-back) backend before historical rows.
	if backend.Status == model.BackendStatusRolledBack {
		active, activeErr := s.repo.ActiveBackend(ctx)
		if activeErr == nil {
			backend = active
		} else if !errors.Is(activeErr, errs.ErrNotFound) {
			return nil, activeErr
		}
	}
	return s.view(ctx, backend)
}

// Test validates the saved Elasticsearch query/write endpoints and their API
// key privileges without distributing configuration to Edge or changing the
// active log backend. Apply repeats these checks before starting the real-write
// fleet verification and cutover.
func (s *Service) Test(ctx context.Context, id uint64) (*BackendTestResult, error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	backend, err := s.repo.GetBackend(ctx, id)
	if err != nil {
		return nil, err
	}
	switch backend.Status {
	case model.BackendStatusSaved, model.BackendStatusActive, model.BackendStatusDegraded, model.BackendStatusRolledBack:
	default:
		return nil, fmt.Errorf("%w: wait for the current log backend operation to finish", errs.ErrConflict)
	}
	version, err := s.probeBackend(ctx, backend)
	if err != nil {
		return nil, err
	}
	return &BackendTestResult{
		Status:          "ok",
		DetectedVersion: version,
		TestedAt:        time.Now().UTC(),
	}, nil
}

func (s *Service) Apply(ctx context.Context, id uint64) (*BackendView, error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	backend, err := s.repo.GetBackend(ctx, id)
	if err != nil {
		return nil, err
	}
	if backend.Status == model.BackendStatusActive {
		return s.view(ctx, backend)
	}
	if backend.Status != model.BackendStatusSaved && backend.Status != model.BackendStatusDegraded && backend.Status != model.BackendStatusRolledBack {
		return nil, fmt.Errorf("%w: only a saved Elasticsearch configuration can be applied", errs.ErrConflict)
	}
	active, activeErr := s.repo.ActiveBackend(ctx)
	if activeErr != nil && !errors.Is(activeErr, errs.ErrNotFound) {
		return nil, activeErr
	}
	if activeErr == nil && active.Status == model.BackendStatusRollingBack {
		return nil, fmt.Errorf("%w: wait for the Loki apply operation to finish", errs.ErrConflict)
	}
	if err := s.checkApplyGuard(ctx); err != nil {
		return nil, err
	}
	edgeIDs, err := s.fleetRolloutEdgeIDs(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.validateProbeEdges(ctx, edgeIDs); err != nil {
		return nil, err
	}
	version, err := s.probeBackend(ctx, backend)
	if err != nil {
		now := time.Now().UTC()
		stateErr := s.repo.SetBackendState(ctx, id, model.BackendStatusDegraded, "", safeProbeError(err), now)
		if stateErr != nil {
			return nil, errors.Join(err, stateErr)
		}
		return nil, err
	}
	now := time.Now().UTC()
	assignments := make([]*model.BackendAssignment, 0, len(edgeIDs))
	for _, edgeID := range edgeIDs {
		probeID, err := newProbeID()
		if err != nil {
			return nil, err
		}
		assignments = append(assignments, &model.BackendAssignment{
			BackendID: backend.ID, EdgeID: edgeID, DesiredGeneration: backend.Generation,
			Status: model.AssignmentStatusPending, ProbeID: probeID,
		})
	}
	backend.DetectedVersion = version
	backend.LastTestAt = &now
	if err := s.repo.BeginRollout(ctx, backend, assignments); err != nil {
		return nil, err
	}
	s.invalidateCache()
	if err := s.notify(ctx); err != nil {
		return nil, fmt.Errorf("distribute backend notification: %w", err)
	}
	backend, err = s.repo.GetBackend(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.view(ctx, backend)
}

func (s *Service) Rollback(ctx context.Context, id uint64) (*BackendView, error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()

	backend, err := s.repo.GetBackend(ctx, id)
	if err != nil {
		return nil, err
	}
	if backend.Status == model.BackendStatusRolledBack {
		return s.view(ctx, backend)
	}
	if backend.CutoverAt == nil || (backend.Status != model.BackendStatusActive && backend.Status != model.BackendStatusRollingBack) {
		if err := s.repo.CancelBackend(ctx, id); err != nil {
			return nil, err
		}
		s.invalidateCache()
		if err := s.notify(ctx); err != nil {
			return nil, fmt.Errorf("cancel backend rollout notification: %w", err)
		}
		backend, err = s.repo.GetBackend(ctx, id)
		if err != nil {
			return nil, err
		}
		return s.view(ctx, backend)
	}
	if s.loki == nil {
		return nil, fmt.Errorf("%w: built-in Loki query path is required before rollback", errs.ErrConflict)
	}
	edgeIDs, err := s.fleetRolloutEdgeIDs(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.validateProbeEdges(ctx, edgeIDs); err != nil {
		return nil, err
	}
	existing := map[uint64]*model.BackendAssignment{}
	if backend.Status == model.BackendStatusRollingBack {
		rows, listErr := s.repo.ListAssignments(ctx, backend.ID)
		if listErr != nil {
			return nil, listErr
		}
		for _, row := range rows {
			if row != nil {
				existing[row.EdgeID] = row
			}
		}
	}
	assignments := make([]*model.BackendAssignment, 0, len(edgeIDs))
	for _, edgeID := range edgeIDs {
		if prior := existing[edgeID]; prior != nil && prior.DesiredGeneration == backend.Generation &&
			prior.Status == model.AssignmentStatusVerified && prior.LastWriteSuccessAt != nil {
			assignments = append(assignments, cloneAssignmentForRollout(prior))
			continue
		}
		probeID, probeErr := newProbeID()
		if probeErr != nil {
			return nil, probeErr
		}
		assignments = append(assignments, &model.BackendAssignment{
			BackendID: backend.ID, EdgeID: edgeID, DesiredGeneration: backend.Generation,
			Status: model.AssignmentStatusPending, ProbeID: probeID,
		})
	}
	if err := s.repo.BeginRollback(ctx, backend, assignments); err != nil {
		return nil, err
	}
	s.invalidateCache()
	if err := s.notify(ctx); err != nil {
		return nil, fmt.Errorf("distribute rollback notification: %w", err)
	}
	backend, err = s.repo.GetBackend(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.view(ctx, backend)
}

func (s *Service) ActiveRuntime(ctx context.Context) (*RuntimeConfig, error) {
	backend, err := s.repo.ActiveBackend(ctx)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return runtimeConfig(backend)
}

// ActiveElasticsearchDatasource returns the active generation's Grafana
// query configuration. A nil result means Loki is authoritative. Only the
// read-only query API key is resolved.
func (s *Service) ActiveElasticsearchDatasource(ctx context.Context) (*pkggrafana.ElasticsearchDatasourceConfig, error) {
	backend, err := s.repo.ActiveBackend(ctx)
	if errors.Is(err, errs.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.elasticsearchDatasourceConfig(ctx, backend)
}

// PluginRuntimeOverlay implements edge.PluginRuntimeOverlayProvider without
// importing the edge bounded context. Values are non-sensitive; the API key is
// fetched separately by the authenticated Edge and materialized as a file.
func (s *Service) PluginRuntimeOverlay(ctx context.Context, edgeID uint64, plugin string) (map[string]interface{}, error) {
	if plugin != "logs" {
		return nil, nil
	}
	backend, assignment, err := s.runtimeBackendForEdge(ctx, edgeID)
	if err != nil {
		return nil, err
	}
	if backend == nil {
		return map[string]interface{}{
			"backend":            "builtin_loki",
			"backend_generation": uint64(0),
		}, nil
	}
	if backend.Status == model.BackendStatusRollingBack && assignment != nil {
		return rollbackRuntimeOverlay(backend, assignment)
	}
	runtime, err := runtimeConfig(backend)
	if err != nil {
		return nil, err
	}
	overlay := map[string]interface{}{
		"backend":                                "external_elasticsearch",
		"backend_id":                             runtime.BackendID,
		"backend_generation":                     runtime.Generation,
		"elasticsearch_endpoints":                append([]string(nil), runtime.WriteEndpoints...),
		"elasticsearch_dataset":                  runtime.Dataset,
		"elasticsearch_namespace":                runtime.Namespace,
		"elasticsearch_ca_pem":                   runtime.CAPEM,
		"elasticsearch_tls_insecure_skip_verify": runtime.TLSInsecure,
		"elasticsearch_secret_slot":              SecretSlotESAPIKey,
	}
	if assignment != nil && assignment.ProbeID != "" {
		overlay["log_probe_id"] = assignment.ProbeID
	}
	if assignment != nil {
		// Apply verification uses a bounded shadow write. The current backend
		// keeps receiving every log until all Edge probes pass, while the saved
		// configuration receives the same records for real Edge-path checks.
		// This avoids an observability blind spot without routing log bytes
		// through Manager or querying two backends at once.
		overlay["rollout_shadow"] = true
		active, activeErr := s.repo.ActiveBackend(ctx)
		if errors.Is(activeErr, errs.ErrNotFound) {
			overlay["baseline_backend"] = "builtin_loki"
		} else if activeErr != nil {
			return nil, activeErr
		} else {
			baseline, runtimeErr := runtimeConfig(active)
			if runtimeErr != nil {
				return nil, runtimeErr
			}
			overlay["baseline_backend"] = "external_elasticsearch"
			overlay["baseline_backend_id"] = baseline.BackendID
			overlay["baseline_backend_generation"] = baseline.Generation
			overlay["baseline_elasticsearch_endpoints"] = append([]string(nil), baseline.WriteEndpoints...)
			overlay["baseline_elasticsearch_dataset"] = baseline.Dataset
			overlay["baseline_elasticsearch_namespace"] = baseline.Namespace
			overlay["baseline_elasticsearch_ca_pem"] = baseline.CAPEM
			overlay["baseline_elasticsearch_tls_insecure_skip_verify"] = baseline.TLSInsecure
			overlay["baseline_elasticsearch_secret_slot"] = SecretSlotESAPIKey
		}
	}
	return overlay, nil
}

// PluginSecretForEdge is called only from the authenticated tunnel handler.
// During apply, only explicitly assigned Edges may obtain the saved
// generation. Once active, every authenticated Edge may obtain that active
// generation. The request can never select an arbitrary vault entry.
func (s *Service) PluginSecretForEdge(ctx context.Context, edgeID uint64, plugin, slot string, generation uint64) (*PluginSecret, error) {
	if edgeID == 0 || plugin != "logs" || slot != SecretSlotESAPIKey {
		return nil, errs.ErrForbidden
	}
	backend, assignment, err := s.backendForEdgeGeneration(ctx, edgeID, generation)
	if err != nil {
		return nil, err
	}
	if generation == 0 || generation != backend.Generation {
		return nil, fmt.Errorf("%w: stale log backend generation", errs.ErrConflict)
	}
	fields, err := s.secrets.ResolveFields(ctx, backend.WriteCredentialRef)
	if err != nil {
		return nil, fmt.Errorf("resolve Elasticsearch write credential: %w", err)
	}
	apiKey := strings.TrimSpace(fields["api_key"])
	if apiKey == "" {
		return nil, fmt.Errorf("%w: Elasticsearch credential has no api_key", errs.ErrInvalid)
	}
	if assignment != nil {
		assignment.DesiredGeneration = backend.Generation
		// Pulling the same secret again is normal after an Edge restart. Do
		// not erase a previously acknowledged generation or downgrade its
		// rollout status back to pending.
		if assignment.AppliedGeneration != backend.Generation && assignment.Status != model.AssignmentStatusFailed {
			assignment.Status = model.AssignmentStatusPending
		}
		if err := s.repo.UpsertAssignment(ctx, assignment); err != nil {
			return nil, err
		}
	}
	sum := sha256.Sum256([]byte(apiKey))
	return &PluginSecret{Generation: backend.Generation, Content: apiKey, SHA256: hex.EncodeToString(sum[:])}, nil
}

// MarkApplied closes the real-path gate: a successful local Collector start
// is not enough. Manager searches the external backend for the unique probe
// emitted by that Edge before marking it verified or promoting fleet-wide.
func (s *Service) MarkApplied(ctx context.Context, edgeID, generation uint64, probeID, applyErr string) error {
	backend, assignment, err := s.backendForEdgeGeneration(ctx, edgeID, generation)
	if err != nil {
		return err
	}
	isRollback := backend.Status == model.BackendStatusRollingBack
	if backend.Status != model.BackendStatusDistributing && backend.Status != model.BackendStatusVerifying && !isRollback {
		return fmt.Errorf("%w: log backend is not rolling out", errs.ErrConflict)
	}
	if assignment == nil || generation != backend.Generation || probeID == "" || probeID != assignment.ProbeID {
		return fmt.Errorf("%w: stale log backend generation", errs.ErrConflict)
	}
	now := time.Now().UTC()
	assignment.DesiredGeneration = generation
	assignment.AppliedGeneration = generation
	assignment.Status = model.AssignmentStatusApplied
	assignment.CutoverAt = &now
	assignment.LastProbeAt = &now
	assignment.LastError = ""
	if strings.TrimSpace(applyErr) != "" {
		assignment.AppliedGeneration = 0
		assignment.Status = model.AssignmentStatusFailed
		assignment.LastError = truncate(strings.TrimSpace(applyErr), 1024)
		if err := s.repo.UpsertAssignment(ctx, assignment); err != nil {
			return err
		}
		nextStatus := model.BackendStatusDegraded
		if isRollback {
			nextStatus = model.BackendStatusRollingBack
		}
		if err := s.repo.SetRolloutBackendState(ctx, backend.ID, nextStatus, backend.DetectedVersion, assignment.LastError, now); err != nil {
			return err
		}
		return s.notify(ctx)
	}
	if err := s.repo.UpsertAssignment(ctx, assignment); err != nil {
		return err
	}
	nextStatus := model.BackendStatusVerifying
	if isRollback {
		nextStatus = model.BackendStatusRollingBack
	}
	if err := s.repo.SetRolloutBackendState(ctx, backend.ID, nextStatus, backend.DetectedVersion, "", now); err != nil {
		return err
	}
	verify := s.verifyEdgeProbe
	if isRollback {
		verify = s.verifyLokiEdgeProbe
	}
	if err := verify(ctx, backend, assignment); err != nil {
		assignment.Status = model.AssignmentStatusFailed
		assignment.LastError = safeProbeError(err)
		if saveErr := s.repo.UpsertAssignment(ctx, assignment); saveErr != nil {
			return errors.Join(err, saveErr)
		}
		failureStatus := model.BackendStatusDegraded
		if isRollback {
			failureStatus = model.BackendStatusRollingBack
		}
		if stateErr := s.repo.SetRolloutBackendState(ctx, backend.ID, failureStatus, backend.DetectedVersion, assignment.LastError, time.Now().UTC()); stateErr != nil {
			return errors.Join(err, stateErr)
		}
		if notifyErr := s.notify(ctx); notifyErr != nil {
			return errors.Join(err, notifyErr)
		}
		return err
	}
	verifiedAt := time.Now().UTC()
	assignment.Status = model.AssignmentStatusVerified
	assignment.LastWriteSuccessAt = &verifiedAt
	assignment.LastError = ""
	if err := s.repo.UpsertAssignment(ctx, assignment); err != nil {
		return err
	}
	if isRollback {
		if err := s.completeVerifiedRollback(ctx, backend); err != nil && !errors.Is(err, errs.ErrConflict) {
			return err
		}
	} else {
		if err := s.promoteVerifiedRollout(ctx, backend); err != nil {
			// Other selected Edges may still be pending; that is a normal
			// partial convergence, not a failed RPC.
			if !errors.Is(err, errs.ErrConflict) {
				return err
			}
		}
	}
	return nil
}

func (s *Service) runtimeBackendForEdge(ctx context.Context, edgeID uint64) (*model.Backend, *model.BackendAssignment, error) {
	if edgeID != 0 {
		latest, err := s.repo.LatestBackend(ctx)
		if err == nil && (latest.Status == model.BackendStatusDistributing || latest.Status == model.BackendStatusVerifying) {
			assignment, assignmentErr := s.repo.GetAssignment(ctx, latest.ID, edgeID)
			if assignmentErr == nil && assignment.DesiredGeneration == latest.Generation && assignment.Status != model.AssignmentStatusFailed {
				return latest, assignment, nil
			}
			if assignmentErr != nil && !errors.Is(assignmentErr, errs.ErrNotFound) {
				return nil, nil, assignmentErr
			}
		} else if err != nil && !errors.Is(err, errs.ErrNotFound) {
			return nil, nil, err
		}
	}
	active, err := s.repo.ActiveBackend(ctx)
	if errors.Is(err, errs.ErrNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if edgeID != 0 && active.Status == model.BackendStatusRollingBack {
		assignment, assignmentErr := s.repo.GetAssignment(ctx, active.ID, edgeID)
		if assignmentErr == nil && assignment.DesiredGeneration == active.Generation && assignment.Status != model.AssignmentStatusFailed {
			return active, assignment, nil
		}
		if assignmentErr != nil && !errors.Is(assignmentErr, errs.ErrNotFound) {
			return nil, nil, assignmentErr
		}
	}
	return active, nil, nil
}

func (s *Service) backendForEdgeGeneration(ctx context.Context, edgeID, generation uint64) (*model.Backend, *model.BackendAssignment, error) {
	if edgeID == 0 || generation == 0 {
		return nil, nil, errs.ErrForbidden
	}
	latest, err := s.repo.LatestBackend(ctx)
	if err == nil && latest.Generation == generation &&
		(latest.Status == model.BackendStatusDistributing || latest.Status == model.BackendStatusVerifying) {
		assignment, assignmentErr := s.repo.GetAssignment(ctx, latest.ID, edgeID)
		if assignmentErr != nil {
			if errors.Is(assignmentErr, errs.ErrNotFound) {
				return nil, nil, errs.ErrForbidden
			}
			return nil, nil, assignmentErr
		}
		if assignment.DesiredGeneration != generation || assignment.Status == model.AssignmentStatusFailed {
			return nil, nil, errs.ErrForbidden
		}
		return latest, assignment, nil
	}
	if err != nil && !errors.Is(err, errs.ErrNotFound) {
		return nil, nil, err
	}
	active, err := s.repo.ActiveBackend(ctx)
	if err != nil {
		return nil, nil, err
	}
	if active.Generation != generation {
		return nil, nil, fmt.Errorf("%w: stale log backend generation", errs.ErrConflict)
	}
	if active.Status == model.BackendStatusRollingBack {
		assignment, assignmentErr := s.repo.GetAssignment(ctx, active.ID, edgeID)
		if assignmentErr != nil && !errors.Is(assignmentErr, errs.ErrNotFound) {
			return nil, nil, assignmentErr
		}
		if assignmentErr == nil && assignment.DesiredGeneration == generation && assignment.Status != model.AssignmentStatusFailed {
			return active, assignment, nil
		}
		// Elasticsearch remains the authoritative backend until every Loki
		// rollback probe succeeds. A missing or failed rollback assignment must
		// not prevent an authenticated Edge from re-fetching that active
		// generation after a restart; MarkApplied still rejects it because the
		// assignment returned here is nil.
		return active, nil, nil
	}
	return active, nil, nil
}

func rollbackRuntimeOverlay(backend *model.Backend, assignment *model.BackendAssignment) (map[string]interface{}, error) {
	baseline, err := runtimeConfig(backend)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"backend":                                         "builtin_loki",
		"backend_id":                                      backend.ID,
		"backend_generation":                              backend.Generation,
		"log_probe_id":                                    assignment.ProbeID,
		"rollout_shadow":                                  true,
		"baseline_backend":                                "external_elasticsearch",
		"baseline_backend_id":                             baseline.BackendID,
		"baseline_backend_generation":                     baseline.Generation,
		"baseline_elasticsearch_endpoints":                append([]string(nil), baseline.WriteEndpoints...),
		"baseline_elasticsearch_dataset":                  baseline.Dataset,
		"baseline_elasticsearch_namespace":                baseline.Namespace,
		"baseline_elasticsearch_ca_pem":                   baseline.CAPEM,
		"baseline_elasticsearch_tls_insecure_skip_verify": baseline.TLSInsecure,
		"baseline_elasticsearch_secret_slot":              SecretSlotESAPIKey,
	}, nil
}

func runtimeConfig(backend *model.Backend) (*RuntimeConfig, error) {
	if backend == nil {
		return nil, nil
	}
	endpoints, err := decodeEndpoints(backend.WriteEndpointsJSON)
	if err != nil {
		return nil, err
	}
	return &RuntimeConfig{
		BackendID: backend.ID, Backend: string(backend.Type), Generation: backend.Generation,
		WriteEndpoints: endpoints, Dataset: backend.Dataset, Namespace: backend.Namespace,
		CAPEM: backend.CAPEM, TLSInsecure: backend.TLSInsecure,
	}, nil
}

func (s *Service) verifyEdgeProbe(ctx context.Context, backend *model.Backend, assignment *model.BackendAssignment) error {
	s.mu.RLock()
	deviceResolver := s.devices
	s.mu.RUnlock()
	if deviceResolver == nil {
		return errors.New("host device resolver is not configured")
	}
	deviceID, err := deviceResolver.LookupHostDevice(ctx, assignment.EdgeID)
	if err != nil {
		return fmt.Errorf("resolve host device for Edge %d: %w", assignment.EdgeID, err)
	}
	if deviceID == 0 {
		return fmt.Errorf("resolve host device for Edge %d: empty device id", assignment.EdgeID)
	}
	queryKey, err := s.apiKey(ctx, backend.QueryCredentialRef)
	if err != nil {
		return err
	}
	client, err := s.newESClient(backend.QueryEndpoint, backend.IndexPattern, queryKey, backend)
	if err != nil {
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	started := time.Now().UTC()
	if assignment.LastProbeAt != nil {
		started = assignment.LastProbeAt.Add(-time.Minute)
	}
	var lastErr error
	for {
		result, searchErr := client.Search(probeCtx, logquery.SearchRequest{
			Start: started, End: time.Now().UTC().Add(10 * time.Second), Limit: 10,
			Direction: logquery.SortBackward,
			Scope:     logquery.Scope{DeviceIDs: []uint64{deviceID}},
			Keywords:  logquery.Keywords{Include: []string{assignment.ProbeID}, Mode: logquery.MatchPhrase},
		})
		if searchErr == nil {
			for _, record := range result.Records {
				if strings.Contains(record.Message, assignment.ProbeID) {
					return nil
				}
			}
			lastErr = errors.New("probe log is not visible yet")
		} else {
			lastErr = searchErr
		}
		select {
		case <-probeCtx.Done():
			return fmt.Errorf("Elasticsearch Edge write probe %q not found: %w", assignment.ProbeID, lastErr)
		case <-time.After(750 * time.Millisecond):
		}
	}
}

func (s *Service) verifyLokiEdgeProbe(ctx context.Context, _ *model.Backend, assignment *model.BackendAssignment) error {
	s.mu.RLock()
	deviceResolver := s.devices
	loki := s.loki
	s.mu.RUnlock()
	if deviceResolver == nil {
		return errors.New("host device resolver is not configured")
	}
	if loki == nil {
		return errors.New("built-in Loki query path is not configured")
	}
	deviceID, err := deviceResolver.LookupHostDevice(ctx, assignment.EdgeID)
	if err != nil {
		return fmt.Errorf("resolve host device for Edge %d: %w", assignment.EdgeID, err)
	}
	if deviceID == 0 {
		return fmt.Errorf("resolve host device for Edge %d: empty device id", assignment.EdgeID)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	started := time.Now().UTC()
	if assignment.LastProbeAt != nil {
		started = assignment.LastProbeAt.Add(-time.Minute)
	}
	var lastErr error
	for {
		result, searchErr := loki.Search(probeCtx, logquery.SearchRequest{
			Start: started, End: time.Now().UTC().Add(10 * time.Second), Limit: 10,
			Direction: logquery.SortBackward,
			Scope:     logquery.Scope{DeviceIDs: []uint64{deviceID}},
			Keywords:  logquery.Keywords{Include: []string{assignment.ProbeID}, Mode: logquery.MatchPhrase},
		})
		if searchErr == nil {
			for _, record := range result.Records {
				if strings.Contains(record.Message, assignment.ProbeID) {
					return nil
				}
			}
			lastErr = errors.New("probe log is not visible yet")
		} else {
			lastErr = searchErr
		}
		select {
		case <-probeCtx.Done():
			return fmt.Errorf("built-in Loki Edge write probe %q not found: %w", assignment.ProbeID, lastErr)
		case <-time.After(750 * time.Millisecond):
		}
	}
}

func (s *Service) promoteVerifiedRollout(ctx context.Context, backend *model.Backend) error {
	assignments, err := s.repo.ListAssignments(ctx, backend.ID)
	if err != nil {
		return err
	}
	if len(assignments) == 0 {
		return fmt.Errorf("%w: no Edge write probes selected", errs.ErrConflict)
	}
	for _, assignment := range assignments {
		if assignment.DesiredGeneration != backend.Generation || assignment.Status != model.AssignmentStatusVerified || assignment.LastWriteSuccessAt == nil {
			return fmt.Errorf("%w: Edge write probes are not fully verified", errs.ErrConflict)
		}
	}
	cutover := time.Now().UTC()
	if err := s.repo.ActivateBackend(ctx, backend.ID, backend.DetectedVersion, cutover); err != nil {
		return err
	}
	s.invalidateCache()
	s.syncGrafanaElasticsearchAsync(ctx, backend)
	if err := s.notify(ctx); err != nil {
		return fmt.Errorf("activate backend notification: %w", err)
	}
	return nil
}

func (s *Service) completeVerifiedRollback(ctx context.Context, backend *model.Backend) error {
	assignments, err := s.repo.ListAssignments(ctx, backend.ID)
	if err != nil {
		return err
	}
	if !assignmentsVerified(assignments, backend.Generation) {
		return fmt.Errorf("%w: built-in Loki write probes are not fully verified", errs.ErrConflict)
	}
	endedAt := time.Now().UTC()
	if err := s.repo.CompleteRollback(ctx, backend.ID, endedAt); err != nil {
		return err
	}
	s.invalidateCache()
	s.syncGrafanaLokiAsync(ctx)
	if err := s.notify(ctx); err != nil {
		return fmt.Errorf("complete backend rollback notification: %w", err)
	}
	return nil
}

func (s *Service) elasticsearchDatasourceConfig(ctx context.Context, backend *model.Backend) (*pkggrafana.ElasticsearchDatasourceConfig, error) {
	if backend == nil {
		return nil, errors.New("active Elasticsearch backend is nil")
	}
	queryKey, err := s.apiKey(ctx, backend.QueryCredentialRef)
	if err != nil {
		return nil, fmt.Errorf("resolve Elasticsearch query credential for Grafana: %w", err)
	}
	endpoints, err := decodeEndpoints(backend.WriteEndpointsJSON)
	if err != nil {
		return nil, err
	}
	return &pkggrafana.ElasticsearchDatasourceConfig{
		// QueryEndpoint is explicitly Manager-scoped and may be a loopback
		// address. Grafana runs in a separate process/container, so point it at
		// the first endpoint already verified for direct Edge access instead.
		URL:          endpoints[0],
		IndexPattern: backend.IndexPattern,
		APIKey:       queryKey,
		CAPEM:        backend.CAPEM,
		TLSInsecure:  backend.TLSInsecure,
	}, nil
}

func (s *Service) syncGrafanaElasticsearchAsync(ctx context.Context, backend *model.Backend) {
	s.mu.RLock()
	syncer := s.grafana
	s.mu.RUnlock()
	if syncer == nil || backend == nil {
		return
	}
	backendCopy := *backend
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.log.Error("Grafana Elasticsearch datasource sync panicked", slog.Any("panic", recovered))
			}
		}()
		syncCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		config, err := s.elasticsearchDatasourceConfig(syncCtx, &backendCopy)
		if err == nil {
			err = syncer.SyncElasticsearch(syncCtx, *config)
		}
		if err != nil {
			s.log.WarnContext(syncCtx, "Grafana Elasticsearch datasource sync failed; log backend remains active",
				slog.Uint64("backend_id", backendCopy.ID), slog.Any("error", err))
		}
	}()
}

func (s *Service) syncGrafanaLokiAsync(ctx context.Context) {
	s.mu.RLock()
	syncer := s.grafana
	s.mu.RUnlock()
	if syncer == nil {
		return
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.log.Error("Grafana Loki datasource sync panicked", slog.Any("panic", recovered))
			}
		}()
		syncCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		if err := syncer.SyncLoki(syncCtx); err != nil {
			s.log.WarnContext(syncCtx, "Grafana Loki datasource sync failed; Loki remains active", slog.Any("error", err))
		}
	}()
}

func normalizeEdgeIDs(input []uint64) ([]uint64, error) {
	const maxRolloutEdges = 10_000
	if len(input) == 0 || len(input) > maxRolloutEdges {
		return nil, fmt.Errorf("%w: edge_ids must contain 1..%d entries", errs.ErrInvalid, maxRolloutEdges)
	}
	seen := make(map[uint64]struct{}, len(input))
	out := make([]uint64, 0, len(input))
	for _, edgeID := range input {
		if edgeID == 0 {
			return nil, fmt.Errorf("%w: edge_id must be greater than zero", errs.ErrInvalid)
		}
		if _, exists := seen[edgeID]; exists {
			continue
		}
		seen[edgeID] = struct{}{}
		out = append(out, edgeID)
	}
	return out, nil
}

func (s *Service) fleetRolloutEdgeIDs(ctx context.Context) ([]uint64, error) {
	s.mu.RLock()
	inventory := s.inventory
	s.mu.RUnlock()
	if inventory == nil {
		return nil, errors.New("log backend rollout Edge inventory is not configured")
	}
	edges, err := inventory.ListRolloutEdges(ctx)
	if err != nil {
		return nil, fmt.Errorf("list Edges for logs rollout: %w", err)
	}
	edgeIDs := make([]uint64, 0, len(edges))
	offline := make([]uint64, 0)
	for _, edge := range edges {
		if edge.EdgeID == 0 {
			return nil, fmt.Errorf("%w: logs rollout inventory contains an empty Edge id", errs.ErrInvalid)
		}
		edgeIDs = append(edgeIDs, edge.EdgeID)
		if !edge.Online {
			offline = append(offline, edge.EdgeID)
		}
	}
	if len(offline) > 0 {
		return nil, fmt.Errorf("%w: all log-enabled Edges must be online before a fleet log cutover; offline edge_ids=%v", errs.ErrConflict, offline)
	}
	edgeIDs, err = normalizeEdgeIDs(edgeIDs)
	if err != nil {
		return nil, fmt.Errorf("fleet rollout requires at least one log-enabled Edge: %w", err)
	}
	return edgeIDs, nil
}

func (s *Service) validateProbeEdges(ctx context.Context, edgeIDs []uint64) error {
	s.mu.RLock()
	resolver := s.devices
	s.mu.RUnlock()
	if resolver == nil {
		return errors.New("host device resolver is not configured")
	}
	for _, edgeID := range edgeIDs {
		deviceID, err := resolver.LookupHostDevice(ctx, edgeID)
		if err != nil {
			return fmt.Errorf("resolve host device for Edge %d: %w", edgeID, err)
		}
		if deviceID == 0 {
			return fmt.Errorf("resolve host device for Edge %d: empty device id", edgeID)
		}
	}
	return nil
}

func cloneAssignmentForRollout(in *model.BackendAssignment) *model.BackendAssignment {
	if in == nil {
		return nil
	}
	return &model.BackendAssignment{
		BackendID: in.BackendID, EdgeID: in.EdgeID,
		DesiredGeneration: in.DesiredGeneration, AppliedGeneration: in.AppliedGeneration,
		Status: in.Status, ProbeID: in.ProbeID, CutoverAt: in.CutoverAt,
		LastProbeAt: in.LastProbeAt, LastWriteSuccessAt: in.LastWriteSuccessAt,
		LastError: in.LastError,
	}
}

func assignmentsVerified(assignments []*model.BackendAssignment, generation uint64) bool {
	if len(assignments) == 0 || generation == 0 {
		return false
	}
	for _, assignment := range assignments {
		if assignment == nil || assignment.DesiredGeneration != generation ||
			assignment.AppliedGeneration != generation || assignment.Status != model.AssignmentStatusVerified ||
			assignment.LastWriteSuccessAt == nil {
			return false
		}
	}
	return true
}

func newProbeID() (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate Edge log probe id: %w", err)
	}
	return "ongrid-log-probe-" + base64.RawURLEncoding.EncodeToString(raw), nil
}

func (s *Service) probeBackend(ctx context.Context, backend *model.Backend) (string, error) {
	queryKey, err := s.apiKey(ctx, backend.QueryCredentialRef)
	if err != nil {
		return "", err
	}
	queryClient, err := s.newESClient(backend.QueryEndpoint, backend.IndexPattern, queryKey, backend)
	if err != nil {
		return "", err
	}
	if err := queryClient.RequirePrivileges(ctx, []string{"monitor"}, []string{"read", "view_index_metadata"}); err != nil {
		return "", fmt.Errorf("Elasticsearch query privileges: %w", err)
	}
	version, err := queryClient.Probe(ctx)
	if err != nil {
		return "", fmt.Errorf("Elasticsearch query probe: %w", err)
	}
	writeKey, err := s.apiKey(ctx, backend.WriteCredentialRef)
	if err != nil {
		return "", err
	}
	endpoints, err := decodeEndpoints(backend.WriteEndpointsJSON)
	if err != nil {
		return "", err
	}
	for i, endpoint := range endpoints {
		writeClient, clientErr := s.newESClient(endpoint, backend.IndexPattern, writeKey, backend)
		if clientErr != nil {
			return "", clientErr
		}
		if privilegeErr := writeClient.RequirePrivileges(ctx, nil, []string{"auto_configure", "create_doc"}); privilegeErr != nil {
			return "", fmt.Errorf("Elasticsearch write endpoint %d privileges: %w", i+1, privilegeErr)
		}

		// Use the Manager-only query credential for the version check. The
		// runtime write credential sent to Edge deliberately has no cluster
		// monitor permission.
		writeEndpointProbe, clientErr := s.newESClient(endpoint, backend.IndexPattern, queryKey, backend)
		if clientErr != nil {
			return "", clientErr
		}
		writeVersion, probeErr := writeEndpointProbe.Probe(ctx)
		if probeErr != nil {
			return "", fmt.Errorf("Elasticsearch write endpoint %d probe: %w", i+1, probeErr)
		}
		if writeVersion != version {
			return "", fmt.Errorf("Elasticsearch write endpoint %d reports version %s, query endpoint reports %s", i+1, writeVersion, version)
		}
	}
	return version, nil
}

func (s *Service) apiKey(ctx context.Context, ref string) (string, error) {
	if s.secrets == nil {
		return "", errors.New("log backend secret resolver is disabled")
	}
	fields, err := s.secrets.ResolveFields(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("resolve Elasticsearch credential: %w", err)
	}
	key := strings.TrimSpace(fields["api_key"])
	if key == "" {
		return "", errors.New("Elasticsearch credential has no api_key")
	}
	return key, nil
}

func (s *Service) storeManagedAPIKey(ctx context.Context, role, apiKey string, generation uint64) (string, error) {
	store, ok := s.secrets.(ManagedSecretStore)
	if !ok {
		return "", errors.New("encrypted log backend credential storage is unavailable")
	}
	// A directly pasted key always gets a new isolated reference. The backend
	// row is switched only after both keys validate and are stored, so a failed
	// save cannot rotate the credential referenced by the current generation.
	name, err := newManagedLogsSecretName(role, generation)
	if err != nil {
		return "", fmt.Errorf("generate managed Elasticsearch credential name: %w", err)
	}
	description := fmt.Sprintf("Managed by Ongrid logs backend (%s key, generation %d)", role, generation)
	if err := store.CreateManaged(ctx, name, elasticsearchCredType, description, map[string]string{"api_key": apiKey}); err != nil {
		return "", fmt.Errorf("store managed Elasticsearch %s credential: %w", role, err)
	}
	return name, nil
}

func (s *Service) deleteManagedAPIKeys(ctx context.Context, refs []string) error {
	store, ok := s.secrets.(ManagedSecretStore)
	if !ok {
		return errors.New("encrypted log backend credential storage is unavailable")
	}
	var cleanupErr error
	for i := len(refs) - 1; i >= 0; i-- {
		if err := store.DeleteManaged(ctx, refs[i]); err != nil && !errors.Is(err, errs.ErrNotFound) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete managed credential: %w", err))
		}
	}
	return cleanupErr
}

func (s *Service) cleanupSupersededManagedAPIKeys(ctx context.Context, previous, current *model.Backend) {
	refs := supersededManagedAPIKeyRefs(previous, current)
	if len(refs) == 0 {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), managedSecretCleanupTTL)
	defer cancel()
	if err := s.deleteManagedAPIKeys(cleanupCtx, refs); err != nil {
		s.log.WarnContext(cleanupCtx, "failed to delete superseded managed Elasticsearch credentials",
			slog.Int("credential_count", len(refs)), slog.Any("error", err))
	}
}

func supersededManagedAPIKeyRefs(previous, current *model.Backend) []string {
	if previous == nil || current == nil {
		return nil
	}
	retained := map[string]struct{}{
		current.WriteCredentialRef: {},
		current.QueryCredentialRef: {},
	}
	seen := make(map[string]struct{}, 2)
	refs := make([]string, 0, 2)
	for _, ref := range []string{previous.WriteCredentialRef, previous.QueryCredentialRef} {
		if !strings.HasPrefix(ref, managedLogsSecretPrefix) {
			continue
		}
		if _, ok := retained[ref]; ok {
			continue
		}
		if _, ok := seen[ref]; ok {
			continue
		}
		seen[ref] = struct{}{}
		refs = append(refs, ref)
	}
	return refs
}

func newManagedLogsSecretName(role string, generation uint64) (string, error) {
	if role != "write" && role != "query" {
		return "", fmt.Errorf("unsupported Elasticsearch credential role %q", role)
	}
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%s-g%d-%s", managedLogsSecretPrefix, role, generation, hex.EncodeToString(random)), nil
}

func (s *Service) newESClient(endpoint, pattern, apiKey string, backend *model.Backend) (*logquery.ElasticsearchClient, error) {
	httpClient, err := backendHTTPClient(backend)
	if err != nil {
		return nil, err
	}
	return logquery.NewElasticsearchClient(logquery.ElasticsearchConfig{
		Endpoint:          endpoint,
		IndexPattern:      pattern,
		APIKey:            apiKey,
		AllowInsecureHTTP: backend.TLSInsecure,
	}, httpClient, nil)
}

func (s *Service) view(ctx context.Context, backend *model.Backend) (*BackendView, error) {
	endpoints, err := decodeEndpoints(backend.WriteEndpointsJSON)
	if err != nil {
		return nil, err
	}
	currentBackend := "loki"
	var currentBackendID uint64
	active, activeErr := s.repo.ActiveBackend(ctx)
	if activeErr == nil && active != nil {
		currentBackend = string(active.Type)
		currentBackendID = active.ID
	} else if activeErr != nil && !errors.Is(activeErr, errs.ErrNotFound) {
		return nil, activeErr
	}
	assignments, err := s.repo.ListAssignments(ctx, backend.ID)
	if err != nil {
		return nil, err
	}
	return &BackendView{
		ID: backend.ID, Name: backend.Name, Type: backend.Type, CurrentBackend: currentBackend, CurrentBackendID: currentBackendID, Status: backend.Status,
		Generation: backend.Generation, WriteEndpoints: endpoints, QueryEndpoint: backend.QueryEndpoint,
		Dataset: backend.Dataset, Namespace: backend.Namespace, IndexPattern: backend.IndexPattern,
		WriteCredentialRef: backend.WriteCredentialRef, QueryCredentialRef: backend.QueryCredentialRef,
		HasCustomCA: strings.TrimSpace(backend.CAPEM) != "", KibanaURL: backend.KibanaURL,
		TLSInsecure:     backend.TLSInsecure,
		DetectedVersion: backend.DetectedVersion,
		CutoverAt:       backend.CutoverAt, EndedAt: backend.EndedAt,
		LastTestAt: backend.LastTestAt, LastError: backend.LastError,
		Assignments: assignments, CreatedAt: backend.CreatedAt, UpdatedAt: backend.UpdatedAt,
	}, nil
}

func (s *Service) notify(ctx context.Context) error {
	s.mu.RLock()
	notifier := s.notifier
	s.mu.RUnlock()
	if notifier == nil {
		return nil
	}
	return notifier.NotifyLogsBackendChanged(ctx)
}

func (s *Service) checkApplyGuard(ctx context.Context) error {
	s.mu.RLock()
	guard := s.applyGuard
	s.mu.RUnlock()
	if guard == nil {
		return nil
	}
	if err := guard(ctx); err != nil {
		return fmt.Errorf("%w: %v", errs.ErrConflict, err)
	}
	return nil
}

func (s *Service) invalidateCache() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cacheKey = ""
	s.cachedES = nil
}

func normalizeSaveInput(input SaveInput) (SaveInput, error) {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		input.Name = DefaultBackendName
	}
	if len(input.Name) > 128 {
		return SaveInput{}, fmt.Errorf("%w: backend name too long", errs.ErrInvalid)
	}
	if len(input.WriteEndpoints) == 0 || len(input.WriteEndpoints) > maxBackendEndpoints {
		return SaveInput{}, fmt.Errorf("%w: write_endpoints must contain 1..%d entries", errs.ErrInvalid, maxBackendEndpoints)
	}
	seen := make(map[string]struct{}, len(input.WriteEndpoints))
	endpoints := make([]string, 0, len(input.WriteEndpoints))
	for _, raw := range input.WriteEndpoints {
		endpoint, err := normalizeHTTPSURL(raw, input.TLSInsecure, false)
		if err != nil {
			return SaveInput{}, fmt.Errorf("%w: invalid write endpoint", errs.ErrInvalid)
		}
		if _, ok := seen[endpoint]; ok {
			continue
		}
		seen[endpoint] = struct{}{}
		endpoints = append(endpoints, endpoint)
	}
	input.WriteEndpoints = endpoints
	if strings.TrimSpace(input.QueryEndpoint) == "" {
		input.QueryEndpoint = endpoints[0]
	}
	queryEndpoint, err := normalizeHTTPSURL(input.QueryEndpoint, input.TLSInsecure, false)
	if err != nil {
		return SaveInput{}, fmt.Errorf("%w: invalid query endpoint", errs.ErrInvalid)
	}
	input.QueryEndpoint = queryEndpoint
	input.Dataset = strings.ToLower(strings.TrimSpace(input.Dataset))
	if input.Dataset == "" {
		input.Dataset = "ongrid.generic"
	}
	if !datasetRE.MatchString(input.Dataset) {
		return SaveInput{}, fmt.Errorf("%w: dataset must match ongrid.<safe-slug>", errs.ErrInvalid)
	}
	input.Namespace = strings.ToLower(strings.TrimSpace(input.Namespace))
	if input.Namespace == "" {
		input.Namespace = "default"
	}
	if !namespaceRE.MatchString(input.Namespace) {
		return SaveInput{}, fmt.Errorf("%w: invalid data stream namespace", errs.ErrInvalid)
	}
	input.WriteCredentialRef = strings.TrimSpace(input.WriteCredentialRef)
	input.QueryCredentialRef = strings.TrimSpace(input.QueryCredentialRef)
	if len(input.WriteCredentialRef) > 128 || len(input.QueryCredentialRef) > 128 {
		return SaveInput{}, fmt.Errorf("%w: credential ref too long", errs.ErrInvalid)
	}
	input.WriteAPIKey, err = normalizeDirectAPIKey(input.WriteAPIKey)
	if err != nil {
		return SaveInput{}, fmt.Errorf("%w: invalid write_api_key", errs.ErrInvalid)
	}
	input.QueryAPIKey, err = normalizeDirectAPIKey(input.QueryAPIKey)
	if err != nil {
		return SaveInput{}, fmt.Errorf("%w: invalid query_api_key", errs.ErrInvalid)
	}
	if input.ReuseWriteAPIKey && input.QueryAPIKey != "" {
		return SaveInput{}, fmt.Errorf("%w: query_api_key must be empty when reuse_write_api_key is enabled", errs.ErrInvalid)
	}
	input.CAPEM = strings.TrimSpace(input.CAPEM)
	if len(input.CAPEM) > maxCAPEMBytes {
		return SaveInput{}, fmt.Errorf("%w: CA bundle too large", errs.ErrInvalid)
	}
	if input.CAPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(input.CAPEM)) {
			return SaveInput{}, fmt.Errorf("%w: invalid CA PEM", errs.ErrInvalid)
		}
	}
	input.KibanaURL = strings.TrimSpace(input.KibanaURL)
	if input.KibanaURL != "" {
		input.KibanaURL, err = normalizeHTTPSURL(input.KibanaURL, false, true)
		if err != nil {
			return SaveInput{}, fmt.Errorf("%w: invalid Kibana URL", errs.ErrInvalid)
		}
	}
	return input, nil
}

func normalizeDirectAPIKey(raw string) (string, error) {
	key := strings.TrimSpace(raw)
	if len(key) >= len("ApiKey ") && strings.EqualFold(key[:len("ApiKey ")], "ApiKey ") {
		key = strings.TrimSpace(key[len("ApiKey "):])
	}
	if len(key) > maxAPIKeyBytes {
		return "", errors.New("API key too large")
	}
	if strings.ContainsAny(key, " \t\r\n") {
		return "", errors.New("API key contains whitespace")
	}
	return key, nil
}

func normalizeHTTPSURL(raw string, allowHTTP, allowPath bool) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("invalid URL")
	}
	if parsed.Scheme != "https" && !(allowHTTP && parsed.Scheme == "http") {
		return "", errors.New("HTTPS required")
	}
	if !allowPath && parsed.Path != "" && parsed.Path != "/" {
		return "", errors.New("path not allowed")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func backendHTTPClient(backend *model.Backend) (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if strings.TrimSpace(backend.CAPEM) != "" && !pool.AppendCertsFromPEM([]byte(backend.CAPEM)) {
		return nil, errors.New("invalid Elasticsearch CA PEM")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{
		MinVersion:         tls.VersionTLS12,
		RootCAs:            pool,
		InsecureSkipVerify: backend.TLSInsecure, //nolint:gosec // explicit admin-only compatibility switch
	}
	return &http.Client{
		Timeout:   15 * time.Second,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errors.New("Elasticsearch redirects are disabled")
		},
	}, nil
}

func decodeEndpoints(raw string) ([]string, error) {
	var endpoints []string
	if err := json.Unmarshal([]byte(raw), &endpoints); err != nil {
		return nil, fmt.Errorf("decode Elasticsearch endpoints: %w", err)
	}
	if len(endpoints) == 0 {
		return nil, errors.New("Elasticsearch endpoints are empty")
	}
	return endpoints, nil
}

func indexPattern(namespace string) string { return "logs-ongrid.*.otel-" + namespace }

func safeProbeError(err error) string {
	if err == nil {
		return ""
	}
	return truncate(err.Error(), 1024)
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
