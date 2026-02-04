// Package stats provides system statistics collection for the RMM agent.
//
// This package implements a collector that gathers CPU, memory, disk, network,
// load average, and uptime metrics from the host system using gopsutil v4.
// The collected statistics are used for monitoring and reporting to the
// central RMM server.
//
// The collector is designed to be called periodically by the agent's reporting
// loop, with each call returning a snapshot of current system metrics.
package stats

import (
	"context"
	"log/slog"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
)

// SystemStats contains a snapshot of system statistics at a point in time.
// All byte values are in bytes, percentages are 0-100, and time is in seconds.
type SystemStats struct {
	// Timestamp is when this statistics snapshot was collected.
	Timestamp time.Time `json:"timestamp"`

	// CPU is the current CPU usage percentage (0-100).
	// This is measured over a short sample interval (100ms).
	CPU float64 `json:"cpu"`

	// Memory metrics
	MemoryUsed  uint64  `json:"memoryUsed"`  // Used memory in bytes
	MemoryTotal uint64  `json:"memoryTotal"` // Total memory in bytes
	MemoryPct   float64 `json:"memoryPct"`   // Memory usage percentage (0-100)

	// Disk metrics for root filesystem
	DiskUsed  uint64  `json:"diskUsed"`  // Used disk space in bytes
	DiskTotal uint64  `json:"diskTotal"` // Total disk space in bytes
	DiskPct   float64 `json:"diskPct"`   // Disk usage percentage (0-100)

	// Network metrics (cumulative counters since boot)
	NetBytesSent uint64 `json:"netBytesSent"` // Total bytes sent
	NetBytesRecv uint64 `json:"netBytesRecv"` // Total bytes received

	// Load averages
	Load1  float64 `json:"load1"`  // 1-minute load average
	Load5  float64 `json:"load5"`  // 5-minute load average
	Load15 float64 `json:"load15"` // 15-minute load average

	// Uptime is the system uptime in seconds since boot.
	Uptime uint64 `json:"uptime"`
}

// Collector gathers system statistics from the host.
// It maintains internal state for tracking network counter deltas
// between collection intervals.
type Collector struct {
	logger *slog.Logger

	// Previous network counters for delta calculation by the reporter.
	// These are stored but not currently used - the reporter (Plan 04-03)
	// will calculate deltas based on consecutive SystemStats snapshots.
	prevNetBytesSent uint64
	prevNetBytesRecv uint64
}

// NewCollector creates a new stats collector with the given logger.
func NewCollector(logger *slog.Logger) *Collector {
	return &Collector{
		logger: logger,
	}
}

// Collect gathers a snapshot of current system statistics.
// It collects CPU, memory, disk, network, load, and uptime metrics.
//
// If individual metric collection fails, it logs a warning and continues
// with partial data. The returned SystemStats will have zero values for
// metrics that couldn't be collected.
//
// The context is used for cancellation - if cancelled, collection will
// stop and return an error.
func (c *Collector) Collect(ctx context.Context) (*SystemStats, error) {
	stats := &SystemStats{
		Timestamp: time.Now(),
	}

	// Collect CPU usage with a 100ms sample interval.
	// The sample interval is needed to measure CPU usage accurately.
	cpuPcts, err := cpu.PercentWithContext(ctx, 100*time.Millisecond, false)
	if err != nil {
		c.logger.Warn("failed to collect CPU stats", slog.String("error", err.Error()))
	} else if len(cpuPcts) > 0 {
		stats.CPU = cpuPcts[0]
	}

	// Check context after CPU collection (which takes time due to sampling)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Collect memory statistics
	memInfo, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		c.logger.Warn("failed to collect memory stats", slog.String("error", err.Error()))
	} else {
		stats.MemoryUsed = memInfo.Used
		stats.MemoryTotal = memInfo.Total
		stats.MemoryPct = memInfo.UsedPercent
	}

	// Check context
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Collect disk usage for root filesystem
	diskInfo, err := disk.UsageWithContext(ctx, "/")
	if err != nil {
		c.logger.Warn("failed to collect disk stats", slog.String("error", err.Error()))
	} else {
		stats.DiskUsed = diskInfo.Used
		stats.DiskTotal = diskInfo.Total
		stats.DiskPct = diskInfo.UsedPercent
	}

	// Check context
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Collect network I/O counters (combined for all interfaces)
	// The false parameter means we get combined stats, not per-interface
	netCounters, err := net.IOCountersWithContext(ctx, false)
	if err != nil {
		c.logger.Warn("failed to collect network stats", slog.String("error", err.Error()))
	} else if len(netCounters) > 0 {
		stats.NetBytesSent = netCounters[0].BytesSent
		stats.NetBytesRecv = netCounters[0].BytesRecv

		// Store for potential delta calculation
		c.prevNetBytesSent = stats.NetBytesSent
		c.prevNetBytesRecv = stats.NetBytesRecv
	}

	// Check context
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Collect load averages
	loadInfo, err := load.AvgWithContext(ctx)
	if err != nil {
		c.logger.Warn("failed to collect load stats", slog.String("error", err.Error()))
	} else {
		stats.Load1 = loadInfo.Load1
		stats.Load5 = loadInfo.Load5
		stats.Load15 = loadInfo.Load15
	}

	// Check context
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	// Collect system uptime
	uptime, err := host.UptimeWithContext(ctx)
	if err != nil {
		c.logger.Warn("failed to collect uptime", slog.String("error", err.Error()))
	} else {
		stats.Uptime = uptime
	}

	return stats, nil
}
