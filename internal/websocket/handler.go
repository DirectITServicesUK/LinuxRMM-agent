// handler.go implements WebSocket message handling for the RMM agent.
//
// The handler processes incoming messages from the WebSocket connection,
// routing them by type. The primary message type is "command" which triggers
// immediate command execution for time-sensitive operations.
//
// Message format (JSON):
//
//	{
//	  "type": "command",
//	  "payload": {
//	    "id": "command-id",
//	    "command": "echo hello",
//	    "timeout": 60000,
//	    "runAsRoot": false
//	  }
//	}
//
// The handler uses the same command execution infrastructure as HTTP polling,
// ensuring consistent behavior regardless of delivery mechanism.
//
// Deduplication: The handler shares a deduplicator with the poller command handler
// to prevent duplicate execution when commands arrive via both channels.
package websocket

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/doughall/linuxrmm/agent/internal/client"
	"github.com/doughall/linuxrmm/agent/internal/commands"
	"github.com/doughall/linuxrmm/agent/internal/executor"
	"github.com/doughall/linuxrmm/agent/internal/helper"
	"github.com/doughall/linuxrmm/agent/internal/scheduler"
	"github.com/gorilla/websocket"
)

// Handler processes incoming WebSocket messages.
type Handler struct {
	executor      *executor.Executor
	helperClient  *helper.Client
	httpClient    *client.Client
	dedup         *commands.Deduplicator
	scheduleCache *scheduler.ScheduleCache
	logger        *slog.Logger
}

// NewHandler creates a new WebSocket message handler.
// executor runs unprivileged commands, helperClient runs privileged commands,
// httpClient reports results to the server.
// dedup is shared with the poller command handler for cross-channel deduplication.
// scheduleCache stores schedule assignments for offline execution.
func NewHandler(httpClient *client.Client, dedup *commands.Deduplicator, scheduleCache *scheduler.ScheduleCache, logger *slog.Logger) *Handler {
	return &Handler{
		executor:      executor.New(),
		helperClient:  helper.NewClient(),
		httpClient:    httpClient,
		dedup:         dedup,
		scheduleCache: scheduleCache,
		logger:        logger,
	}
}

// WSMessage is the envelope for all WebSocket messages.
type WSMessage struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// CommandPayload contains the details of a command to execute.
type CommandPayload struct {
	ID        string `json:"id"`
	Command   string `json:"command"`
	Timeout   int64  `json:"timeout"`   // milliseconds
	RunAsRoot bool   `json:"runAsRoot"` // reserved for future use
}

// HandleMessage processes a message received from the WebSocket.
// Implements the MessageHandler interface.
func (h *Handler) HandleMessage(messageType int, data []byte) {
	// Only handle text messages
	if messageType != websocket.TextMessage {
		h.logger.Debug("ignoring non-text websocket message",
			slog.Int("message_type", messageType),
		)
		return
	}

	// Parse the envelope
	var msg WSMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		h.logger.Warn("failed to parse websocket message",
			slog.String("error", err.Error()),
		)
		return
	}

	// Route by message type
	switch msg.Type {
	case "command":
		h.handleCommand(msg.Payload)

	case "schedule_assignment":
		h.handleScheduleAssignment(msg.Payload)

	case "ping":
		// Application-level ping - just log it
		// Server uses WebSocket ping/pong frames for keepalive
		h.logger.Debug("received ping message")

	default:
		h.logger.Warn("unknown websocket message type",
			slog.String("type", msg.Type),
		)
	}
}

// handleCommand processes a command message from the WebSocket.
func (h *Handler) handleCommand(payload json.RawMessage) {
	// Parse command payload
	var cmd CommandPayload
	if err := json.Unmarshal(payload, &cmd); err != nil {
		h.logger.Warn("failed to parse command payload",
			slog.String("error", err.Error()),
		)
		return
	}

	cmdLogger := h.logger.With(slog.String("command_id", cmd.ID))
	cmdLogger.Info("received command via websocket",
		slog.Int64("timeout_ms", cmd.Timeout),
	)

	// Check deduplication - skip if already processed (e.g., via poll)
	if h.dedup != nil && !h.dedup.MarkSeen(cmd.ID) {
		cmdLogger.Debug("skipping duplicate command from websocket")
		return
	}

	// Execute command in goroutine to not block the read loop
	go h.executeCommand(cmd)
}

// executeCommand executes a command and reports the result.
// This mirrors the logic in commands/handler.go for consistency.
func (h *Handler) executeCommand(cmd CommandPayload) {
	ctx := context.Background()
	cmdLogger := h.logger.With(slog.String("command_id", cmd.ID))

	// Mark as started
	if err := h.httpClient.ReportCommandStarted(ctx, cmd.ID); err != nil {
		cmdLogger.Warn("failed to report start",
			slog.String("error", err.Error()),
		)
		// Continue anyway - execution is more important than status tracking
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

	// Handle execution error
	if execErr != nil {
		failResult := &client.CommandResult{
			Stderr:            execErr.Error(),
			ExitCode:          -1,
			ExecutionDuration: 0,
			TimedOut:          false,
		}
		if err := h.httpClient.ReportCommandResult(ctx, cmd.ID, failResult); err != nil {
			cmdLogger.Error("failed to report error result",
				slog.String("error", err.Error()),
			)
		}
		return
	}

	// Report success/completion
	cmdResult := &client.CommandResult{
		Stdout:            result.Stdout,
		Stderr:            result.Stderr,
		ExitCode:          result.ExitCode,
		ExecutionDuration: result.Duration.Milliseconds(),
		TimedOut:          result.TimedOut,
	}

	if err := h.httpClient.ReportCommandResult(ctx, cmd.ID, cmdResult); err != nil {
		cmdLogger.Error("failed to report result",
			slog.String("error", err.Error()),
		)
		return
	}

	cmdLogger.Info("command completed",
		slog.Int("exit_code", result.ExitCode),
		slog.Bool("timed_out", result.TimedOut),
		slog.Int64("duration_ms", result.Duration.Milliseconds()),
	)
}

// handleScheduleAssignment processes a schedule_assignment message from the WebSocket.
// This allows the server to push schedule updates and deletions to the agent.
func (h *Handler) handleScheduleAssignment(payload json.RawMessage) {
	// Parse into map for flexible field access
	var data map[string]any
	if err := json.Unmarshal(payload, &data); err != nil {
		h.logger.Warn("failed to parse schedule_assignment payload",
			slog.String("error", err.Error()),
		)
		return
	}

	// Get action (update or delete)
	action := getString(data, "action")
	if action == "" {
		h.logger.Warn("schedule_assignment missing action field")
		return
	}

	schedLogger := h.logger.With(slog.String("action", action))

	switch action {
	case "update":
		h.handleScheduleUpdate(data, schedLogger)
	case "delete":
		h.handleScheduleDelete(data, schedLogger)
	default:
		schedLogger.Warn("unknown schedule_assignment action")
	}
}

// handleScheduleUpdate processes a schedule update (create or modify)
func (h *Handler) handleScheduleUpdate(data map[string]any, logger *slog.Logger) {
	// Check if cache is available
	if h.scheduleCache == nil {
		logger.Warn("schedule cache not available, ignoring schedule update")
		return
	}

	// Extract schedule fields
	id := getString(data, "id")
	if id == "" {
		logger.Warn("schedule update missing id")
		return
	}

	schedule := &scheduler.CachedSchedule{
		ID:              id,
		ScriptContent:   getString(data, "scriptContent"),
		Interpreter:     getString(data, "interpreter"),
		CronExpression:  getString(data, "cronExpression"),
		IntervalMinutes: getInt(data, "intervalMinutes"),
		Variables:       getMap(data, "variables"),
		OutputRetention: getString(data, "outputRetention"),
		NextRunAt:       getTime(data, "nextRunAt"),
		LastSyncAt:      time.Now(),
	}

	// Validate required fields
	if schedule.ScriptContent == "" {
		logger.Warn("schedule update missing scriptContent")
		return
	}

	// Save to cache
	if err := h.scheduleCache.SaveSchedule(schedule); err != nil {
		logger.Error("failed to save schedule",
			slog.String("schedule_id", id),
			slog.String("error", err.Error()),
		)
		return
	}

	logger.Info("schedule cached for offline execution",
		slog.String("schedule_id", id),
		slog.Time("next_run_at", schedule.NextRunAt),
	)
}

// handleScheduleDelete processes a schedule deletion
func (h *Handler) handleScheduleDelete(data map[string]any, logger *slog.Logger) {
	// Check if cache is available
	if h.scheduleCache == nil {
		logger.Warn("schedule cache not available, ignoring schedule delete")
		return
	}

	id := getString(data, "id")
	if id == "" {
		logger.Warn("schedule delete missing id")
		return
	}

	if err := h.scheduleCache.DeleteSchedule(id); err != nil {
		logger.Error("failed to delete schedule",
			slog.String("schedule_id", id),
			slog.String("error", err.Error()),
		)
		return
	}

	logger.Info("schedule removed from cache",
		slog.String("schedule_id", id),
	)
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
		// Handle both float64 (JSON number) and int
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
			// Try parsing as RFC3339
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return t
			}
		}
	}
	return time.Now()
}
