#!/usr/bin/env bash
# Starts a real Mirage with live traffic so every UI page has real data:
#   demo-api :7001  <-  mirage "demo" (record) :9001  <-  trafficgen (continuous)
#   mirage "demo-virtual" (virtualize) :9002 serving the recording, replay jobs against it
# Admin UI on http://127.0.0.1:8484. State lives in $REAL_DIR (default: a temp dir);
# web/dev/real/down.sh stops everything. Writes $REAL_DIR/ids.json for the scenarios.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
REAL_DIR="${REAL_DIR:-${TMPDIR:-/tmp}/mirage-ui-real}"
mkdir -p "$REAL_DIR"
"$ROOT/web/dev/real/down.sh" >/dev/null 2>&1 || true
rm -rf "$REAL_DIR"/*
cd "$ROOT"
mkdir -p bin
go build -o bin/mirage ./cmd/mirage
go build -o bin/demo-api ./examples/demo-api
go build -o bin/trafficgen ./examples/trafficgen

start() { # start NAME CMD...
  local name=$1; shift
  "$@" >"$REAL_DIR/$name.log" 2>&1 &
  echo $! >"$REAL_DIR/$name.pid"
}
start demo-api ./bin/demo-api -addr 127.0.0.1:7001 -error-rate 0.04
sleep 0.4
start mirage ./bin/mirage serve --config examples/mirage.yaml --data "$REAL_DIR/mirage.db" --log-level debug
for _ in $(seq 1 50); do curl -sf http://127.0.0.1:8484/api/v1/health >/dev/null && break; sleep 0.1; done

echo "filling the recording (35 s of traffic) ..."
./bin/trafficgen -target http://127.0.0.1:9001 -rps 40 -duration 35s -users 12 -seed 3 >"$REAL_DIR/trafficgen-fill.log" 2>&1
sleep 1.5
BASE=http://127.0.0.1:8484 REAL_DIR="$REAL_DIR" node web/dev/real/seed.mjs

# continuous background traffic through the recording service
start trafficgen ./bin/trafficgen -target http://127.0.0.1:9001 -rps 18 -duration 0 -users 8 -seed 9
echo "up: http://127.0.0.1:8484   (logs and ids.json in $REAL_DIR)"
