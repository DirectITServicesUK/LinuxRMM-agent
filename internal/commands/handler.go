// handler.go coordinates command execution between the poller, executor, and server.
// It handles the full lifecycle: fetch commands, mark as started, execute, report results.
//
// The handler includes deduplication to prevent duplicate execution when commands
// arrive via both WebSocket push and HTTP polling (dual delivery mechanism).
package commands

import (
	"context"
	"log/slog"

	"github.com/doughall/linuxrmm/agent/internal/client"
	"github.com/doughall/linuxrmm/agent/internal/executor"
	"github.com/doughall/linuxrmm/agent/internal/helper"
)

// Handler processes commands from the server.
type Handler struct {
	client       *client.Client
	executor     *executor.Executor
	helperClient *helper.Client
	dedup        *Deduplicator
	logger       *slog.Logger
}

// NewHandler creates a new command handler.
func NewHandler(c *client.Client, logger *slog.Logger) *Handler {
	return &Handler{
		client:       c,
		executor:     executor.New(),
		helperClient: helper.NewClient(),
		dedup:        NewDeduplicator(logger),
		logger:       logger,
	}
}

// GetDeduplicator returns the handler's deduplicator for use by other components.
// This allows the WebSocket handler to share deduplication state with the poller.
func (h *Handler) GetDeduplicator() *Deduplicator {
	return h.dedup
}

// ProcessPendingCommands fetches and executes all pending commands.
// Returns the number of commands processed.
//
// Commands are checked against the deduplicator to prevent double execution
// when the same command arrives via both WebSocket and HTTP polling.
func (h *Handler) ProcessPendingCommands(ctx context.Context) int {
	commands, err := h.client.FetchPendingCommands(ctx)
	if err != nil {
		h.logger.Error("failed to fetch commands", slog.String("error", err.Error()))
		return 0
	}

	if len(commands) == 0 {
		return 0
	}

	h.logger.Info("processing commands", slog.Int("count", len(commands)))

	processed := 0
	for _, cmd := range commands {
		// Check deduplication - skip if already processed (e.g., via WebSocket)
		if !h.dedup.MarkSeen(cmd.ID) {
			h.logger.Debug("skipping duplicate command from poll",
				slog.String("command_id", cmd.ID),
			)
			continue
		}

		if err := h.executeCommand(ctx, cmd); err != nil {
			h.logger.Error("command execution failed",
				slog.String("command_id", cmd.ID),
				slog.String("error", err.Error()),
			)
			continue
		}
		processed++
	}

	return processed
}

// executeCommand handles a single command: mark started, execute, report result.
func (h *Handler) executeCommand(ctx context.Context, cmd client.PendingCommand) error {
	cmdLogger := h.logger.With(slog.String("command_id", cmd.ID))

	// Mark as started
	if err := h.client.ReportCommandStarted(ctx, cmd.ID); err != nil {
		cmdLogger.Warn("failed to report start", slog.String("error", err.Error()))
		// Continue anyway - execution is more important than status tracking
	}

	cmdLogger.Info("executing command",
		slog.Int64("timeout_ms", cmd.Timeout),
		slog.String("interpreter", cmd.Interpreter),
	)

	// Execute command
	timeout := cmd.GetTimeout()
	var result *executor.Result
	var execErr error

	// If interpreter is specified, use script execution with interpreter verification
	if cmd.Interpreter != "" {
		result, execErr = h.executor.ExecuteScript(ctx, cmd.Command, cmd.Interpreter, timeout)
	} else {
		// Ad-hoc command - try privileged execution first if helper is available
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
	}

	// Handle execution error (not exit code errors)
	if execErr != nil {
		// Report failure
		failResult := &client.CommandResult{
			Stderr:            execErr.Error(),
			ExitCode:          -1,
			ExecutionDuration: 0,
			TimedOut:          false,
		}
		if err := h.client.ReportCommandResult(ctx, cmd.ID, failResult); err != nil {
			cmdLogger.Error("failed to report error result", slog.String("error", err.Error()))
		}
		return execErr
	}

	// Report success/completion
	cmdResult := &client.CommandResult{
		Stdout:            result.Stdout,
		Stderr:            result.Stderr,
		ExitCode:          result.ExitCode,
		ExecutionDuration: result.Duration.Milliseconds(),
		TimedOut:          result.TimedOut,
	}

	if err := h.client.ReportCommandResult(ctx, cmd.ID, cmdResult); err != nil {
		cmdLogger.Error("failed to report result", slog.String("error", err.Error()))
		return err
	}

	cmdLogger.Info("command completed",
		slog.Int("exit_code", result.ExitCode),
		slog.Bool("timed_out", result.TimedOut),
		slog.Int64("duration_ms", result.Duration.Milliseconds()),
	)

	return nil
}
