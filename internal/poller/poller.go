// Package poller implements the agent polling loop with jitter.
// It periodically sends heartbeats to the server and (in future) fetches
// pending commands for execution.
//
// The polling loop uses time.Timer instead of time.Ticker to support
// variable intervals with jitter. This prevents "thundering herd" problems
// where many agents poll the server simultaneously.
//
// The poller coordinates graceful shutdown using sync.WaitGroup to track
// in-flight work, ensuring all active operations complete before exit.
//
// Usage:
//
//	poller := poller.NewPoller(client, 60*time.Second, 30*time.Second, logger)
//	go poller.Run(ctx)
//	// ... on shutdown:
//	poller.Shutdown(shutdownCtx)
package poller

import (
	"context"
	"log/slog"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/doughall/linuxrmm/agent/internal/client"
	"github.com/doughall/linuxrmm/agent/internal/version"
)

// Poller manages the periodic polling loop for server communication.
// It sends heartbeats at regular intervals with random jitter and
// tracks in-flight work for graceful shutdown.
type Poller struct {
	client             *client.Client
	baseInterval       time.Duration
	jitter             time.Duration
	logger             *slog.Logger
	commandHandler     CommandHandler
	heartbeatPublisher HeartbeatPublisher

	// Synchronization for graceful shutdown
	wg      sync.WaitGroup
	running atomic.Bool

	// Cancel function for the polling goroutine
	cancel context.CancelFunc
}

// CommandHandler is the interface for processing commands.
type CommandHandler interface {
	ProcessPendingCommands(ctx context.Context) int
}

// HeartbeatPublisher defines the interface for publishing heartbeats via NATS.
// This allows the poller to use NATS when available without importing the nats package.
type HeartbeatPublisher interface {
	// PublishHeartbeat publishes a heartbeat via NATS.
	PublishHeartbeat(version, platform string) error
	// IsConnected returns whether the NATS connection is active.
	IsConnected() bool
}

// NewPoller creates a new Poller with the specified configuration.
//
// Parameters:
//   - client: HTTP client for server communication
//   - interval: Base polling interval (e.g., 60 seconds)
//   - jitter: Maximum random jitter added to interval (e.g., 30 seconds)
//   - logger: Structured logger for poll events
//
// The actual poll interval will be: interval + random(0, jitter)
func NewPoller(c *client.Client, interval, jitter time.Duration, logger *slog.Logger) *Poller {
	return &Poller{
		client:       c,
		baseInterval: interval,
		jitter:       jitter,
		logger:       logger.With(slog.String("component", "poller")),
	}
}

// SetCommandHandler sets the command handler for processing pending commands.
func (p *Poller) SetCommandHandler(handler CommandHandler) {
	p.commandHandler = handler
}

// SetHeartbeatPublisher sets the NATS publisher for sending heartbeats.
// When set and connected, heartbeats will be sent via NATS instead of HTTP.
func (p *Poller) SetHeartbeatPublisher(publisher HeartbeatPublisher) {
	p.heartbeatPublisher = publisher
}

// Run starts the polling loop. It blocks until the context is cancelled
// or an unrecoverable error occurs. The loop:
//
//  1. Waits for the interval (with jitter)
//  2. Sends heartbeat to server
//  3. (Future: Fetches and executes commands)
//  4. Repeats
//
// Run should be called in a goroutine. To stop the poller, cancel the
// context and then call Shutdown() to wait for in-flight work.
func (p *Poller) Run(ctx context.Context) {
	// Create internal context with cancel function for shutdown
	internalCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	p.running.Store(true)

	p.logger.Info("poller starting",
		slog.Duration("interval", p.baseInterval),
		slog.Duration("jitter", p.jitter),
	)

	// Send initial heartbeat immediately
	p.doPoll(internalCtx)

	for {
		// Calculate interval with jitter
		jitterAmount := time.Duration(rand.Int63n(int64(p.jitter)))
		interval := p.baseInterval + jitterAmount

		p.logger.Debug("waiting for next poll",
			slog.Duration("interval", interval),
		)

		// Use Timer instead of Ticker for variable intervals
		timer := time.NewTimer(interval)

		select {
		case <-internalCtx.Done():
			// Context cancelled - stop the timer and exit
			if !timer.Stop() {
				// Drain the timer channel if it already fired
				select {
				case <-timer.C:
				default:
				}
			}
			p.running.Store(false)
			p.logger.Info("poller stopped")
			return

		case <-timer.C:
			// Timer fired - do the poll
			p.doPoll(internalCtx)
		}
	}
}

// doPoll performs a single poll cycle (heartbeat + future command fetch).
// It tracks the work with WaitGroup for graceful shutdown.
func (p *Poller) doPoll(ctx context.Context) {
	// Track this poll for graceful shutdown
	p.wg.Add(1)
	defer p.wg.Done()

	// Check if context is already cancelled before starting
	select {
	case <-ctx.Done():
		return
	default:
	}

	// Send heartbeat via NATS if available, otherwise fall back to HTTP
	var heartbeatErr error
	var transport string

	if p.heartbeatPublisher != nil && p.heartbeatPublisher.IsConnected() {
		// Use NATS (preferred)
		platform := runtime.GOOS + "-" + runtime.GOARCH
		heartbeatErr = p.heartbeatPublisher.PublishHeartbeat(version.Version, platform)
		transport = "nats"
	} else {
		// Fall back to HTTP
		heartbeatErr = p.client.SendHeartbeat(ctx)
		transport = "http"
	}

	if heartbeatErr != nil {
		p.logger.Error("heartbeat failed",
			slog.String("transport", transport),
			slog.String("error", heartbeatErr.Error()),
		)
		// Continue polling - transient errors are expected
	} else {
		p.logger.Debug("heartbeat sent successfully",
			slog.String("transport", transport),
		)
	}

	// Process pending commands
	if p.commandHandler != nil {
		processed := p.commandHandler.ProcessPendingCommands(ctx)
		if processed > 0 {
			p.logger.Debug("commands processed", slog.Int("count", processed))
		}
	}
}

// Shutdown stops the poller and waits for in-flight work to complete.
// It respects the shutdown context's deadline/timeout.
//
// Returns nil if shutdown completes successfully, or ctx.Err() if
// the timeout is exceeded before all work completes.
func (p *Poller) Shutdown(ctx context.Context) error {
	p.logger.Info("poller shutting down")

	// Cancel the polling goroutine
	if p.cancel != nil {
		p.cancel()
	}

	// Wait for in-flight work with timeout
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		p.logger.Info("poller shutdown complete")
		return nil
	case <-ctx.Done():
		p.logger.Warn("poller shutdown timed out, some work may be incomplete")
		return ctx.Err()
	}
}

// IsHealthy returns true if the poller is running normally.
// This is used by the systemd watchdog to determine service health.
func (p *Poller) IsHealthy() bool {
	return p.running.Load()
}
