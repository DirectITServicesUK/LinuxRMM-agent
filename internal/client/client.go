// Package client provides an HTTP client for communicating with the RMM server.
// It handles all agent-server communication including registration, heartbeat,
// and future command polling.
//
// The client uses hashicorp/go-retryablehttp for automatic retry with exponential
// backoff and jitter, which is essential for resilient communication in
// distributed systems where network hiccups are common.
//
// Usage:
//
//	client := client.NewClient("https://rmm.example.com", logger)
//	client.SetAPIKey(apiKey) // After registration
//	err := client.SendHeartbeat(ctx)
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"time"

	"github.com/doughall/linuxrmm/agent/internal/results"
	"github.com/doughall/linuxrmm/agent/internal/version"
	"github.com/hashicorp/go-retryablehttp"
)

// Client is the HTTP client for communicating with the RMM server.
// It wraps go-retryablehttp for automatic retry with backoff.
type Client struct {
	httpClient *http.Client
	serverURL  string
	apiKey     string
	agentID    string
	logger     *slog.Logger
}

// NewClient creates a new Client configured with retryable HTTP settings.
// The serverURL should be the base URL of the RMM server (e.g., "https://rmm.example.com").
// The logger is used for request-level logging.
//
// The client is configured with:
//   - RetryMax: 3 retries
//   - RetryWaitMin: 1 second
//   - RetryWaitMax: 10 seconds
//   - Backoff: Linear jitter (prevents thundering herd)
//   - Timeout: 30 seconds per request
func NewClient(serverURL string, logger *slog.Logger) *Client {
	// Create retryable HTTP client with proper configuration
	retryClient := retryablehttp.NewClient()
	retryClient.RetryMax = 3
	retryClient.RetryWaitMin = 1 * time.Second
	retryClient.RetryWaitMax = 10 * time.Second
	retryClient.Backoff = retryablehttp.LinearJitterBackoff

	// Disable retryablehttp's internal logging - we use slog instead
	retryClient.Logger = nil

	// Configure underlying HTTP client timeout
	retryClient.HTTPClient.Timeout = 30 * time.Second

	// Configure transport for long-running polling
	retryClient.HTTPClient.Transport = &http.Transport{
		MaxIdleConns:        10,
		IdleConnTimeout:     60 * time.Second,
		MaxIdleConnsPerHost: 2,
		DisableCompression:  false,
	}

	return &Client{
		httpClient: retryClient.StandardClient(),
		serverURL:  serverURL,
		logger:     logger,
	}
}

// SetAPIKey sets the API key used for authenticating requests after registration.
// This should be called after successful registration with the key received from
// the server.
func (c *Client) SetAPIKey(key string) {
	c.apiKey = key
}

// SetAgentID sets the agent's unique identifier for URL construction.
// This should be called after successful registration with the ID received from the server.
func (c *Client) SetAgentID(id string) {
	c.agentID = id
}

// SendHeartbeat sends a heartbeat to the server to indicate the agent is alive.
// This should be called periodically (e.g., every 60 seconds) to maintain
// ACTIVE status. The server uses lastSeen to detect offline agents.
//
// The request is authenticated using the Bearer token (API key) set via SetAPIKey.
// Returns an error if the API key is not set or the request fails.
func (c *Client) SendHeartbeat(ctx context.Context) error {
	if c.apiKey == "" {
		return fmt.Errorf("api key not set: call SetAPIKey first or complete registration")
	}

	url := c.serverURL + "/api/agents/heartbeat"

	// Create request with context for cancellation support
	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create heartbeat request: %w", err)
	}

	// Set authorization header
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	// Set version headers for server-side tracking (used by rollout system)
	// Platform format matches rollout model enum: linux-amd64, linux-arm64
	req.Header.Set("X-Agent-Version", version.Version)
	req.Header.Set("X-Agent-Platform", runtime.GOOS+"-"+runtime.GOARCH)

	c.logger.Debug("sending heartbeat",
		slog.String("url", url),
	)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("heartbeat request failed: %w", err)
	}
	// CRITICAL: Always close response body to prevent connection leaks
	defer resp.Body.Close()

	// Drain body to allow connection reuse
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("heartbeat failed with status %d", resp.StatusCode)
	}

	c.logger.Debug("heartbeat successful")
	return nil
}

// heartbeatResponse represents the server response to a heartbeat.
// Currently unused but available for future extensions.
type heartbeatResponse struct {
	Status     string `json:"status"`
	ServerTime string `json:"server_time"`
}

// StatsPayload represents system statistics to send to the server.
// Network counters are deltas (bytes since last report), not cumulative.
type StatsPayload struct {
	Timestamp    time.Time `json:"timestamp"`
	CPU          float64   `json:"cpu"`
	MemoryUsed   uint64    `json:"memoryUsed"`
	MemoryTotal  uint64    `json:"memoryTotal"`
	MemoryPct    float64   `json:"memoryPct"`
	DiskUsed     uint64    `json:"diskUsed"`
	DiskTotal    uint64    `json:"diskTotal"`
	DiskPct      float64   `json:"diskPct"`
	NetBytesSent uint64    `json:"netBytesSent"` // Delta since last report
	NetBytesRecv uint64    `json:"netBytesRecv"` // Delta since last report
	Load1        float64   `json:"load1"`
	Load5        float64   `json:"load5"`
	Load15       float64   `json:"load15"`
	Uptime       uint64    `json:"uptime"`
}

// SendStats sends system statistics to the server.
// This is called periodically (e.g., every 60 seconds) to report system metrics.
//
// The stats are posted to /api/agents/:agentId/stats where agentId is derived
// from the API key. The server validates the agent owns the API key.
//
// Returns an error if the request fails. The caller decides whether to retry
// or continue (stats are typically non-critical).
func (c *Client) SendStats(ctx context.Context, stats *StatsPayload) error {
	if c.apiKey == "" {
		return fmt.Errorf("api key not set: call SetAPIKey first or complete registration")
	}
	if c.agentID == "" {
		return fmt.Errorf("agent ID not set: call SetAgentID first or complete registration")
	}

	url := c.serverURL + "/api/agents/" + c.agentID + "/stats"

	c.logger.Debug("sending stats",
		slog.String("url", url),
		slog.Float64("cpu", stats.CPU),
		slog.Float64("memory_pct", stats.MemoryPct),
	)

	resp, err := c.doJSONRequest(ctx, "POST", url, stats, nil)
	if err != nil {
		return fmt.Errorf("stats request failed: %w", err)
	}

	// If doJSONRequest didn't close the body (response was nil), close it
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
		// Drain body to allow connection reuse
		_, _ = io.Copy(io.Discard, resp.Body)
	}

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stats failed with status %d", resp.StatusCode)
	}

	c.logger.Debug("stats sent successfully")
	return nil
}

// doJSONRequest is a helper for making JSON requests to the server.
// It handles JSON encoding of the request body and decoding of the response.
func (c *Client) doJSONRequest(ctx context.Context, method, url string, body interface{}, response interface{}) (*http.Response, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	// If response decoding is requested, do it before returning
	if response != nil {
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(response); err != nil {
			// Ensure body is drained even on decode error
			_, _ = io.Copy(io.Discard, resp.Body)
			return resp, fmt.Errorf("failed to decode response: %w", err)
		}
	}

	return resp, nil
}

// scheduleResultPayload represents the JSON payload for uploading schedule results.
type scheduleResultPayload struct {
	Results []scheduleResultItem `json:"results"`
}

// scheduleResultItem represents a single schedule result in the upload payload.
type scheduleResultItem struct {
	ScheduleID string `json:"schedule_id"`
	ExecutedAt string `json:"executed_at"`
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
	DurationMs int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
	TimedOut   bool   `json:"timed_out"`
}

// SubmitScheduleResults uploads a batch of schedule execution results to the server.
// This is called by the result uploader to sync queued results.
//
// The results are posted to /api/agents/schedule-results as a batch.
// The server expects a 201 Created response on success.
//
// Returns an error if the request fails. The caller should retry on the next cycle.
func (c *Client) SubmitScheduleResults(ctx context.Context, pendingResults []*results.PendingResult) error {
	if c.apiKey == "" {
		return fmt.Errorf("api key not set: call SetAPIKey first or complete registration")
	}

	url := c.serverURL + "/api/agents/schedule-results"

	// Transform results to JSON payload matching server schema
	payload := scheduleResultPayload{
		Results: make([]scheduleResultItem, len(pendingResults)),
	}
	for i, r := range pendingResults {
		payload.Results[i] = scheduleResultItem{
			ScheduleID: r.ScheduleID,
			ExecutedAt: r.ExecutedAt.Format(time.RFC3339),
			ExitCode:   r.ExitCode,
			Stdout:     r.Stdout,
			Stderr:     r.Stderr,
			DurationMs: r.DurationMs,
			Error:      r.Error,
			TimedOut:   r.TimedOut,
		}
	}

	c.logger.Debug("submitting schedule results",
		slog.String("url", url),
		slog.Int("count", len(pendingResults)),
	)

	resp, err := c.doJSONRequest(ctx, "POST", url, payload, nil)
	if err != nil {
		return fmt.Errorf("schedule results request failed: %w", err)
	}

	// Close and drain body
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
	}

	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("schedule results failed with status %d", resp.StatusCode)
	}

	c.logger.Debug("schedule results submitted successfully",
		slog.Int("count", len(pendingResults)),
	)
	return nil
}
