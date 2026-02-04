#!/bin/bash
# RMM Agent Installation Script
#
# This script installs the RMM agent on a Linux system, setting up:
# - System user (rmm-agent)
# - Binary (/usr/local/bin/rmm-agent)
# - Configuration directory (/etc/rmm-agent/)
# - systemd service unit
#
# Usage:
#   sudo ./install.sh --group=<GROUP> --server-url=<URL>
#
# Example:
#   sudo ./install.sh --group=production --server-url=https://rmm.example.com
#
# After installation:
# 1. Add your device token to /etc/rmm-agent/config.yaml
# 2. Start the service: systemctl start rmm-agent
# 3. Check status: systemctl status rmm-agent
#
# The script is idempotent - safe to run multiple times.

set -e

# Configuration
INSTALL_DIR="/usr/local/bin"
CONFIG_DIR="/etc/rmm-agent"
SERVICE_FILE="/etc/systemd/system/rmm-agent.service"
STATE_DIR="/var/lib/rmm-agent"
SERVICE_USER="rmm-agent"

# Script directory (where the script and related files are located)
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Logging functions
log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Print usage
usage() {
    cat <<EOF
Usage: $0 --group=<GROUP> --server-url=<URL>

Required arguments:
  --group=<GROUP>        Agent group (e.g., production, staging)
  --server-url=<URL>     RMM server URL (e.g., https://rmm.example.com)

Optional arguments:
  --binary=<PATH>        Path to rmm-agent binary (default: ./rmm-agent)
  --help                 Show this help message

Example:
  $0 --group=production --server-url=https://rmm.example.com
EOF
    exit 1
}

# Check if running as root
check_root() {
    if [ "$EUID" -ne 0 ]; then
        log_error "This script must be run as root (use sudo)"
        exit 1
    fi
}

# Parse command-line arguments
parse_args() {
    GROUP=""
    SERVER_URL=""
    BINARY_PATH="$SCRIPT_DIR/../rmm-agent"

    for arg in "$@"; do
        case $arg in
            --group=*)
                GROUP="${arg#*=}"
                ;;
            --server-url=*)
                SERVER_URL="${arg#*=}"
                ;;
            --binary=*)
                BINARY_PATH="${arg#*=}"
                ;;
            --help)
                usage
                ;;
            *)
                log_error "Unknown argument: $arg"
                usage
                ;;
        esac
    done

    # Validate required arguments
    if [ -z "$GROUP" ]; then
        log_error "Missing required argument: --group"
        usage
    fi

    if [ -z "$SERVER_URL" ]; then
        log_error "Missing required argument: --server-url"
        usage
    fi
}

# Create service user if it doesn't exist
create_user() {
    if id -u "$SERVICE_USER" >/dev/null 2>&1; then
        log_info "User '$SERVICE_USER' already exists"
    else
        log_info "Creating system user '$SERVICE_USER'"
        useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
    fi
}

# Install the binary
install_binary() {
    # Check if binary exists
    if [ ! -f "$BINARY_PATH" ]; then
        log_error "Binary not found at: $BINARY_PATH"
        log_error "Build the agent first: go build -o rmm-agent ./cmd/rmm-agent"
        exit 1
    fi

    log_info "Installing binary to $INSTALL_DIR/rmm-agent"
    install -m 0755 "$BINARY_PATH" "$INSTALL_DIR/rmm-agent"
}

# Install the helper binary
install_helper() {
    # Detect architecture
    ARCH=$(uname -m)
    case $ARCH in
        x86_64)
            ARCH="amd64"
            ;;
        aarch64|arm64)
            ARCH="arm64"
            ;;
        *)
            log_warn "Unknown architecture: $ARCH, skipping helper installation"
            return
            ;;
    esac

    HELPER_BIN="rmm-agent-helper-linux-${ARCH}"
    HELPER_PATH="$SCRIPT_DIR/../build/${HELPER_BIN}"

    # Check if helper binary exists
    if [ ! -f "$HELPER_PATH" ]; then
        log_warn "Helper binary not found at: $HELPER_PATH"
        log_warn "Helper will not be installed (privileged operations unavailable)"
        return
    fi

    log_info "Installing helper binary to $INSTALL_DIR/rmm-agent-helper"
    install -m 4755 -o root -g root "$HELPER_PATH" "$INSTALL_DIR/rmm-agent-helper"
    log_info "Helper installed with setuid root (4755)"

    # Create runtime directory for socket
    log_info "Creating runtime directory for helper socket"
    install -d -m 0750 -o "$SERVICE_USER" -g "$SERVICE_USER" /run/rmm-agent
}

# Create configuration directory and file
setup_config() {
    # Create config directory
    log_info "Creating configuration directory: $CONFIG_DIR"
    install -d -m 0755 -o "$SERVICE_USER" -g "$SERVICE_USER" "$CONFIG_DIR"

    # Create state directory
    log_info "Creating state directory: $STATE_DIR"
    install -d -m 0755 -o "$SERVICE_USER" -g "$SERVICE_USER" "$STATE_DIR"

    # Create config file if it doesn't exist
    CONFIG_FILE="$CONFIG_DIR/config.yaml"
    if [ -f "$CONFIG_FILE" ]; then
        log_warn "Configuration file already exists at $CONFIG_FILE"
        log_warn "Skipping config generation - please update manually if needed"
    else
        log_info "Creating configuration file: $CONFIG_FILE"
        cat > "$CONFIG_FILE" <<EOF
# RMM Agent Configuration
# Generated by install.sh on $(date -u +"%Y-%m-%dT%H:%M:%SZ")

# Server URL (required)
server_url: "$SERVER_URL"

# Device token for registration (required for first run)
# Get this from the RMM server admin panel
device_token: "REPLACE_WITH_YOUR_DEVICE_TOKEN"

# Agent group (required)
group: "$GROUP"

# Polling interval in seconds (default: 60)
poll_interval: 60

# Jitter in seconds added to poll interval (default: 30)
# Actual interval = poll_interval + random(0, jitter_seconds)
jitter_seconds: 30

# Log level: debug, info, warn, error (default: info)
log_level: info

# API key is set automatically after successful registration
# DO NOT set this manually - it will be populated by the agent
# api_key:
EOF
        # Set restrictive permissions (contains secrets)
        chown "$SERVICE_USER":"$SERVICE_USER" "$CONFIG_FILE"
        chmod 0600 "$CONFIG_FILE"
    fi
}

# Install systemd service unit
install_service() {
    # Look for service file in expected locations
    SERVICE_SOURCE=""
    if [ -f "$SCRIPT_DIR/../systemd/rmm-agent.service" ]; then
        SERVICE_SOURCE="$SCRIPT_DIR/../systemd/rmm-agent.service"
    elif [ -f "$SCRIPT_DIR/rmm-agent.service" ]; then
        SERVICE_SOURCE="$SCRIPT_DIR/rmm-agent.service"
    fi

    if [ -z "$SERVICE_SOURCE" ]; then
        log_error "Service file not found"
        exit 1
    fi

    log_info "Installing systemd service unit"
    install -m 0644 "$SERVICE_SOURCE" "$SERVICE_FILE"

    log_info "Reloading systemd daemon"
    systemctl daemon-reload

    log_info "Enabling rmm-agent service"
    systemctl enable rmm-agent.service
}

# Print post-installation instructions
print_instructions() {
    echo ""
    echo "=============================================="
    echo -e "${GREEN}Installation complete!${NC}"
    echo "=============================================="
    echo ""
    echo "Next steps:"
    echo ""
    echo "1. Edit the configuration file and add your device token:"
    echo "   sudo nano $CONFIG_DIR/config.yaml"
    echo ""
    echo "   Replace 'REPLACE_WITH_YOUR_DEVICE_TOKEN' with your actual token"
    echo "   from the RMM server admin panel."
    echo ""
    echo "2. Start the service:"
    echo "   sudo systemctl start rmm-agent"
    echo ""
    echo "3. Check the service status:"
    echo "   sudo systemctl status rmm-agent"
    echo ""
    echo "4. View logs:"
    echo "   sudo journalctl -u rmm-agent -f"
    echo ""
}

# Main installation flow
main() {
    log_info "RMM Agent Installation Script"
    echo ""

    check_root
    parse_args "$@"

    log_info "Configuration:"
    log_info "  Group: $GROUP"
    log_info "  Server URL: $SERVER_URL"
    log_info "  Binary: $BINARY_PATH"
    echo ""

    create_user
    install_binary
    install_helper
    setup_config
    install_service

    print_instructions
}

# Run main
main "$@"
