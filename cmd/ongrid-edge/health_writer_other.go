//go:build !windows

// health_writer_other.go: stub for non-Windows platforms. A Linux edge has
// no supervisor.exe parent and needs no health.json IPC.

package main

import (
	"context"
	"log/slog"
)

// startHeartbeatWriter is a no-op on non-Windows platforms (no supervisor
// is monitoring the process).
func startHeartbeatWriter(_ context.Context, _ *slog.Logger) error {
	return nil
}
