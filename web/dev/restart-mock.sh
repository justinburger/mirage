#!/bin/sh
# Restart the mock server in the background (state resets).
pkill -f "web/dev/mock-server.mjs" 2>/dev/null; pkill -f "node mock-server.mjs" 2>/dev/null
sleep 0.4
cd "$(dirname "$0")" && (MOCK_QUIET=1 nohup node mock-server.mjs > /tmp/mirage-mock.log 2>&1 &)
sleep 1.2
