package logquery

import (
	"encoding/json"
	"errors"
	"fmt"
)

// QueryLogQLResult is the closed result set returned by query_logql. The
// concrete value always matches the single selected backend: Loki returns its
// native QueryRangeResult, while Elasticsearch returns SearchResult.
type QueryLogQLResult interface {
	isQueryLogQLResult()
}

func (*QueryRangeResult) isQueryLogQLResult() {}
func (*SearchResult) isQueryLogQLResult()     {}

// MarshalQueryLogQLResult preserves the selected backend's response shape and
// rejects invalid nil or foreign implementations.
func MarshalQueryLogQLResult(result QueryLogQLResult) ([]byte, error) {
	switch typed := result.(type) {
	case *QueryRangeResult:
		if typed == nil {
			return nil, errors.New("logquery: Loki query result is nil")
		}
		return json.Marshal(typed)
	case *SearchResult:
		if typed == nil {
			return nil, errors.New("logquery: Elasticsearch query result is nil")
		}
		return json.Marshal(typed)
	default:
		return nil, fmt.Errorf("logquery: unsupported query_logql result type %T", result)
	}
}
