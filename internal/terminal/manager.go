// Package terminal provides PTY (pseudo-terminal) session management for interactive
// terminal access via the web interface. It manages the lifecycle of terminal sessions,
// including spawning shells, handling I/O, and cleanup.
//
// The manager supports multiple concurrent sessions (configurable limit) and handles
// graceful shutdown of all active sessions.
package terminal

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// DefaultMaxSessions is the default maximum concurrent terminal sessions per agent.
const DefaultMaxSessions = 5

// Manager manages terminal PTY sessions.
type Manager struct {
	sessions    map[string]*Session
	mu          sync.RWMutex
	maxSessions int
	logger      *slog.Logger

	// outputCallback is called when PTY output is available
	outputCallback func(sessionID string, data []byte)

	// closeCallback is called when a session closes
	closeCallback func(sessionID string, reason string)
}

// NewManager creates a new terminal manager.
func NewManager(logger *slog.Logger) *Manager {
	return &Manager{
		sessions:    make(map[string]*Session),
		maxSessions: DefaultMaxSessions,
		logger:      logger,
	}
}

// SetMaxSessions sets the maximum number of concurrent sessions.
func (m *Manager) SetMaxSessions(max int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.maxSessions = max
}

// SetOutputCallback sets the callback for PTY output.
// This is called from the PTY reader goroutine.
func (m *Manager) SetOutputCallback(cb func(sessionID string, data []byte)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outputCallback = cb
}

// SetCloseCallback sets the callback for session close events.
func (m *Manager) SetCloseCallback(cb func(sessionID string, reason string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeCallback = cb
}

// StartSession creates a new terminal session with the given parameters.
// Returns an error if the session limit is reached or the shell cannot be started.
func (m *Manager) StartSession(sessionID, shell string, cols, rows uint16) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if session already exists
	if _, exists := m.sessions[sessionID]; exists {
		return fmt.Errorf("session %s already exists", sessionID)
	}

	// Check session limit
	if len(m.sessions) >= m.maxSessions {
		return fmt.Errorf("maximum sessions (%d) reached", m.maxSessions)
	}

	// Default shell
	if shell == "" {
		shell = "/bin/bash"
	}

	// Verify shell exists
	if _, err := exec.LookPath(shell); err != nil {
		// Fallback to /bin/sh
		shell = "/bin/sh"
		if _, err := exec.LookPath(shell); err != nil {
			return fmt.Errorf("no shell available: %w", err)
		}
	}

	// Create the command
	cmd := exec.Command(shell)

	// Set up environment
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
	)

	// Create new process group so we can kill all children on cleanup
	// This helps prevent orphaned processes if the agent crashes
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	// Start with PTY
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Cols: cols,
		Rows: rows,
	})
	if err != nil {
		return fmt.Errorf("failed to start PTY: %w", err)
	}

	session := &Session{
		ID:        sessionID,
		Shell:     shell,
		Cols:      cols,
		Rows:      rows,
		cmd:       cmd,
		ptmx:      ptmx,
		startedAt: time.Now(),
		logger:    m.logger.With(slog.String("session_id", sessionID)),
	}

	m.sessions[sessionID] = session

	// Start PTY output reader
	go m.readPTYOutput(session)

	// Wait for process exit in background
	go m.waitForExit(session)

	m.logger.Info("Terminal session started",
		slog.String("session_id", sessionID),
		slog.String("shell", shell),
		slog.Uint64("cols", uint64(cols)),
		slog.Uint64("rows", uint64(rows)),
	)

	return nil
}

// readPTYOutput reads from PTY and calls the output callback.
func (m *Manager) readPTYOutput(session *Session) {
	buf := make([]byte, 4096)

	for {
		n, err := session.ptmx.Read(buf)
		if err != nil {
			if err != io.EOF {
				session.logger.Debug("PTY read error",
					slog.String("error", err.Error()),
				)
			}
			return
		}

		if n > 0 {
			m.mu.RLock()
			cb := m.outputCallback
			m.mu.RUnlock()

			if cb != nil {
				// Make a copy of the data to avoid race conditions
				data := make([]byte, n)
				copy(data, buf[:n])
				cb(session.ID, data)
			}
		}
	}
}

// waitForExit waits for the shell process to exit and cleans up.
func (m *Manager) waitForExit(session *Session) {
	err := session.cmd.Wait()

	reason := "process_exit"
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			reason = fmt.Sprintf("exit_code_%d", exitErr.ExitCode())
		} else {
			reason = "error"
		}
	}

	// Close PTY
	session.ptmx.Close()

	// Remove from manager
	m.mu.Lock()
	delete(m.sessions, session.ID)
	cb := m.closeCallback
	m.mu.Unlock()

	// Notify callback
	if cb != nil {
		cb(session.ID, reason)
	}

	session.logger.Info("Terminal session ended",
		slog.String("reason", reason),
		slog.Duration("duration", time.Since(session.startedAt)),
	)
}

// WriteInput sends input to a terminal session's PTY.
func (m *Manager) WriteInput(sessionID string, data []byte) error {
	m.mu.RLock()
	session, exists := m.sessions[sessionID]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("session %s not found", sessionID)
	}

	_, err := session.ptmx.Write(data)
	return err
}

// Resize changes the terminal dimensions for a session.
func (m *Manager) Resize(sessionID string, cols, rows uint16) error {
	m.mu.RLock()
	session, exists := m.sessions[sessionID]
	m.mu.RUnlock()

	if !exists {
		return fmt.Errorf("session %s not found", sessionID)
	}

	err := pty.Setsize(session.ptmx, &pty.Winsize{
		Cols: cols,
		Rows: rows,
	})
	if err != nil {
		return fmt.Errorf("failed to resize: %w", err)
	}

	session.Cols = cols
	session.Rows = rows

	session.logger.Debug("Terminal resized",
		slog.Uint64("cols", uint64(cols)),
		slog.Uint64("rows", uint64(rows)),
	)

	return nil
}

// CloseSession terminates a terminal session.
func (m *Manager) CloseSession(sessionID, reason string) error {
	m.mu.Lock()
	session, exists := m.sessions[sessionID]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("session %s not found", sessionID)
	}
	delete(m.sessions, sessionID)
	cb := m.closeCallback
	m.mu.Unlock()

	// Send SIGHUP to the entire process group (terminal hangup)
	// Using negative PID sends signal to the process group
	if session.cmd.Process != nil {
		pgid, err := syscall.Getpgid(session.cmd.Process.Pid)
		if err == nil {
			syscall.Kill(-pgid, syscall.SIGHUP)
		} else {
			// Fallback to direct signal if we can't get pgid
			session.cmd.Process.Signal(syscall.SIGHUP)
		}

		// Give it a moment to exit gracefully
		done := make(chan struct{})
		go func() {
			session.cmd.Wait()
			close(done)
		}()

		select {
		case <-done:
			// Process exited gracefully
		case <-time.After(2 * time.Second):
			// Force kill the process group
			if pgid, err := syscall.Getpgid(session.cmd.Process.Pid); err == nil {
				syscall.Kill(-pgid, syscall.SIGKILL)
			} else {
				session.cmd.Process.Kill()
			}
		}
	}

	// Close PTY
	session.ptmx.Close()

	// Notify callback
	if cb != nil {
		cb(sessionID, reason)
	}

	session.logger.Info("Terminal session closed",
		slog.String("reason", reason),
		slog.Duration("duration", time.Since(session.startedAt)),
	)

	return nil
}

// HasSession checks if a session exists.
func (m *Manager) HasSession(sessionID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, exists := m.sessions[sessionID]
	return exists
}

// SessionCount returns the number of active sessions.
func (m *Manager) SessionCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// GetSessionInfo returns information about a session.
func (m *Manager) GetSessionInfo(sessionID string) (*SessionInfo, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	session, exists := m.sessions[sessionID]
	if !exists {
		return nil, false
	}

	return &SessionInfo{
		ID:        session.ID,
		Shell:     session.Shell,
		Cols:      session.Cols,
		Rows:      session.Rows,
		StartedAt: session.startedAt,
	}, true
}

// CloseAll terminates all active sessions.
// Called during graceful shutdown.
func (m *Manager) CloseAll(reason string) {
	m.mu.Lock()
	sessionIDs := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		sessionIDs = append(sessionIDs, id)
	}
	m.mu.Unlock()

	for _, id := range sessionIDs {
		m.CloseSession(id, reason)
	}

	m.logger.Info("All terminal sessions closed",
		slog.String("reason", reason),
		slog.Int("count", len(sessionIDs)),
	)
}

// Session represents an active terminal session.
type Session struct {
	ID        string
	Shell     string
	Cols      uint16
	Rows      uint16
	cmd       *exec.Cmd
	ptmx      *os.File
	startedAt time.Time
	logger    *slog.Logger
}

// SessionInfo contains public information about a session.
type SessionInfo struct {
	ID        string
	Shell     string
	Cols      uint16
	Rows      uint16
	StartedAt time.Time
}
