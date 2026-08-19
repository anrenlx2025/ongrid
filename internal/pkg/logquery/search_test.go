package logquery

import (
	"strings"
	"testing"
	"time"
)

func validSearchRequest() SearchRequest {
	end := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	return SearchRequest{Start: end.Add(-time.Hour), End: end}
}

func TestSearchRequestNormalizeAndValidate_Defaults(t *testing.T) {
	req := validSearchRequest()
	if err := req.NormalizeAndValidate(); err != nil {
		t.Fatalf("NormalizeAndValidate() error = %v", err)
	}
	if req.Limit != DefaultSearchLimit {
		t.Fatalf("Limit = %d, want %d", req.Limit, DefaultSearchLimit)
	}
	if req.Direction != SortBackward {
		t.Fatalf("Direction = %q, want %q", req.Direction, SortBackward)
	}
	if req.Keywords.Mode != MatchAny {
		t.Fatalf("Match mode = %q, want %q", req.Keywords.Mode, MatchAny)
	}
}

func TestSearchRequestNormalizeAndValidate_RejectsUnsafeInputs(t *testing.T) {
	cases := []struct {
		name string
		edit func(*SearchRequest)
		want string
	}{
		{"wide_window", func(r *SearchRequest) { r.Start = r.End.Add(-31 * 24 * time.Hour) }, "time window"},
		{"zero_device", func(r *SearchRequest) { r.Scope.DeviceIDs = []uint64{0} }, "device_id"},
		{"unknown_field", func(r *SearchRequest) {
			r.Filters = []FieldFilter{{Field: "_index", Operator: FilterEqual, Values: []string{"secret"}}}
		}, "not allowed"},
		{"bad_limit", func(r *SearchRequest) { r.Limit = MaxSearchLimit + 1 }, "limit"},
		{"equal_requires_one_value", func(r *SearchRequest) {
			r.Filters = []FieldFilter{{Field: "service_name", Operator: FilterEqual, Values: []string{"api", "worker"}}}
		}, "exactly one"},
		{"bad_cursor", func(r *SearchRequest) { r.Cursor = "not-base64***" }, "invalid cursor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := validSearchRequest()
			tc.edit(&req)
			err := req.NormalizeAndValidate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestAllowedFields_DoNotExposeElasticsearchIndexControls(t *testing.T) {
	for _, field := range AllowedFields() {
		if strings.HasPrefix(field.Name, "_") || field.Name == "elasticsearch.index" {
			t.Fatalf("unsafe field exposed: %q", field.Name)
		}
	}
}
