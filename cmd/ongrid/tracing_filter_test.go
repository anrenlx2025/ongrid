package main

import (
	"net/http/httptest"
	"testing"
)

func TestShouldTraceHTTPRequest(t *testing.T) {
	tests := map[string]bool{
		"/healthz":                        false,
		"/readyz":                         false,
		"/api/v1/prometheus/auth":         false,
		"/api/v1/traces":                  false,
		"/api/v1/traces/search":           false,
		"/internal/auth/dataPlane-verify": true,
		"/api/v1/aiops/sessions":          true,
	}
	for path, want := range tests {
		if got := shouldTraceHTTPRequest(httptest.NewRequest("GET", path, nil)); got != want {
			t.Errorf("shouldTraceHTTPRequest(%q) = %v, want %v", path, got, want)
		}
	}
}
