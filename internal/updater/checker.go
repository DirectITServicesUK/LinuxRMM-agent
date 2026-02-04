// Package updater provides self-update functionality for the RMM agent.
// The Checker component runs on a configurable schedule, fetching update
// manifests from the server and determining if updates are available.
package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// UpdateCallback is called when an update is available.
// It receives the manifest with download details.
// The callback should coordinate the update process (download, verify, apply).
type UpdateCallback func(manifest Manifest)

// Checker periodically checks for updates from the server.
// It runs on a configurable interval (default: 24 hours) and calls
// a callback when an update is available.
//
// The checker runs in its own goroutine and can be stopped gracefully
// via the Stop method.
type Checker struct {
	serverURL     string
	agentID       string
	apiKey        string
	currentVer    string
	checkInterval time.Duration
	callback      UpdateCallback
	logger        *slog.Logger

	httpClient *http.Client

	// Coordination for graceful shutdown
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewChecker creates a new update checker.
//
// Parameters:
//   - serverURL: Base URL of the RMM server (e.g., "https://rmm.example.com")
//   - agentID: Unique identifier for this agent
//   - apiKey: Authentication key for API requests
//   - currentVersion: Current semantic version of the agent (e.g., "1.0.0")
//   - checkIntervalHours: How often to check for updates (in hours)
//   - callback: Function to call when an update is available
//   - logger: Structured logger for diagnostic output
func NewChecker(
	serverURL string,
	agentID string,
	apiKey string,
	currentVersion string,
	checkIntervalHours int,
	callback UpdateCallback,
	logger *slog.Logger,
) *Checker {
	return &Checker{
		serverURL:     serverURL,
		agentID:       agentID,
		apiKey:        apiKey,
		currentVer:    currentVersion,
		checkInterval: time.Duration(checkIntervalHours) * time.Hour,
		callback:      callback,
		logger:        logger,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		stopCh: make(chan struct{}),
	}
}

// Run starts the update checker loop.
// It performs an immediate check on startup, then checks at the configured interval.
// This method blocks until Stop() is called.
//
// The checker runs in a goroutine managed by the wait group for coordinated shutdown.
func (c *Checker) Run(ctx context.Context) {
	c.wg.Add(1)
	defer c.wg.Done()

	c.logger.Info("update checker started",
		slog.Duration("interval", c.checkInterval),
		slog.String("current_version", c.currentVer),
	)

	// Perform immediate check on startup
	c.checkForUpdate(ctx)

	ticker := time.NewTicker(c.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.logger.Info("update checker stopping (context cancelled)")
			return
		case <-c.stopCh:
			c.logger.Info("update checker stopping (stop signal)")
			return
		case <-ticker.C:
			c.checkForUpdate(ctx)
		}
	}
}

// Stop signals the update checker to stop and waits for it to complete.
// This is part of the LIFO shutdown ordering.
func (c *Checker) Stop() {
	c.logger.Info("stopping update checker")
	close(c.stopCh)
	c.wg.Wait()
	c.logger.Info("update checker stopped")
}

// checkForUpdate fetches the update manifest from the server and
// invokes the callback if an update is available and needed.
func (c *Checker) checkForUpdate(ctx context.Context) {
	c.logger.Debug("checking for updates")

	manifest, err := c.fetchManifest(ctx)
	if err != nil {
		c.logger.Warn("failed to fetch update manifest",
			slog.String("error", err.Error()),
		)
		return
	}

	c.logger.Debug("fetched update manifest",
		slog.Bool("update_available", manifest.UpdateAvailable),
		slog.String("version", manifest.Version),
	)

	// Use NeedsUpdate to safely check if we should proceed
	if NeedsUpdate(c.currentVer, manifest) {
		c.logger.Info("update available",
			slog.String("current_version", c.currentVer),
			slog.String("new_version", manifest.Version),
			slog.String("download_url", manifest.DownloadURL),
		)

		// Invoke callback with manifest details
		c.callback(manifest)
	} else {
		c.logger.Debug("no update needed",
			slog.String("current_version", c.currentVer),
			slog.String("server_version", manifest.Version),
		)
	}
}

// fetchManifest makes an authenticated request to the server to fetch
// the update manifest for this agent.
//
// The server endpoint is: GET /api/updates/manifest
// The agent sends its version and platform in request headers:
//   - X-Agent-Version: Current semantic version
//   - X-Agent-Platform: Platform identifier (e.g., "linux-amd64")
//
// The server uses this information along with the agent's group membership
// to determine update eligibility (version comparison, group targeting,
// staged rollout percentage).
func (c *Checker) fetchManifest(ctx context.Context) (Manifest, error) {
	url := c.serverURL + "/api/updates/manifest"

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return Manifest{}, fmt.Errorf("failed to create request: %w", err)
	}

	// Set authentication header
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	// Send current version and platform in headers
	// Server uses these to determine update eligibility
	req.Header.Set("X-Agent-Version", c.currentVer)
	// Platform detection: runtime.GOOS + runtime.GOARCH
	// For now, we'll let the server infer from the agent record
	// (server tracks platform from heartbeat headers set in Phase 08-01)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Manifest{}, fmt.Errorf("manifest request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return Manifest{}, fmt.Errorf("manifest request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var manifest Manifest
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("failed to decode manifest: %w", err)
	}

	// Drain body to allow connection reuse
	_, _ = io.Copy(io.Discard, resp.Body)

	return manifest, nil
}
