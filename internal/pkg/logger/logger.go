// Package logger wraps log/slog with ongrid conventions.
//
// gospec red line: structured logs only, JSON handler, never log raw user
// content (chat messages, request bodies, secrets). Context-aware log calls
// automatically include the active OpenTelemetry trace and span IDs.
package logger

import (
	"context"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel/trace"
)

// New returns a *slog.Logger that writes JSON lines to stderr at the given
// minimum level.
func New(level slog.Level) *slog.Logger {
	h := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	})
	return slog.New(traceHandler{Handler: h})
}

// WithService returns a logger decorated with a "service" attribute.
// Used by cmd/ongrid and cmd/ongrid-edge at startup.
func WithService(l *slog.Logger, name string) *slog.Logger {
	return l.With(slog.String("service", name))
}

type traceHandler struct {
	slog.Handler
}

func (h traceHandler) Handle(ctx context.Context, record slog.Record) error {
	spanContext := trace.SpanContextFromContext(ctx)
	if spanContext.IsValid() {
		record.AddAttrs(
			slog.String("trace_id", spanContext.TraceID().String()),
			slog.String("span_id", spanContext.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, record)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{Handler: h.Handler.WithGroup(name)}
}
