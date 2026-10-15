#!/usr/bin/env bash
# Runs Advance against the bundled PayPal mock so you can try the whole flow
# offline, with no credentials. Open http://localhost:8080 when it starts.
set -euo pipefail
cd "$(dirname "$0")/.."

mkdir -p bin
go build -o bin/advance ./cmd/advance
go build -o bin/mockpaypal ./cmd/mockpaypal

ADVANCE_PORT="${ADVANCE_PORT:-8080}"
MOCK_PORT="${MOCK_PORT:-9090}"

ADDR=":${MOCK_PORT}" WEBHOOK_TARGET="http://localhost:${ADVANCE_PORT}/webhooks/paypal" ./bin/mockpaypal &
MOCK_PID=$!
ADDR=":${ADVANCE_PORT}" PAYPAL_BASE_URL="http://localhost:${MOCK_PORT}" \
  PAYPAL_CLIENT_ID=demo PAYPAL_CLIENT_SECRET=demo DEMO_MODE=true \
  ANTHROPIC_API_KEY="${ANTHROPIC_API_KEY:-}" DATA_FILE="" ./bin/advance &
ADV_PID=$!
trap 'kill $MOCK_PID $ADV_PID 2>/dev/null || true' EXIT INT TERM

echo
echo "Advance demo is running: http://localhost:${ADVANCE_PORT}"
echo "Press Ctrl+C to stop."
wait
