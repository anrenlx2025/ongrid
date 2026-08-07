package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/ongridio/ongrid/internal/pkg/logquery"
)

// ToolNameQueryLogQL is the stable wire name the LLM sees for the LogQL tool.
const ToolNameQueryLogQL = "query_logql"

// QueryLogQLDescription is the single-sentence description shown to the LLM.
// Phrased to push the model toward this tool whenever raw log inspection
// is needed beyond what the metric-side tools can express.
const QueryLogQLDescription = "Run a LogQL range query against Loki. " +
	"Use this to investigate log patterns, error counts, or filter a specific device. " +
	"When device_id is set, start with {} plus a line filter because the backend injects the device selector; do not guess job/hostname/instance labels. " +
	"Once a scoped query returns matching lines, summarize them unless the user asks for a narrower follow-up. " +
	"Returns the raw Loki response (streams or matrix)."

var logQLDeviceIDMatcher = regexp.MustCompile(`(?:^|,)\s*device_id\s*(=|!=|=~|!~)\s*"([^"]*)"`)

func parseLogQLTime(value string, fallback time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "now") {
		return fallback, nil
	}
	if rest, ok := strings.CutPrefix(strings.ToLower(value), "now-"); ok {
		d, err := time.ParseDuration(rest)
		if err != nil {
			return time.Time{}, err
		}
		return time.Now().Add(-d), nil
	}
	if rest, ok := strings.CutPrefix(strings.ToLower(value), "now+"); ok {
		d, err := time.ParseDuration(rest)
		if err != nil {
			return time.Time{}, err
		}
		return time.Now().Add(d), nil
	}
	return time.Parse(time.RFC3339, value)
}

// QueryLogQLSchema is the JSON Schema of the tool's argument object.
var QueryLogQLSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "LogQL expression. Example: \"{unit=\\\"nginx.service\\\"} |= \\\"error\\\"\"."
    },
    "device_id": {
      "type": "integer",
      "minimum": 1,
      "description": "Optional stable Device ID. The backend injects device_id into the LogQL stream selector and rejects a conflicting selector."
    },
    "start": {
      "type": "string",
      "description": "RFC3339 start time. Defaults to now-1h."
    },
    "end": {
      "type": "string",
      "description": "RFC3339 end time. Defaults to now."
    },
    "limit": {
      "type": "integer",
      "minimum": 1,
      "maximum": 5000,
      "description": "Max number of result rows (default 200)."
    },
    "direction": {
      "type": "string",
      "enum": ["backward", "forward"],
      "description": "Order of results, default \"backward\" (newest first)."
    }
  },
  "required": ["query"]
}`)

// QueryLogQLArgs is the typed form of QueryLogQLSchema.
type QueryLogQLArgs struct {
	Query     string  `json:"query"`
	DeviceID  *uint64 `json:"device_id,omitempty"`
	Start     string  `json:"start,omitempty"`
	End       string  `json:"end,omitempty"`
	Limit     int     `json:"limit,omitempty"`
	Direction string  `json:"direction,omitempty"`
}

// scopeLogQLToDevice injects a stable device label into the leading stream
// selector. LogQL's stream selector is the only safe place for this scope.
func scopeLogQLToDevice(query string, deviceID uint64) (string, error) {
	if deviceID == 0 {
		return query, nil
	}
	trimmed := strings.TrimSpace(query)
	if !strings.HasPrefix(trimmed, "{") {
		return "", fmt.Errorf("query_logql: device_id requires a LogQL stream selector")
	}
	selectorEnd, ok := logQLStreamSelectorEnd(trimmed)
	if !ok {
		return "", fmt.Errorf("query_logql: device_id requires a valid LogQL stream selector")
	}
	selector := trimmed[1:selectorEnd]
	matches := logQLDeviceIDMatcher.FindAllStringSubmatch(selector, -1)
	if len(matches) > 0 {
		want := fmt.Sprintf("%d", deviceID)
		for _, match := range matches {
			if match[1] != "=" || match[2] != want {
				return "", fmt.Errorf("query_logql: device_id conflicts with the LogQL selector")
			}
		}
		return trimmed, nil
	}
	prefix := `{device_id="` + fmt.Sprintf("%d", deviceID) + `"`
	if strings.TrimSpace(selector) != "" {
		prefix += `,` + selector
	}
	return prefix + `}` + trimmed[selectorEnd+1:], nil
}

func logQLStreamSelectorEnd(query string) (int, bool) {
	inQuote := false
	escaped := false
	for i := 1; i < len(query); i++ {
		switch query[i] {
		case '\\':
			if inQuote {
				escaped = !escaped
			}
		case '"':
			if !escaped {
				inQuote = !inQuote
			}
			escaped = false
		case '}':
			if !inQuote {
				return i, true
			}
		default:
			escaped = false
		}
	}
	return 0, false
}

// queryLogqlCallTimeout caps how long a single dispatch may wait. Mirrors
// query_promql for symmetry across signal types.
const queryLogqlCallTimeout = 30 * time.Second

// executeQueryLogQL runs the LogQL range query and hands the raw Loki
// response back to the LLM via ResultJSON.
func (r *Registry) executeQueryLogQL(ctx context.Context, args json.RawMessage) (ExecuteResult, error) {
	if r.logQuery == nil {
		// Should not happen — when logQuery is nil at NewRegistry the
		// tool is never registered. Defensive guard.
		return ExecuteResult{}, fmt.Errorf("query_logql: log query client not configured")
	}
	var in QueryLogQLArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return ExecuteResult{}, fmt.Errorf("query_logql: bad args: %w", err)
	}
	if strings.TrimSpace(in.Query) == "" {
		return ExecuteResult{}, fmt.Errorf("query_logql: query required")
	}
	query := in.Query
	if in.DeviceID != nil {
		if *in.DeviceID == 0 {
			return ExecuteResult{}, fmt.Errorf("query_logql: device_id must be greater than zero")
		}
		var err error
		query, err = scopeLogQLToDevice(in.Query, *in.DeviceID)
		if err != nil {
			return ExecuteResult{}, err
		}
	}

	end := time.Now()
	start := end.Add(-time.Hour)
	if in.End != "" {
		t, err := parseLogQLTime(in.End, end)
		if err != nil {
			return ExecuteResult{}, fmt.Errorf("query_logql: parse end: %w", err)
		}
		end = t
	}
	if in.Start != "" {
		t, err := parseLogQLTime(in.Start, start)
		if err != nil {
			return ExecuteResult{}, fmt.Errorf("query_logql: parse start: %w", err)
		}
		start = t
	} else if in.End != "" {
		// User pinned end but not start — keep the 1h window relative to
		// the supplied end so the call is still bounded.
		start = end.Add(-time.Hour)
	}

	limit := in.Limit
	if limit <= 0 {
		limit = 200
	}
	direction := in.Direction
	if direction == "" {
		direction = "backward"
	}

	callCtx, cancel := context.WithTimeout(ctx, queryLogqlCallTimeout)
	defer cancel()

	res, err := r.logQuery.QueryRange(callCtx, logquery.QueryRangeOptions{
		Query:     query,
		Start:     start,
		End:       end,
		Limit:     limit,
		Direction: direction,
	})
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("query_logql: dispatch: %w", err)
	}
	out, err := json.Marshal(res)
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("query_logql: marshal response: %w", err)
	}
	return ExecuteResult{ResultJSON: out}, nil
}

// LogQuerier is the narrow surface the query_logql executor needs from the
// logquery client. Declared here so tests can inject a fake.
//
// NOTE: this interface is what r.logQuery is typed as. The concrete
// *logquery.Client satisfies it.
type LogQuerier interface {
	QueryRange(ctx context.Context, opts logquery.QueryRangeOptions) (*logquery.QueryRangeResult, error)
}
