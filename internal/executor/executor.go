// executor.go implements shell command execution with timeout and process group management.
// It ensures all child processes are killed on timeout using process groups,
// preventing orphan processes from accumulating.
package executor

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Executor runs shell commands with timeout and output capture.
type Executor struct {
	// Shell is the shell to use for command execution. Default: /bin/sh
	Shell string
}

// New creates a new Executor with default settings.
func New() *Executor {
	return &Executor{
		Shell: "/bin/sh",
	}
}

// Execute runs a command with the given timeout.
// It creates a new process group and kills all processes in the group on timeout.
// Returns Result with stdout, stderr, exit code, duration, and timeout status.
func (e *Executor) Execute(ctx context.Context, command string, timeout time.Duration) (*Result, error) {
	// Create context with timeout
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(execCtx, e.Shell, "-c", command)

	// Create new process group so we can kill all children
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Capture stdout and stderr separately
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Custom cancel function to kill process group
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Kill entire process group (negative PID)
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	// WaitDelay ensures orphaned processes don't block Wait()
	cmd.WaitDelay = 5 * time.Second

	result := &Result{
		StartedAt: time.Now(),
	}

	err := cmd.Run()
	result.Duration = time.Since(result.StartedAt)
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()

	if err != nil {
		// Check if timeout occurred
		if execCtx.Err() == context.DeadlineExceeded {
			result.ExitCode = -1
			result.TimedOut = true
			return result, nil
		}

		// Check for exit error (non-zero exit code)
		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}

		// Other error (command not found, permission denied, etc.)
		return nil, fmt.Errorf("execution failed: %w", err)
	}

	result.ExitCode = 0
	return result, nil
}

// ExecuteWithResult is a convenience method that wraps Execute and handles errors
// by returning them in the Result stderr field.
func (e *Executor) ExecuteWithResult(ctx context.Context, command string, timeout time.Duration) *Result {
	result, err := e.Execute(ctx, command, timeout)
	if err != nil {
		return &Result{
			ExitCode:  -1,
			Stderr:    err.Error(),
			StartedAt: time.Now(),
			Duration:  0,
		}
	}
	return result
}

// ExecuteScript runs a script with a specific interpreter.
// It verifies the interpreter exists before execution (SCRP-07, EXEC-06).
// For scripts, the content is passed via stdin to the interpreter.
func (e *Executor) ExecuteScript(ctx context.Context, content string, interpreter string, timeout time.Duration) (*Result, error) {
	// Verify interpreter exists (fail fast)
	interpreterPath, err := VerifyInterpreter(interpreter)
	if err != nil {
		// Return result with error, not error - so it gets reported to server
		return &Result{
			ExitCode:  -1,
			Stderr:    fmt.Sprintf("Interpreter verification failed: %s", err.Error()),
			StartedAt: time.Now(),
			Duration:  0,
		}, nil
	}

	// Create context with timeout
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Execute with interpreter, content via stdin
	cmd := exec.CommandContext(execCtx, interpreterPath)

	// Create new process group so we can kill all children
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Pass script content via stdin
	cmd.Stdin = strings.NewReader(content)

	// Capture stdout and stderr separately
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Custom cancel function to kill process group
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}

	cmd.WaitDelay = 5 * time.Second

	result := &Result{
		StartedAt: time.Now(),
	}

	err = cmd.Run()
	result.Duration = time.Since(result.StartedAt)
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()

	if err != nil {
		if execCtx.Err() == context.DeadlineExceeded {
			result.ExitCode = -1
			result.TimedOut = true
			return result, nil
		}

		if exitErr, ok := err.(*exec.ExitError); ok {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}

		return nil, fmt.Errorf("execution failed: %w", err)
	}

	result.ExitCode = 0
	return result, nil
}
