// commands.go provides HTTP client methods for the command API.
// These methods handle fetching pending commands and reporting execution results.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// PendingCommand represents a command fetched from the server.
type PendingCommand struct {
	ID          string `json:"id"`
	Command     string `json:"command"`
	Timeout     int64  `json:"timeout"`               // milliseconds
	Interpreter string `json:"interpreter,omitempty"` // Interpreter for script execution (bash, sh, python, perl)
}

// CommandResult is sent to report execution results.
type CommandResult struct {
	Stdout            string `json:"stdout"`
	Stderr            string `json:"stderr"`
	ExitCode          int    `json:"exitCode"`
	ExecutionDuration int64  `json:"executionDuration"` // milliseconds
	TimedOut          bool   `json:"timedOut"`
}

// FetchPendingCommands retrieves pending commands for this agent.
func (c *Client) FetchPendingCommands(ctx context.Context) ([]PendingCommand, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.serverURL+"/api/commands/pending", nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch commands: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	var result struct {
		Commands []PendingCommand `json:"commands"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return result.Commands, nil
}

// ReportCommandStarted notifies the server that command execution has begun.
func (c *Client) ReportCommandStarted(ctx context.Context, commandID string) error {
	req, err := http.NewRequestWithContext(ctx, "POST", c.serverURL+"/api/commands/"+commandID+"/start", nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("report start: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	return nil
}

// ReportCommandResult sends execution results to the server.
func (c *Client) ReportCommandResult(ctx context.Context, commandID string, result *CommandResult) error {
	body, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.serverURL+"/api/commands/"+commandID+"/result", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("report result: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	return nil
}

// GetTimeout returns the timeout duration for a command, defaulting to 60 seconds.
func (cmd *PendingCommand) GetTimeout() time.Duration {
	if cmd.Timeout <= 0 {
		return 60 * time.Second
	}
	return time.Duration(cmd.Timeout) * time.Millisecond
}
