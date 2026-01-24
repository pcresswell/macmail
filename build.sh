#!/bin/bash
set -e

VERSION_FILE="VERSION"
PACKAGE="github.com/pcresswell/macmail/cmd/macmail"

# Read current version
if [[ ! -f "$VERSION_FILE" ]]; then
    echo "1.0.0" > "$VERSION_FILE"
fi

CURRENT_VERSION=$(cat "$VERSION_FILE" | tr -d '[:space:]')

# Parse semver components
IFS='.' read -r MAJOR MINOR PATCH <<< "$CURRENT_VERSION"

# Increment patch version
PATCH=$((PATCH + 1))
NEW_VERSION="${MAJOR}.${MINOR}.${PATCH}"

# Write new version
echo "$NEW_VERSION" > "$VERSION_FILE"

echo "Building macmail v${NEW_VERSION}..."

# Build with version injected via ldflags
go build -ldflags "-X main.version=${NEW_VERSION}" -o macmail ./cmd/macmail

echo "Built macmail v${NEW_VERSION}"
