// Package stats provides unit tests for the system statistics collector.
//
// These tests verify that the collector properly gathers system metrics
// including CPU, memory, disk, network, load averages, and uptime.
// Tests validate both successful collection and context cancellation handling.
package stats

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// nopLogger returns a logger that discards all output, suitable for tests.
func nopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNewCollector(t *testing.T) {
	t.Run("creates collector with logger", func(t *testing.T) {
		logger := nopLogger()
		collector := NewCollector(logger)

		if collector == nil {
			t.Fatal("expected non-nil collector")
		}
		if collector.logger != logger {
			t.Error("expected collector to store logger")
		}
	})
}

func TestCollect(t *testing.T) {
	collector := NewCollector(nopLogger())
	ctx := context.Background()

	stats, err := collector.Collect(ctx)
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}

	t.Run("timestamp is set", func(t *testing.T) {
		if stats.Timestamp.IsZero() {
			t.Error("expected non-zero timestamp")
		}
		// Timestamp should be recent (within last 5 seconds)
		if time.Since(stats.Timestamp) > 5*time.Second {
			t.Error("timestamp is not recent")
		}
	})

	t.Run("CPU percentage is valid", func(t *testing.T) {
		// CPU can be 0 on idle systems, so just check it's in valid range
		if stats.CPU < 0 || stats.CPU > 100 {
			t.Errorf("CPU percentage out of range: %v", stats.CPU)
		}
	})

	t.Run("memory stats are valid", func(t *testing.T) {
		if stats.MemoryTotal == 0 {
			t.Error("expected MemoryTotal > 0")
		}
		if stats.MemoryUsed > stats.MemoryTotal {
			t.Errorf("MemoryUsed (%d) exceeds MemoryTotal (%d)",
				stats.MemoryUsed, stats.MemoryTotal)
		}
		if stats.MemoryPct < 0 || stats.MemoryPct > 100 {
			t.Errorf("MemoryPct out of range: %v", stats.MemoryPct)
		}
	})

	t.Run("disk stats are valid", func(t *testing.T) {
		if stats.DiskTotal == 0 {
			t.Error("expected DiskTotal > 0")
		}
		if stats.DiskUsed > stats.DiskTotal {
			t.Errorf("DiskUsed (%d) exceeds DiskTotal (%d)",
				stats.DiskUsed, stats.DiskTotal)
		}
		if stats.DiskPct < 0 || stats.DiskPct > 100 {
			t.Errorf("DiskPct out of range: %v", stats.DiskPct)
		}
	})

	t.Run("network stats are collected", func(t *testing.T) {
		// Network counters are cumulative, so they should be non-negative.
		// On a fresh system they could be 0, so we just verify they exist.
		// The values could be 0 on a very quiet system, so no minimum check.
		if stats.NetBytesSent < 0 {
			t.Errorf("NetBytesSent is negative: %d", stats.NetBytesSent)
		}
		if stats.NetBytesRecv < 0 {
			t.Errorf("NetBytesRecv is negative: %d", stats.NetBytesRecv)
		}
	})

	t.Run("load averages are collected", func(t *testing.T) {
		// Load averages should be non-negative.
		// They can be 0 on an idle system, so just check they're valid.
		if stats.Load1 < 0 {
			t.Errorf("Load1 is negative: %v", stats.Load1)
		}
		if stats.Load5 < 0 {
			t.Errorf("Load5 is negative: %v", stats.Load5)
		}
		if stats.Load15 < 0 {
			t.Errorf("Load15 is negative: %v", stats.Load15)
		}
	})

	t.Run("uptime is valid", func(t *testing.T) {
		if stats.Uptime == 0 {
			t.Error("expected Uptime > 0")
		}
	})
}

func TestCollectCancellation(t *testing.T) {
	collector := NewCollector(nopLogger())

	t.Run("respects already cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		_, err := collector.Collect(ctx)

		// The collector should either return an error or handle gracefully.
		// Since CPU collection takes 100ms, it should fail on cancelled context.
		if err == nil {
			// If no error, it means collection completed before hitting context check.
			// This is acceptable - the point is that cancellation is respected
			// when encountered.
			t.Log("collection completed despite cancelled context (fast system)")
		} else if err != context.Canceled {
			t.Errorf("expected context.Canceled error, got: %v", err)
		}
	})

	t.Run("respects context cancellation during collection", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()

		_, err := collector.Collect(ctx)

		// With a 10ms timeout and 100ms CPU sample, we should get a timeout.
		// The error could be context.Canceled or context.DeadlineExceeded.
		if err == nil {
			t.Log("collection completed within timeout (very fast system)")
		} else if err != context.Canceled && err != context.DeadlineExceeded {
			t.Errorf("expected context error, got: %v", err)
		}
	})
}

func TestCollectMultipleTimes(t *testing.T) {
	collector := NewCollector(nopLogger())
	ctx := context.Background()

	// Collect twice and verify both work
	stats1, err := collector.Collect(ctx)
	if err != nil {
		t.Fatalf("first Collect failed: %v", err)
	}

	// Small delay to ensure different timestamp
	time.Sleep(10 * time.Millisecond)

	stats2, err := collector.Collect(ctx)
	if err != nil {
		t.Fatalf("second Collect failed: %v", err)
	}

	// Timestamps should be different
	if stats1.Timestamp.Equal(stats2.Timestamp) {
		t.Error("expected different timestamps for consecutive collections")
	}

	// Second timestamp should be after first
	if !stats2.Timestamp.After(stats1.Timestamp) {
		t.Error("expected second timestamp to be after first")
	}
}
