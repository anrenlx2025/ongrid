package logs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ongridio/ongrid/internal/pkg/logquery"
)

type idleStructuredSearcher struct{}

func (idleStructuredSearcher) Search(context.Context, logquery.SearchRequest) (*logquery.SearchResult, error) {
	return &logquery.SearchResult{}, nil
}

func (idleStructuredSearcher) Count(context.Context, logquery.SearchRequest) (uint64, error) {
	return 0, nil
}

func (idleStructuredSearcher) Fields(context.Context, time.Time, time.Time, logquery.Scope) ([]logquery.Field, error) {
	return nil, nil
}

func (idleStructuredSearcher) FieldValues(context.Context, logquery.FieldValuesRequest) ([]string, error) {
	return nil, nil
}

func (idleStructuredSearcher) Histogram(context.Context, logquery.SearchRequest, time.Duration) ([]logquery.HistogramBucket, error) {
	return nil, nil
}

func TestSearchLogsRejectsRequestsAboveConcurrencyLimit(t *testing.T) {
	handler := NewHandlerWithSearcher(nil, idleStructuredSearcher{})
	for range maxConcurrentStructuredSearches {
		if !handler.acquireSearchSlot() {
			t.Fatal("failed to reserve search slot")
		}
	}
	t.Cleanup(func() {
		for range maxConcurrentStructuredSearches {
			handler.releaseSearchSlot()
		}
	})

	end := time.Now().UTC()
	body := `{"start":"` + end.Add(-time.Hour).Format(time.RFC3339Nano) + `","end":"` + end.Format(time.RFC3339Nano) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/logs/search", strings.NewReader(body))
	rec := httptest.NewRecorder()
	backendTestRouter(handler).ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") != "1" || !strings.Contains(rec.Body.String(), "LOG_QUERY_BUSY") {
		t.Fatalf("headers=%v body=%s", rec.Header(), rec.Body.String())
	}
}
