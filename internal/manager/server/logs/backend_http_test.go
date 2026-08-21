package logs

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	bizlogs "github.com/ongridio/ongrid/internal/manager/biz/logs"
	logsmodel "github.com/ongridio/ongrid/internal/manager/model/logs"
	"github.com/ongridio/ongrid/internal/pkg/tenantctx"
)

type stubBackendService struct {
	saved   *bizlogs.SaveInput
	applied bool
}

func (s *stubBackendService) Get(context.Context) (*bizlogs.BackendView, error) {
	return &bizlogs.BackendView{ID: 7, Type: logsmodel.BackendTypeElasticsearch, Status: logsmodel.BackendStatusSaved}, nil
}

func (s *stubBackendService) Save(_ context.Context, input bizlogs.SaveInput) (*bizlogs.BackendView, error) {
	s.saved = &input
	return &bizlogs.BackendView{ID: 7, Dataset: input.Dataset, Status: logsmodel.BackendStatusSaved}, nil
}

func (s *stubBackendService) Apply(context.Context, uint64) (*bizlogs.BackendView, error) {
	s.applied = true
	return &bizlogs.BackendView{ID: 7, Status: logsmodel.BackendStatusDistributing}, nil
}

func (s *stubBackendService) Rollback(context.Context, uint64) (*bizlogs.BackendView, error) {
	return &bizlogs.BackendView{ID: 7, Status: logsmodel.BackendStatusRolledBack}, nil
}

func TestBackendRoutesRequireAdministrator(t *testing.T) {
	svc := &stubBackendService{}
	router := backendTestRouter(NewHandlerWithServices(nil, nil, svc))

	anon := httptest.NewRequest(http.MethodGet, "/v1/logs/backend", nil)
	anonRec := httptest.NewRecorder()
	router.ServeHTTP(anonRec, anon)
	if anonRec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d body=%s", anonRec.Code, anonRec.Body.String())
	}

	user := httptest.NewRequest(http.MethodGet, "/v1/logs/backend", nil)
	user = user.WithContext(tenantctx.With(user.Context(), tenantctx.Tenant{UserID: 2, Role: "user"}))
	userRec := httptest.NewRecorder()
	router.ServeHTTP(userRec, user)
	if userRec.Code != http.StatusForbidden {
		t.Fatalf("user status = %d body=%s", userRec.Code, userRec.Body.String())
	}
}

func TestPutBackendUsesStrictJSONAndCallsService(t *testing.T) {
	svc := &stubBackendService{}
	router := backendTestRouter(NewHandlerWithServices(nil, nil, svc))

	bad := adminBackendRequest(http.MethodPut, "/v1/logs/backend", []byte(`{
		"write_endpoints":["https://es.example"],
		"write_credential_ref":"write",
		"query_credential_ref":"query",
		"unknown":true
	}`))
	badRec := httptest.NewRecorder()
	router.ServeHTTP(badRec, bad)
	if badRec.Code != http.StatusBadRequest || svc.saved != nil {
		t.Fatalf("unknown field status=%d saved=%+v body=%s", badRec.Code, svc.saved, badRec.Body.String())
	}

	good := adminBackendRequest(http.MethodPut, "/v1/logs/backend", []byte(`{
		"write_endpoints":["https://es.example"],
		"query_endpoint":"https://es-query.example",
		"dataset":"ongrid.system",
		"namespace":"prod",
		"write_api_key":"direct-write-value",
		"query_api_key":"direct-query-value"
	}`))
	goodRec := httptest.NewRecorder()
	router.ServeHTTP(goodRec, good)
	if goodRec.Code != http.StatusOK {
		t.Fatalf("valid status=%d body=%s", goodRec.Code, goodRec.Body.String())
	}
	if svc.saved == nil || svc.saved.Dataset != "ongrid.system" || svc.saved.Namespace != "prod" ||
		svc.saved.WriteAPIKey != "direct-write-value" || svc.saved.QueryAPIKey != "direct-query-value" {
		t.Fatalf("saved input = %+v", svc.saved)
	}
	if bytes.Contains(goodRec.Body.Bytes(), []byte("direct-write-value")) || bytes.Contains(goodRec.Body.Bytes(), []byte("direct-query-value")) {
		t.Fatalf("response leaked a direct API key: %s", goodRec.Body.String())
	}
}

func TestBackendActionRejectsInvalidID(t *testing.T) {
	router := backendTestRouter(NewHandlerWithServices(nil, nil, &stubBackendService{}))
	req := adminBackendRequest(http.MethodPost, "/v1/logs/backend/not-a-number/apply", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestApplyBackendCallsService(t *testing.T) {
	svc := &stubBackendService{}
	router := backendTestRouter(NewHandlerWithServices(nil, nil, svc))
	req := adminBackendRequest(http.MethodPost, "/v1/logs/backend/7/apply", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !svc.applied {
		t.Fatal("Apply was not called")
	}
}

func backendTestRouter(handler *Handler) http.Handler {
	router := chi.NewRouter()
	handler.Register(router)
	return router
}

func adminBackendRequest(method, path string, body []byte) *http.Request {
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	return request.WithContext(tenantctx.With(request.Context(), tenantctx.Tenant{UserID: 1, Role: "admin"}))
}
