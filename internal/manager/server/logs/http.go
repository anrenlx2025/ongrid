// Package logs proxies Loki query API to authenticated SPA users so the
// Logs page can run LogQL against the embedded Loki without exposing
// /loki/* through nginx for queries (the data plane /loki/api/v1/push
// route is auth_request-gated for ingest only —).
//
// Routes mounted under /api/v1 by cmd/ongrid/main.go:
//
//	GET /v1/logs/query_range — proxy /loki/api/v1/query_range
//	GET /v1/logs/labels — proxy /loki/api/v1/labels
//	GET /v1/logs/labels/{name}/values — proxy /loki/api/v1/label/<name>/values
package logs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	bizlogs "github.com/ongridio/ongrid/internal/manager/biz/logs"
	"github.com/ongridio/ongrid/internal/pkg/errs"
	"github.com/ongridio/ongrid/internal/pkg/logquery"
	"github.com/ongridio/ongrid/internal/pkg/tenantctx"
)

const maxConcurrentStructuredSearches = 4

// Querier is the narrow surface this handler needs. *logquery.Client
// satisfies it.
type Querier interface {
	QueryRange(ctx context.Context, opts logquery.QueryRangeOptions) (*logquery.QueryRangeResult, error)
	LabelNames(ctx context.Context, start, end time.Time) ([]string, error)
	LabelValues(ctx context.Context, name string, start, end time.Time) ([]string, error)
}

// Handler exposes the auth'd Loki query proxy. Requires an underlying
// Querier; when nil the routes return 503 so the SPA can show a clear
// "logs disabled" state instead of failing silently.
type Handler struct {
	q           Querier
	search      logquery.Searcher
	backend     BackendService
	searchSlots chan struct{}
}

type BackendService interface {
	Get(ctx context.Context) (*bizlogs.BackendView, error)
	SaveDraft(ctx context.Context, input bizlogs.SaveInput) (*bizlogs.BackendView, error)
	Test(ctx context.Context, id uint64) (*bizlogs.BackendView, error)
	Activate(ctx context.Context, id uint64, input bizlogs.ActivationInput) (*bizlogs.BackendView, error)
	Rollback(ctx context.Context, id uint64) (*bizlogs.BackendView, error)
}

// NewHandler builds the handler. q may be nil when Loki is disabled.
func NewHandler(q Querier) *Handler {
	h := newHandler(q, nil, nil)
	if searcher, ok := q.(logquery.Searcher); ok {
		h.search = searcher
	}
	return h
}

// NewHandlerWithSearcher wires a backend-neutral search implementation while
// optionally retaining a Loki querier for the legacy LogQL endpoints.
func NewHandlerWithSearcher(q Querier, searcher logquery.Searcher) *Handler {
	return newHandler(q, searcher, nil)
}

// NewHandlerWithServices wires the backend-neutral query surface and the
// administrator-only Elasticsearch backend lifecycle API.
func NewHandlerWithServices(q Querier, searcher logquery.Searcher, backend BackendService) *Handler {
	return newHandler(q, searcher, backend)
}

func newHandler(q Querier, searcher logquery.Searcher, backend BackendService) *Handler {
	return &Handler{
		q: q, search: searcher, backend: backend,
		searchSlots: make(chan struct{}, maxConcurrentStructuredSearches),
	}
}

// Register attaches routes on r. Caller must wrap r in the auth
// middleware before calling — this handler trusts the caller is
// authenticated (any role; logs are not org-scoped post-pivot).
func (h *Handler) Register(r chi.Router) {
	r.Post("/v1/logs/search", h.searchLogs)
	r.Get("/v1/logs/fields", h.fields)
	r.Post("/v1/logs/field-values", h.fieldValues)
	r.Post("/v1/logs/histogram", h.histogram)
	r.Post("/v1/logs/context", h.contextLogs)
	r.Get("/v1/logs/backend", h.getBackend)
	r.Put("/v1/logs/backend", h.putBackend)
	r.Post("/v1/logs/backend/{id}/test", h.testBackend)
	r.Post("/v1/logs/backend/{id}/activate", h.activateBackend)
	r.Post("/v1/logs/backend/{id}/rollback", h.rollbackBackend)
	r.Get("/v1/logs/query_range", h.queryRange)
	r.Get("/v1/logs/labels", h.labels)
	r.Get("/v1/logs/labels/{name}/values", h.labelValues)
}

// getBackend godoc
// @Summary Get the configured external log backend
// @Router /api/v1/logs/backend [get]
// @Success 200 {object} apiEnvelope
func (h *Handler) getBackend(w http.ResponseWriter, r *http.Request) {
	if !requireBackendAdmin(w, r) {
		return
	}
	if h.backend == nil {
		writeAPIErr(w, http.StatusServiceUnavailable, "LOG_BACKEND_DISABLED", "log backend management disabled")
		return
	}
	out, err := h.backend.Get(r.Context())
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, apiEnvelope{Code: http.StatusOK, Message: "success", Data: out})
}

// putBackend godoc
// @Summary Create or update an Elasticsearch log backend draft
// @Router /api/v1/logs/backend [put]
// @Success 200 {object} apiEnvelope
func (h *Handler) putBackend(w http.ResponseWriter, r *http.Request) {
	if !requireBackendAdmin(w, r) {
		return
	}
	if h.backend == nil {
		writeAPIErr(w, http.StatusServiceUnavailable, "LOG_BACKEND_DISABLED", "log backend management disabled")
		return
	}
	var input bizlogs.SaveInput
	if err := decodeJSONBody(r, &input); err != nil {
		writeAPIErr(w, http.StatusBadRequest, "LOG_BACKEND_INVALID", "invalid log backend request")
		return
	}
	out, err := h.backend.SaveDraft(r.Context(), input)
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, apiEnvelope{Code: http.StatusOK, Message: "success", Data: out})
}

// testBackend godoc
// @Summary Test Elasticsearch read and write endpoint credentials
// @Router /api/v1/logs/backend/{id}/test [post]
// @Success 200 {object} apiEnvelope
func (h *Handler) testBackend(w http.ResponseWriter, r *http.Request) {
	h.backendAction(w, r, "test")
}

// activateBackend godoc
// @Summary Activate an Elasticsearch log backend generation
// @Router /api/v1/logs/backend/{id}/activate [post]
// @Success 200 {object} apiEnvelope
func (h *Handler) activateBackend(w http.ResponseWriter, r *http.Request) {
	h.backendAction(w, r, "activate")
}

// rollbackBackend godoc
// @Summary Cancel a candidate rollout or roll an active backend back to Loki
// @Router /api/v1/logs/backend/{id}/rollback [post]
// @Success 200 {object} apiEnvelope
func (h *Handler) rollbackBackend(w http.ResponseWriter, r *http.Request) {
	h.backendAction(w, r, "rollback")
}

func (h *Handler) backendAction(w http.ResponseWriter, r *http.Request, action string) {
	if !requireBackendAdmin(w, r) {
		return
	}
	if h.backend == nil {
		writeAPIErr(w, http.StatusServiceUnavailable, "LOG_BACKEND_DISABLED", "log backend management disabled")
		return
	}
	id, err := strconv.ParseUint(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id == 0 {
		writeAPIErr(w, http.StatusBadRequest, "LOG_BACKEND_INVALID", "invalid backend id")
		return
	}
	var out *bizlogs.BackendView
	switch action {
	case "test":
		out, err = h.backend.Test(r.Context(), id)
	case "activate":
		var input bizlogs.ActivationInput
		if decodeErr := decodeOptionalJSONBody(r, &input); decodeErr != nil {
			err = fmt.Errorf("%w: invalid activation request", errs.ErrInvalid)
		} else {
			out, err = h.backend.Activate(r.Context(), id, input)
		}
	case "rollback":
		out, err = h.backend.Rollback(r.Context(), id)
	default:
		err = errs.ErrInvalid
	}
	if err != nil {
		writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, apiEnvelope{Code: http.StatusOK, Message: "success", Data: out})
}

type apiEnvelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type histogramRequest struct {
	Search   logquery.SearchRequest `json:"search"`
	Interval string                 `json:"interval"`
}

type contextRequest struct {
	Timestamp time.Time      `json:"timestamp"`
	Scope     logquery.Scope `json:"scope,omitempty"`
	Before    int            `json:"before,omitempty"`
	After     int            `json:"after,omitempty"`
}

// searchLogs godoc
// @Summary Search logs with backend-neutral filters
// @Router /api/v1/logs/search [post]
// @Success 200 {object} apiEnvelope
func (h *Handler) searchLogs(w http.ResponseWriter, r *http.Request) {
	if h.search == nil {
		writeAPIErr(w, http.StatusServiceUnavailable, "LOGS_BACKEND_DISABLED", "logs backend disabled")
		return
	}
	var in logquery.SearchRequest
	if err := decodeJSONBody(r, &in); err != nil {
		writeAPIErr(w, http.StatusBadRequest, "LOG_QUERY_INVALID", "invalid log search request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if !h.acquireSearchSlot() {
		w.Header().Set("Retry-After", "1")
		writeAPIErr(w, http.StatusTooManyRequests, "LOG_QUERY_BUSY", "too many concurrent log searches")
		return
	}
	defer h.releaseSearchSlot()
	out, err := h.search.Search(ctx, in)
	if err != nil {
		writeSearchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, apiEnvelope{Code: http.StatusOK, Message: "success", Data: out})
}

func (h *Handler) acquireSearchSlot() bool {
	if h.searchSlots == nil {
		return true
	}
	select {
	case h.searchSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (h *Handler) releaseSearchSlot() {
	if h.searchSlots != nil {
		<-h.searchSlots
	}
}

// fields godoc
// @Summary List product-supported log fields
// @Router /api/v1/logs/fields [get]
// @Success 200 {object} apiEnvelope
func (h *Handler) fields(w http.ResponseWriter, r *http.Request) {
	if h.search == nil {
		writeAPIErr(w, http.StatusServiceUnavailable, "LOGS_BACKEND_DISABLED", "logs backend disabled")
		return
	}
	start, end, err := parseOptionalRange(r)
	if err != nil {
		writeAPIErr(w, http.StatusBadRequest, "LOG_QUERY_INVALID", "invalid time range")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	out, err := h.search.Fields(ctx, start, end, logquery.Scope{})
	if err != nil {
		writeSearchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, apiEnvelope{Code: http.StatusOK, Message: "success", Data: out})
}

// fieldValues godoc
// @Summary List values for an allowed log field
// @Router /api/v1/logs/field-values [post]
// @Success 200 {object} apiEnvelope
func (h *Handler) fieldValues(w http.ResponseWriter, r *http.Request) {
	if h.search == nil {
		writeAPIErr(w, http.StatusServiceUnavailable, "LOGS_BACKEND_DISABLED", "logs backend disabled")
		return
	}
	var in logquery.FieldValuesRequest
	if err := decodeJSONBody(r, &in); err != nil {
		writeAPIErr(w, http.StatusBadRequest, "LOG_QUERY_INVALID", "invalid field values request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	out, err := h.search.FieldValues(ctx, in)
	if err != nil {
		writeSearchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, apiEnvelope{Code: http.StatusOK, Message: "success", Data: out})
}

// histogram godoc
// @Summary Aggregate matching logs into time buckets
// @Router /api/v1/logs/histogram [post]
// @Success 200 {object} apiEnvelope
func (h *Handler) histogram(w http.ResponseWriter, r *http.Request) {
	if h.search == nil {
		writeAPIErr(w, http.StatusServiceUnavailable, "LOGS_BACKEND_DISABLED", "logs backend disabled")
		return
	}
	var in histogramRequest
	if err := decodeJSONBody(r, &in); err != nil {
		writeAPIErr(w, http.StatusBadRequest, "LOG_QUERY_INVALID", "invalid histogram request")
		return
	}
	interval, err := time.ParseDuration(in.Interval)
	if err != nil || interval <= 0 {
		writeAPIErr(w, http.StatusBadRequest, "LOG_QUERY_INVALID", "invalid histogram interval")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	out, err := h.search.Histogram(ctx, in.Search, interval)
	if err != nil {
		writeSearchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, apiEnvelope{Code: http.StatusOK, Message: "success", Data: out})
}

// contextLogs godoc
// @Summary Read log records immediately before and after a timestamp
// @Router /api/v1/logs/context [post]
// @Success 200 {object} apiEnvelope
func (h *Handler) contextLogs(w http.ResponseWriter, r *http.Request) {
	if h.search == nil {
		writeAPIErr(w, http.StatusServiceUnavailable, "LOGS_BACKEND_DISABLED", "logs backend disabled")
		return
	}
	var in contextRequest
	if err := decodeJSONBody(r, &in); err != nil || in.Timestamp.IsZero() {
		writeAPIErr(w, http.StatusBadRequest, "LOG_QUERY_INVALID", "invalid log context request")
		return
	}
	if in.Before == 0 {
		in.Before = 50
	}
	if in.After == 0 {
		in.After = 50
	}
	if in.Before < 0 || in.Before > 100 || in.After < 0 || in.After > 100 {
		writeAPIErr(w, http.StatusBadRequest, "LOG_QUERY_INVALID", "context limits must be between 0 and 100")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	before, err := h.search.Search(ctx, logquery.SearchRequest{
		Start: in.Timestamp.Add(-15 * time.Minute), End: in.Timestamp,
		Scope: in.Scope, Limit: max(in.Before, 1), Direction: logquery.SortBackward,
	})
	if err != nil {
		writeSearchError(w, err)
		return
	}
	after, err := h.search.Search(ctx, logquery.SearchRequest{
		Start: in.Timestamp, End: in.Timestamp.Add(15 * time.Minute),
		Scope: in.Scope, Limit: max(in.After, 1), Direction: logquery.SortForward,
	})
	if err != nil {
		writeSearchError(w, err)
		return
	}
	records := append(before.Records, after.Records...)
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].Timestamp.Equal(records[j].Timestamp) {
			return records[i].ID < records[j].ID
		}
		return records[i].Timestamp.Before(records[j].Timestamp)
	})
	writeJSON(w, http.StatusOK, apiEnvelope{Code: http.StatusOK, Message: "success", Data: records})
}

type queryRangeResp struct {
	ResultType string          `json:"resultType"`
	Result     json.RawMessage `json:"result"`
	From       string          `json:"from"`
	To         string          `json:"to"`
}

func (h *Handler) queryRange(w http.ResponseWriter, r *http.Request) {
	if h.q == nil {
		writeErr(w, http.StatusServiceUnavailable, "logs backend disabled")
		return
	}
	q := r.URL.Query()
	from, err := parseTime(q.Get("start"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("start: %v", err))
		return
	}
	to, err := parseTime(q.Get("end"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("end: %v", err))
		return
	}
	limit := 1000
	if s := q.Get("limit"); s != "" {
		n, perr := strconv.Atoi(s)
		if perr != nil || n <= 0 || n > 5000 {
			writeErr(w, http.StatusBadRequest, "limit must be 1..5000")
			return
		}
		limit = n
	}
	var step time.Duration
	if s := q.Get("step"); s != "" {
		d, perr := time.ParseDuration(s)
		if perr != nil || d <= 0 {
			writeErr(w, http.StatusBadRequest, "step must be a positive duration")
			return
		}
		step = d
	}
	dir := q.Get("direction")
	if dir != "" && dir != "forward" && dir != "backward" {
		writeErr(w, http.StatusBadRequest, "direction must be forward|backward")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	out, err := h.q.QueryRange(ctx, logquery.QueryRangeOptions{
		Query:     q.Get("query"),
		Start:     from,
		End:       to,
		Limit:     limit,
		Step:      step,
		Direction: dir,
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, queryRangeResp{
		ResultType: out.ResultType,
		Result:     out.Result,
		From:       from.UTC().Format(time.RFC3339),
		To:         to.UTC().Format(time.RFC3339),
	})
}

func (h *Handler) labels(w http.ResponseWriter, r *http.Request) {
	if h.q == nil {
		writeErr(w, http.StatusServiceUnavailable, "logs backend disabled")
		return
	}
	q := r.URL.Query()
	from, _ := parseTime(q.Get("start"))
	to, _ := parseTime(q.Get("end"))

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	out, err := h.q.LabelNames(ctx, from, to)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"labels": out})
}

func (h *Handler) labelValues(w http.ResponseWriter, r *http.Request) {
	if h.q == nil {
		writeErr(w, http.StatusServiceUnavailable, "logs backend disabled")
		return
	}
	name := chi.URLParam(r, "name")
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name required")
		return
	}
	q := r.URL.Query()
	from, _ := parseTime(q.Get("start"))
	to, _ := parseTime(q.Get("end"))

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	out, err := h.q.LabelValues(ctx, name, from, to)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"values": out})
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("missing")
	}
	// Accept RFC3339 + unix-seconds-as-string for easy curl testing.
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		// >1e12 = millis; >1e15 = nanos; smaller = seconds.
		switch {
		case n > 1e15:
			return time.Unix(0, n), nil
		case n > 1e12:
			return time.UnixMilli(n), nil
		default:
			return time.Unix(n, 0), nil
		}
	}
	return time.Time{}, errs.ErrInvalid
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decodeJSONBody(r *http.Request, dst any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func decodeOptionalJSONBody(r *http.Request, dst any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func parseOptionalRange(r *http.Request) (time.Time, time.Time, error) {
	now := time.Now().UTC()
	start := now.Add(-time.Hour)
	end := now
	var err error
	if raw := r.URL.Query().Get("start"); raw != "" {
		start, err = parseTime(raw)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	if raw := r.URL.Query().Get("end"); raw != "" {
		end, err = parseTime(raw)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	if !end.After(start) || end.Sub(start) > logquery.MaxSearchWindow {
		return time.Time{}, time.Time{}, errors.New("invalid time range")
	}
	return start, end, nil
}

func writeSearchError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		writeAPIErr(w, http.StatusGatewayTimeout, "LOG_QUERY_TIMEOUT", "log query timed out")
		return
	}
	message := err.Error()
	for _, marker := range []string{
		"required", "must be", "exceeds", "not allowed", "unsupported", "invalid cursor",
		"too many", "time window", "does not fit", "aggregatable",
	} {
		if strings.Contains(message, marker) {
			writeAPIErr(w, http.StatusBadRequest, "LOG_QUERY_INVALID", message)
			return
		}
	}
	writeAPIErr(w, http.StatusBadGateway, "LOG_BACKEND_ERROR", "logs backend request failed")
}

func writeAPIErr(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"code":    code,
		"message": message,
		"data":    nil,
	})
}

func requireBackendAdmin(w http.ResponseWriter, r *http.Request) bool {
	caller, ok := tenantctx.From(r.Context())
	if !ok {
		writeAPIErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return false
	}
	if caller.Role != "admin" {
		writeAPIErr(w, http.StatusForbidden, "FORBIDDEN", "administrator role required")
		return false
	}
	return true
}

func writeBackendError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errs.ErrInvalid):
		writeAPIErr(w, http.StatusBadRequest, "LOG_BACKEND_INVALID", err.Error())
	case errors.Is(err, errs.ErrNotFound):
		writeAPIErr(w, http.StatusNotFound, "LOG_BACKEND_NOT_FOUND", "log backend not found")
	case errors.Is(err, errs.ErrConflict):
		writeAPIErr(w, http.StatusConflict, "LOG_BACKEND_CONFLICT", err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		writeAPIErr(w, http.StatusGatewayTimeout, "LOG_BACKEND_TIMEOUT", "log backend operation timed out")
	default:
		writeAPIErr(w, http.StatusBadGateway, "LOG_BACKEND_ERROR", "log backend operation failed")
	}
}
