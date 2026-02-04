// Package systemd provides integration with systemd service management.
//
// This package wraps the coreos/go-systemd library to provide:
// - sd_notify READY/STOPPING notifications for Type=notify services
// - Watchdog pinging for WatchdogSec health monitoring
// - Graceful degradation when systemd is not available (e.g., development)
//
// The agent uses Type=notify in its systemd unit file, which requires
// explicit notification when the service is ready to accept requests.
// This prevents systemd from routing traffic before initialization completes.
//
// Watchdog integration allows systemd to automatically restart the agent
// if health checks fail (no watchdog ping within WatchdogSec interval).
package systemd

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"
)

// NotifyReady sends sd_notify READY=1 to systemd.
// This tells systemd that the service has completed initialization
// and is ready to handle requests.
//
// Safe to call when not running under systemd (no NOTIFY_SOCKET) - it's a no-op.
// Returns true if notification was sent, false if systemd is not available.
func NotifyReady() bool {
	sent, err := daemon.SdNotify(false, daemon.SdNotifyReady)
	if err != nil {
		slog.Warn("failed to send systemd ready notification", "error", err)
		return false
	}
	if sent {
		slog.Debug("sent systemd ready notification")
	} else {
		slog.Debug("systemd notification not available (not running under systemd)")
	}
	return sent
}

// NotifyStopping sends sd_notify STOPPING=1 to systemd.
// This tells systemd that the service is beginning its shutdown sequence.
// systemd will wait for the process to exit rather than killing it.
//
// Safe to call when not running under systemd - it's a no-op.
// Returns true if notification was sent, false if systemd is not available.
func NotifyStopping() bool {
	sent, err := daemon.SdNotify(false, daemon.SdNotifyStopping)
	if err != nil {
		slog.Warn("failed to send systemd stopping notification", "error", err)
		return false
	}
	if sent {
		slog.Debug("sent systemd stopping notification")
	}
	return sent
}

// HealthCheckFunc is a function that returns true if the service is healthy.
// Used by StartWatchdog to determine whether to send watchdog pings.
type HealthCheckFunc func() bool

// StartWatchdog starts a goroutine that sends watchdog pings to systemd.
// The healthCheck function is called before each ping - if it returns false,
// the ping is skipped and systemd will eventually restart the service.
//
// The watchdog is only started if systemd provides a WatchdogSec value
// (detected via sd_watchdog_enabled). Pings are sent every interval/2
// as recommended by the systemd documentation.
//
// The goroutine exits when the context is cancelled.
// Safe to call when not running under systemd - returns immediately.
func StartWatchdog(ctx context.Context, healthCheck HealthCheckFunc) {
	// Check if watchdog is enabled and get interval
	interval, err := daemon.SdWatchdogEnabled(false)
	if err != nil {
		slog.Debug("watchdog not enabled", "error", err)
		return
	}
	if interval == 0 {
		slog.Debug("watchdog interval is zero, watchdog disabled")
		return
	}

	// Ping every half-interval as per systemd documentation
	pingInterval := interval / 2
	slog.Info("starting systemd watchdog",
		"watchdog_interval", interval,
		"ping_interval", pingInterval,
	)

	go watchdogLoop(ctx, pingInterval, healthCheck)
}

// watchdogLoop sends periodic watchdog pings until context is cancelled.
func watchdogLoop(ctx context.Context, interval time.Duration, healthCheck HealthCheckFunc) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Debug("watchdog loop stopping due to context cancellation")
			return
		case <-ticker.C:
			if healthCheck() {
				if _, err := daemon.SdNotify(false, daemon.SdNotifyWatchdog); err != nil {
					slog.Warn("failed to send watchdog ping", "error", err)
				} else {
					slog.Debug("sent watchdog ping")
				}
			} else {
				slog.Warn("health check failed, skipping watchdog ping")
			}
		}
	}
}

// IsRunningUnderSystemd returns true if the process was started by systemd.
// Detected by checking for the NOTIFY_SOCKET environment variable.
func IsRunningUnderSystemd() bool {
	return os.Getenv("NOTIFY_SOCKET") != ""
}
