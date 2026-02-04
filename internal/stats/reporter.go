// Package stats - Reporter Component
//
// This file implements the stats reporter that sends collected system statistics
// to the RMM server at regular intervals. It coordinates with the Collector to
// gather metrics and calculates network traffic deltas to report bytes/second
// rather than cumulative counters.
//
// The reporter runs as a separate goroutine from the main polling loop, operating
// independently to ensure consistent stats collection even during command processing.
//
// Key features:
//   - 60-second collection interval (configurable)
//   - Network delta calculation (handles counter resets)
//   - Non-blocking: failures log warnings but don't stop collection
//   - Graceful shutdown via context cancellation
//   - Supports both HTTP (fallback) and NATS (preferred) transport
package stats

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/doughall/linuxrmm/agent/internal/client"
)

// StatsPublisher defines the interface for publishing stats via NATS.
// This allows the reporter to use NATS when available without importing the nats package.
type StatsPublisher interface {
	// PublishStatsData publishes system statistics via NATS.
	// The stats parameter contains the metrics to publish.
	PublishStatsData(stats StatsData) error
	// IsConnected returns whether the NATS connection is active.
	IsConnected() bool
}

// StatsData contains the metrics to publish.
// This interface allows the reporter to pass stats without importing the nats package.
type StatsData struct {
	Timestamp    string
	CPU          float64
	MemoryUsed   uint64
	MemoryTotal  uint64
	MemoryPct    float64
	DiskUsed     uint64
	DiskTotal    uint64
	DiskPct      float64
	NetBytesSent uint64
	NetBytesRecv uint64
	Load1        float64
	Load5        float64
	Load15       float64
	Uptime       uint64
}

// Reporter collects system statistics and sends them to the RMM server.
// It runs independently of the polling loop and handles network counter
// delta calculations.
type Reporter struct {
	collector     *Collector
	client        *client.Client
	natsPublisher StatsPublisher
	logger        *slog.Logger
	interval      time.Duration

	// Previous network counters for delta calculation
	prevNetSent uint64
	prevNetRecv uint64
	firstReport bool // Track if this is the first report (deltas will be 0)

	// Synchronization for graceful shutdown
	wg     sync.WaitGroup
	cancel context.CancelFunc
}

// NewReporter creates a new stats reporter.
//
// Parameters:
//   - collector: The stats collector to use for gathering metrics
//   - c: HTTP client for sending stats to server (fallback when NATS unavailable)
//   - logger: Structured logger for stats events
//   - interval: How often to collect and send stats (e.g., 60 seconds)
func NewReporter(collector *Collector, c *client.Client, logger *slog.Logger, interval time.Duration) *Reporter {
	return &Reporter{
		collector:   collector,
		client:      c,
		logger:      logger.With(slog.String("component", "stats-reporter")),
		interval:    interval,
		firstReport: true,
	}
}

// SetStatsPublisher sets the NATS publisher for sending stats.
// When set and connected, stats will be sent via NATS instead of HTTP.
func (r *Reporter) SetStatsPublisher(publisher StatsPublisher) {
	r.natsPublisher = publisher
}

// Run starts the stats collection and reporting loop.
// It blocks until the context is cancelled.
//
// The loop:
//  1. Collects system statistics via the Collector
//  2. Calculates network deltas (bytes since last report)
//  3. Sends stats to server
//  4. Waits for the interval
//  5. Repeats
//
// Run should be called in a goroutine. To stop the reporter, cancel the context.
func (r *Reporter) Run(ctx context.Context) {
	// Create internal context with cancel function for shutdown
	internalCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel

	r.logger.Info("stats reporter starting",
		slog.Duration("interval", r.interval),
	)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	// Collect and send immediately on first run
	r.collectAndSend(internalCtx)

	for {
		select {
		case <-internalCtx.Done():
			r.logger.Info("stats reporter stopped")
			return

		case <-ticker.C:
			r.collectAndSend(internalCtx)
		}
	}
}

// collectAndSend performs a single collection and send cycle.
// Errors are logged but do not stop the reporter - stats are not critical.
func (r *Reporter) collectAndSend(ctx context.Context) {
	// Track this work for graceful shutdown
	r.wg.Add(1)
	defer r.wg.Done()

	// Check if context is already cancelled
	select {
	case <-ctx.Done():
		return
	default:
	}

	// Collect system statistics
	stats, err := r.collector.Collect(ctx)
	if err != nil {
		r.logger.Warn("failed to collect stats",
			slog.String("error", err.Error()),
		)
		return
	}

	// Calculate network deltas
	deltaSent, deltaRecv := r.calculateNetworkDeltas(stats.NetBytesSent, stats.NetBytesRecv)

	// Build stats payload with deltas
	payload := &client.StatsPayload{
		Timestamp:    stats.Timestamp,
		CPU:          stats.CPU,
		MemoryUsed:   stats.MemoryUsed,
		MemoryTotal:  stats.MemoryTotal,
		MemoryPct:    stats.MemoryPct,
		DiskUsed:     stats.DiskUsed,
		DiskTotal:    stats.DiskTotal,
		DiskPct:      stats.DiskPct,
		NetBytesSent: deltaSent,
		NetBytesRecv: deltaRecv,
		Load1:        stats.Load1,
		Load5:        stats.Load5,
		Load15:       stats.Load15,
		Uptime:       stats.Uptime,
	}

	r.logger.Debug("collected stats",
		slog.Float64("cpu_pct", stats.CPU),
		slog.Float64("memory_pct", stats.MemoryPct),
		slog.Float64("disk_pct", stats.DiskPct),
		slog.Uint64("net_sent_delta", deltaSent),
		slog.Uint64("net_recv_delta", deltaRecv),
	)

	// Send stats via NATS if available, otherwise fall back to HTTP
	var sendErr error
	var transport string

	if r.natsPublisher != nil && r.natsPublisher.IsConnected() {
		// Use NATS (preferred)
		natsPayload := StatsData{
			Timestamp:    stats.Timestamp.Format(time.RFC3339),
			CPU:          stats.CPU,
			MemoryUsed:   stats.MemoryUsed,
			MemoryTotal:  stats.MemoryTotal,
			MemoryPct:    stats.MemoryPct,
			DiskUsed:     stats.DiskUsed,
			DiskTotal:    stats.DiskTotal,
			DiskPct:      stats.DiskPct,
			NetBytesSent: deltaSent,
			NetBytesRecv: deltaRecv,
			Load1:        stats.Load1,
			Load5:        stats.Load5,
			Load15:       stats.Load15,
			Uptime:       stats.Uptime,
		}
		sendErr = r.natsPublisher.PublishStatsData(natsPayload)
		transport = "nats"
	} else {
		// Fall back to HTTP
		sendErr = r.client.SendStats(ctx, payload)
		transport = "http"
	}

	if sendErr != nil {
		r.logger.Warn("failed to send stats",
			slog.String("transport", transport),
			slog.String("error", sendErr.Error()),
		)
		// Continue - stats failures are not critical
		return
	}

	if r.firstReport {
		r.logger.Info("first stats sent successfully",
			slog.String("transport", transport),
		)
		r.firstReport = false
	} else {
		r.logger.Debug("stats sent successfully",
			slog.String("transport", transport),
		)
	}
}

// calculateNetworkDeltas computes the bytes sent/received since the last report.
// It handles:
//   - First report: Returns 0 deltas (no previous baseline)
//   - Counter reset: If current < previous, uses current as delta (assumes reset)
//   - Normal case: Returns current - previous
func (r *Reporter) calculateNetworkDeltas(currentSent, currentRecv uint64) (deltaSent, deltaRecv uint64) {
	// First report: no previous values, delta is 0
	if r.prevNetSent == 0 && r.prevNetRecv == 0 {
		r.prevNetSent = currentSent
		r.prevNetRecv = currentRecv
		return 0, 0
	}

	// Calculate sent delta (handle counter reset)
	if currentSent >= r.prevNetSent {
		deltaSent = currentSent - r.prevNetSent
	} else {
		// Counter reset detected - use current value as delta
		// This is an approximation; in reality we lost some data
		deltaSent = currentSent
		r.logger.Debug("network sent counter reset detected",
			slog.Uint64("prev", r.prevNetSent),
			slog.Uint64("current", currentSent),
		)
	}

	// Calculate recv delta (handle counter reset)
	if currentRecv >= r.prevNetRecv {
		deltaRecv = currentRecv - r.prevNetRecv
	} else {
		// Counter reset detected
		deltaRecv = currentRecv
		r.logger.Debug("network recv counter reset detected",
			slog.Uint64("prev", r.prevNetRecv),
			slog.Uint64("current", currentRecv),
		)
	}

	// Update previous values for next calculation
	r.prevNetSent = currentSent
	r.prevNetRecv = currentRecv

	return deltaSent, deltaRecv
}

// Shutdown stops the reporter and waits for any in-flight work to complete.
// It respects the shutdown context's deadline/timeout.
func (r *Reporter) Shutdown(ctx context.Context) error {
	r.logger.Info("stats reporter shutting down")

	// Cancel the reporter goroutine
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
		r.logger.Info("stats reporter shutdown complete")
		return nil
	case <-ctx.Done():
		r.logger.Warn("stats reporter shutdown timed out")
		return ctx.Err()
	}
}
