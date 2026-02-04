// Package nats handler processes incoming NATS messages for the RMM agent.
//
// The handler routes messages by type and executes commands using the same
// infrastructure as the WebSocket handler, ensuring consistent behavior.
//
// This handler shares a deduplicator with WebSocket/poller handlers to prevent
// duplicate execution when commands arrive via multiple channels.
package nats

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/doughall/linuxrmm/agent/internal/client"
	"github.com/doughall/linuxrmm/agent/internal/commands"
	"github.com/doughall/linuxrmm/agent/internal/executor"
	"github.com/doughall/linuxrmm/agent/internal/helper"
	"github.com/doughall/linuxrmm/agent/internal/scheduler"
)

// Handler processes incoming NATS messages.
type Handler struct {
	executor      *executor.Executor
	helperClient  *helper.Client
	httpClient    *client.Client
	natsPublisher *Publisher
	dedup         *commands.Deduplicator
	scheduleCache *scheduler.ScheduleCache
	logger        *slog.Logger
}

// NewHandler creates a new NATS message handler.
// httpClient is used to report command start (still over HTTP for now).
// natsPublisher publishes results via NATS.
// dedup is shared with other handlers for cross-channel deduplication.
// scheduleCache stores schedule assignments for offline execution.
func NewHandler(httpClient *client.Client, natsPublisher *Publisher, dedup *commands.Deduplicator, scheduleCache *scheduler.ScheduleCache, logger *slog.Logger) *Handler {
	return &Handler{
		executor:      executor.New(),
		helperClient:  helper.NewClient(),
		httpClient:    httpClient,
		natsPublisher: natsPublisher,
		dedup:         dedup,
		scheduleCache: scheduleCache,
		logger:        logger,
	}
}

// HandleCommand processes a command message from NATS.
// Implements the MessageHandler interface.
func (h *Handler) HandleCommand(cmd *CommandMessage) error {
	cmdLogger := h.logger.With(slog.String("command_id", cmd.ID))
	cmdLogger.Info("received command via NATS",
		slog.Int("timeout_ms", cmd.Timeout),
	)

	// Check deduplication - skip if already processed
	if h.dedup != nil && !h.dedup.MarkSeen(cmd.ID) {
		cmdLogger.Debug("skipping duplicate command from NATS")
		return nil
	}

	// Execute command in goroutine to not block the consumer
	go h.executeCommand(cmd)

	return nil
}

// HandleSchedule processes a schedule assignment message from NATS.
// Implements the MessageHandler interface.
func (h *Handler) HandleSchedule(msg *ScheduleMessage) error {
	schedLogger := h.logger.With(
		slog.String("schedule_id", msg.ID),
		slog.String("action", msg.Action),
	)

	switch msg.Action {
	case "update":
		return h.handleScheduleUpdate(msg, schedLogger)
	case "delete":
		return h.handleScheduleDelete(msg, schedLogger)
	default:
		schedLogger.Warn("unknown schedule_assignment action")
		return nil
	}
}

// executeCommand executes a command and reports the result via NATS.
func (h *Handler) executeCommand(cmd *CommandMessage) {
	ctx := context.Background()
	cmdLogger := h.logger.With(slog.String("command_id", cmd.ID))

	// Publish command start notification
	if h.natsPublisher != nil {
		if err := h.natsPublisher.PublishCommandStart(cmd.ID); err != nil {
			cmdLogger.Warn("failed to publish command start",
				slog.String("error", err.Error()),
			)
		}
	}

	// Determine timeout
	timeout := 60 * time.Second
	if cmd.Timeout > 0 {
		timeout = time.Duration(cmd.Timeout) * time.Millisecond
	}

	cmdLogger.Info("executing command",
		slog.Int64("timeout_ms", timeout.Milliseconds()),
	)

	// Try privileged execution first if helper is available
	var result *executor.Result
	var execErr error

	if h.helperClient.Available() {
		result, execErr = h.helperClient.Execute(ctx, cmd.Command, timeout)
		if execErr != nil {
			cmdLogger.Warn("helper execution failed, falling back to unprivileged",
				slog.String("error", execErr.Error()),
			)
		}
	}

	// Fall back to unprivileged execution
	if result == nil {
		result, execErr = h.executor.Execute(ctx, cmd.Command, timeout)
	}

	// Build result message
	resultMsg := &CommandResultMessage{
		CommandID: cmd.ID,
	}

	if execErr != nil {
		resultMsg.Error = execErr.Error()
		resultMsg.ExitCode = -1
	} else {
		resultMsg.Stdout = result.Stdout
		resultMsg.Stderr = result.Stderr
		resultMsg.ExitCode = result.ExitCode
		resultMsg.ExecutionDuration = result.Duration.Milliseconds()
		resultMsg.TimedOut = result.TimedOut
	}

	// Publish result via NATS
	if h.natsPublisher != nil {
		if err := h.natsPublisher.PublishCommandResult(resultMsg); err != nil {
			cmdLogger.Error("failed to publish result via NATS",
				slog.String("error", err.Error()),
			)
			// Fallback to HTTP if NATS fails
			h.reportResultViaHTTP(ctx, cmd.ID, result, execErr, cmdLogger)
			return
		}
	} else {
		// NATS publisher not available, use HTTP
		h.reportResultViaHTTP(ctx, cmd.ID, result, execErr, cmdLogger)
		return
	}

	cmdLogger.Info("command completed",
		slog.Int("exit_code", resultMsg.ExitCode),
		slog.Bool("timed_out", resultMsg.TimedOut),
		slog.Int64("duration_ms", resultMsg.ExecutionDuration),
	)
}

// reportResultViaHTTP reports command result via HTTP as fallback.
func (h *Handler) reportResultViaHTTP(ctx context.Context, cmdID string, result *executor.Result, execErr error, logger *slog.Logger) {
	var cmdResult *client.CommandResult

	if execErr != nil {
		cmdResult = &client.CommandResult{
			Stderr:            execErr.Error(),
			ExitCode:          -1,
			ExecutionDuration: 0,
			TimedOut:          false,
		}
	} else {
		cmdResult = &client.CommandResult{
			Stdout:            result.Stdout,
			Stderr:            result.Stderr,
			ExitCode:          result.ExitCode,
			ExecutionDuration: result.Duration.Milliseconds(),
			TimedOut:          result.TimedOut,
		}
	}

	if err := h.httpClient.ReportCommandResult(ctx, cmdID, cmdResult); err != nil {
		logger.Error("failed to report result via HTTP",
			slog.String("error", err.Error()),
		)
	}
}

// handleScheduleUpdate processes a schedule update (create or modify).
func (h *Handler) handleScheduleUpdate(msg *ScheduleMessage, logger *slog.Logger) error {
	if h.scheduleCache == nil {
		logger.Warn("schedule cache not available, ignoring schedule update")
		return nil
	}

	// Parse schedule details from raw JSON
	var scheduleData map[string]any
	if err := json.Unmarshal(msg.Schedule, &scheduleData); err != nil {
		logger.Warn("failed to parse schedule data", slog.String("error", err.Error()))
		return nil
	}

	schedule := &scheduler.CachedSchedule{
		ID:              msg.ID,
		ScriptContent:   msg.Script,
		Interpreter:     getString(scheduleData, "interpreter"),
		CronExpression:  getString(scheduleData, "cronExpression"),
		IntervalMinutes: getInt(scheduleData, "intervalMinutes"),
		Variables:       getMap(scheduleData, "variables"),
		OutputRetention: getString(scheduleData, "outputRetention"),
		NextRunAt:       getTime(scheduleData, "nextRunAt"),
		LastSyncAt:      time.Now(),
	}

	// Validate required fields
	if schedule.ScriptContent == "" {
		logger.Warn("schedule update missing scriptContent")
		return nil
	}

	// Save to cache
	if err := h.scheduleCache.SaveSchedule(schedule); err != nil {
		logger.Error("failed to save schedule",
			slog.String("error", err.Error()),
		)
		return err
	}

	logger.Info("schedule cached for offline execution",
		slog.Time("next_run_at", schedule.NextRunAt),
	)

	return nil
}

// handleScheduleDelete processes a schedule deletion.
func (h *Handler) handleScheduleDelete(msg *ScheduleMessage, logger *slog.Logger) error {
	if h.scheduleCache == nil {
		logger.Warn("schedule cache not available, ignoring schedule delete")
		return nil
	}

	if err := h.scheduleCache.DeleteSchedule(msg.ID); err != nil {
		logger.Error("failed to delete schedule",
			slog.String("error", err.Error()),
		)
		return err
	}

	logger.Info("schedule removed from cache")

	return nil
}

// Helper functions for safe field extraction from map[string]any

func getString(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func getInt(m map[string]any, key string) int {
	if v, ok := m[key]; ok {
		switch val := v.(type) {
		case float64:
			return int(val)
		case int:
			return val
		}
	}
	return 0
}

func getMap(m map[string]any, key string) map[string]any {
	if v, ok := m[key]; ok {
		if mapVal, ok := v.(map[string]any); ok {
			return mapVal
		}
	}
	return make(map[string]any)
}

func getTime(m map[string]any, key string) time.Time {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return t
			}
		}
	}
	return time.Now()
}

// HandleUninstall processes an uninstall command from the server.
// Sends confirmation, then executes uninstall script and exits.
func (h *Handler) HandleUninstall(msg *UninstallMessage) error {
	h.logger.Info("received uninstall command",
		slog.String("reason", msg.Reason),
	)

	// Send confirmation back to server first
	if h.natsPublisher != nil {
		if err := h.natsPublisher.PublishUninstallConfirm(); err != nil {
			h.logger.Warn("failed to publish uninstall confirmation",
				slog.String("error", err.Error()),
			)
			// Continue with uninstall anyway
		} else {
			// Flush to ensure message is sent before we exit
			if err := h.natsPublisher.Flush(); err != nil {
				h.logger.Warn("failed to flush uninstall confirmation",
					slog.String("error", err.Error()),
				)
			}
			h.logger.Info("uninstall confirmation sent")
		}
	}

	// Execute uninstall in goroutine to not block
	go h.executeUninstall()

	return nil
}

// executeUninstall performs the actual agent uninstallation.
func (h *Handler) executeUninstall() {
	h.logger.Info("executing uninstall")

	// Give time for confirmation message to be delivered via JetStream
	// The 3-second wait allows for network latency and JetStream persistence
	time.Sleep(3 * time.Second)

	// Create uninstall script
	// Note: Script cleans up itself at the end with rm -f "$0"
	uninstallScript := `#!/bin/bash
set -e

SCRIPT_PATH="$0"

# Stop and disable the service
systemctl stop rmm-agent 2>/dev/null || true
systemctl disable rmm-agent 2>/dev/null || true

# Remove systemd service file
rm -f /etc/systemd/system/rmm-agent.service

# Remove binaries
rm -f /usr/local/bin/rmm-agent
rm -f /usr/local/bin/rmm-agent-helper

# Remove configuration and data
rm -rf /etc/rmm-agent
rm -rf /var/lib/rmm-agent

# Reload systemd
systemctl daemon-reload

# Clean up this script
rm -f "$SCRIPT_PATH"

echo "RMM agent uninstalled successfully"
`

	// Write script to temp file
	tmpFile, err := os.CreateTemp("", "rmm-uninstall-*.sh")
	if err != nil {
		h.logger.Error("failed to create uninstall script",
			slog.String("error", err.Error()),
		)
		os.Exit(1)
	}
	scriptPath := tmpFile.Name()

	if _, err := tmpFile.WriteString(uninstallScript); err != nil {
		h.logger.Error("failed to write uninstall script",
			slog.String("error", err.Error()),
		)
		tmpFile.Close()
		os.Exit(1)
	}
	tmpFile.Close()

	// Make script executable
	if err := os.Chmod(scriptPath, 0700); err != nil {
		h.logger.Error("failed to chmod uninstall script",
			slog.String("error", err.Error()),
		)
		os.Exit(1)
	}

	h.logger.Info("running uninstall script", slog.String("path", scriptPath))

	// Execute the uninstall script
	// Use nohup to continue even after this process exits
	cmd := exec.Command("/bin/bash", "-c", "nohup "+scriptPath+" > /dev/null 2>&1 &")
	if err := cmd.Start(); err != nil {
		h.logger.Error("failed to start uninstall script",
			slog.String("error", err.Error()),
		)
	}

	// Exit the agent process
	h.logger.Info("agent exiting for uninstall")
	os.Exit(0)
}
