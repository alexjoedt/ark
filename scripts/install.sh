#!/bin/bash

# ark installation script
# Installs the ark binary and registers 'ark serve' as a systemd service

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Configuration
BINARY_DIR="/usr/local/bin"
SYSTEMD_DIR="/etc/systemd/system"
DATA_DIR="/var/lib/ark"
REPO_DIR="${DATA_DIR}/repo"
DAEMON_CONFIG_DIR="/etc/ark"
DAEMON_CONFIG="${DAEMON_CONFIG_DIR}/daemon.json"
SERVICE_NAME="ark.service"

echo_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

echo_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

echo_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Guard: Check if running on macOS
if [[ "$OSTYPE" == "darwin"* ]]; then
    echo_error "This installation script is for Linux systems only."
    echo_error "macOS is not supported for systemd service installation."
    exit 1
fi

# Check if running as root
if [[ $EUID -ne 0 ]]; then
   echo_error "This script must be run as root (use sudo)"
   exit 1
fi

# Optional: admin password for the web UI
ADMIN_PASSWORD="${ARK_ADMIN_PASSWORD:-}"
if [[ -z "${ADMIN_PASSWORD}" ]]; then
    echo_warn "ARK_ADMIN_PASSWORD is not set. The web UI admin account will not be created automatically."
    echo_warn "Set it via: export ARK_ADMIN_PASSWORD=yourpassword before running this script,"
    echo_warn "or add it to the systemd service environment after installation."
fi

# Check if Go is installed
GO_BIN="/usr/local/go/bin/go"
if [[ ! -f "${GO_BIN}" ]]; then
    echo_error "Go is not installed at ${GO_BIN}. Please install Go first."
    echo_error "Visit: https://golang.org/doc/install"
    exit 1
fi

echo_info "Starting ark installation..."
echo_info "Go version: $(${GO_BIN} version)"

# Get the script directory and project root
SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
PROJECT_ROOT="$( cd "${SCRIPT_DIR}/.." && pwd )"

cd "${PROJECT_ROOT}"

# Detect versions for build
echo_info "Detecting versions..."
ARK_VERSION=$(./scripts/get-version.sh ark 2>/dev/null || echo "v0.0.0-dev")
BUILD_DATE=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
COMMIT_HASH=$(git rev-parse --short=7 HEAD 2>/dev/null || echo "unknown")

echo_info "ark version: ${ARK_VERSION}"

# Build ldflags
LDFLAGS="-X github.com/alexjoedt/ark/cmd/ark/internal/version.ArkVersion=${ARK_VERSION} \
    -X github.com/alexjoedt/ark/cmd/ark/internal/version.BuildDate=${BUILD_DATE} \
    -X github.com/alexjoedt/ark/cmd/ark/internal/version.CommitHash=${COMMIT_HASH}"

# Compile binary, pure Go, no C toolchain needed
echo_info "Compiling ark..."
CGO_ENABLED=0 ${GO_BIN} build -ldflags "${LDFLAGS}" -o ark ./cmd/ark

if [[ ! -f "./ark" ]]; then
    echo_error "Build failed. Binary not found."
    exit 1
fi

echo_info "Build successful."

# Create data directory
echo_info "Creating data directory..."
mkdir -p "${REPO_DIR}"

# Create daemon config directory and write daemon.json
echo_info "Creating daemon config at ${DAEMON_CONFIG}..."
mkdir -p "${DAEMON_CONFIG_DIR}"
chmod 750 "${DAEMON_CONFIG_DIR}"

# Write daemon.json only if it does not already exist (preserve operator edits).
if [[ ! -f "${DAEMON_CONFIG}" ]]; then
    cat > "${DAEMON_CONFIG}" << EODAEMON
{
  "repoPath": "${REPO_DIR}",
  "httpAddr": ":8080",
  "adminUsername": "admin",
  "csrfEnabled": true,
  "devMode": false
}
EODAEMON
    chmod 640 "${DAEMON_CONFIG}"
    echo_info "Daemon config written."
else
    echo_warn "${DAEMON_CONFIG} already exists — skipping (preserving existing config)."
fi

# Install binary
echo_info "Installing ark to ${BINARY_DIR}..."
install -m 0755 ark "${BINARY_DIR}/ark"
rm -f ./ark

# Build environment block for the service file.
# ARK_REPOSITORY and other non-secret settings live in /etc/ark/daemon.json.
# Only secrets that must never be on disk go here.
PASSWORD_FILE="${DAEMON_CONFIG_DIR}/password"
ENV_BLOCK="Environment=\"ARK_PASSWORD_FILE=${PASSWORD_FILE}\""
if [[ -n "${ADMIN_PASSWORD}" ]]; then
    ENV_BLOCK="${ENV_BLOCK}
Environment=\"ARK_ADMIN_PASSWORD=${ADMIN_PASSWORD}\""
fi

# Create systemd service file
echo_info "Creating systemd service file..."
cat > "${SYSTEMD_DIR}/${SERVICE_NAME}" << EOF
[Unit]
Description=Ark Docker Volume Backup Server
Documentation=https://github.com/alexjoedt/ark
After=network.target docker.service
Requires=docker.service

[Service]
Type=simple
ExecStart=/usr/local/bin/ark serve
Restart=on-failure
RestartSec=5
User=root

# Security settings
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=${DATA_DIR}

# Environment
Environment="PATH=/usr/local/bin:/usr/bin:/bin"
${ENV_BLOCK}

[Install]
WantedBy=multi-user.target
EOF

echo_info "Systemd service file created."

# Reload systemd daemon
echo_info "Reloading systemd daemon..."
systemctl daemon-reload

# Enable service
echo_info "Enabling ${SERVICE_NAME}..."
systemctl enable "${SERVICE_NAME}"

# Start service
echo_info "Starting ${SERVICE_NAME}..."
systemctl start "${SERVICE_NAME}"

# Wait a moment and check status
sleep 2
if systemctl is-active --quiet "${SERVICE_NAME}"; then
    echo_info "ark service is running successfully!"
else
    echo_error "ark service failed to start. Check logs with: journalctl -u ark -n 50"
    exit 1
fi

# Print success message and instructions
echo ""
echo -e "${GREEN}════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}✓ Installation completed successfully!${NC}"
echo -e "${GREEN}════════════════════════════════════════════════════════${NC}"
echo ""
echo "Installed components:"
echo "  • ark binary:      ${BINARY_DIR}/ark (${ARK_VERSION})"
echo "  • systemd service: ${SYSTEMD_DIR}/${SERVICE_NAME}"
echo "  • Daemon config:   ${DAEMON_CONFIG}"
echo "  • Data directory:  ${DATA_DIR}"
echo "  • Repo path:       ${REPO_DIR}"
echo ""
echo "Service status:"
systemctl status "${SERVICE_NAME}" --no-pager -l
echo ""
echo "Web UI is available at: http://localhost:8080"
echo ""
echo "Configuration:"
echo "  • Non-secret settings (httpAddr, repoPath, etc.): ${DAEMON_CONFIG}"
echo "  • Admin password (secret, never on disk):         ARK_ADMIN_PASSWORD env var in the systemd unit"
echo "  • Repository password:                           ${PASSWORD_FILE} (ARK_PASSWORD_FILE)"
echo ""
if [[ ! -f "${PASSWORD_FILE}" ]]; then
    echo_warn "ark serve needs an initialized repository and its password. Create both:"
    echo_warn "  sudo install -m 0600 /dev/null ${PASSWORD_FILE} && sudo \$EDITOR ${PASSWORD_FILE}"
    echo_warn "  sudo ark init --repo ${REPO_DIR} --password-file ${PASSWORD_FILE}"
    echo_warn "Then: sudo systemctl restart ark.service"
    echo_warn "A lost password is lost data. Keep a copy outside this host."
    echo ""
fi
if [[ -z "${ADMIN_PASSWORD}" ]]; then
    echo_warn "No admin password was set. To create the admin account:"
    echo_warn "  sudo systemctl edit ark.service"
    echo_warn "Add under [Service]: Environment=\"ARK_ADMIN_PASSWORD=yourpassword\""
    echo_warn "Then: sudo systemctl restart ark.service"
    echo ""
fi
echo "Useful commands:"
echo "  • Edit daemon config:   \$EDITOR ${DAEMON_CONFIG} && sudo systemctl restart ark"
echo "  • Check service status: systemctl status ark"
echo "  • View logs:            journalctl -u ark -f"
echo "  • Restart service:      sudo systemctl restart ark"
echo "  • List volumes:         ark volumes"
echo ""
echo -e "${GREEN}Happy backing up! 🚀${NC}"
echo ""