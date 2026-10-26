#!/usr/bin/env bash
# Stops what up.sh started.
REAL_DIR="${REAL_DIR:-${TMPDIR:-/tmp}/mirage-ui-real}"
for name in trafficgen mirage demo-api; do
  if [ -f "$REAL_DIR/$name.pid" ]; then
    kill "$(cat "$REAL_DIR/$name.pid")" 2>/dev/null || true
    rm -f "$REAL_DIR/$name.pid"
  fi
done
sleep 0.5
