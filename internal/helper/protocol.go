// protocol.go defines the IPC protocol between the main agent and the privileged helper.
// Communication uses JSON-encoded messages over a Unix domain socket.
package helper

// SocketPath is the Unix domain socket path for helper communication.
const SocketPath = "/run/rmm-agent/helper.sock"

// RequestType identifies the type of privileged operation requested.
type RequestType string

const (
	// RequestTypeExecute requests command execution with root privileges.
	RequestTypeExecute RequestType = "execute"
)

// Request is sent from the main agent to the helper.
type Request struct {
	Type    RequestType `json:"type"`
	Command string      `json:"command"`
	Timeout int64       `json:"timeout_ms"` // milliseconds
}

// Response is sent from the helper back to the main agent.
type Response struct {
	Success  bool   `json:"success"`
	Error    string `json:"error,omitempty"`
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Duration int64  `json:"duration_ms"`
	TimedOut bool   `json:"timed_out"`
}
