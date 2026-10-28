#!/usr/bin/env bash
# get-version.sh - Detect the CLI version from git tags
# Usage: ./get-version.sh
#   Looks for CLI module tags: cmd/ark/v*.*.* and prints the version without the prefix

set -euo pipefail

# Check if we're in a git repository
if ! git rev-parse --git-dir > /dev/null 2>&1; then
    echo "v0.0.0-dev"
    exit 0
fi

# Try to find the most recent CLI tag (cmd/ark/v*.*.*)
# --abbrev=0 gives us just the tag name without commit info
LATEST_TAG=$(git describe --tags --match "cmd/ark/v[0-9]*.[0-9]*.[0-9]*" --abbrev=0 2>/dev/null || echo "")
VERSION="${LATEST_TAG#cmd/ark/}"

if [[ -z "$LATEST_TAG" ]]; then
    # No tag found, it's a dirty dev build
    SHORT_COMMIT=$(git rev-parse --short=7 HEAD 2>/dev/null || echo "unknown")
    echo "v0.0.0-dirty-${SHORT_COMMIT}"
    exit 0
fi

# Check if HEAD is exactly at this tag
CURRENT_COMMIT=$(git rev-parse HEAD 2>/dev/null || echo "")
TAG_COMMIT=$(git rev-parse "${LATEST_TAG}^{}" 2>/dev/null || echo "")

if [[ "$CURRENT_COMMIT" == "$TAG_COMMIT" ]]; then
    # Exact tag match - clean release build
    echo "${VERSION}"
else
    # HEAD is not at the tag - dirty build
    SHORT_COMMIT=$(git rev-parse --short=7 HEAD 2>/dev/null || echo "unknown")
    echo "${VERSION}-dirty-${SHORT_COMMIT}"
fi
