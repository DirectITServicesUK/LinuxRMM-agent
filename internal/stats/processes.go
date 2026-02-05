// Process collection for the RMM agent.
//
// This file implements process enumeration using gopsutil v4, collecting
// information about running processes including PID, name, user, CPU/memory
// usage, and command line. The collected data is used for the processes tab
// in the portal UI.
package stats

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/shirou/gopsutil/v4/process"
)

// ProcessInfo contains information about a single process.
// This matches the ProcessInfo type in the nats package for serialization.
type ProcessInfo struct {
	PID        int32   `json:"pid"`
	Name       string  `json:"name"`
	Username   string  `json:"username"`
	CPUPercent float64 `json:"cpuPercent"`
	MemPercent float32 `json:"memPercent"`
	Cmdline    string  `json:"cmdline"`
	Status     string  `json:"status"`
}

// ProcessCollector gathers process information from the host.
type ProcessCollector struct {
	logger *slog.Logger
}

// NewProcessCollector creates a new process collector with the given logger.
func NewProcessCollector(logger *slog.Logger) *ProcessCollector {
	return &ProcessCollector{
		logger: logger,
	}
}

// Collect gathers information about all running processes.
// It returns the list of processes and the agent's own PID (for UI exclusion).
//
// Processes that cannot be accessed (due to permissions) are silently skipped.
// The context is used for cancellation.
func (c *ProcessCollector) Collect(ctx context.Context) ([]ProcessInfo, int32, error) {
	agentPID := int32(os.Getpid())

	procs, err := process.ProcessesWithContext(ctx)
	if err != nil {
		return nil, agentPID, err
	}

	result := make([]ProcessInfo, 0, len(procs))

	for _, p := range procs {
		// Check for context cancellation periodically
		if ctx.Err() != nil {
			return nil, agentPID, ctx.Err()
		}

		info, err := c.getProcessInfo(ctx, p)
		if err != nil {
			// Skip processes we can't access (permission denied, process exited, etc.)
			continue
		}

		result = append(result, info)
	}

	c.logger.Debug("collected process list",
		slog.Int("count", len(result)),
		slog.Int("agent_pid", int(agentPID)),
	)

	return result, agentPID, nil
}

// getProcessInfo extracts information from a single process.
// Returns an error if essential information cannot be retrieved.
func (c *ProcessCollector) getProcessInfo(ctx context.Context, p *process.Process) (ProcessInfo, error) {
	info := ProcessInfo{
		PID: p.Pid,
	}

	// Get process name - this is essential
	name, err := p.NameWithContext(ctx)
	if err != nil {
		return info, err
	}
	info.Name = name

	// Get username (optional - may fail for system processes)
	username, err := p.UsernameWithContext(ctx)
	if err == nil {
		info.Username = username
	}

	// Get CPU percentage (optional)
	// Note: First call returns 0, subsequent calls show actual usage
	cpuPct, err := p.CPUPercentWithContext(ctx)
	if err == nil {
		info.CPUPercent = cpuPct
	}

	// Get memory percentage (optional)
	memPct, err := p.MemoryPercentWithContext(ctx)
	if err == nil {
		info.MemPercent = memPct
	}

	// Get command line (optional - truncate if too long)
	cmdline, err := p.CmdlineWithContext(ctx)
	if err == nil {
		// Truncate long command lines for display
		if len(cmdline) > 500 {
			cmdline = cmdline[:500] + "..."
		}
		info.Cmdline = cmdline
	}

	// Get status (optional)
	status, err := p.StatusWithContext(ctx)
	if err == nil {
		info.Status = strings.Join(status, ",")
	}

	return info, nil
}
