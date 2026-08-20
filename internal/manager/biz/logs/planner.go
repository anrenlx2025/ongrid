package logs

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"golang.org/x/sync/errgroup"

	model "github.com/ongridio/ongrid/internal/manager/model/logs"
	apperrs "github.com/ongridio/ongrid/internal/pkg/errs"
	"github.com/ongridio/ongrid/internal/pkg/logquery"
)

const (
	plannerBackendName  = "manager"
	maxHistogramBuckets = 500
)

type plannerCursor struct {
	Backend    string `json:"backend"`
	PlanSum    string `json:"plan_sum"`
	Phase      string `json:"phase"`
	Cursor     string `json:"cursor,omitempty"`
	RequestSum string `json:"request_sum"`
}

type queryPhase struct {
	name       string
	start, end time.Time
	backend    *model.Backend // nil means built-in Loki
}

func (s *Service) Search(ctx context.Context, req logquery.SearchRequest) (*logquery.SearchResult, error) {
	if err := req.NormalizeAndValidate(); err != nil {
		return nil, err
	}
	phases, planSum, err := s.plan(ctx, req.Start, req.End, req.Direction)
	if err != nil {
		return nil, err
	}
	requestSum, err := plannerRequestSum(req)
	if err != nil {
		return nil, err
	}
	phaseIndex := 0
	backendCursor := ""
	if req.Cursor != "" {
		var cursor plannerCursor
		if err := decodePlannerCursor(req.Cursor, &cursor); err != nil {
			return nil, err
		}
		if cursor.Backend != plannerBackendName || cursor.PlanSum != planSum || cursor.RequestSum != requestSum {
			return nil, errors.New("logquery: invalid cursor")
		}
		phaseIndex = phasePosition(phases, cursor.Phase)
		if phaseIndex < 0 {
			return nil, errors.New("logquery: invalid cursor")
		}
		backendCursor = cursor.Cursor
	}

	started := time.Now()
	result := &logquery.SearchResult{Records: []logquery.Record{}, Backends: []string{}}
	remaining := req.Limit
	for phaseIndex < len(phases) && remaining > 0 {
		phase := phases[phaseIndex]
		phaseReq := req
		// Query phases use the same (start, end] ownership convention as
		// Count. Backend search APIs accept an inclusive lower bound, so move
		// it by one nanosecond to prevent duplicates at cutover/rollback.
		phaseReq.Start, phaseReq.End = phase.start.Add(time.Nanosecond), phase.end
		phaseReq.Limit, phaseReq.Cursor = remaining, backendCursor
		searcher, err := s.searcherForPhase(ctx, phase)
		if err != nil {
			return nil, err
		}
		page, err := searcher.Search(ctx, phaseReq)
		if err != nil {
			return nil, err
		}
		result.Records = append(result.Records, page.Records...)
		result.Backends = appendUnique(result.Backends, page.Backends...)
		remaining = req.Limit - len(result.Records)
		if page.HasMore {
			result.HasMore = true
			result.NextCursor, err = encodePlannerCursor(plannerCursor{
				Backend: plannerBackendName, PlanSum: planSum, Phase: phase.name,
				Cursor: page.NextCursor, RequestSum: requestSum,
			})
			if err != nil {
				return nil, err
			}
			break
		}
		phaseIndex++
		backendCursor = ""
		if phaseIndex < len(phases) && remaining == 0 {
			result.HasMore = true
			result.NextCursor, err = encodePlannerCursor(plannerCursor{
				Backend: plannerBackendName, PlanSum: planSum,
				Phase: phases[phaseIndex].name, RequestSum: requestSum,
			})
			if err != nil {
				return nil, err
			}
		}
	}
	result.TookMS = time.Since(started).Milliseconds()
	return result, nil
}

// Count uses the same single active backend as Search. Data retained in an
// inactive backend is intentionally outside the product query surface.
func (s *Service) Count(ctx context.Context, req logquery.SearchRequest) (uint64, error) {
	if err := req.NormalizeAndValidate(); err != nil {
		return 0, err
	}
	phases, _, err := s.plan(ctx, req.Start, req.End, logquery.SortForward)
	if err != nil {
		return 0, err
	}
	var total uint64
	for _, phase := range phases {
		phaseReq := req
		phaseReq.Start, phaseReq.End, phaseReq.Cursor = phase.start, phase.end, ""
		searcher, err := s.searcherForPhase(ctx, phase)
		if err != nil {
			return 0, err
		}
		count, err := searcher.Count(ctx, phaseReq)
		if err != nil {
			return 0, err
		}
		total += count
	}
	return total, nil
}

func (s *Service) Fields(_ context.Context, _, _ time.Time, _ logquery.Scope) ([]logquery.Field, error) {
	return logquery.AllowedFields(), nil
}

func (s *Service) FieldValues(ctx context.Context, req logquery.FieldValuesRequest) ([]string, error) {
	if err := req.NormalizeAndValidate(); err != nil {
		return nil, err
	}
	phases, _, err := s.plan(ctx, req.Start, req.End, logquery.SortForward)
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{})
	for _, phase := range phases {
		phaseReq := req
		phaseReq.Start, phaseReq.End = phase.start, phase.end
		searcher, err := s.searcherForPhase(ctx, phase)
		if err != nil {
			return nil, err
		}
		values, err := searcher.FieldValues(ctx, phaseReq)
		if err != nil {
			return nil, err
		}
		for _, value := range values {
			set[value] = struct{}{}
		}
	}
	values := make([]string, 0, len(set))
	for value := range set {
		values = append(values, value)
	}
	sort.Strings(values)
	if len(values) > req.Limit {
		values = values[:req.Limit]
	}
	return values, nil
}

func (s *Service) Histogram(ctx context.Context, req logquery.SearchRequest, interval time.Duration) ([]logquery.HistogramBucket, error) {
	if err := req.NormalizeAndValidate(); err != nil {
		return nil, err
	}
	if interval <= 0 || interval > logquery.MaxSearchWindow {
		return nil, errors.New("logquery: histogram interval is invalid")
	}
	span := req.End.Sub(req.Start)
	bucketCount := int((span-1)/interval) + 1
	if bucketCount > maxHistogramBuckets {
		return nil, fmt.Errorf("logquery: histogram exceeds %d buckets; increase interval", maxHistogramBuckets)
	}
	phases, _, err := s.plan(ctx, req.Start, req.End, logquery.SortForward)
	if err != nil {
		return nil, err
	}
	type plannedPhase struct {
		phase    queryPhase
		searcher logquery.Searcher
	}
	planned := make([]plannedPhase, 0, len(phases))
	for _, phase := range phases {
		searcher, searcherErr := s.searcherForPhase(ctx, phase)
		if searcherErr != nil {
			return nil, searcherErr
		}
		planned = append(planned, plannedPhase{phase: phase, searcher: searcher})
	}

	// Backend-native histogram APIs do not share one bucket origin: Loki's
	// range vectors are trailing windows while Elasticsearch rounds fixed
	// intervals to epoch boundaries. Build one product-level grid and use exact
	// Count calls so switching the active backend never changes bucket shape.
	// The bounded worker group keeps a normal 60-120 bucket UI request practical
	// without turning it into unbounded fan-out.
	out := make([]logquery.HistogramBucket, bucketCount)
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(8)
	for i := range out {
		i := i
		bucketStart := req.Start.Add(time.Duration(i) * interval)
		bucketEnd := bucketStart.Add(interval)
		if bucketEnd.After(req.End) {
			bucketEnd = req.End
		}
		out[i].Start = bucketStart.UTC()
		group.Go(func() error {
			var count uint64
			for _, item := range planned {
				start := laterTime(bucketStart, item.phase.start)
				end := earlierTime(bucketEnd, item.phase.end)
				if !end.After(start) {
					continue
				}
				phaseReq := req
				phaseReq.Start, phaseReq.End, phaseReq.Cursor = start, end, ""
				value, countErr := item.searcher.Count(groupCtx, phaseReq)
				if countErr != nil {
					return countErr
				}
				count += value
			}
			out[i].Count = count
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}

func laterTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func earlierTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func (s *Service) plan(ctx context.Context, start, end time.Time, _ logquery.SortDirection) ([]queryPhase, string, error) {
	backend, err := s.repo.ActiveBackend(ctx)
	if err != nil && !errors.Is(err, apperrs.ErrNotFound) {
		return nil, "", err
	}
	if errors.Is(err, apperrs.ErrNotFound) {
		backend = nil
	}
	phases := []queryPhase{buildActiveQueryPhase(start, end, backend)}
	planSum, err := queryPlanSum(phases)
	if err != nil {
		return nil, "", err
	}
	return phases, planSum, nil
}

func buildActiveQueryPhase(start, end time.Time, backend *model.Backend) queryPhase {
	if backend == nil {
		return queryPhase{name: "loki", start: start, end: end}
	}
	return queryPhase{
		name: fmt.Sprintf("elasticsearch:%d", backend.ID), start: start, end: end, backend: backend,
	}
}

func (s *Service) searcherForPhase(ctx context.Context, phase queryPhase) (logquery.Searcher, error) {
	if phase.backend == nil {
		if s.loki == nil {
			return nil, errors.New("current Loki backend is unavailable")
		}
		return s.loki, nil
	}
	return s.elasticsearchClient(ctx, phase.backend)
}

func (s *Service) elasticsearchClient(ctx context.Context, backend *model.Backend) (*logquery.ElasticsearchClient, error) {
	cacheKey := fmt.Sprintf("%d/%d/%s/%s/%s", backend.ID, backend.Generation, backend.QueryEndpoint, backend.QueryCredentialRef, backend.IndexPattern)
	s.mu.RLock()
	if s.cacheKey == cacheKey && s.cachedES != nil {
		client := s.cachedES
		s.mu.RUnlock()
		return client, nil
	}
	s.mu.RUnlock()
	apiKey, err := s.apiKey(ctx, backend.QueryCredentialRef)
	if err != nil {
		return nil, err
	}
	client, err := s.newESClient(backend.QueryEndpoint, backend.IndexPattern, apiKey, backend)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cacheKey, s.cachedES = cacheKey, client
	s.mu.Unlock()
	return client, nil
}

func queryPlanSum(phases []queryPhase) (string, error) {
	type fingerprint struct {
		Name       string    `json:"name"`
		Start      time.Time `json:"start"`
		End        time.Time `json:"end"`
		Generation uint64    `json:"generation,omitempty"`
	}
	items := make([]fingerprint, 0, len(phases))
	for _, phase := range phases {
		item := fingerprint{Name: phase.name, Start: phase.start, End: phase.end}
		if phase.backend != nil {
			item.Generation = phase.backend.Generation
		}
		items = append(items, item)
	}
	body, err := json.Marshal(items)
	if err != nil {
		return "", fmt.Errorf("encode log query plan fingerprint: %w", err)
	}
	sum := sha256.Sum256(body)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func phasePosition(phases []queryPhase, name string) int {
	for i := range phases {
		if phases[i].name == name {
			return i
		}
	}
	return -1
}

func plannerRequestSum(req logquery.SearchRequest) (string, error) {
	req.Cursor = ""
	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("encode log query fingerprint: %w", err)
	}
	sum := sha256.Sum256(body)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func encodePlannerCursor(cursor plannerCursor) (string, error) {
	body, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("encode log query cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(body), nil
}

func decodePlannerCursor(raw string, cursor *plannerCursor) error {
	body, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(body) > 8192 || json.Unmarshal(body, cursor) != nil {
		return errors.New("logquery: invalid cursor")
	}
	return nil
}

func appendUnique(base []string, values ...string) []string {
	seen := make(map[string]struct{}, len(base)+len(values))
	for _, value := range base {
		seen[value] = struct{}{}
	}
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		base = append(base, value)
	}
	return base
}
