#!/bin/bash

# ark update script
# Updates the ark binary to the latest version
#
# Usage:
#   sudo ./update.sh              # Use default Go installation paths
#   sudo ./update.sh /path/to/go  # Use custom Go binary path

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration
BINARY_DIR="/usr/local/bin"
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

echo_step() {
    echo -e "${BLUE}[STEP]${NC} $1"
}

# Parse arguments
CUSTOM_GO_BIN="${1:-}"

# Guard: Check if running on macOS
if [[ "$OSTYPE" == "darwin"* ]]; then
    echo_error "This update script is for Linux systems only."
    echo_error "macOS is not supported for systemd service installation."
    exit 1
fi

# Check if running as root
if [[ $EUID -ne 0 ]]; then
   echo_error "This script must be run as root (use sudo)"
   exit 1
fi

# Check if ark is installed
if [[ ! -f "${BINARY_DIR}/ark" ]]; then
    echo_error "ark is not installed. Please run install.sh first."
    exit 1
fi

# Check if Go is installed
if [[ -n "${CUSTOM_GO_BIN}" ]]; then
    GO_BIN="${CUSTOM_GO_BIN}"
    if [[ ! -f "${GO_BIN}" ]]; then
        echo_error "Go is not found at custom path: ${GO_BIN}"
        exit 1
    fi
else
    GO_BIN="/usr/local/go/bin/go"
    if [[ ! -f "${GO_BIN}" ]]; then
        GO_BIN="${HOME}/.local/go/bin/go"
        if [[ ! -f "${GO_BIN}" ]]; then
            echo_error "Go is not installed at /usr/local/go/bin/go or ${HOME}/.local/go/bin/go"
            echo_error "You can specify a custom Go path as an argument: $0 /path/to/go"
            echo_error "Visit: https://golang.org/doc/install"
            exit 1
        fi
    fi
fi

echo_info "Starting ark update process..."
echo_info "Go version: $(${GO_BIN} version)"
echo ""

# Get current version
echo_step "Checking current version..."
CURRENT_VERSION=$("${BINARY_DIR}/ark" version 2>/dev/null | head -1 || echo "unknown")
echo "  Current ark version: ${CURRENT_VERSION}"
echo ""

# Get the script directory and project root
SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
PROJECT_ROOT="$( cd "${SCRIPT_DIR}/.." && pwd )"

cd "${PROJECT_ROOT}"

# Detect versions for build
echo_step "Detecting target version..."
ARK_VERSION=$(./scripts/get-version.sh ark 2>/dev/null || echo "v0.0.0-dev")
BUILD_DATE=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
COMMIT_HASH=$(git rev-parse --short=7 HEAD 2>/dev/null || echo "unknown")

echo "  Target ark version: ${ARK_VERSION}"
echo ""

# Build ldflags
LDFLAGS="-X github.com/alexjoedt/ark/cmd/ark/internal/version.ArkVersion=${ARK_VERSION} \
    -X github.com/alexjoedt/ark/cmd/ark/internal/version.BuildDate=${BUILD_DATE} \
    -X github.com/alexjoedt/ark/cmd/ark/internal/version.CommitHash=${COMMIT_HASH}"

# Compile new binary, pure Go, no C toolchain needed
echo_step "Compiling new ark binary..."
CGO_ENABLED=0 ${GO_BIN} build -ldflags "${LDFLAGS}" -o ark_new ./cmd/ark

if [[ ! -f "./ark_new" ]]; then
    echo_error "Build failed. Binary not found."
    echo_error "Old binary is preserved. No changes made."
    exit 1
fi

echo_info "Build successful."
echo ""

# Check if service is running
SERVICE_WAS_ACTIVE=false
if systemctl is-active --quiet "${SERVICE_NAME}"; then
    SERVICE_WAS_ACTIVE=true
    echo_step "Stopping ark service..."
    systemctl stop "${SERVICE_NAME}"
    echo_info "Service stopped."
    echo ""
fi

# Replace binary
echo_step "Installing new binary..."
install -m 0755 ark_new "${BINARY_DIR}/ark"
rm -f ./ark_new
echo_info "Binary installed to ${BINARY_DIR}/ark"
echo ""

# Start service if it was running
if [[ "$SERVICE_WAS_ACTIVE" == true ]]; then
    echo_step "Starting ark service..."
    systemctl start "${SERVICE_NAME}"

    sleep 2
    if systemctl is-active --quiet "${SERVICE_NAME}"; then
        echo_info "Service started successfully!"
    else
        echo_error "Service failed to start!"
        echo_error "Check logs with: journalctl -u ark -n 50"
        exit 1
    fi
else
    echo_warn "Service was not running. Skipped service restart."
fi

echo ""
echo -e "${GREEN}════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}✓ Update completed successfully!${NC}"
echo -e "${GREEN}════════════════════════════════════════════════════════${NC}"
echo ""
echo "  ark: ${CURRENT_VERSION} → ${ARK_VERSION}"
echo ""

if [[ "$SERVICE_WAS_ACTIVE" == true ]]; then
    echo "Service status:"
    systemctl status "${SERVICE_NAME}" --no-pager -l
    echo ""
fi

echo "Useful commands:"
echo "  • Check version: ark version"
echo "  • View logs:     journalctl -u ark -f"
echo "  • Check status:  systemctl status ark"
echo ""
echo -e "${GREEN}Update complete! 🎉${NC}"
echo ""
