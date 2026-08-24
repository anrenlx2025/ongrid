package logs

import (
	"context"
	"errors"
	"fmt"
	"time"

	model "github.com/ongridio/ongrid/internal/manager/model/logs"
	apperrs "github.com/ongridio/ongrid/internal/pkg/errs"
	"github.com/ongridio/ongrid/internal/pkg/logquery"
)

const maxHistogramBuckets = 500

func (s *Service) Search(ctx context.Context, req logquery.SearchRequest) (*logquery.SearchResult, error) {
	if err := req.NormalizeAndValidate(); err != nil {
		return nil, err
	}
	searcher, err := s.selectedSearcher(ctx)
	if err != nil {
		return nil, err
	}
	// Product search windows own (start, end]. Backend search APIs use an
	// inclusive lower bound, so exclude the start boundary explicitly.
	req.Start = req.Start.Add(time.Nanosecond)
	return searcher.Search(ctx, req)
}

// CloseCursor releases backend resources when a caller abandons pagination.
// It is safe for the built-in Loki path, whose cursors are stateless.
func (s *Service) CloseCursor(ctx context.Context, cursor string) error {
	searcher, err := s.selectedSearcher(ctx)
	if err != nil {
		return err
	}
	return logquery.CloseCursor(ctx, searcher, cursor)
}

// Count uses only the selected backend. Retained data in an inactive backend
// is intentionally outside the product query surface.
func (s *Service) Count(ctx context.Context, req logquery.SearchRequest) (uint64, error) {
	if err := req.NormalizeAndValidate(); err != nil {
		return 0, err
	}
	searcher, err := s.selectedSearcher(ctx)
	if err != nil {
		return 0, err
	}
	req.Cursor = ""
	return searcher.Count(ctx, req)
}

func (s *Service) Fields(_ context.Context, _, _ time.Time, _ logquery.Scope) ([]logquery.Field, error) {
	return logquery.AllowedFields(), nil
}

func (s *Service) FieldValues(ctx context.Context, req logquery.FieldValuesRequest) ([]string, error) {
	if err := req.NormalizeAndValidate(); err != nil {
		return nil, err
	}
	searcher, err := s.selectedSearcher(ctx)
	if err != nil {
		return nil, err
	}
	return searcher.FieldValues(ctx, req)
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
	searcher, err := s.selectedSearcher(ctx)
	if err != nil {
		return nil, err
	}
	req.Cursor = ""
	buckets, err := searcher.Histogram(ctx, req, interval)
	if err != nil {
		return nil, err
	}

	// Backend adapters return buckets aligned to the request start. Normalize
	// sparse backend results onto the product grid so both backends expose the
	// same zero-filled bucket positions.
	out := make([]logquery.HistogramBucket, bucketCount)
	for i := range out {
		out[i].Start = req.Start.Add(time.Duration(i) * interval).UTC()
	}
	for _, bucket := range buckets {
		delta := bucket.Start.Sub(req.Start)
		if delta < 0 || delta%interval != 0 {
			return nil, fmt.Errorf("logquery: backend histogram bucket %s is not aligned to request start %s", bucket.Start, req.Start)
		}
		index := int(delta / interval)
		// A record exactly at an interval-aligned end belongs to the final
		// product bucket even when Elasticsearch returns a bucket keyed by end.
		if index == len(out) && bucket.Start.Equal(req.End) {
			out[len(out)-1].Count += bucket.Count
			continue
		}
		if index >= len(out) {
			return nil, fmt.Errorf("logquery: backend histogram bucket %s is outside the request window", bucket.Start)
		}
		out[index].Count += bucket.Count
	}
	return out, nil
}

func (s *Service) selectedBackend(ctx context.Context) (*model.Backend, error) {
	backend, err := s.repo.SelectedBackend(ctx)
	if errors.Is(err, apperrs.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return backend, nil
}

func (s *Service) selectedSearcher(ctx context.Context) (logquery.Searcher, error) {
	backend, err := s.selectedBackend(ctx)
	if err != nil {
		return nil, err
	}
	if backend == nil {
		if s.loki == nil {
			return nil, errors.New("current Loki backend is unavailable")
		}
		return s.loki, nil
	}
	return s.elasticsearchClient(ctx, backend)
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
