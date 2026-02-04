// Package nats publisher handles outgoing messages from the agent to the server.
//
// The publisher sends command results, statistics, and heartbeat messages
// via NATS subjects. Results use JetStream for durability, while stats
// and heartbeats use core NATS for efficiency.
package nats

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/doughall/linuxrmm/agent/internal/stats"
	"github.com/nats-io/nats.go"
)

// Publisher handles publishing messages to NATS.
type Publisher struct {
	client *Client
	logger *slog.Logger
}

// NewPublisher creates a new NATS publisher.
func NewPublisher(client *Client, logger *slog.Logger) *Publisher {
	return &Publisher{
		client: client,
		logger: logger,
	}
}

// PublishCommandStart publishes a notification that command execution has started.
func (p *Publisher) PublishCommandStart(commandID string) error {
	subject := fmt.Sprintf("rmm.%s.results.%s", p.client.TenantID(), p.client.AgentID())

	msg := MessageEnvelope{
		Type: "command_start",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	payload := CommandStartMessage{
		CommandID: commandID,
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	msg.Payload = payloadBytes

	return p.publishJetStream(subject, msg)
}

// PublishCommandResult publishes command execution results.
func (p *Publisher) PublishCommandResult(result *CommandResultMessage) error {
	subject := fmt.Sprintf("rmm.%s.results.%s", p.client.TenantID(), p.client.AgentID())

	msg := MessageEnvelope{
		Type: "command_result",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	payloadBytes, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	msg.Payload = payloadBytes

	return p.publishJetStream(subject, msg)
}

// PublishScheduleResult publishes scheduled command execution results.
func (p *Publisher) PublishScheduleResult(result *ScheduleResultMessage) error {
	subject := fmt.Sprintf("rmm.%s.results.%s", p.client.TenantID(), p.client.AgentID())

	msg := MessageEnvelope{
		Type: "schedule_result",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	payloadBytes, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	msg.Payload = payloadBytes

	return p.publishJetStream(subject, msg)
}

// PublishStats publishes system statistics.
// Uses core NATS (not JetStream) for high-frequency metrics.
func (p *Publisher) PublishStats(stats *StatsMessage) error {
	subject := fmt.Sprintf("rmm.%s.stats.%s", p.client.TenantID(), p.client.AgentID())

	msg := MessageEnvelope{
		Type: "stats",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	payloadBytes, err := json.Marshal(stats)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	msg.Payload = payloadBytes

	return p.publish(subject, msg)
}

// PublishHeartbeat publishes a heartbeat for presence detection.
// Uses core NATS (not JetStream) for ephemeral presence.
func (p *Publisher) PublishHeartbeat(version, platform string) error {
	subject := fmt.Sprintf("rmm.%s.status.%s", p.client.TenantID(), p.client.AgentID())

	msg := MessageEnvelope{
		Type: "heartbeat",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	payload := HeartbeatMessage{
		Online:    true,
		Version:   version,
		Platform:  platform,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	msg.Payload = payloadBytes

	return p.publish(subject, msg)
}

// publish sends a message via core NATS (fire-and-forget).
func (p *Publisher) publish(subject string, msg MessageEnvelope) error {
	nc := p.client.Connection()
	if nc == nil {
		return fmt.Errorf("not connected")
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}

	if err := nc.Publish(subject, data); err != nil {
		return fmt.Errorf("publish: %w", err)
	}

	p.logger.Debug("Published message",
		slog.String("subject", subject),
		slog.String("type", msg.Type),
	)

	return nil
}

// publishJetStream sends a message via JetStream for durability.
func (p *Publisher) publishJetStream(subject string, msg MessageEnvelope) error {
	js := p.client.JetStream()
	if js == nil {
		return fmt.Errorf("jetstream not available")
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}

	ctx := nats.Context(nil) // Use default context
	ack, err := js.Publish(ctx, subject, data)
	if err != nil {
		return fmt.Errorf("publish: %w", err)
	}

	p.logger.Debug("Published message to JetStream",
		slog.String("subject", subject),
		slog.String("type", msg.Type),
		slog.String("stream", ack.Stream),
		slog.Uint64("seq", ack.Sequence),
	)

	return nil
}

// PublishUninstallConfirm publishes confirmation that uninstall command was received.
// Uses JetStream for reliable delivery before agent exits.
func (p *Publisher) PublishUninstallConfirm() error {
	subject := fmt.Sprintf("rmm.%s.results.%s", p.client.TenantID(), p.client.AgentID())

	msg := MessageEnvelope{
		Type:      "uninstall_confirm",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	payload := UninstallConfirmMessage{
		Confirmed: true,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	msg.Payload = payloadBytes

	return p.publishJetStream(subject, msg)
}

// Flush flushes the NATS connection to ensure all pending messages are sent.
// This is useful before exiting to ensure critical messages like uninstall
// confirmations are delivered.
func (p *Publisher) Flush() error {
	nc := p.client.Connection()
	if nc == nil {
		return fmt.Errorf("not connected")
	}
	return nc.Flush()
}

// IsConnected returns whether the publisher can send messages.
func (p *Publisher) IsConnected() bool {
	return p.client.IsConnected()
}

// PublishStatsData publishes system statistics using the stats.StatsData type.
// This method satisfies the stats.StatsPublisher interface.
func (p *Publisher) PublishStatsData(data stats.StatsData) error {
	msg := &StatsMessage{
		Timestamp:    data.Timestamp,
		CPU:          data.CPU,
		MemoryUsed:   data.MemoryUsed,
		MemoryTotal:  data.MemoryTotal,
		MemoryPct:    data.MemoryPct,
		DiskUsed:     data.DiskUsed,
		DiskTotal:    data.DiskTotal,
		DiskPct:      data.DiskPct,
		NetBytesSent: data.NetBytesSent,
		NetBytesRecv: data.NetBytesRecv,
		Load1:        data.Load1,
		Load5:        data.Load5,
		Load15:       data.Load15,
		Uptime:       data.Uptime,
	}
	return p.PublishStats(msg)
}
