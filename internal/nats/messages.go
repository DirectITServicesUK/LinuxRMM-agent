// Package nats message types for NATS communication.
//
// Defines the message envelope and payload structures used for
// communication between the RMM server and agents via NATS.
package nats

import "encoding/json"

// MessageEnvelope wraps all NATS messages with type information.
type MessageEnvelope struct {
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	Timestamp string          `json:"timestamp"`
}

// CommandMessage represents a command to execute on the agent.
type CommandMessage struct {
	ID         string `json:"id"`
	Command    string `json:"command"`
	Timeout    int    `json:"timeout"` // Milliseconds
	RunAsRoot  bool   `json:"runAsRoot"`
	ScheduleID string `json:"scheduleId,omitempty"`
}

// ScheduleMessage represents a schedule assignment for offline execution.
type ScheduleMessage struct {
	ID       string          `json:"id"`
	Action   string          `json:"action"` // "update" or "delete"
	Schedule json.RawMessage `json:"schedule,omitempty"`
	Script   string          `json:"script,omitempty"`
}

// CommandStartMessage is published when command execution begins.
type CommandStartMessage struct {
	CommandID string `json:"commandId"`
}

// CommandResultMessage is published when command execution completes.
type CommandResultMessage struct {
	CommandID         string `json:"commandId"`
	Stdout            string `json:"stdout"`
	Stderr            string `json:"stderr"`
	ExitCode          int    `json:"exitCode"`
	ExecutionDuration int64  `json:"executionDuration"` // Milliseconds
	TimedOut          bool   `json:"timedOut"`
	Error             string `json:"error,omitempty"`
}

// ScheduleResultMessage is published for scheduled command results.
type ScheduleResultMessage struct {
	ScheduleID        string `json:"scheduleId"`
	CommandID         string `json:"commandId,omitempty"`
	Stdout            string `json:"stdout"`
	Stderr            string `json:"stderr"`
	ExitCode          int    `json:"exitCode"`
	ExecutionDuration int64  `json:"executionDuration"`
	TimedOut          bool   `json:"timedOut"`
	ExecutedAt        string `json:"executedAt"`
	Error             string `json:"error,omitempty"`
}

// StatsMessage contains system statistics from the agent.
type StatsMessage struct {
	Timestamp    string  `json:"timestamp"`
	CPU          float64 `json:"cpu"`
	MemoryUsed   uint64  `json:"memoryUsed"`
	MemoryTotal  uint64  `json:"memoryTotal"`
	MemoryPct    float64 `json:"memoryPct"`
	DiskUsed     uint64  `json:"diskUsed"`
	DiskTotal    uint64  `json:"diskTotal"`
	DiskPct      float64 `json:"diskPct"`
	NetBytesSent uint64  `json:"netBytesSent"`
	NetBytesRecv uint64  `json:"netBytesRecv"`
	Load1        float64 `json:"load1"`
	Load5        float64 `json:"load5"`
	Load15       float64 `json:"load15"`
	Uptime       uint64  `json:"uptime"`
}

// HeartbeatMessage is published for presence detection.
type HeartbeatMessage struct {
	Online    bool   `json:"online"`
	Version   string `json:"version,omitempty"`
	Platform  string `json:"platform,omitempty"`
	Timestamp string `json:"timestamp"`
}

// Terminal message types for interactive PTY sessions

// TerminalStartMessage requests a new terminal session.
type TerminalStartMessage struct {
	SessionID string `json:"sessionId"`
	Cols      uint16 `json:"cols"`
	Rows      uint16 `json:"rows"`
	Shell     string `json:"shell,omitempty"`
}

// TerminalInputMessage contains input data for a terminal session.
type TerminalInputMessage struct {
	SessionID string `json:"sessionId"`
	Data      string `json:"data"` // Base64 encoded for binary safety
}

// TerminalResizeMessage requests a terminal resize.
type TerminalResizeMessage struct {
	SessionID string `json:"sessionId"`
	Cols      uint16 `json:"cols"`
	Rows      uint16 `json:"rows"`
}

// TerminalCloseMessage requests closing a terminal session.
type TerminalCloseMessage struct {
	SessionID string `json:"sessionId"`
	Reason    string `json:"reason,omitempty"`
}

// TerminalOutputMessage is sent when PTY output is available.
type TerminalOutputMessage struct {
	SessionID string `json:"sessionId"`
	Data      string `json:"data"` // Base64 encoded
}

// TerminalStartedMessage confirms terminal session started.
type TerminalStartedMessage struct {
	SessionID string `json:"sessionId"`
	Shell     string `json:"shell"`
}

// TerminalClosedMessage indicates terminal session ended.
type TerminalClosedMessage struct {
	SessionID string `json:"sessionId"`
	Reason    string `json:"reason"`
}

// TerminalErrorMessage indicates a terminal error.
type TerminalErrorMessage struct {
	SessionID string `json:"sessionId"`
	Error     string `json:"error"`
}

// UninstallMessage is sent by the server to trigger agent uninstallation.
type UninstallMessage struct {
	Reason string `json:"reason,omitempty"`
}

// UninstallConfirmMessage is sent back to confirm uninstall command received.
type UninstallConfirmMessage struct {
	Confirmed bool   `json:"confirmed"`
	Timestamp string `json:"timestamp"`
}

// Process management message types

// ProcessListRequestMessage requests process list from agent.
type ProcessListRequestMessage struct {
	RequestID string `json:"requestId"`
}

// ProcessInfo contains information about a single process.
type ProcessInfo struct {
	PID        int32   `json:"pid"`
	Name       string  `json:"name"`
	Username   string  `json:"username"`
	CPUPercent float64 `json:"cpuPercent"`
	MemPercent float32 `json:"memPercent"`
	Cmdline    string  `json:"cmdline"`
	Status     string  `json:"status"`
}

// ProcessListMessage contains the list of running processes.
type ProcessListMessage struct {
	RequestID string        `json:"requestId"`
	Processes []ProcessInfo `json:"processes"`
	AgentPID  int32         `json:"agentPid"`
	Timestamp string        `json:"timestamp"`
}

// Note: SystemInfoMessage is defined in internal/sysinfo/reporter.go
// The NATS publisher uses sysinfo.SystemInfoMessage directly to avoid duplication.
