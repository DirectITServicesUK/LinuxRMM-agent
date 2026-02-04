// client.go provides a WebSocket client for receiving real-time command push from the RMM server.
//
// This client complements HTTP polling by providing immediate command delivery for time-sensitive
// operations. It maintains a persistent WebSocket connection to the server with automatic
// reconnection using exponential backoff with jitter.
//
// Connection lifecycle:
//  1. Connect to ws(s)://server/ws/agent?token=apiKey
//  2. Read messages and dispatch to handler
//  3. On disconnect, wait with exponential backoff (1s to 5m, +/-30% jitter)
//  4. Reconnect and resume
//
// Thread safety:
//   - The Client is safe for concurrent use
//   - Send() can be called from any goroutine
//   - Stop() cleanly terminates the Run() loop
//
// Usage:
//
//	handler := websocket.NewHandler(executor, client, logger)
//	wsClient := websocket.NewClient(serverURL, apiKey, logger, handler)
//	go wsClient.Run(ctx)
//	// Later...
//	wsClient.Stop()
package websocket

import (
	"context"
	"log/slog"
	"math/rand"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Exponential backoff configuration for reconnection
const (
	initialBackoff = 1 * time.Second  // Start with 1 second delay
	maxBackoff     = 5 * time.Minute  // Cap at 5 minutes
	backoffFactor  = 2.0              // Double each attempt
	jitterFactor   = 0.3              // +/- 30% random jitter
)

// MessageHandler processes messages received from the WebSocket connection.
// Implementations should be lightweight - heavy processing should be queued.
type MessageHandler interface {
	// HandleMessage is called for each message received from the server.
	// messageType is websocket.TextMessage or websocket.BinaryMessage.
	HandleMessage(messageType int, data []byte)
}

// Client manages a WebSocket connection to the RMM server for real-time command push.
// It automatically reconnects with exponential backoff on connection failure.
type Client struct {
	serverURL string            // Base server URL (http/https)
	apiKey    string            // API key for authentication
	conn      *websocket.Conn   // Current WebSocket connection
	logger    *slog.Logger      // Structured logger
	handler   MessageHandler    // Processes incoming messages
	mu        sync.Mutex        // Protects conn
	running   bool              // Indicates if Run loop is active
	stopChan  chan struct{}     // Signal to stop the Run loop
}

// NewClient creates a new WebSocket client.
// serverURL should be the base server URL (e.g., "https://rmm.example.com").
// apiKey is the agent's API key received during registration.
// handler processes incoming WebSocket messages.
func NewClient(serverURL, apiKey string, logger *slog.Logger, handler MessageHandler) *Client {
	return &Client{
		serverURL: serverURL,
		apiKey:    apiKey,
		logger:    logger,
		handler:   handler,
	}
}

// Run starts the WebSocket client loop.
// It maintains a connection to the server, automatically reconnecting with
// exponential backoff on disconnection.
//
// Run blocks until ctx is cancelled or Stop() is called.
// It should be called from a dedicated goroutine.
func (c *Client) Run(ctx context.Context) {
	c.mu.Lock()
	c.running = true
	c.stopChan = make(chan struct{})
	c.mu.Unlock()

	c.logger.Info("websocket client starting")

	backoff := initialBackoff

	for {
		// Check for shutdown before each connection attempt
		select {
		case <-ctx.Done():
			c.logger.Info("websocket client stopping: context cancelled")
			return
		case <-c.stopChan:
			c.logger.Info("websocket client stopping: stop requested")
			return
		default:
		}

		// Attempt to connect
		err := c.connect(ctx)
		if err != nil {
			c.logger.Warn("websocket connection failed",
				slog.String("error", err.Error()),
				slog.Duration("backoff", backoff),
			)

			// Wait with backoff before retry
			select {
			case <-ctx.Done():
				return
			case <-c.stopChan:
				return
			case <-time.After(backoff):
			}

			// Increase backoff with jitter for next attempt
			backoff = c.calculateNextBackoff(backoff)
			continue
		}

		// Connection successful - reset backoff
		backoff = initialBackoff

		// Enter read loop - this blocks until connection drops
		c.readLoop(ctx)

		// Connection dropped - log and will reconnect
		c.logger.Info("websocket connection closed, will reconnect",
			slog.Duration("backoff", backoff),
		)
	}
}

// connect establishes a WebSocket connection to the server.
func (c *Client) connect(ctx context.Context) error {
	// Build WebSocket URL: ws(s)://server/ws/agent?token=apiKey
	wsURL, err := c.buildWebSocketURL()
	if err != nil {
		return err
	}

	c.logger.Debug("connecting to websocket",
		slog.String("url", wsURL),
	)

	// Configure dialer with timeout
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	// Dial with context for cancellation support
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return err
	}

	// Store connection
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	c.logger.Info("websocket connected")
	return nil
}

// buildWebSocketURL constructs the WebSocket URL from the server URL.
// Converts http(s):// to ws(s):// and appends /ws/agent?token=apiKey.
func (c *Client) buildWebSocketURL() (string, error) {
	// Parse the server URL
	u, err := url.Parse(c.serverURL)
	if err != nil {
		return "", err
	}

	// Convert scheme: http -> ws, https -> wss
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "ws", "wss":
		// Already WebSocket scheme, keep as-is
	default:
		// Default to wss for unknown schemes
		u.Scheme = "wss"
	}

	// Set path to WebSocket endpoint
	u.Path = strings.TrimSuffix(u.Path, "/") + "/ws/agent"

	// Add token query parameter
	q := u.Query()
	q.Set("token", c.apiKey)
	u.RawQuery = q.Encode()

	return u.String(), nil
}

// readLoop reads messages from the WebSocket connection until it closes.
func (c *Client) readLoop(ctx context.Context) {
	// Get connection reference
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	if conn == nil {
		return
	}

	// Ensure connection is closed when we exit
	defer func() {
		c.mu.Lock()
		if c.conn == conn {
			c.conn.Close()
			c.conn = nil
		}
		c.mu.Unlock()
	}()

	for {
		// Check for shutdown
		select {
		case <-ctx.Done():
			return
		case <-c.stopChan:
			return
		default:
		}

		// Read message (blocking call)
		messageType, data, err := conn.ReadMessage()
		if err != nil {
			// Connection closed or error - exit loop to trigger reconnect
			c.logger.Debug("websocket read error",
				slog.String("error", err.Error()),
			)
			return
		}

		c.logger.Debug("websocket message received",
			slog.Int("message_type", messageType),
			slog.Int("length", len(data)),
		)

		// Dispatch to handler
		if c.handler != nil {
			c.handler.HandleMessage(messageType, data)
		}
	}
}

// Send sends a message over the WebSocket connection.
// Returns an error if not connected.
// Thread-safe.
func (c *Client) Send(data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return ErrNotConnected
	}

	return c.conn.WriteMessage(websocket.TextMessage, data)
}

// Stop gracefully stops the WebSocket client.
// It closes the connection and signals the Run loop to exit.
func (c *Client) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.running {
		return
	}

	// Signal stop
	close(c.stopChan)
	c.running = false

	// Close connection if exists
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}

	c.logger.Info("websocket client stopped")
}

// Shutdown implements the shutdown.Shutdowner interface for coordinated shutdown.
// It calls Stop() and returns nil (WebSocket shutdown is always graceful).
func (c *Client) Shutdown(ctx context.Context) error {
	c.Stop()
	return nil
}

// calculateNextBackoff computes the next backoff duration with jitter.
// Formula: min(current * factor + jitter, maxBackoff)
// Jitter: +/- 30% random variance
func (c *Client) calculateNextBackoff(current time.Duration) time.Duration {
	// Apply exponential factor
	next := time.Duration(float64(current) * backoffFactor)

	// Add jitter (+/- 30%)
	jitter := float64(next) * jitterFactor * (2*rand.Float64() - 1)
	next = time.Duration(float64(next) + jitter)

	// Cap at maximum
	if next > maxBackoff {
		next = maxBackoff
	}

	return next
}

// ErrNotConnected is returned when Send is called without an active connection.
var ErrNotConnected = &notConnectedError{}

type notConnectedError struct{}

func (e *notConnectedError) Error() string {
	return "websocket not connected"
}
