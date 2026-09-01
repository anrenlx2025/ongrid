package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestTraceHandler_ContextLogIncludesTraceAndSpanIDs(t *testing.T) {
	traceID, err := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("TraceIDFromHex: %v", err)
	}
	spanID, err := trace.SpanIDFromHex("0123456789abcdef")
	if err != nil {
		t.Fatalf("SpanIDFromHex: %v", err)
	}
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID,
		SpanID:  spanID,
	}))

	var output bytes.Buffer
	log := slog.New(traceHandler{Handler: slog.NewJSONHandler(&output, nil)}).With("component", "test")
	log.InfoContext(ctx, "request failed")

	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("decode log record: %v", err)
	}
	if got := record["trace_id"]; got != traceID.String() {
		t.Fatalf("trace_id = %v, want %s", got, traceID)
	}
	if got := record["span_id"]; got != spanID.String() {
		t.Fatalf("span_id = %v, want %s", got, spanID)
	}
}
