// Package scheduler implements local schedule execution for offline operation.
// This file implements the scheduler loop that checks for due schedules every
// 30 seconds and executes them using the existing executor infrastructure.

package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/doughall/linuxrmm/agent/internal/executor"
	"github.com/doughall/linuxrmm/agent/internal/results"
)

// Scheduler runs the local schedule execution loop
type Scheduler struct {
	cache       *ScheduleCache
	executor    *executor.Executor
	resultQueue *results.ResultQueue
	logger      *slog.Logger
}

// NewScheduler creates a new local scheduler
func NewScheduler(cache *ScheduleCache, resultQueue *results.ResultQueue, logger *slog.Logger) *Scheduler {
	return &Scheduler{
		cache:       cache,
		executor:    executor.New(),
		resultQueue: resultQueue,
		logger:      logger,
	}
}

// Run starts the scheduler loop (blocking).
// It checks for due schedules every 30 seconds and executes them.
// This should be run in a goroutine.
func (s *Scheduler) Run(ctx context.Context) {
	s.logger.Info("scheduler started")
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// Check immediately on startup to catch any overdue schedules
	s.processSchedules(ctx)

	for {
		select {
		case <-ctx.Done():
			s.logger.Info("scheduler stopping")
			return
		case <-ticker.C:
			s.processSchedules(ctx)
		}
	}
}

// processSchedules checks for due schedules and executes them
func (s *Scheduler) processSchedules(ctx context.Context) {
	due, err := s.cache.GetDueSchedules()
	if err != nil {
		s.logger.Error("failed to get due schedules",
			slog.String("error", err.Error()),
		)
		return
	}

	if len(due) == 0 {
		s.logger.Debug("no due schedules")
		return
	}

	s.logger.Info("found due schedules",
		slog.Int("count", len(due)),
	)

	for _, schedule := range due {
		s.executeSchedule(ctx, schedule)
	}
}

// executeSchedule executes a single schedule and queues the result
func (s *Scheduler) executeSchedule(ctx context.Context, schedule *CachedSchedule) {
	schedLogger := s.logger.With(
		slog.String("schedule_id", schedule.ID),
		slog.String("interpreter", schedule.Interpreter),
	)

	schedLogger.Info("executing schedule")
	startedAt := time.Now()

	// Determine timeout (default 60 seconds)
	timeout := 60 * time.Second

	// Execute the script
	var result *executor.Result
	var execErr error

	if schedule.Interpreter != "" && schedule.ScriptContent != "" {
		// Execute as script with interpreter
		result, execErr = s.executor.ExecuteScript(ctx, schedule.ScriptContent, schedule.Interpreter, timeout)
	} else if schedule.ScriptContent != "" {
		// Execute as shell command
		result, execErr = s.executor.Execute(ctx, schedule.ScriptContent, timeout)
	} else {
		schedLogger.Warn("schedule has no script content, skipping")
		s.updateNextRun(schedule)
		return
	}

	// Build result for queueing
	pendingResult := &results.PendingResult{
		ScheduleID: schedule.ID,
		ExecutedAt: startedAt,
		DurationMs: 0,
		ExitCode:   -1,
		Error:      "",
		TimedOut:   false,
	}

	// Handle execution error
	if execErr != nil {
		pendingResult.Error = execErr.Error()
		schedLogger.Warn("schedule execution failed",
			slog.String("error", execErr.Error()),
		)
	} else if result != nil {
		// Success - populate result
		pendingResult.ExitCode = result.ExitCode
		pendingResult.Stdout = result.Stdout
		pendingResult.Stderr = result.Stderr
		pendingResult.DurationMs = result.Duration.Milliseconds()
		pendingResult.TimedOut = result.TimedOut
	}

	// Determine if output should be stored based on retention policy
	shouldStore := s.shouldStoreOutput(schedule.OutputRetention, pendingResult, execErr)

	if shouldStore {
		// Queue result for upload
		if err := s.resultQueue.Enqueue(pendingResult); err != nil {
			schedLogger.Error("failed to queue result",
				slog.String("error", err.Error()),
			)
		} else {
			schedLogger.Debug("result queued for upload")
		}
	} else {
		schedLogger.Debug("output not stored per retention policy",
			slog.String("policy", schedule.OutputRetention),
			slog.Int("exit_code", pendingResult.ExitCode),
		)
	}

	schedLogger.Info("schedule execution complete",
		slog.Int("exit_code", pendingResult.ExitCode),
		slog.Bool("timed_out", pendingResult.TimedOut),
		slog.Int64("duration_ms", pendingResult.DurationMs),
	)

	// Update nextRunAt for next execution
	s.updateNextRun(schedule)
}

// shouldStoreOutput determines if output should be queued based on retention policy
func (s *Scheduler) shouldStoreOutput(policy string, result *results.PendingResult, err error) bool {
	switch policy {
	case "always":
		return true
	case "on_failure":
		// Store if execution error or non-zero exit code
		return err != nil || result.ExitCode != 0
	case "never":
		return false
	default:
		// Default to always if policy is unrecognized
		s.logger.Warn("unknown output retention policy, defaulting to always",
			slog.String("policy", policy),
		)
		return true
	}
}

// updateNextRun calculates and saves the next run time for a schedule
func (s *Scheduler) updateNextRun(schedule *CachedSchedule) {
	nextRun := s.cache.CalculateNextRun(schedule)
	schedule.NextRunAt = nextRun

	if err := s.cache.SaveSchedule(schedule); err != nil {
		s.logger.Error("failed to update next run time",
			slog.String("schedule_id", schedule.ID),
			slog.String("error", err.Error()),
		)
	} else {
		s.logger.Debug("updated next run time",
			slog.String("schedule_id", schedule.ID),
			slog.Time("next_run_at", nextRun),
		)
	}
}

// Shutdown gracefully stops the scheduler.
// This is a no-op as context cancellation handles shutdown.
func (s *Scheduler) Shutdown(ctx context.Context) error {
	s.logger.Info("scheduler shutdown initiated")
	return nil
}
