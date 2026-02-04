// Registration handling for the RMM agent.
//
// This file implements the agent registration flow. New agents register with
// the server using a one-time device token and receive an API key for future
// authentication.
//
// The registration flow:
// 1. Agent sends POST /api/agents/register with { device_token, hostname }
// 2. Server validates token, marks it as used, creates agent record
// 3. Server returns { agent_id, api_key }
// 4. Agent stores api_key in config file for future requests
//
// The API key is only returned once during registration. It must be persisted
// to the config file immediately after registration (see config.Save).
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/hashicorp/go-retryablehttp"
)

// registrationRequest is the JSON body sent to POST /api/agents/register.
type registrationRequest struct {
	DeviceToken string `json:"device_token"`
	Hostname    string `json:"hostname"`
}

// registrationResponse is the JSON response from POST /api/agents/register.
type registrationResponse struct {
	AgentID  string                `json:"agent_id"`
	APIKey   string                `json:"api_key"`
	TenantID string                `json:"tenant_id"`
	NATS     *natsRegistrationInfo `json:"nats,omitempty"`
}

// natsRegistrationInfo contains NATS credentials from registration.
type natsRegistrationInfo struct {
	Servers  string `json:"servers"`
	NKeySeed string `json:"nkey_seed"`
}

// RegistrationResult contains all credentials received from registration.
type RegistrationResult struct {
	AgentID      string
	APIKey       string
	TenantID     string
	NATSServers  string
	NATSNKeySeed string
}

// registrationError is the JSON error response from the server.
type registrationError struct {
	Error string `json:"error"`
}

// ErrInvalidToken indicates the device token was invalid or already used.
var ErrInvalidToken = fmt.Errorf("invalid or already used device token")

// ErrBadRequest indicates a malformed registration request.
var ErrBadRequest = fmt.Errorf("bad registration request")

// Register registers a new agent with the RMM server.
//
// It sends the device token and hostname to the server and receives credentials
// upon successful registration. The credentials should be immediately saved to
// the config file using config.Save().
//
// Parameters:
//   - ctx: Context for cancellation/timeout
//   - serverURL: Base URL of the RMM server (e.g., "https://rmm.example.com")
//   - deviceToken: One-time registration token from the server
//   - hostname: This machine's hostname
//
// Returns:
//   - result: All credentials including agent ID, API key, tenant ID, and NATS config
//   - error: ErrInvalidToken (401), ErrBadRequest (400), or network/server error
//
// Usage:
//
//	result, err := client.RegisterFull(ctx, serverURL, token, hostname, logger)
//	if err != nil {
//	    return err
//	}
//	cfg.APIKey = result.APIKey
//	cfg.TenantID = result.TenantID
//	config.Save(configPath, cfg)
func RegisterFull(ctx context.Context, serverURL, deviceToken, hostname string, logger *slog.Logger) (*RegistrationResult, error) {
	// Create a dedicated HTTP client for registration (no auth needed)
	retryClient := retryablehttp.NewClient()
	retryClient.RetryMax = 3
	retryClient.RetryWaitMin = 1 * time.Second
	retryClient.RetryWaitMax = 10 * time.Second
	retryClient.Backoff = retryablehttp.LinearJitterBackoff
	retryClient.Logger = nil
	retryClient.HTTPClient.Timeout = 30 * time.Second

	httpClient := retryClient.StandardClient()

	// Build request body
	reqBody := registrationRequest{
		DeviceToken: deviceToken,
		Hostname:    hostname,
	}
	bodyData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal registration request: %w", err)
	}

	url := serverURL + "/api/agents/register"

	logger.Info("registering with server",
		slog.String("url", url),
		slog.String("hostname", hostname),
	)

	// Create request with context
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyData))
	if err != nil {
		return nil, fmt.Errorf("failed to create registration request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Send request
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registration request failed: %w", err)
	}
	defer resp.Body.Close()

	// Read response body
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read registration response: %w", err)
	}

	// Handle error responses
	switch resp.StatusCode {
	case http.StatusCreated: // 201 - Success
		var regResp registrationResponse
		if err := json.Unmarshal(respBody, &regResp); err != nil {
			return nil, fmt.Errorf("failed to parse registration response: %w", err)
		}

		result := &RegistrationResult{
			AgentID:  regResp.AgentID,
			APIKey:   regResp.APIKey,
			TenantID: regResp.TenantID,
		}

		// Extract NATS credentials if present
		if regResp.NATS != nil {
			result.NATSServers = regResp.NATS.Servers
			result.NATSNKeySeed = regResp.NATS.NKeySeed
		}

		logger.Info("registration successful",
			slog.String("agent_id", result.AgentID),
			slog.String("tenant_id", result.TenantID),
			slog.Bool("nats_enabled", result.NATSServers != ""),
		)

		return result, nil

	case http.StatusUnauthorized:
		return nil, ErrInvalidToken

	case http.StatusBadRequest:
		var errResp registrationError
		if err := json.Unmarshal(respBody, &errResp); err == nil && errResp.Error != "" {
			return nil, fmt.Errorf("%w: %s", ErrBadRequest, errResp.Error)
		}
		return nil, ErrBadRequest

	default:
		var errResp registrationError
		if err := json.Unmarshal(respBody, &errResp); err == nil && errResp.Error != "" {
			return nil, fmt.Errorf("registration failed with status %d: %s", resp.StatusCode, errResp.Error)
		}
		return nil, fmt.Errorf("registration failed with status %d", resp.StatusCode)
	}
}

// Register registers a new agent with the RMM server (legacy interface).
//
// Deprecated: Use RegisterFull for access to all credentials including NATS.
func Register(ctx context.Context, serverURL, deviceToken, hostname string, logger *slog.Logger) (agentID, apiKey string, err error) {
	// Create a dedicated HTTP client for registration (no auth needed)
	retryClient := retryablehttp.NewClient()
	retryClient.RetryMax = 3
	retryClient.RetryWaitMin = 1 * time.Second
	retryClient.RetryWaitMax = 10 * time.Second
	retryClient.Backoff = retryablehttp.LinearJitterBackoff
	retryClient.Logger = nil
	retryClient.HTTPClient.Timeout = 30 * time.Second

	httpClient := retryClient.StandardClient()

	// Build request body
	reqBody := registrationRequest{
		DeviceToken: deviceToken,
		Hostname:    hostname,
	}
	bodyData, err := json.Marshal(reqBody)
	if err != nil {
		return "", "", fmt.Errorf("failed to marshal registration request: %w", err)
	}

	url := serverURL + "/api/agents/register"

	logger.Info("registering with server",
		slog.String("url", url),
		slog.String("hostname", hostname),
	)

	// Create request with context
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyData))
	if err != nil {
		return "", "", fmt.Errorf("failed to create registration request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Send request
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("registration request failed: %w", err)
	}
	// CRITICAL: Always close response body to prevent connection leaks
	defer resp.Body.Close()

	// Read response body for error handling
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("failed to read registration response: %w", err)
	}

	// Handle error responses
	switch resp.StatusCode {
	case http.StatusCreated: // 201 - Success
		// Parse successful response
		var result registrationResponse
		if err := json.Unmarshal(respBody, &result); err != nil {
			return "", "", fmt.Errorf("failed to parse registration response: %w", err)
		}

		// Log NATS availability
		natsEnabled := result.NATS != nil && result.NATS.Servers != ""
		logger.Info("registration successful",
			slog.String("agent_id", result.AgentID),
			slog.String("tenant_id", result.TenantID),
			slog.Bool("nats_enabled", natsEnabled),
		)

		return result.AgentID, result.APIKey, nil

	case http.StatusUnauthorized: // 401 - Invalid token
		return "", "", ErrInvalidToken

	case http.StatusBadRequest: // 400 - Bad request
		var errResp registrationError
		if err := json.Unmarshal(respBody, &errResp); err == nil && errResp.Error != "" {
			return "", "", fmt.Errorf("%w: %s", ErrBadRequest, errResp.Error)
		}
		return "", "", ErrBadRequest

	default:
		// Unexpected status code
		var errResp registrationError
		if err := json.Unmarshal(respBody, &errResp); err == nil && errResp.Error != "" {
			return "", "", fmt.Errorf("registration failed with status %d: %s", resp.StatusCode, errResp.Error)
		}
		return "", "", fmt.Errorf("registration failed with status %d", resp.StatusCode)
	}
}
