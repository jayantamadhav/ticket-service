#!/usr/bin/env bash
# One-command on-sale stampede against a running ticket-service instance.
#
# Usage:
#   ./burst.sh [BASE_URL]
#   ./burst.sh https://paytm-assignment.work.gd
#
# Defaults to http://localhost:8080 if no URL is given.
set -euo pipefail

BASE_URL="${1:-${BASE_URL:-http://localhost:8080}}"

cd "$(dirname "${BASH_SOURCE[0]}")"
go run ./cmd/burst -base-url "$BASE_URL"
