// Package nats provides a NATS client for real-time communication with the RMM server.
//
// This client replaces WebSocket-based communication with NATS JetStream,
// providing durable message delivery, automatic reconnection, and efficient
// pub/sub patterns for command distribution.
//
// Features:
//   - NKey authentication (public-key cryptography)
//   - JetStream consumers for durable command delivery
//   - Automatic reconnection with exponential backoff
//   - Message publishing for results, stats, and heartbeats
//
// Usage:
//
//	client := nats.NewClient(cfg, logger)
//	err := client.Connect(ctx)
//	defer client.Close()
//	go client.Run(ctx)
package nats

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"
)

// Config holds NATS connection configuration.
type Config struct {
	Servers  string // Comma-separated list of NATS server URLs
	NKeySeed string // NKey seed for authentication (starts with SU)
	TenantID string // Tenant ID for subject routing
	AgentID  string // Agent ID for subject routing
	Group    string // Agent's group name for group subscriptions
}

// MessageHandler processes incoming messages from the server.
type MessageHandler interface {
	// HandleCommand processes a command message.
	HandleCommand(cmd *CommandMessage) error
	// HandleSchedule processes a schedule assignment message.
	HandleSchedule(schedule *ScheduleMessage) error
	// HandleUninstall processes an uninstall command.
	HandleUninstall(msg *UninstallMessage) error
}

// TerminalMessageHandler processes terminal-related messages.
type TerminalMessageHandler interface {
	HandleTerminalStart(msg *TerminalStartMessage) error
	HandleTerminalInput(msg *TerminalInputMessage) error
	HandleTerminalResize(msg *TerminalResizeMessage) error
	HandleTerminalClose(msg *TerminalCloseMessage) error
	Shutdown()
}

// Client manages the NATS connection for the RMM agent.
type Client struct {
	config          Config
	nc              *nats.Conn
	js              jetstream.JetStream
	consumer        jetstream.Consumer
	logger          *slog.Logger
	handler         MessageHandler
	terminalHandler TerminalMessageHandler
	terminalSubs    []*nats.Subscription
	mu              sync.RWMutex
	running         bool
	stopChan        chan struct{}
	connected       bool
}

// NewClient creates a new NATS client with the given configuration.
func NewClient(cfg Config, logger *slog.Logger) *Client {
	return &Client{
		config: cfg,
		logger: logger,
	}
}

// SetHandler sets the message handler for incoming messages.
func (c *Client) SetHandler(handler MessageHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handler = handler
}

// SetTerminalHandler sets the handler for terminal messages.
func (c *Client) SetTerminalHandler(handler TerminalMessageHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.terminalHandler = handler
}

// Connect establishes a connection to the NATS server.
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Parse NKey seed for authentication
	kp, err := nkeys.FromSeed([]byte(c.config.NKeySeed))
	if err != nil {
		return fmt.Errorf("invalid nkey seed: %w", err)
	}

	pubKey, err := kp.PublicKey()
	if err != nil {
		return fmt.Errorf("failed to get public key: %w", err)
	}

	// Configure connection options
	opts := []nats.Option{
		nats.Name(fmt.Sprintf("rmm-agent-%s", c.config.AgentID)),
		nats.Nkey(pubKey, func(nonce []byte) ([]byte, error) {
			return kp.Sign(nonce)
		}),
		nats.ReconnectWait(time.Second),
		nats.MaxReconnects(-1), // Unlimited reconnects
		nats.ReconnectBufSize(5 * 1024 * 1024), // 5MB buffer for reconnect
		nats.PingInterval(30 * time.Second),
		nats.MaxPingsOutstanding(3),
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			c.mu.Lock()
			c.connected = false
			c.mu.Unlock()
			if err != nil {
				c.logger.Warn("NATS disconnected", slog.String("error", err.Error()))
			} else {
				c.logger.Info("NATS disconnected")
			}
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			c.mu.Lock()
			c.connected = true
			c.mu.Unlock()
			c.logger.Info("NATS reconnected", slog.String("server", nc.ConnectedUrl()))
		}),
		nats.ErrorHandler(func(nc *nats.Conn, sub *nats.Subscription, err error) {
			// sub can be nil for connection-level errors
			if sub != nil {
				c.logger.Error("NATS error",
					slog.String("error", err.Error()),
					slog.String("subject", sub.Subject),
				)
			} else {
				c.logger.Error("NATS error",
					slog.String("error", err.Error()),
				)
			}
		}),
	}

	// Connect to NATS
	nc, err := nats.Connect(c.config.Servers, opts...)
	if err != nil {
		return fmt.Errorf("nats connect: %w", err)
	}

	c.nc = nc
	c.connected = true

	// Initialize JetStream
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return fmt.Errorf("jetstream init: %w", err)
	}
	c.js = js

	c.logger.Info("NATS connected",
		slog.String("server", nc.ConnectedUrl()),
		slog.String("agent_id", c.config.AgentID),
	)

	return nil
}

// Run starts the message processing loop.
// It creates a consumer for commands and processes messages until stopped.
func (c *Client) Run(ctx context.Context) {
	c.mu.Lock()
	c.running = true
	c.stopChan = make(chan struct{})
	c.mu.Unlock()

	c.logger.Info("NATS client starting")

	// Create or get the consumer for this agent
	if err := c.setupConsumer(ctx); err != nil {
		c.logger.Error("Failed to setup consumer", slog.String("error", err.Error()))
		return
	}

	// Set up terminal subscriptions (core NATS for low-latency)
	if err := c.setupTerminalSubscriptions(); err != nil {
		c.logger.Error("Failed to setup terminal subscriptions", slog.String("error", err.Error()))
		// Continue without terminal - it's not critical
	}

	// Start consuming messages
	c.consumeMessages(ctx)
}

// setupConsumer creates or retrieves the durable consumer for this agent.
func (c *Client) setupConsumer(ctx context.Context) error {
	// Consumer name is unique per agent
	consumerName := fmt.Sprintf("agent-%s", c.config.AgentID)

	// Define subjects this agent subscribes to
	filterSubjects := []string{
		fmt.Sprintf("rmm.%s.commands.agent.%s", c.config.TenantID, c.config.AgentID),
		fmt.Sprintf("rmm.%s.commands.group.%s", c.config.TenantID, c.config.Group),
		fmt.Sprintf("rmm.%s.commands.fleet", c.config.TenantID),
	}

	// Try to get existing consumer or create new one
	stream, err := c.js.Stream(ctx, "COMMANDS")
	if err != nil {
		return fmt.Errorf("get stream: %w", err)
	}

	// Create consumer configuration
	consumerCfg := jetstream.ConsumerConfig{
		Durable:         consumerName,
		FilterSubjects:  filterSubjects,
		AckPolicy:       jetstream.AckExplicitPolicy,
		AckWait:         60 * time.Second,
		MaxDeliver:      3,
		MaxAckPending:   10,
		DeliverPolicy:   jetstream.DeliverAllPolicy,
		ReplayPolicy:    jetstream.ReplayInstantPolicy,
	}

	consumer, err := stream.CreateOrUpdateConsumer(ctx, consumerCfg)
	if err != nil {
		return fmt.Errorf("create consumer: %w", err)
	}

	c.consumer = consumer

	c.logger.Info("NATS consumer ready",
		slog.String("consumer", consumerName),
		slog.Any("subjects", filterSubjects),
	)

	return nil
}

// setupTerminalSubscriptions creates subscriptions for terminal messages.
// Uses core NATS (not JetStream) for low-latency interactive I/O.
func (c *Client) setupTerminalSubscriptions() error {
	c.mu.RLock()
	handler := c.terminalHandler
	c.mu.RUnlock()

	if handler == nil {
		c.logger.Debug("No terminal handler set, skipping terminal subscriptions")
		return nil
	}

	// Subscribe to terminal subjects for this agent
	subjects := []string{
		fmt.Sprintf("rmm.%s.terminal.start.%s", c.config.TenantID, c.config.AgentID),
		fmt.Sprintf("rmm.%s.terminal.input.%s", c.config.TenantID, c.config.AgentID),
		fmt.Sprintf("rmm.%s.terminal.resize.%s", c.config.TenantID, c.config.AgentID),
		fmt.Sprintf("rmm.%s.terminal.close.%s", c.config.TenantID, c.config.AgentID),
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, subject := range subjects {
		sub, err := c.nc.Subscribe(subject, func(msg *nats.Msg) {
			c.handleTerminalMessage(msg)
		})
		if err != nil {
			return fmt.Errorf("subscribe %s: %w", subject, err)
		}
		c.terminalSubs = append(c.terminalSubs, sub)
	}

	c.logger.Info("Terminal subscriptions ready",
		slog.Any("subjects", subjects),
	)

	return nil
}

// handleTerminalMessage processes a terminal message from core NATS.
func (c *Client) handleTerminalMessage(msg *nats.Msg) {
	c.mu.RLock()
	handler := c.terminalHandler
	c.mu.RUnlock()

	if handler == nil {
		return
	}

	// Parse message envelope
	var envelope MessageEnvelope
	if err := json.Unmarshal(msg.Data, &envelope); err != nil {
		c.logger.Warn("Invalid terminal message",
			slog.String("subject", msg.Subject),
			slog.String("error", err.Error()),
		)
		return
	}

	c.logger.Debug("Processing terminal message",
		slog.String("type", envelope.Type),
		slog.String("subject", msg.Subject),
	)

	switch envelope.Type {
	case "terminal_start":
		var startMsg TerminalStartMessage
		if err := json.Unmarshal(envelope.Payload, &startMsg); err != nil {
			c.logger.Warn("Invalid terminal_start payload", slog.String("error", err.Error()))
			return
		}
		handler.HandleTerminalStart(&startMsg)

	case "terminal_input":
		var inputMsg TerminalInputMessage
		if err := json.Unmarshal(envelope.Payload, &inputMsg); err != nil {
			c.logger.Warn("Invalid terminal_input payload", slog.String("error", err.Error()))
			return
		}
		handler.HandleTerminalInput(&inputMsg)

	case "terminal_resize":
		var resizeMsg TerminalResizeMessage
		if err := json.Unmarshal(envelope.Payload, &resizeMsg); err != nil {
			c.logger.Warn("Invalid terminal_resize payload", slog.String("error", err.Error()))
			return
		}
		handler.HandleTerminalResize(&resizeMsg)

	case "terminal_close":
		var closeMsg TerminalCloseMessage
		if err := json.Unmarshal(envelope.Payload, &closeMsg); err != nil {
			c.logger.Warn("Invalid terminal_close payload", slog.String("error", err.Error()))
			return
		}
		handler.HandleTerminalClose(&closeMsg)

	default:
		c.logger.Warn("Unknown terminal message type", slog.String("type", envelope.Type))
	}
}

// consumeMessages processes messages from the consumer.
func (c *Client) consumeMessages(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			c.logger.Info("NATS consumer stopping: context cancelled")
			return
		case <-c.stopChan:
			c.logger.Info("NATS consumer stopping: stop requested")
			return
		default:
		}

		// Fetch messages with timeout
		msgs, err := c.consumer.Fetch(10, jetstream.FetchMaxWait(5*time.Second))
		if err != nil {
			if err == context.DeadlineExceeded || err == nats.ErrTimeout {
				// Normal timeout, continue polling
				continue
			}
			c.logger.Warn("Fetch error", slog.String("error", err.Error()))
			time.Sleep(time.Second) // Brief pause before retry
			continue
		}

		// Process fetched messages
		for msg := range msgs.Messages() {
			if err := c.processMessage(msg); err != nil {
				c.logger.Error("Message processing failed",
					slog.String("subject", msg.Subject()),
					slog.String("error", err.Error()),
				)
				// NAK to retry later
				msg.Nak()
			} else {
				// Acknowledge successful processing
				msg.Ack()
			}
		}

		// Check for fetch errors
		if msgs.Error() != nil && msgs.Error() != nats.ErrTimeout {
			c.logger.Warn("Fetch completed with error",
				slog.String("error", msgs.Error().Error()),
			)
		}
	}
}

// processMessage handles a single incoming message.
func (c *Client) processMessage(msg jetstream.Msg) error {
	c.mu.RLock()
	handler := c.handler
	c.mu.RUnlock()

	if handler == nil {
		return fmt.Errorf("no message handler set")
	}

	// Parse message envelope
	var envelope MessageEnvelope
	if err := json.Unmarshal(msg.Data(), &envelope); err != nil {
		return fmt.Errorf("unmarshal envelope: %w", err)
	}

	c.logger.Debug("Processing message",
		slog.String("type", envelope.Type),
		slog.String("subject", msg.Subject()),
	)

	switch envelope.Type {
	case "command":
		var cmd CommandMessage
		if err := json.Unmarshal(envelope.Payload, &cmd); err != nil {
			return fmt.Errorf("unmarshal command: %w", err)
		}
		return handler.HandleCommand(&cmd)

	case "schedule_assignment":
		var schedule ScheduleMessage
		if err := json.Unmarshal(envelope.Payload, &schedule); err != nil {
			return fmt.Errorf("unmarshal schedule: %w", err)
		}
		return handler.HandleSchedule(&schedule)

	case "uninstall":
		var uninstall UninstallMessage
		if err := json.Unmarshal(envelope.Payload, &uninstall); err != nil {
			return fmt.Errorf("unmarshal uninstall: %w", err)
		}
		return handler.HandleUninstall(&uninstall)

	default:
		c.logger.Warn("Unknown message type", slog.String("type", envelope.Type))
		return nil // Don't error on unknown types, just ignore
	}
}

// IsConnected returns whether the client is currently connected.
func (c *Client) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected && c.nc != nil && c.nc.IsConnected()
}

// Stop gracefully stops the NATS client.
func (c *Client) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.running {
		return
	}

	close(c.stopChan)
	c.running = false

	c.logger.Info("NATS client stopped")
}

// Close closes the NATS connection.
func (c *Client) Close() error {
	c.Stop()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Shutdown terminal handler
	if c.terminalHandler != nil {
		c.terminalHandler.Shutdown()
	}

	// Unsubscribe from terminal subjects
	for _, sub := range c.terminalSubs {
		sub.Unsubscribe()
	}
	c.terminalSubs = nil

	if c.nc != nil {
		c.nc.Drain()
		c.nc = nil
	}

	return nil
}

// Shutdown implements the shutdown.Shutdowner interface.
func (c *Client) Shutdown(ctx context.Context) error {
	return c.Close()
}

// Connection returns the underlying NATS connection for publishing.
func (c *Client) Connection() *nats.Conn {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.nc
}

// JetStream returns the JetStream context for publishing.
func (c *Client) JetStream() jetstream.JetStream {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.js
}

// TenantID returns the configured tenant ID.
func (c *Client) TenantID() string {
	return c.config.TenantID
}

// AgentID returns the configured agent ID.
func (c *Client) AgentID() string {
	return c.config.AgentID
}
