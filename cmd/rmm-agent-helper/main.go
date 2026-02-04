// main.go is the privileged helper for rmm-agent.
// It runs as root (via setuid) and executes commands that require elevated privileges.
// Communication is via Unix domain socket with the unprivileged main agent.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/doughall/linuxrmm/agent/internal/executor"
	"github.com/doughall/linuxrmm/agent/internal/helper"
)

const socketDir = "/run/rmm-agent"

func main() {
	// Setup structured logging
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// Create socket directory if needed
	if err := os.MkdirAll(socketDir, 0755); err != nil {
		slog.Error("failed to create socket directory", slog.String("error", err.Error()))
		os.Exit(1)
	}

	// Remove old socket if exists
	os.Remove(helper.SocketPath)

	// Create Unix socket listener
	listener, err := net.Listen("unix", helper.SocketPath)
	if err != nil {
		slog.Error("failed to create socket", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer listener.Close()
	defer os.Remove(helper.SocketPath)

	// Set socket permissions - only rmm-agent user can connect
	// The install script will chown this to rmm-agent:rmm-agent
	if err := os.Chmod(helper.SocketPath, 0600); err != nil {
		slog.Error("failed to set socket permissions", slog.String("error", err.Error()))
		os.Exit(1)
	}

	slog.Info("helper started", slog.String("socket", helper.SocketPath))

	// Handle shutdown
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// Accept connections in goroutine
	go func() {
		exec := executor.New()
		for {
			conn, err := listener.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
					slog.Error("accept error", slog.String("error", err.Error()))
					continue
				}
			}
			go handleConnection(conn, exec)
		}
	}()

	<-ctx.Done()
	slog.Info("helper shutting down")
}

func handleConnection(conn net.Conn, exec *executor.Executor) {
	defer conn.Close()

	// Set connection timeout
	conn.SetDeadline(time.Now().Add(time.Hour)) // Max 1 hour for long-running commands

	// Read request
	var req helper.Request
	decoder := json.NewDecoder(conn)
	if err := decoder.Decode(&req); err != nil {
		sendError(conn, fmt.Sprintf("invalid request: %s", err.Error()))
		return
	}

	slog.Info("executing command",
		slog.String("type", string(req.Type)),
		slog.Int64("timeout_ms", req.Timeout),
	)

	// Validate request
	if req.Type != helper.RequestTypeExecute {
		sendError(conn, fmt.Sprintf("unknown request type: %s", req.Type))
		return
	}

	if req.Command == "" {
		sendError(conn, "empty command")
		return
	}

	if req.Timeout <= 0 {
		req.Timeout = 60000 // Default 60 seconds
	}

	// Execute command
	timeout := time.Duration(req.Timeout) * time.Millisecond
	result, err := exec.Execute(context.Background(), req.Command, timeout)

	var resp helper.Response
	if err != nil {
		resp = helper.Response{
			Success: false,
			Error:   err.Error(),
		}
	} else {
		resp = helper.Response{
			Success:  true,
			ExitCode: result.ExitCode,
			Stdout:   result.Stdout,
			Stderr:   result.Stderr,
			Duration: result.Duration.Milliseconds(),
			TimedOut: result.TimedOut,
		}
	}

	encoder := json.NewEncoder(conn)
	encoder.Encode(&resp)

	slog.Info("command completed",
		slog.Int("exit_code", resp.ExitCode),
		slog.Bool("timed_out", resp.TimedOut),
		slog.Int64("duration_ms", resp.Duration),
	)
}

func sendError(conn net.Conn, msg string) {
	slog.Error("request error", slog.String("error", msg))
	resp := helper.Response{
		Success: false,
		Error:   msg,
	}
	encoder := json.NewEncoder(conn)
	encoder.Encode(&resp)
}
