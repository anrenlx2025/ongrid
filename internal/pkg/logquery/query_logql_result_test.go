package logquery

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMarshalQueryLogQLResultKeepsConcreteBackendShape(t *testing.T) {
	tests := []struct {
		name       string
		result     QueryLogQLResult
		wantField  string
		avoidField string
	}{
		{
			name: "loki", result: &QueryRangeResult{ResultType: "streams", Result: json.RawMessage(`[]`)},
			wantField: `"resultType"`, avoidField: `"records"`,
		},
		{
			name: "elasticsearch", result: &SearchResult{Records: []Record{}, Backends: []string{"elasticsearch"}},
			wantField: `"records"`, avoidField: `"resultType"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := MarshalQueryLogQLResult(tt.result)
			if err != nil {
				t.Fatalf("MarshalQueryLogQLResult() error = %v", err)
			}
			if !strings.Contains(string(body), tt.wantField) || strings.Contains(string(body), tt.avoidField) {
				t.Fatalf("body = %s", body)
			}
		})
	}
}

func TestMarshalQueryLogQLResultRejectsTypedNil(t *testing.T) {
	var result *SearchResult
	if _, err := MarshalQueryLogQLResult(result); err == nil {
		t.Fatal("MarshalQueryLogQLResult() unexpectedly accepted typed nil")
	}
}
