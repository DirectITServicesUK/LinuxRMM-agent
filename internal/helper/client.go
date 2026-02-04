// client.go provides a client for communicating with the privileged helper via Unix socket.
// The helper runs as root and executes commands that require elevated privileges.
package helper

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/doughall/linuxrmm/agent/internal/executor"
)

// Client communicates with the privileged helper.
type Client struct {
	socketPath string
}

// NewClient creates a new helper client.
func NewClient() *Client {
	return &Client{
		socketPath: SocketPath,
	}
}

// Available returns true if the helper socket exists and is accessible.
func (c *Client) Available() bool {
	conn, err := net.DialTimeout("unix", c.socketPath, time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Execute sends a command to the helper for privileged execution.
func (c *Client) Execute(ctx context.Context, command string, timeout time.Duration) (*executor.Result, error) {
	conn, err := net.DialTimeout("unix", c.socketPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("helper not available: %w", err)
	}
	defer conn.Close()

	// Set deadline from context
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}

	// Send request
	req := Request{
		Type:    RequestTypeExecute,
		Command: command,
		Timeout: timeout.Milliseconds(),
	}
	encoder := json.NewEncoder(conn)
	if err := encoder.Encode(&req); err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	// Receive response
	var resp Response
	decoder := json.NewDecoder(conn)
	if err := decoder.Decode(&resp); err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if !resp.Success && resp.Error != "" {
		return nil, fmt.Errorf("helper error: %s", resp.Error)
	}

	return &executor.Result{
		ExitCode:  resp.ExitCode,
		Stdout:    resp.Stdout,
		Stderr:    resp.Stderr,
		Duration:  time.Duration(resp.Duration) * time.Millisecond,
		TimedOut:  resp.TimedOut,
		StartedAt: time.Now().Add(-time.Duration(resp.Duration) * time.Millisecond),
	}, nil
}
