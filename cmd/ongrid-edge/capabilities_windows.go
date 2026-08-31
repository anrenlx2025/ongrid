//go:build windows

package main

import (
	"log/slog"

	edgehostfiles "github.com/ongridio/ongrid/internal/edgeagent/host_files"
	"github.com/ongridio/ongrid/internal/pkg/tunnel"
)

// registerEdgeCapabilities registers the platform-specific edge capability
// handlers with the tunnel client. Windows: host_files only (the package
// itself is cross-platform after the build-tag split).
//
// bash / restart_service are Linux-only (cmdpolicy/systemctl) and are not
// ported to Windows. Windows-native equivalents are future work and need
// a PowerShell subprocess framework.
//
// All Register calls are soft-fail: the edge keeps booting and the
// operator sees a warning in the log when a capability is disabled.
func registerEdgeCapabilities(client tunnel.Client, log *slog.Logger) {
	if err := edgehostfiles.Register(client, log); err != nil {
		log.Warn("host_files register failed; capability disabled", slog.Any("err", err))
	}
}
