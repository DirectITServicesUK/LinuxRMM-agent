// health.go - Startup health check with automatic rollback on failure.
// Runs at startup to verify the agent is functioning correctly.
// If health check fails and a recent update occurred, rolls back to previous version.
package updater

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"
)

const (
	// HealthCheckTimeout is the maximum time for health checks to complete.
	HealthCheckTimeout = 30 * time.Second

	// UpdateGracePeriod is how long after an update we consider failures update-related.
	UpdateGracePeriod = 10 * time.Minute

	// RollbackAttemptedMarker prevents rollback loops.
	RollbackAttemptedMarker = "/var/lib/rmm-agent/rollback_attempted"

	// UpdateMarkerPath tracks when last update occurred.
	// Used by both health.go (to detect recent updates for rollback)
	// and updater.go (to write marker after applying update).
	UpdateMarkerPath = "/var/lib/rmm-agent/last_update"
)

// HealthCheckResult contains the result of a health check.
type HealthCheckResult struct {
	ConfigValid     bool
	ServerReachable bool
	Errors          []string
}

// HealthChecker performs startup health validation.
type HealthChecker struct {
	serverURL string
	apiKey    string
	backup    *BackupManager
	logger    *slog.Logger
}

// NewHealthChecker creates a new health checker.
func NewHealthChecker(serverURL, apiKey string, backup *BackupManager, logger *slog.Logger) *HealthChecker {
	return &HealthChecker{
		serverURL: serverURL,
		apiKey:    apiKey,
		backup:    backup,
		logger:    logger.With(slog.String("component", "health")),
	}
}

// RunStartupHealthCheck performs health validation at startup.
// If health check fails after a recent update, attempts rollback.
// Returns error if health check fails and rollback is not possible.
func (h *HealthChecker) RunStartupHealthCheck(ctx context.Context, binaryPath string) error {
	h.logger.Info("running startup health check")

	// Check if we've already attempted rollback (prevent loop)
	if h.rollbackAttempted() {
		h.logger.Warn("rollback already attempted, skipping rollback on failure")
		// Clear the marker so next real update can try rollback
		h.clearRollbackMarker()
	}

	// Run health checks with timeout
	checkCtx, cancel := context.WithTimeout(ctx, HealthCheckTimeout)
	defer cancel()

	result := h.performHealthChecks(checkCtx)

	if len(result.Errors) == 0 {
		h.logger.Info("health check passed",
			slog.Bool("config_valid", result.ConfigValid),
			slog.Bool("server_reachable", result.ServerReachable),
		)

		// Clear update marker on successful startup
		if err := ClearUpdateMarker(); err != nil {
			h.logger.Warn("failed to clear update marker",
				slog.String("error", err.Error()),
			)
		}

		return nil
	}

	// Health check failed
	h.logger.Error("health check failed",
		slog.Any("errors", result.Errors),
	)

	// Check if we should attempt rollback
	if !WasRecentUpdate(UpdateGracePeriod) {
		// No recent update - this is not an update-related failure
		h.logger.Warn("health check failed but no recent update, not rolling back")
		return fmt.Errorf("health check failed: %v", result.Errors)
	}

	// Recent update + health failure = rollback
	if h.rollbackAttempted() {
		// Already tried rollback - don't loop
		return fmt.Errorf("health check failed after rollback attempt: %v", result.Errors)
	}

	// Attempt rollback
	return h.performRollback(binaryPath)
}

// performHealthChecks runs all health validation checks.
func (h *HealthChecker) performHealthChecks(ctx context.Context) *HealthCheckResult {
	result := &HealthCheckResult{
		ConfigValid:     true,
		ServerReachable: false,
		Errors:          make([]string, 0),
	}

	// Check 1: Server is reachable
	if err := h.checkServerReachable(ctx); err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("server unreachable: %v", err))
	} else {
		result.ServerReachable = true
	}

	// Note: Config validation happens earlier in main.go (Load will fail if invalid)
	// We could add additional validation here if needed

	return result
}

// checkServerReachable verifies the server can be contacted.
func (h *HealthChecker) checkServerReachable(ctx context.Context) error {
	// Try to hit the heartbeat endpoint
	url := h.serverURL + "/api/agents/heartbeat"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+h.apiKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Any response (even 401) means server is reachable
	// We're just checking connectivity, not auth
	return nil
}

// performRollback restores the previous binary version and exits.
func (h *HealthChecker) performRollback(binaryPath string) error {
	h.logger.Warn("performing rollback due to health check failure after update")

	// Mark that we're attempting rollback (prevent loops)
	if err := h.markRollbackAttempted(); err != nil {
		h.logger.Warn("failed to mark rollback attempted",
			slog.String("error", err.Error()),
		)
	}

	// Check if we have backups
	if !h.backup.HasBackups() {
		return fmt.Errorf("no backups available for rollback")
	}

	// Restore the latest backup
	if err := h.backup.RestoreLatest(binaryPath); err != nil {
		return fmt.Errorf("rollback failed: %w", err)
	}

	h.logger.Info("rollback complete, exiting for restart")

	// Exit with non-zero to signal failure (systemd will restart with rolled-back binary)
	os.Exit(1)

	return nil // Never reached
}

// rollbackAttempted returns true if a rollback was already attempted.
func (h *HealthChecker) rollbackAttempted() bool {
	_, err := os.Stat(RollbackAttemptedMarker)
	return err == nil
}

// markRollbackAttempted creates the rollback attempted marker file.
func (h *HealthChecker) markRollbackAttempted() error {
	return os.WriteFile(RollbackAttemptedMarker, []byte(time.Now().Format(time.RFC3339)), 0644)
}

// clearRollbackMarker removes the rollback attempted marker.
func (h *HealthChecker) clearRollbackMarker() {
	os.Remove(RollbackAttemptedMarker)
}

// WasRecentUpdate returns true if an update happened within the grace period.
// Used by health checker to determine if failure should trigger rollback.
func WasRecentUpdate(gracePeriod time.Duration) bool {
	info, err := os.Stat(UpdateMarkerPath)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		return false
	}

	return time.Since(info.ModTime()) < gracePeriod
}

// ClearUpdateMarker removes the update marker after successful health check.
// This prevents rollback on subsequent restarts.
func ClearUpdateMarker() error {
	err := os.Remove(UpdateMarkerPath)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
