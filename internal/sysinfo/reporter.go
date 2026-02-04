// Package sysinfo - Reporter Component
//
// This file implements the system info reporter that sends collected system
// information to the RMM server at startup and periodically (hourly by default).
// Unlike stats which change every minute, system info changes infrequently
// (OS upgrades, hardware changes) so we report it less often.
//
// Key features:
//   - Reports immediately on startup
//   - Periodic refresh (default 1 hour) to detect system changes
//   - Supports both HTTP (fallback) and NATS (preferred) transport
//   - Graceful shutdown via context cancellation
package sysinfo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Default reporting interval - how often to refresh system info
const DefaultReportInterval = 1 * time.Hour

// SysInfoPublisher defines the interface for publishing system info via NATS.
type SysInfoPublisher interface {
	// PublishSystemInfo publishes system information via NATS.
	PublishSystemInfo(info *SystemInfoMessage) error
	// IsConnected returns whether the NATS connection is active.
	IsConnected() bool
}

// SystemInfoMessage mirrors the NATS message type for system info.
// This allows the reporter to work with the NATS publisher without importing it.
type SystemInfoMessage struct {
	OS                   string `json:"os"`
	Platform             string `json:"platform"`
	PlatformFamily       string `json:"platformFamily"`
	PlatformVersion      string `json:"platformVersion"`
	KernelVersion        string `json:"kernelVersion"`
	KernelArch           string `json:"kernelArch"`
	Arch                 string `json:"arch"`
	Hostname             string `json:"hostname"`
	HostID               string `json:"hostId"`
	VirtualizationSystem string `json:"virtSystem,omitempty"`
	VirtualizationRole   string `json:"virtRole,omitempty"`
	CPUModel             string `json:"cpuModel"`
	CPUCores             int    `json:"cpuCores"`
	CPUThreads           int    `json:"cpuThreads"`
	MemoryTotal          uint64 `json:"memoryTotal"`
	BootTime             uint64 `json:"bootTime"`
	AgentVersion         string `json:"agentVersion"`
}

// HTTPClient defines the interface for HTTP communication.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Reporter collects and sends system information to the RMM server.
type Reporter struct {
	serverURL     string
	apiKey        string
	agentID       string
	agentVersion  string
	httpClient    HTTPClient
	natsPublisher SysInfoPublisher
	logger        *slog.Logger
	interval      time.Duration

	// Synchronization for graceful shutdown
	wg     sync.WaitGroup
	cancel context.CancelFunc
}

// NewReporter creates a new system info reporter.
//
// Parameters:
//   - serverURL: Base URL of the RMM server
//   - apiKey: API key for authentication
//   - agentID: Agent's unique identifier
//   - agentVersion: Current agent version string
//   - httpClient: HTTP client for fallback transport
//   - logger: Structured logger for sysinfo events
//   - interval: How often to collect and send system info (e.g., 1 hour)
func NewReporter(serverURL, apiKey, agentID, agentVersion string, httpClient HTTPClient, logger *slog.Logger, interval time.Duration) *Reporter {
	return &Reporter{
		serverURL:    serverURL,
		apiKey:       apiKey,
		agentID:      agentID,
		agentVersion: agentVersion,
		httpClient:   httpClient,
		logger:       logger.With(slog.String("component", "sysinfo-reporter")),
		interval:     interval,
	}
}

// SetPublisher sets the NATS publisher for sending system info.
// When set and connected, system info will be sent via NATS instead of HTTP.
func (r *Reporter) SetPublisher(publisher SysInfoPublisher) {
	r.natsPublisher = publisher
}

// Run starts the system info collection and reporting loop.
// It blocks until the context is cancelled.
func (r *Reporter) Run(ctx context.Context) {
	// Create internal context with cancel function for shutdown
	internalCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel

	r.logger.Info("sysinfo reporter starting",
		slog.Duration("interval", r.interval),
	)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	// Collect and send immediately on first run
	r.collectAndSend(internalCtx)

	for {
		select {
		case <-internalCtx.Done():
			r.logger.Info("sysinfo reporter stopped")
			return

		case <-ticker.C:
			r.collectAndSend(internalCtx)
		}
	}
}

// collectAndSend performs a single collection and send cycle.
func (r *Reporter) collectAndSend(ctx context.Context) {
	r.wg.Add(1)
	defer r.wg.Done()

	// Check if context is already cancelled
	select {
	case <-ctx.Done():
		return
	default:
	}

	// Collect system information
	info, err := Collect(ctx)
	if err != nil {
		r.logger.Warn("failed to collect system info",
			slog.String("error", err.Error()),
		)
		return
	}

	// Add agent version
	info.AgentVersion = r.agentVersion

	r.logger.Debug("collected system info",
		slog.String("os", info.OS),
		slog.String("platform", info.Platform),
		slog.String("platform_version", info.PlatformVersion),
		slog.String("kernel", info.KernelVersion),
	)

	// Send via NATS if available, otherwise fall back to HTTP
	var sendErr error
	var transport string

	if r.natsPublisher != nil && r.natsPublisher.IsConnected() {
		// Use NATS (preferred)
		natsMsg := &SystemInfoMessage{
			OS:                   info.OS,
			Platform:             info.Platform,
			PlatformFamily:       info.PlatformFamily,
			PlatformVersion:      info.PlatformVersion,
			KernelVersion:        info.KernelVersion,
			KernelArch:           info.KernelArch,
			Arch:                 info.Arch,
			Hostname:             info.Hostname,
			HostID:               info.HostID,
			VirtualizationSystem: info.VirtualizationSystem,
			VirtualizationRole:   info.VirtualizationRole,
			CPUModel:             info.CPUModel,
			CPUCores:             info.CPUCores,
			CPUThreads:           info.CPUThreads,
			MemoryTotal:          info.MemoryTotal,
			BootTime:             info.BootTime,
			AgentVersion:         info.AgentVersion,
		}
		sendErr = r.natsPublisher.PublishSystemInfo(natsMsg)
		transport = "nats"
	} else {
		// Fall back to HTTP
		sendErr = r.sendHTTP(ctx, info)
		transport = "http"
	}

	if sendErr != nil {
		r.logger.Warn("failed to send system info",
			slog.String("transport", transport),
			slog.String("error", sendErr.Error()),
		)
		return
	}

	r.logger.Debug("system info sent successfully",
		slog.String("transport", transport),
	)
}

// sendHTTP sends system info via HTTP POST.
func (r *Reporter) sendHTTP(ctx context.Context, info *SystemInfo) error {
	url := fmt.Sprintf("%s/api/agents/%s/sysinfo", r.serverURL, r.agentID)

	body, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("marshal system info: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.apiKey)

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	return nil
}

// Shutdown stops the reporter and waits for any in-flight work to complete.
func (r *Reporter) Shutdown(ctx context.Context) error {
	r.logger.Info("sysinfo reporter shutting down")

	if r.cancel != nil {
		r.cancel()
	}

	// Wait for in-flight work with timeout
	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		r.logger.Info("sysinfo reporter shutdown complete")
		return nil
	case <-ctx.Done():
		r.logger.Warn("sysinfo reporter shutdown timed out")
		return ctx.Err()
	}
}
