// Package nats terminal_handler processes incoming terminal messages via NATS.
//
// Handles terminal session lifecycle: start, input, resize, close.
// Terminal output is published back to the server via NATS.
package nats

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/doughall/linuxrmm/agent/internal/terminal"
)

// TerminalHandler processes terminal-related NATS messages.
type TerminalHandler struct {
	manager   *terminal.Manager
	publisher *Publisher
	logger    *slog.Logger
}

// NewTerminalHandler creates a new terminal message handler.
func NewTerminalHandler(publisher *Publisher, logger *slog.Logger) *TerminalHandler {
	h := &TerminalHandler{
		manager:   terminal.NewManager(logger),
		publisher: publisher,
		logger:    logger,
	}

	// Set up callbacks for PTY output and session close
	h.manager.SetOutputCallback(h.onOutput)
	h.manager.SetCloseCallback(h.onClose)

	return h
}

// HandleTerminalStart processes a terminal start request.
func (h *TerminalHandler) HandleTerminalStart(msg *TerminalStartMessage) error {
	sessionLogger := h.logger.With(slog.String("session_id", msg.SessionID))
	sessionLogger.Info("Terminal start request received",
		slog.Uint64("cols", uint64(msg.Cols)),
		slog.Uint64("rows", uint64(msg.Rows)),
		slog.String("shell", msg.Shell),
	)

	// Default dimensions
	cols := msg.Cols
	rows := msg.Rows
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}

	// Start the session
	err := h.manager.StartSession(msg.SessionID, msg.Shell, cols, rows)
	if err != nil {
		sessionLogger.Error("Failed to start terminal session",
			slog.String("error", err.Error()),
		)
		// Send error response
		h.publishTerminalError(msg.SessionID, err.Error())
		return err
	}

	// Send confirmation
	h.publishTerminalStarted(msg.SessionID, msg.Shell)

	return nil
}

// HandleTerminalInput processes terminal input.
func (h *TerminalHandler) HandleTerminalInput(msg *TerminalInputMessage) error {
	// Decode base64 data
	data, err := base64.StdEncoding.DecodeString(msg.Data)
	if err != nil {
		h.logger.Warn("Invalid base64 in terminal input",
			slog.String("session_id", msg.SessionID),
			slog.String("error", err.Error()),
		)
		return err
	}

	// Write to PTY
	err = h.manager.WriteInput(msg.SessionID, data)
	if err != nil {
		h.logger.Debug("Failed to write terminal input",
			slog.String("session_id", msg.SessionID),
			slog.String("error", err.Error()),
		)
		return err
	}

	return nil
}

// HandleTerminalResize processes terminal resize requests.
func (h *TerminalHandler) HandleTerminalResize(msg *TerminalResizeMessage) error {
	err := h.manager.Resize(msg.SessionID, msg.Cols, msg.Rows)
	if err != nil {
		h.logger.Debug("Failed to resize terminal",
			slog.String("session_id", msg.SessionID),
			slog.String("error", err.Error()),
		)
		return err
	}

	return nil
}

// HandleTerminalClose processes terminal close requests.
func (h *TerminalHandler) HandleTerminalClose(msg *TerminalCloseMessage) error {
	reason := msg.Reason
	if reason == "" {
		reason = "user"
	}

	err := h.manager.CloseSession(msg.SessionID, reason)
	if err != nil {
		h.logger.Debug("Failed to close terminal",
			slog.String("session_id", msg.SessionID),
			slog.String("error", err.Error()),
		)
		return err
	}

	return nil
}

// onOutput is called when PTY output is available.
func (h *TerminalHandler) onOutput(sessionID string, data []byte) {
	// Encode data as base64 for binary safety
	encoded := base64.StdEncoding.EncodeToString(data)

	h.publishTerminalOutput(sessionID, encoded)
}

// onClose is called when a session closes.
func (h *TerminalHandler) onClose(sessionID, reason string) {
	h.publishTerminalClosed(sessionID, reason)
}

// publishTerminalOutput sends PTY output to the server.
func (h *TerminalHandler) publishTerminalOutput(sessionID, data string) {
	subject := fmt.Sprintf("rmm.%s.terminal.output.%s",
		h.publisher.client.TenantID(),
		h.publisher.client.AgentID(),
	)

	msg := MessageEnvelope{
		Type:      "terminal_output",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	payload := TerminalOutputMessage{
		SessionID: sessionID,
		Data:      data,
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		h.logger.Error("Failed to marshal terminal output",
			slog.String("error", err.Error()),
		)
		return
	}
	msg.Payload = payloadBytes

	if err := h.publisher.publish(subject, msg); err != nil {
		h.logger.Debug("Failed to publish terminal output",
			slog.String("session_id", sessionID),
			slog.String("error", err.Error()),
		)
	}
}

// publishTerminalStarted confirms session started.
func (h *TerminalHandler) publishTerminalStarted(sessionID, shell string) {
	subject := fmt.Sprintf("rmm.%s.terminal.output.%s",
		h.publisher.client.TenantID(),
		h.publisher.client.AgentID(),
	)

	msg := MessageEnvelope{
		Type:      "terminal_started",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	payload := TerminalStartedMessage{
		SessionID: sessionID,
		Shell:     shell,
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return
	}
	msg.Payload = payloadBytes

	h.publisher.publish(subject, msg)
}

// publishTerminalClosed indicates session ended.
func (h *TerminalHandler) publishTerminalClosed(sessionID, reason string) {
	subject := fmt.Sprintf("rmm.%s.terminal.output.%s",
		h.publisher.client.TenantID(),
		h.publisher.client.AgentID(),
	)

	msg := MessageEnvelope{
		Type:      "terminal_closed",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	payload := TerminalClosedMessage{
		SessionID: sessionID,
		Reason:    reason,
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return
	}
	msg.Payload = payloadBytes

	h.publisher.publish(subject, msg)
}

// publishTerminalError sends an error response.
func (h *TerminalHandler) publishTerminalError(sessionID, errMsg string) {
	subject := fmt.Sprintf("rmm.%s.terminal.output.%s",
		h.publisher.client.TenantID(),
		h.publisher.client.AgentID(),
	)

	msg := MessageEnvelope{
		Type:      "terminal_error",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	payload := TerminalErrorMessage{
		SessionID: sessionID,
		Error:     errMsg,
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return
	}
	msg.Payload = payloadBytes

	h.publisher.publish(subject, msg)
}

// Shutdown gracefully closes all terminal sessions.
func (h *TerminalHandler) Shutdown() {
	h.manager.CloseAll("agent_shutdown")
}

// SessionCount returns the number of active terminal sessions.
func (h *TerminalHandler) SessionCount() int {
	return h.manager.SessionCount()
}
