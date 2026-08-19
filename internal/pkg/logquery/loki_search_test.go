package logquery

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLokiCountUsesInstantStructuredQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/query" {
			http.NotFound(w, r)
			return
		}
		query := r.URL.Query().Get("query")
		for _, want := range []string{`sum(count_over_time(`, `namespace="prod"`, `|~ "(?i)panic"`, `[5m]))`} {
			if !strings.Contains(query, want) {
				t.Fatalf("query = %q, missing %q", query, want)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{"resultType": "vector", "result": []any{
				map[string]any{"metric": map[string]string{}, "value": []any{1787054400, "7"}},
			}},
		})
	}))
	t.Cleanup(server.Close)
	client := NewWithHTTPClient(server.URL, server.Client(), nil)
	req := validSearchRequest()
	req.Start = req.End.Add(-5 * time.Minute)
	req.Scope.Namespaces = []string{"prod"}
	req.Keywords = Keywords{Include: []string{"panic"}, Mode: MatchPhrase}
	count, err := client.Count(t.Context(), req)
	if err != nil || count != 7 {
		t.Fatalf("Count() = %d, %v", count, err)
	}
}

func TestCompileLogQL_MapsScopeAndKeywords(t *testing.T) {
	req := validSearchRequest()
	req.Scope = Scope{
		DeviceIDs:    []uint64{42},
		Namespaces:   []string{"prod"},
		Nodes:        []string{"node-a"},
		ServiceNames: []string{"api", "worker"},
		Files:        []string{"/var/log/app.log"},
		Units:        []string{"sshd.service"},
	}
	req.Filters = []FieldFilter{{Field: "message", Operator: FilterIn, Values: []string{"connection refused", "broken pipe"}}}
	req.Keywords = Keywords{Include: []string{"timeout", "refused"}, Exclude: []string{"healthcheck"}, Mode: MatchAny}
	if err := req.NormalizeAndValidate(); err != nil {
		t.Fatalf("NormalizeAndValidate() error = %v", err)
	}
	got, err := compileLogQL(req)
	if err != nil {
		t.Fatalf("compileLogQL() error = %v", err)
	}
	for _, want := range []string{
		`device_id="42"`,
		`namespace="prod"`,
		`| node="node-a"`,
		`service_name=~"(?:api|worker)"`,
		`| filename="/var/log/app.log"`,
		`| unit="sshd.service"`,
		`|~ "(?i)(connection refused|broken pipe)"`,
		`|~ "(?i)(timeout|refused)"`,
		`!~ "(?i)healthcheck"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("compileLogQL() = %q, missing %q", got, want)
		}
	}
}

func TestDecodeLokiRecords_ProducesStableRecord(t *testing.T) {
	raw, err := json.Marshal([]map[string]any{{
		"stream": map[string]string{
			"device_id": "42", "ongrid_source": "journald", "level": "ERROR",
		},
		"values": []any{[]any{
			"1787054400000000000", "connection refused",
			map[string]string{"filename": "/var/log/app.log", "trace_id": "abc123"},
		}},
	}})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	result := &QueryRangeResult{ResultType: "streams", Result: raw}
	first, err := decodeLokiRecords(result)
	if err != nil {
		t.Fatalf("decodeLokiRecords() error = %v", err)
	}
	second, err := decodeLokiRecords(result)
	if err != nil {
		t.Fatalf("decodeLokiRecords() second error = %v", err)
	}
	if len(first) != 1 || first[0].ID == "" || first[0].ID != second[0].ID {
		t.Fatalf("records do not have a stable id: %#v %#v", first, second)
	}
	if first[0].Timestamp.IsZero() || first[0].Message != "connection refused" || first[0].SeverityText != "ERROR" {
		t.Fatalf("decoded record = %#v", first[0])
	}
	if first[0].ResourceAttributes["device_id"] != "42" {
		t.Fatalf("resource attributes = %#v", first[0].ResourceAttributes)
	}
	if first[0].Attributes["filename"] != "/var/log/app.log" || first[0].TraceID != "abc123" {
		t.Fatalf("structured metadata = %#v trace=%q", first[0].Attributes, first[0].TraceID)
	}
}

func TestLokiSearchCursorDoesNotSkipRecordsSharingTimestamp(t *testing.T) {
	const timestamp = "1787054400000000000"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/loki/api/v1/query_range" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{"resultType": "streams", "result": []any{
				map[string]any{"stream": map[string]string{"device_id": "42", "source": "a"}, "values": []any{[]any{timestamp, "alpha"}}},
				map[string]any{"stream": map[string]string{"device_id": "42", "source": "b"}, "values": []any{[]any{timestamp, "bravo"}}},
				map[string]any{"stream": map[string]string{"device_id": "42", "source": "c"}, "values": []any{[]any{timestamp, "charlie"}}},
			}},
		})
	}))
	t.Cleanup(server.Close)
	client := NewWithHTTPClient(server.URL, server.Client(), nil)
	req := validSearchRequest()
	req.Limit = 1
	seen := map[string]bool{}
	for page := 0; page < 3; page++ {
		result, err := client.Search(t.Context(), req)
		if err != nil {
			t.Fatalf("Search(page %d): %v", page, err)
		}
		if len(result.Records) != 1 || seen[result.Records[0].ID] {
			t.Fatalf("page %d records = %#v, seen=%#v", page, result.Records, seen)
		}
		seen[result.Records[0].ID] = true
		req.Cursor = result.NextCursor
	}
	if len(seen) != 3 {
		t.Fatalf("seen = %#v", seen)
	}
}

func TestLogQLDuration_UsesSupportedUnits(t *testing.T) {
	cases := map[time.Duration]string{time.Hour: "1h", 5 * time.Minute: "5m", 30 * time.Second: "30s"}
	for input, want := range cases {
		if got := logQLDuration(input); got != want {
			t.Fatalf("logQLDuration(%s) = %q, want %q", input, got, want)
		}
	}
}
