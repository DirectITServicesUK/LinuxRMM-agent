// Package results - Uploader Component
//
// This file implements the result uploader that syncs queued schedule execution
// results to the RMM server. Results are queued locally when schedules execute
// (especially when offline) and uploaded in batches when the server is reachable.
//
// The uploader runs as a background goroutine, checking the result queue every
// 30 seconds. Successfully uploaded results are removed from the queue; failures
// are retried on the next cycle.
//
// Key features:
//   - 30-second upload interval
//   - Batch upload (up to 50 results at a time)
//   - Non-blocking: failures log warnings but don't stop the uploader
//   - Graceful shutdown via context cancellation
package results

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// HTTPClient defines the interface for uploading results to the server.
// This interface allows for easy testing with mock implementations.
type HTTPClient interface {
	SubmitScheduleResults(ctx context.Context, results []*PendingResult) error
}

// Uploader periodically uploads queued schedule results to the RMM server.
// It runs as a background goroutine and handles retry on failure.
type Uploader struct {
	queue    *ResultQueue
	client   HTTPClient
	logger   *slog.Logger
	interval time.Duration

	// Synchronization for graceful shutdown
	wg     sync.WaitGroup
	cancel context.CancelFunc
}

// NewUploader creates a new result uploader.
//
// Parameters:
//   - queue: The result queue to read pending results from
//   - client: HTTP client for uploading results to server
//   - logger: Structured logger for uploader events
func NewUploader(queue *ResultQueue, client HTTPClient, logger *slog.Logger) *Uploader {
	return &Uploader{
		queue:    queue,
		client:   client,
		logger:   logger.With(slog.String("component", "result-uploader")),
		interval: 30 * time.Second,
	}
}

// Run starts the result upload loop.
// It blocks until the context is cancelled.
//
// The loop:
//  1. Processes immediately on startup (catch any pending results)
//  2. Dequeues up to 50 results
//  3. Uploads to server
//  4. Removes successfully uploaded results from queue
//  5. Waits for the interval (30 seconds)
//  6. Repeats
//
// Run should be called in a goroutine. To stop the uploader, cancel the context.
func (u *Uploader) Run(ctx context.Context) {
	// Create internal context with cancel function for shutdown
	internalCtx, cancel := context.WithCancel(ctx)
	u.cancel = cancel

	u.logger.Info("result uploader started",
		slog.Duration("interval", u.interval),
	)

	ticker := time.NewTicker(u.interval)
	defer ticker.Stop()

	// Process immediately on startup to catch any pending results
	u.processQueue(internalCtx)

	for {
		select {
		case <-internalCtx.Done():
			u.logger.Info("result uploader stopping")
			return

		case <-ticker.C:
			u.processQueue(internalCtx)
		}
	}
}

// processQueue uploads pending results from the queue.
// Errors are logged but do not stop the uploader - retry on next cycle.
func (u *Uploader) processQueue(ctx context.Context) {
	// Track this work for graceful shutdown
	u.wg.Add(1)
	defer u.wg.Done()

	// Check if context is already cancelled
	select {
	case <-ctx.Done():
		return
	default:
	}

	// Dequeue up to 50 results
	results, err := u.queue.Dequeue(50)
	if err != nil {
		u.logger.Warn("failed to dequeue results",
			slog.String("error", err.Error()),
		)
		return
	}

	// Nothing to upload
	if len(results) == 0 {
		u.logger.Debug("no pending results")
		return
	}

	u.logger.Info("uploading results",
		slog.Int("count", len(results)),
	)

	// Upload to server
	if err := u.client.SubmitScheduleResults(ctx, results); err != nil {
		u.logger.Warn("failed to upload results, will retry next cycle",
			slog.String("error", err.Error()),
			slog.Int("count", len(results)),
		)
		return
	}

	// Extract IDs for removal
	ids := make([]uint64, len(results))
	for i, r := range results {
		ids[i] = r.ID
	}

	// Remove successfully uploaded results from queue
	if err := u.queue.Remove(ids); err != nil {
		u.logger.Warn("failed to remove uploaded results from queue",
			slog.String("error", err.Error()),
			slog.Int("count", len(ids)),
		)
		// Results were uploaded successfully, so this is non-critical
		// They may be uploaded again on next cycle (server should handle duplicates)
		return
	}

	u.logger.Info("results uploaded",
		slog.Int("count", len(results)),
	)
}

// Shutdown stops the uploader and waits for any in-flight work to complete.
// It respects the shutdown context's deadline/timeout.
func (u *Uploader) Shutdown(ctx context.Context) error {
	u.logger.Info("uploader shutdown initiated")

	// Cancel the uploader goroutine
	if u.cancel != nil {
		u.cancel()
	}

	// Wait for in-flight work with timeout
	done := make(chan struct{})
	go func() {
		u.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		u.logger.Info("uploader shutdown complete")
		return nil
	case <-ctx.Done():
		u.logger.Warn("uploader shutdown timed out")
		return ctx.Err()
	}
}
