package logquery

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestElasticsearchClient_SearchUsesPITAndOpaqueCursor(t *testing.T) {
	searchCalls := 0
	closed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "ApiKey test-key" {
			t.Fatalf("Authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/_pit"):
			writeTestJSON(t, w, map[string]any{"id": "pit-1"})
		case r.Method == http.MethodPost && r.URL.Path == "/_search":
			searchCalls++
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("ReadAll() error = %v", err)
			}
			if !strings.Contains(string(body), `"resource.attributes.device_id":["42"]`) || !strings.Contains(string(body), `"match_phrase":{"body.text":"timeout"}`) {
				t.Fatalf("search body missing scoped query: %s", body)
			}
			if strings.Contains(string(body), "simple_query_string") {
				t.Fatalf("search body exposed query-string syntax: %s", body)
			}
			if searchCalls == 2 && !strings.Contains(string(body), `"search_after"`) {
				t.Fatalf("second search body missing search_after: %s", body)
			}
			hits := []any{
				testElasticsearchHit("one", "2026-08-18T12:00:00Z", "timeout", []any{"2026-08-18T12:00:00Z", 7}),
			}
			if searchCalls == 1 {
				hits = append(hits, testElasticsearchHit("two", "2026-08-18T11:59:59Z", "next", []any{"2026-08-18T11:59:59Z", 8}))
			}
			writeTestJSON(t, w, map[string]any{"took": 2, "pit_id": "pit-1", "hits": map[string]any{"hits": hits}})
		case r.Method == http.MethodDelete && r.URL.Path == "/_pit":
			closed = true
			writeTestJSON(t, w, map[string]any{"succeeded": true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	client, err := NewElasticsearchClient(ElasticsearchConfig{
		Endpoint: server.URL, APIKey: "test-key", AllowInsecureHTTP: true,
	}, server.Client(), nil)
	if err != nil {
		t.Fatalf("NewElasticsearchClient() error = %v", err)
	}
	req := validSearchRequest()
	req.Limit = 1
	req.Scope.DeviceIDs = []uint64{42}
	req.Keywords.Include = []string{"timeout"}
	first, err := client.Search(t.Context(), req)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(first.Records) != 1 || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("first result = %#v", first)
	}
	if first.Records[0].ResourceAttributes["device_id"] != "42" {
		t.Fatalf("resource attributes = %#v", first.Records[0].ResourceAttributes)
	}
	req.Cursor = first.NextCursor
	second, err := client.Search(t.Context(), req)
	if err != nil {
		t.Fatalf("Search(cursor) error = %v", err)
	}
	if len(second.Records) != 1 || second.HasMore || second.NextCursor != "" || !closed {
		t.Fatalf("second result = %#v closed=%v", second, closed)
	}
}

func TestElasticsearchClient_ProbeRequiresSupportedVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(t, w, map[string]any{"version": map[string]string{"number": "8.16.3"}})
	}))
	t.Cleanup(server.Close)
	client, err := NewElasticsearchClient(ElasticsearchConfig{
		Endpoint: server.URL, APIKey: "test-key", AllowInsecureHTTP: true,
	}, server.Client(), nil)
	if err != nil {
		t.Fatalf("NewElasticsearchClient() error = %v", err)
	}
	version, err := client.Probe(t.Context())
	if err != nil || version != "8.16.3" {
		t.Fatalf("Probe() = %q, %v", version, err)
	}
}

func TestElasticsearchClient_CountUsesFixedIndexAndStructuredQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/_count") {
			http.NotFound(w, r)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/logs-ongrid.") {
			t.Fatalf("count path = %q", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		for _, want := range []string{`"resource.attributes.namespace":["prod"]`, `"body.text"`, `"panic"`, `"gt":"2026-08-18T11:00:00Z"`} {
			if !strings.Contains(string(body), want) {
				t.Fatalf("count body = %s, missing %s", body, want)
			}
		}
		writeTestJSON(t, w, map[string]any{"count": 9})
	}))
	t.Cleanup(server.Close)
	client, err := NewElasticsearchClient(ElasticsearchConfig{
		Endpoint: server.URL, APIKey: "test-key", AllowInsecureHTTP: true,
	}, server.Client(), nil)
	if err != nil {
		t.Fatalf("NewElasticsearchClient() error = %v", err)
	}
	req := validSearchRequest()
	req.Scope.Namespaces = []string{"prod"}
	req.Keywords = Keywords{Include: []string{"panic"}, Mode: MatchPhrase}
	count, err := client.Count(t.Context(), req)
	if err != nil || count != 9 {
		t.Fatalf("Count() = %d, %v", count, err)
	}
}

func TestNewElasticsearchClient_RejectsUnsafeEndpointAndIndexPattern(t *testing.T) {
	cases := []ElasticsearchConfig{
		{Endpoint: "http://es.example", APIKey: "key"},
		{Endpoint: "https://user:pass@es.example", APIKey: "key"},
		{Endpoint: "https://es.example", APIKey: "key", IndexPattern: "*"},
		{Endpoint: "https://es.example", APIKey: ""},
	}
	for _, cfg := range cases {
		if _, err := NewElasticsearchClient(cfg, nil, nil); err == nil {
			t.Fatalf("NewElasticsearchClient(%#v) unexpectedly succeeded", cfg)
		}
	}
}

func testElasticsearchHit(id, timestamp, message string, sort []any) map[string]any {
	return map[string]any{
		"_id": id,
		"_source": map[string]any{
			"@timestamp":    timestamp,
			"body":          map[string]any{"text": message},
			"severity_text": "ERROR",
			"resource": map[string]any{
				"attributes": map[string]any{"device_id": "42", "service.name": "api"},
			},
		},
		"sort": sort,
	}
}

func writeTestJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
}

func TestElasticsearchDuration_UsesFixedIntervalSyntax(t *testing.T) {
	cases := map[time.Duration]string{time.Hour: "1h", 5 * time.Minute: "5m", 250 * time.Millisecond: "250ms"}
	for input, want := range cases {
		if got := elasticsearchDuration(input); got != want {
			t.Fatalf("elasticsearchDuration(%s) = %q, want %q", input, got, want)
		}
	}
}
