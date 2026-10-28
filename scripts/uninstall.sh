#!/bin/bash

# ark uninstallation script
# Removes the ark binary, systemd service, and optionally backup data

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
    echo_error "This script is for Linux systems only."
    exit 1
fi

# Check if running as root
if [[ $EUID -ne 0 ]]; then
   echo_error "This script must be run as root (use sudo)"
   exit 1
fi

echo_warn "This will uninstall ark from your system."
echo ""

# Ask about data removal
REMOVE_DATA=false
read -p "Do you want to remove backup data in ${DATA_DIR}? [y/N]: " -n 1 -r
echo
if [[ $REPLY =~ ^[Yy]$ ]]; then
    REMOVE_DATA=true
    echo_warn "⚠️  Backup data will be DELETED!"
else
    echo_info "Backup data will be preserved in ${DATA_DIR}"
fi

echo ""
read -p "Continue with uninstallation? [y/N]: " -n 1 -r
echo
if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo_info "Uninstallation cancelled."
    exit 0
fi

echo ""
echo_info "Starting ark uninstallation..."

# Stop and disable service
if systemctl is-active --quiet "${SERVICE_NAME}"; then
    echo_info "Stopping ark service..."
    systemctl stop "${SERVICE_NAME}"
fi

if systemctl is-enabled --quiet "${SERVICE_NAME}" 2>/dev/null; then
    echo_info "Disabling ark service..."
    systemctl disable "${SERVICE_NAME}"
fi

# Remove systemd service file
if [[ -f "${SYSTEMD_DIR}/${SERVICE_NAME}" ]]; then
    echo_info "Removing systemd service file..."
    rm -f "${SYSTEMD_DIR}/${SERVICE_NAME}"
    systemctl daemon-reload
fi

# Remove binary
if [[ -f "${BINARY_DIR}/ark" ]]; then
    echo_info "Removing ark binary..."
    rm -f "${BINARY_DIR}/ark"
fi

# Remove data directory if requested
if [[ "$REMOVE_DATA" == true ]]; then
    if [[ -d "${DATA_DIR}" ]]; then
        echo_warn "Removing data directory ${DATA_DIR}..."
        rm -rf "${DATA_DIR}"
    fi
fi

echo ""
echo -e "${GREEN}════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}✓ Uninstallation completed!${NC}"
echo -e "${GREEN}════════════════════════════════════════════════════════${NC}"
echo ""

if [[ "$REMOVE_DATA" == false ]] && [[ -d "${DATA_DIR}" ]]; then
    echo_info "Your backup data is preserved in: ${DATA_DIR}"
    echo_info "To manually remove it later: sudo rm -rf ${DATA_DIR}"
    echo ""
fi

echo "Removed components:"
echo "  ✓ ark binary"
echo "  ✓ systemd service (ark.service)"
if [[ "$REMOVE_DATA" == true ]]; then
    echo "  ✓ Data directory"
fi
echo ""