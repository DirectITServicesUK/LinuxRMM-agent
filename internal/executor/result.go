// result.go defines the command execution result structure.
// It captures all output from command execution including stdout, stderr,
// exit code, duration, and timeout status for reporting to the server.
package executor

import "time"

// Result holds the output of a command execution.
type Result struct {
	// ExitCode is the process exit code. -1 indicates timeout or signal death.
	ExitCode int `json:"exit_code"`

	// Stdout contains the standard output of the command.
	Stdout string `json:"stdout"`

	// Stderr contains the standard error output of the command.
	Stderr string `json:"stderr"`

	// Duration is how long the command took to execute.
	Duration time.Duration `json:"duration_ms"`

	// TimedOut is true if the command was killed due to timeout.
	TimedOut bool `json:"timed_out"`

	// StartedAt is when execution began.
	StartedAt time.Time `json:"started_at"`
}

// DurationMs returns the duration in milliseconds for JSON serialization.
func (r *Result) DurationMs() int64 {
	return r.Duration.Milliseconds()
}
