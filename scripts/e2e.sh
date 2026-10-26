#!/usr/bin/env bash
# End-to-end test of Mirage: builds the binaries, then drives them over HTTP
# the way a user would.
#
#   record   demo-api <- mirage service <- trafficgen (anonymized capture)
#   check    the recording, live stats, no raw PII in the export or the database
#   virtualize, stop demo-api, recorded responses and 404 JSON misses
#   amplify  error amplification raises the virtual error rate
#   replay   API job at 10x speed, then the `mirage replay` CLI and its exit codes
#   data     HAR export/import round trip, `mirage anonymize`, /metrics, SSE, UI
#   restart  graceful shutdown, data persisted, service comes back virtualized
#
# Requirements: bash, curl, go. jq is used for JSON when installed, otherwise
# python3 (force the fallback with E2E_NO_JQ=1). Everything runs on
# 127.0.0.1 with free ports and a temporary data directory.
#
#   scripts/e2e.sh          run it
#   E2E_KEEP=1 scripts/e2e.sh   keep the temp dir (logs, database) afterwards
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------
if [ -t 1 ]; then
  C_OK=$'\033[32m'; C_BAD=$'\033[31m'; C_DIM=$'\033[2m'; C_BOLD=$'\033[1m'; C_OFF=$'\033[0m'
else
  C_OK=''; C_BAD=''; C_DIM=''; C_BOLD=''; C_OFF=''
fi

TMP="$(mktemp -d "${TMPDIR:-/tmp}/mirage-e2e.XXXXXX")"
PIDS=()
CHECKS=0
STARTED=$SECONDS

step() { printf '\n%s== %s%s\n' "$C_BOLD" "$*" "$C_OFF"; }
ok()   { CHECKS=$((CHECKS + 1)); printf '  %sok%s  %s\n' "$C_OK" "$C_OFF" "$*"; }
note() { printf '  %s..  %s%s\n' "$C_DIM" "$*" "$C_OFF"; }

dump_logs() {
  local f
  for f in "$TMP"/*.log; do
    [ -f "$f" ] || continue
    printf '\n%s--- %s (last 25 lines)%s\n' "$C_DIM" "$(basename "$f")" "$C_OFF" >&2
    tail -n 25 "$f" >&2 || true
  done
}

fail() {
  printf '  %sFAIL%s %s\n' "$C_BAD" "$C_OFF" "$*" >&2
  dump_logs
  printf '\n%se2e: FAILED%s (temp dir kept: %s)\n' "$C_BAD" "$C_OFF" "$TMP" >&2
  KEEP=1
  exit 1
}

cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  local pid
  for pid in ${PIDS[@]+"${PIDS[@]}"}; do
    kill "$pid" 2>/dev/null || true
  done
  for pid in ${PIDS[@]+"${PIDS[@]}"}; do
    wait "$pid" 2>/dev/null || true
  done
  if [ "${KEEP:-${E2E_KEEP:-0}}" = "1" ]; then
    echo "e2e: temp dir kept at $TMP" >&2
  else
    rm -rf "$TMP"
  fi
  exit "$rc"
}
trap cleanup EXIT
trap 'echo; echo "e2e: interrupted" >&2; exit 130' INT TERM

# assert DESCRIPTION COMMAND...  (the command's exit status decides)
assert() {
  local desc=$1; shift
  if "$@"; then ok "$desc"; else fail "$desc"; fi
}

# num_ge A B / num_le A B / num_gt A B / num_lt A B: float comparisons.
num_ge() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a + 0 >= b + 0) }'; }
num_le() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a + 0 <= b + 0) }'; }
num_gt() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a + 0 > b + 0) }'; }
num_lt() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a + 0 < b + 0) }'; }
# between VALUE LOW HIGH: LOW <= VALUE <= HIGH
between() { num_ge "$1" "$2" && num_le "$1" "$3"; }
calc() { awk "BEGIN { printf \"%.4f\", $* }"; }

# ---- JSON access: dotted paths, [N] indexes and a trailing # for length.
#   j '.status.captured' < file      j '.items#' < file      j '.[0].id' < file
USE_JQ=0
if command -v jq >/dev/null 2>&1 && [ "${E2E_NO_JQ:-0}" != "1" ]; then USE_JQ=1; fi
j() {
  local path=$1
  if [ "$USE_JQ" = 1 ]; then
    local len=''
    case $path in *'#') len=' | length'; path=${path%#} ;; esac
    # .a.b[0].c  ->  .["a"]["b"][0]["c"]
    local f
    f=$(printf '%s' "$path" | sed -E 's/\.([A-Za-z0-9_-]+)/["\1"]/g')
    jq -r ".${f#.}${len} | if . == null then \"null\" else . end"
  else
    python3 -c '
import json, re, sys
d = json.load(sys.stdin)
p = sys.argv[1]
length = p.endswith("#")
if length:
    p = p[:-1]
try:
    for m in re.finditer(r"\.([A-Za-z0-9_-]+)|\[(\d+)\]", p):
        d = d[m.group(1)] if m.group(1) is not None else d[int(m.group(2))]
    if length:
        d = len(d)
except (KeyError, IndexError, TypeError):
    d = None
if isinstance(d, bool):
    print("true" if d else "false")
elif d is None:
    print("null")
elif isinstance(d, (dict, list)):
    print(json.dumps(d, separators=(",", ":")))
else:
    print(d)
' "$path"
  fi
}

# json_valid FILE
json_valid() {
  if [ "$USE_JQ" = 1 ]; then jq -e . "$1" >/dev/null 2>&1
  else python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$1" 2>/dev/null; fi
}

# ---- HTTP
ADMIN=""      # http://127.0.0.1:PORT, set when mirage is started
TOKEN_ARGS=()
# api METHOD PATH [BODY]  -> prints the response body, fails on status >= 400
api() {
  local method=$1 path=$2 body=${3-}
  local out="$TMP/api.out" code
  if [ -n "$body" ]; then
    code=$(curl -sS --max-time 30 -o "$out" -w '%{http_code}' -X "$method" \
      -H 'Content-Type: application/json' --data-binary "$body" "$ADMIN/api/v1$path") || fail "curl $method $path"
  else
    code=$(curl -sS --max-time 30 -o "$out" -w '%{http_code}' -X "$method" "$ADMIN/api/v1$path") || fail "curl $method $path"
  fi
  if [ "$code" -ge 400 ]; then
    echo "  $method $path -> $code: $(cat "$out")" >&2
    return 1
  fi
  cat "$out"
}
# api_status METHOD PATH [BODY] -> prints only the status code
api_status() {
  local method=$1 path=$2 body=${3-}
  if [ -n "$body" ]; then
    curl -sS --max-time 30 -o /dev/null -w '%{http_code}' -X "$method" -H 'Content-Type: application/json' --data-binary "$body" "$ADMIN/api/v1$path"
  else
    curl -sS --max-time 30 -o /dev/null -w '%{http_code}' -X "$method" "$ADMIN/api/v1$path"
  fi
}

port_in_use() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
free_port() {
  local p tries=0
  while [ $tries -lt 200 ]; do
    p=$((20000 + RANDOM % 20000))
    if ! port_in_use "$p"; then echo "$p"; return 0; fi
    tries=$((tries + 1))
  done
  fail "no free port found"
}

wait_for() { # wait_for DESCRIPTION SECONDS COMMAND...
  local desc=$1 secs=$2; shift 2
  local i=0 max=$((secs * 10))
  while ! "$@" >/dev/null 2>&1; do
    i=$((i + 1))
    [ $i -ge $max ] && fail "timed out after ${secs}s waiting for $desc"
    sleep 0.1
  done
}

health_ok() { curl -sSf --max-time 2 "$ADMIN/api/v1/health" >/dev/null 2>&1; }

# start_mirage LOGFILE ARGS... : starts the server, sets MIRAGE_PID
start_mirage() {
  local log=$1; shift
  "$BIN/mirage" serve "$@" >"$TMP/$log" 2>&1 &
  MIRAGE_PID=$!
  PIDS+=("$MIRAGE_PID")
  wait_for "mirage to answer on $ADMIN" 20 health_ok
}

# stop_mirage: SIGINT and wait; sets STOP_RC to the exit status
stop_mirage() {
  kill -INT "$MIRAGE_PID" 2>/dev/null || true
  local i=0
  while kill -0 "$MIRAGE_PID" 2>/dev/null; do
    i=$((i + 1))
    [ $i -ge 150 ] && fail "mirage did not shut down within 15s"
    sleep 0.1
  done
  STOP_RC=0
  wait "$MIRAGE_PID" || STOP_RC=$?
}

# wait_replay ID SECONDS: waits for a replay job to reach a final status
wait_replay() {
  local id=$1 secs=$2 i=0 st
  while :; do
    st=$(api GET "/replays/$id" | j '.status')
    case $st in completed|stopped|failed) echo "$st"; return 0 ;; esac
    i=$((i + 1))
    [ $i -ge $((secs * 2)) ] && fail "replay $id still '$st' after ${secs}s"
    sleep 0.5
  done
}

# ---------------------------------------------------------------------------
# 0. build
# ---------------------------------------------------------------------------
step "build"
BIN="$ROOT/bin"
mkdir -p "$BIN"
go build -o "$BIN/mirage" ./cmd/mirage
go build -o "$BIN/demo-api" ./examples/demo-api
go build -o "$BIN/trafficgen" ./examples/trafficgen
ok "built bin/mirage, bin/demo-api, bin/trafficgen"
note "JSON tool: $([ "$USE_JQ" = 1 ] && echo jq || echo python3)"
[ "$USE_JQ" = 1 ] || command -v python3 >/dev/null || fail "need jq or python3"

DEMO_PORT=$(free_port)
ADMIN_PORT=$(free_port)
SVC_PORT=$(free_port)
while [ "$ADMIN_PORT" = "$DEMO_PORT" ] || [ "$SVC_PORT" = "$DEMO_PORT" ] || [ "$SVC_PORT" = "$ADMIN_PORT" ]; do
  ADMIN_PORT=$(free_port); SVC_PORT=$(free_port)
done
DEMO="http://127.0.0.1:$DEMO_PORT"
SVC="http://127.0.0.1:$SVC_PORT"
ADMIN="http://127.0.0.1:$ADMIN_PORT"
DB="$TMP/data/mirage.db"
mkdir -p "$TMP/data"

# Raw values that must never reach the database or an export.
CANARY_EMAIL="canary.e2e.pii7f3a91@example.com"
CANARY_TOKEN="canary-e2e-token-7f3a91c2d8"
CANARY_CARD="4012888888881881"
TEST_CARDS="4242424242424242 4111111111111111 5555555555554444 378282246310005 6011111111111117"

# ---------------------------------------------------------------------------
# 1. start the demo API and mirage
# ---------------------------------------------------------------------------
step "start demo-api and mirage"
"$BIN/demo-api" -addr "127.0.0.1:$DEMO_PORT" -latency 8ms -error-rate 0.06 -seed 7 >"$TMP/demo-api.log" 2>&1 &
DEMO_PID=$!
PIDS+=("$DEMO_PID")
wait_for "demo-api" 10 curl -sf "$DEMO/health"
ok "demo-api answers on $DEMO"

cat >"$TMP/mirage.yaml" <<EOF
server:
  data: $DB
  log_level: info
recordings:
  - name: e2e
    description: recorded by scripts/e2e.sh
services:
  - name: demo
    listen_addr: 127.0.0.1:$SVC_PORT
    upstream: $DEMO
    mode: record
    enabled: true
    recording: e2e
    anonymize_capture: true
EOF
start_mirage mirage.log --admin "127.0.0.1:$ADMIN_PORT" --config "$TMP/mirage.yaml"
ok "mirage serve answers on $ADMIN"
assert "the config file's server.data was used" test -s "$DB"

SVC_ID=$(api GET /services | j '.[0].id')
REC_ID=$(api GET /services | j '.[0].recording_id')
assert "service demo is running in record mode" test "$(api GET "/services/$SVC_ID" | j '.status.running')/$(api GET "/services/$SVC_ID" | j '.status.mode')" = "true/record"
assert "the recording e2e was created from the config" test -n "$REC_ID" -a "$REC_ID" != null

# ---------------------------------------------------------------------------
# 2. record through the proxy
# ---------------------------------------------------------------------------
step "record: canary requests, then trafficgen through the proxy"
# Requests carrying values that must not be stored in the clear.
code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$SVC/api/login" -H 'Content-Type: application/json' \
  --data "{\"email\":\"$CANARY_EMAIL\",\"password\":\"hunter2-canary\"}")
assert "canary login reached the upstream (got $code)" test "$code" -ge 200 -a "$code" -lt 600
code=$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$SVC/api/orders" -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $CANARY_TOKEN" \
  --data "{\"email\":\"$CANARY_EMAIL\",\"card_number\":\"$CANARY_CARD\",\"items\":[{\"product_id\":1,\"quantity\":1}]}")
assert "canary order reached the upstream (got $code)" test "$code" -ge 200 -a "$code" -lt 600
curl -sS -o /dev/null "$SVC/api/search?q=shoe&email=$CANARY_EMAIL&api_key=$CANARY_TOKEN" || true

RATE=60
SECS=8
"$BIN/trafficgen" -target "$SVC" -rps $RATE -duration ${SECS}s -users 8 -seed 11 >"$TMP/trafficgen.log" 2>&1 || fail "trafficgen failed"
ok "trafficgen sent ~$((RATE * SECS)) requests through the proxy"
sleep 1.2 # let the capture queue flush and the registry close the last second

REC=$(api GET "/recordings/$REC_ID")
EX_COUNT=$(echo "$REC" | j '.exchange_count')
ERR_COUNT=$(echo "$REC" | j '.error_count')
note "recording: $EX_COUNT exchanges, $ERR_COUNT errors"
assert "the recording has exchanges (>= 300)" num_ge "$EX_COUNT" 300
assert "the recording saw upstream errors" num_ge "$ERR_COUNT" 3
assert "the recording is marked anonymized" test "$(echo "$REC" | j '.anonymized')" = true
STATUS=$(api GET "/services/$SVC_ID")
assert "no capture was dropped" test "$(echo "$STATUS" | j '.status.capture_dropped')" = 0
assert "captured counter matches the recording" test "$(echo "$STATUS" | j '.status.captured')" = "$EX_COUNT"

# ---------------------------------------------------------------------------
# 3. live stats
# ---------------------------------------------------------------------------
step "live statistics"
STATS=$(api GET "/services/$SVC_ID/stats")
assert "service stats count the traffic" num_ge "$(echo "$STATS" | j '.requests')" "$EX_COUNT"
assert "service stats have latency percentiles" num_gt "$(echo "$STATS" | j '.latency.p99')" 0
assert "service stats list endpoints" num_gt "$(echo "$STATS" | j '.endpoints#')" 3
assert "service stats split status classes" num_gt "$(echo "$STATS" | j '.status_classes.2xx')" 100
assert "/stats lists the service collector" test "$(api GET /stats | j '.[0].name')" = "service:$SVC_ID"
SERIES=$(api GET "/services/$SVC_ID/series")
assert "the per-second series has points" num_ge "$(echo "$SERIES" | j '.#')" 5

# ---------------------------------------------------------------------------
# 4. no raw PII in the export or the database
# ---------------------------------------------------------------------------
step "anonymization at capture"
curl -sSf --max-time 60 -o "$TMP/e2e.har" "$ADMIN/api/v1/recordings/$REC_ID/export.har" || fail "HAR export"
assert "the exported HAR is valid JSON" json_valid "$TMP/e2e.har"
assert "the HAR holds every exchange" test "$(j '.log.entries#' <"$TMP/e2e.har")" = "$EX_COUNT"
assert "the canary order was recorded" test "$(api GET "/recordings/$REC_ID/exchanges?method=POST&path=/api/orders" | j '.total')" -ge 1
raw_hits=0
for needle in "$CANARY_EMAIL" "$CANARY_TOKEN" "$CANARY_CARD" "hunter2-canary" $TEST_CARDS; do
  if grep -aFq -- "$needle" "$TMP/e2e.har"; then echo "  raw value in HAR: $needle" >&2; raw_hits=$((raw_hits + 1)); fi
done
assert "no raw email, token, password or card number in the HAR export" test "$raw_hits" = 0
db_hits=0
for needle in "$CANARY_EMAIL" "$CANARY_TOKEN" "$CANARY_CARD" "hunter2-canary" $TEST_CARDS; do
  for f in "$DB" "$DB-wal"; do
    [ -f "$f" ] || continue
    if LC_ALL=C grep -aFq -- "$needle" "$f"; then echo "  raw value in $(basename "$f"): $needle" >&2; db_hits=$((db_hits + 1)); fi
  done
done
assert "no raw value in the SQLite database or its WAL" test "$db_hits" = 0

# ---------------------------------------------------------------------------
# 5. virtualize
# ---------------------------------------------------------------------------
step "virtualize with demo-api stopped"
# /health never fails and answers instantly in demo-api, so every recorded
# candidate is a 200 and the first one served is the first one recorded.
LIST=$(api GET "/recordings/$REC_ID/exchanges?method=GET&path=/health&limit=5")
PROBE_ID=$(echo "$LIST" | j '.items[0].id')
PROBE_PATH=$(echo "$LIST" | j '.items[0].path')
PROBE_QUERY=$(echo "$LIST" | j '.items[0].raw_query')
[ "$PROBE_QUERY" = null ] && PROBE_QUERY=""
PROBE_URL="$SVC$PROBE_PATH${PROBE_QUERY:+?$PROBE_QUERY}"
curl -sS --max-time 10 -o "$TMP/live.body" "$PROBE_URL" || fail "probe through the proxy"
api GET "/exchanges/$PROBE_ID" | j '.response.body.data' >"$TMP/recorded.body"

M=$(api POST "/services/$SVC_ID/mode" "{\"mode\":\"virtualize\",\"recording_id\":\"$REC_ID\"}")
assert "service switched to virtualize" test "$(echo "$M" | j '.mode')/$(echo "$M" | j '.status.mode')" = "virtualize/virtualize"
sleep 0.3
assert "the virtual index holds the recording" test "$(api GET "/services/$SVC_ID" | j '.status.indexed_exchanges')" -ge 300

kill "$DEMO_PID" 2>/dev/null || true
wait "$DEMO_PID" 2>/dev/null || true
assert "demo-api is down" bash -c "! curl -sf --max-time 1 '$DEMO/health' >/dev/null"

code=$(curl -sS --max-time 10 -D "$TMP/virt.hdr" -o "$TMP/virt.body" -w '%{http_code}' "$PROBE_URL")
assert "a recorded request is still answered (status $code)" test "$code" = 200
assert "the response says which level matched" grep -aqi '^x-mirage-match:' "$TMP/virt.hdr"
assert "the body is the recorded one" test "$(cat "$TMP/virt.body")" = "$(cat "$TMP/recorded.body")"
code=$(curl -sS --max-time 10 -D "$TMP/miss.hdr" -o "$TMP/miss.body" -w '%{http_code}' "$SVC/api/never/recorded/e2e")
assert "an unrecorded request gets 404" test "$code" = 404
assert "the miss is JSON" grep -aqi '^content-type: application/json' "$TMP/miss.hdr"
assert "the miss body explains itself" test "$(j '.error' <"$TMP/miss.body")" != null
assert "hit and miss counters moved" test "$(api GET "/services/$SVC_ID" | j '.status.virtual_hits')" -ge 1

# ---------------------------------------------------------------------------
# 5b. fault injection in front of the virtual service
# ---------------------------------------------------------------------------
step "fault injection"
FAULT=$(api POST /faults '{"name":"e2e boom","enabled":true,"path_pattern":"/api/products/**","probability":1,"type":"error","status":503,"body":"{\"error\":\"boom\"}","response_headers":{"X-E2E":"fault"}}')
FAULT_ID=$(echo "$FAULT" | j '.id')
assert "fault rule created" test -n "$FAULT_ID" -a "$FAULT_ID" != null
code=$(curl -sS --max-time 10 -D "$TMP/fault.hdr" -o "$TMP/fault.body" -w '%{http_code}' "$SVC/api/products/1")
assert "a matching request now gets the injected 503 (got $code)" test "$code" = 503
assert "...with the rule's body and headers" bash -c "grep -q boom '$TMP/fault.body' && grep -qi '^x-e2e: fault' '$TMP/fault.hdr'"
assert "the rule counts how often it fired" test "$(api GET /faults | j '.[0].fired')" -ge 1
code=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' "$PROBE_URL")
assert "a request the rule does not match is unaffected (got $code)" test "$code" = 200
api DELETE "/faults/$FAULT_ID" >/dev/null
code=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' "$SVC/api/products/1")
assert "after deleting the rule the request is served again (got $code)" test "$code" != 503

# ---------------------------------------------------------------------------
# 6. error amplification
# ---------------------------------------------------------------------------
step "error amplification and replay through the API"
set_latency_none() { # the replay phases measure Mirage, not the recorded latency
  local svc
  svc=$(api GET "/services/$SVC_ID")
  svc=$(printf '%s' "$svc" | sed -E 's/"latency":\{"mode":"[a-z]+"/"latency":{"mode":"none"/')
  printf '%s' "$svc"
}
SVC_JSON=$(set_latency_none)
api PUT "/services/$SVC_ID" "$SVC_JSON" >/dev/null
assert "latency simulation is off for the load phases" test "$(api GET "/services/$SVC_ID" | j '.latency.mode')" = none

REPLAY_CFG="{\"recording_id\":\"$REC_ID\",\"target\":\"$SVC\",\"mode\":\"timeline\",\"speed\":10,\"loop\":true,\"duration\":\"5s\",\"compare_status\":true}"
JOB=$(api POST /replays "{\"name\":\"e2e 10x as recorded\",\"config\":$REPLAY_CFG,\"start\":true}")
JOB1=$(echo "$JOB" | j '.id')
assert "replay job created" test -n "$JOB1" -a "$JOB1" != null
st=$(wait_replay "$JOB1" 60)
assert "replay job completed" test "$st" = completed
S1=$(api GET "/replays/$JOB1" | j '.summary')
sent1=$(echo "$S1" | j '.sent'); ach1=$(echo "$S1" | j '.achieved_rps'); err1=$(echo "$S1" | j '.error_rate'); tgt1=$(echo "$S1" | j '.target_rps')
REC_RPS=$(awk -v n="$EX_COUNT" -v s="$SECS" 'BEGIN { printf "%.2f", n / s }')
note "recorded ~${REC_RPS} rps; job 1 sent $sent1, target ${tgt1} rps, achieved ${ach1} rps, error rate ${err1}"
assert "the job sent a sane number of requests" num_ge "$sent1" 1500
assert "achieved rate is within [5x, 15x] of the recorded rate" between "$ach1" "$(calc "$REC_RPS * 5")" "$(calc "$REC_RPS * 15")"
assert "the job measured latency" num_gt "$(echo "$S1" | j '.latency.count')" 1000
assert "the job reports corrected latency" num_gt "$(echo "$S1" | j '.corrected_latency.count')" 1000
HITS=$(api GET "/services/$SVC_ID" | j '.status.virtual_hits')
MISSES=$(api GET "/services/$SVC_ID" | j '.status.virtual_misses')
assert "replayed requests were virtual hits (misses < 1% of lookups, got $MISSES of $((HITS + MISSES)))" num_lt "$(calc "$MISSES / ($HITS + $MISSES)")" 0.01
assert "the replay series has per-second points" num_ge "$(api GET "/replays/$JOB1/series" | j '.#')" 3
assert "the summary lists endpoints" num_gt "$(echo "$S1" | j '.endpoints#')" 3
assert "no transport failures" test "$(echo "$S1" | j '.failed')" -lt $((sent1 / 50 + 1))

# amplify errors x10 and run the identical job again
SVC_JSON=$(api GET "/services/$SVC_ID" | sed -E 's/"error_amplification":[0-9.]+/"error_amplification":10/')
api PUT "/services/$SVC_ID" "$SVC_JSON" >/dev/null
assert "error_amplification is 10" test "$(api GET "/services/$SVC_ID" | j '.error_amplification')" = 10
JOB2=$(api POST /replays "{\"name\":\"e2e 10x errors\",\"config\":$REPLAY_CFG,\"start\":true}" | j '.id')
st=$(wait_replay "$JOB2" 60)
assert "second replay job completed" test "$st" = completed
S2=$(api GET "/replays/$JOB2" | j '.summary')
err2=$(echo "$S2" | j '.error_rate')
note "error rate: recorded $(awk -v e="$ERR_COUNT" -v n="$EX_COUNT" 'BEGIN{printf "%.3f", e/n}'), virtual x1 ${err1}, virtual x10 ${err2}"
assert "virtual error rate at x1 is near the recorded rate" between "$err1" 0.01 0.2
assert "x10 raises the virtual error rate at least 3x" num_ge "$err2" "$(calc "$err1 * 3")"
assert "...to at least 15% of requests" num_ge "$err2" 0.15
assert "injected errors are counted on the service" test "$(api GET "/services/$SVC_ID" | j '.status.virtual_injected')" -gt 0

# ---------------------------------------------------------------------------
# 7. the replay CLI
# ---------------------------------------------------------------------------
step "mirage replay (CLI)"
set +e
"$BIN/mirage" replay --data "$DB" --recording e2e --target "$SVC" --mode max --concurrency 8 --loop --max-requests 600 \
  --json "$TMP/cli.json" --max-error-rate 1 --max-p99 30s --quiet >"$TMP/cli.out" 2>"$TMP/cli.err"
rc=$?
set -e
assert "replay CLI exits 0 within generous thresholds" test "$rc" = 0
assert "the CLI summary was written as JSON" json_valid "$TMP/cli.json"
assert "the CLI sent exactly 600 requests" test "$(j '.sent' <"$TMP/cli.json")" = 600
assert "the human summary is on stdout" grep -q 'Throughput' "$TMP/cli.out"

set +e
"$BIN/mirage" replay --data "$DB" --recording e2e --target "$SVC" --mode max --concurrency 8 --max-requests 300 \
  --max-error-rate 0.0001 --quiet >"$TMP/cli2.out" 2>"$TMP/cli2.err"
rc=$?
set -e
assert "replay CLI exits 1 when --max-error-rate is exceeded" test "$rc" = 1
assert "...and says which threshold" grep -q 'error rate' "$TMP/cli2.err"

set +e
"$BIN/mirage" replay --data "$DB" --recording e2e --target "$SVC" --mode max --concurrency 8 --max-requests 300 \
  --max-p99 1ns --quiet >"$TMP/cli3.out" 2>"$TMP/cli3.err"
rc=$?
set -e
assert "replay CLI exits 1 when --max-p99 is exceeded" test "$rc" = 1

# A target that is down measures nothing: that must fail even without thresholds.
set +e
"$BIN/mirage" replay --har "$TMP/e2e.har" --target "http://127.0.0.1:$(free_port)" --speed 1000 --max-requests 5 \
  --quiet >"$TMP/cli3b.out" 2>"$TMP/cli3b.err"
rc=$?
set -e
assert "replay CLI exits 1 when no request gets a response (target down, no thresholds)" test "$rc" = 1
assert "...and says every request failed" grep -q 'every request failed' "$TMP/cli3b.err"

set +e
"$BIN/mirage" replay --har "$TMP/e2e.har" --target "$SVC" --speed 20 --quiet --json "$TMP/cli-har.json" >"$TMP/cli4.out" 2>"$TMP/cli4.err"
rc=$?
set -e
assert "replay CLI replays a HAR file (exit $rc)" test "$rc" = 0
assert "...sending every exchange of it" test "$(j '.sent' <"$TMP/cli-har.json")" = "$EX_COUNT"

set +e
"$BIN/mirage" replay --data "$DB" --recording e2e --quiet >/dev/null 2>"$TMP/cli5.err"
rc=$?
set -e
assert "replay CLI exits 2 without --target" test "$rc" = 2
set +e
"$BIN/mirage" frobnicate >/dev/null 2>&1
rc=$?
set -e
assert "an unknown subcommand exits 2" test "$rc" = 2
assert "mirage version works" bash -c "'$BIN/mirage' version | grep -q '^mirage '"

# ---------------------------------------------------------------------------
# 8. HAR round trip, anonymize, recordings
# ---------------------------------------------------------------------------
step "export, import, anonymize"
code=$(curl -sS --max-time 60 -o "$TMP/imported.json" -w '%{http_code}' -X POST --data-binary "@$TMP/e2e.har" \
  -H 'Content-Type: application/json' "$ADMIN/api/v1/recordings/import?name=e2e-roundtrip")
assert "API import of the exported HAR returned 201" test "$code" = 201
assert "the imported recording has the same number of exchanges" test "$(j '.exchange_count' <"$TMP/imported.json")" = "$EX_COUNT"

# The probe request in step 5 was recorded after the first count was taken.
EX_NOW=$(api GET "/recordings/$REC_ID" | j '.exchange_count')
"$BIN/mirage" export --data "$DB" --recording e2e --out "$TMP/cli-export.har" >/dev/null 2>&1 || fail "mirage export"
"$BIN/mirage" import --data "$DB" --har "$TMP/cli-export.har" --name e2e-cli-import >"$TMP/import.out" 2>&1 || fail "mirage import"
assert "CLI import reports the exchanges" grep -q "Imported $EX_NOW exchanges" "$TMP/import.out"
"$BIN/mirage" recordings --data "$DB" --json >"$TMP/recs.json" || fail "mirage recordings"
assert "recordings lists e2e, e2e-roundtrip and e2e-cli-import" test "$(j '.#' <"$TMP/recs.json")" = 3
"$BIN/mirage" recordings --data "$DB" >"$TMP/recs.txt" || fail "mirage recordings (table)"
count_of() { awk -v n="$1" '$2 == n { gsub(",", "", $4); print $4 }' "$TMP/recs.txt"; }
assert "the table lists every recording with its exchange count" test "$(count_of e2e)/$(count_of e2e-roundtrip)/$(count_of e2e-cli-import)" = "$EX_NOW/$EX_COUNT/$EX_NOW"

"$BIN/mirage" anonymize --in "$TMP/e2e.har" --out "$TMP/anon.har" >"$TMP/anon.out" 2>"$TMP/anon.err" || fail "mirage anonymize (file)"
assert "anonymize wrote a valid HAR with every entry" test "$(j '.log.entries#' <"$TMP/anon.har")" = "$EX_COUNT"
assert "anonymize prints a report" grep -q "Anonymized $EX_COUNT exchanges" "$TMP/anon.out"
"$BIN/mirage" anonymize --data "$DB" --recording e2e --name e2e-anon-copy >"$TMP/anon2.out" 2>&1 || fail "mirage anonymize (stored)"
assert "anonymize created a stored copy" test "$(api GET /recordings | j '.#')" = 4

# ---------------------------------------------------------------------------
# 9. metrics, SSE, UI
# ---------------------------------------------------------------------------
step "/metrics, /api/v1/stream, UI"
curl -sSf --max-time 10 "$ADMIN/metrics" >"$TMP/metrics.txt" || fail "/metrics"
assert "/metrics has the request counter for the service" grep -q "mirage_requests_total{.*id=\"$SVC_ID\"" "$TMP/metrics.txt"
assert "/metrics has latency summaries" grep -q 'mirage_request_duration_seconds' "$TMP/metrics.txt"
assert "/metrics has corrected latency for replay jobs" grep -q 'mirage_corrected_request_duration_seconds' "$TMP/metrics.txt"

curl -sS -N --max-time 4 "$ADMIN/api/v1/stream" >"$TMP/sse.txt" 2>/dev/null || true
assert "SSE sends a services event on connect" grep -q '^event: services' "$TMP/sse.txt"
assert "SSE sends stats events every second" test "$(grep -c '^event: stats' "$TMP/sse.txt")" -ge 2
grep '^data: ' "$TMP/sse.txt" | head -1 | sed 's/^data: //' >"$TMP/sse1.json" || true
assert "SSE data lines are JSON" json_valid "$TMP/sse1.json"

curl -sS -D "$TMP/ui.hdr" -o "$TMP/ui.html" "$ADMIN/" || fail "GET /"
assert "/ serves the UI (HTML)" bash -c "grep -qi '^content-type: text/html' '$TMP/ui.hdr' && grep -qi '<html' '$TMP/ui.html'"
assert "the UI script is served" test "$(curl -sS -o /dev/null -w '%{http_code}' "$ADMIN/assets/app.js")" = 200
assert "the UI stylesheet is served" test "$(curl -sS -o /dev/null -w '%{http_code}' "$ADMIN/assets/app.css")" = 200
assert "the health endpoint reports a version" test "$(api GET /health | j '.status')" = ok
assert "the info endpoint lists detectors" num_gt "$(api GET /info | j '.detectors#')" 5

# ---------------------------------------------------------------------------
# 9b. hybrid mode and TLS, with the upstream back
# ---------------------------------------------------------------------------
step "hybrid mode and TLS"
"$BIN/demo-api" -addr "127.0.0.1:$DEMO_PORT" -latency 2ms -error-rate 0 -seed 7 >"$TMP/demo-api2.log" 2>&1 &
DEMO2_PID=$!
PIDS+=("$DEMO2_PID")
wait_for "demo-api (second start)" 10 curl -sf "$DEMO/health"
SVC_JSON=$(api GET "/services/$SVC_ID" | sed -E 's/"error_amplification":[0-9.]+/"error_amplification":1/')
api PUT "/services/$SVC_ID" "$SVC_JSON" >/dev/null
api POST "/services/$SVC_ID/mode" "{\"mode\":\"hybrid\",\"recording_id\":\"$REC_ID\"}" >/dev/null
code=$(curl -sS --max-time 10 -D "$TMP/hyb1.hdr" -o /dev/null -w '%{http_code}' "$PROBE_URL")
assert "hybrid answers a recorded request from the recording (got $code)" test "$code" = 200
assert "...and says so" grep -qi '^x-mirage-match: \(exact\|path\|template\)' "$TMP/hyb1.hdr"
NEW_URL="$SVC/api/products/7?e2e=hybrid-miss-$RANDOM"
code=$(curl -sS --max-time 10 -D "$TMP/hyb2.hdr" -o "$TMP/hyb2.body" -w '%{http_code}' "$NEW_URL")
assert "an unrecorded request is proxied to the real upstream (got $code)" test "$code" = 200
assert "...which is not a virtual match" bash -c "! grep -qi '^x-mirage-match: \(exact\|path\|template\)' '$TMP/hyb2.hdr'"
sleep 1.2
EX_HYB=$(api GET "/recordings/$REC_ID" | j '.exchange_count')
assert "the miss was recorded (recording grew from $EX_NOW to $EX_HYB)" num_gt "$EX_HYB" "$EX_NOW"
code=$(curl -sS --max-time 10 -D "$TMP/hyb3.hdr" -o /dev/null -w '%{http_code}' "$NEW_URL")
assert "asked again, the new exchange is served from the index (got $code)" test "$code" = 200
assert "...as a virtual match" grep -qi '^x-mirage-match: \(exact\|path\|template\)' "$TMP/hyb3.hdr"
api POST "/services/$SVC_ID/mode" "{\"mode\":\"virtualize\",\"recording_id\":\"$REC_ID\"}" >/dev/null
BEFORE=$(api GET "/recordings/$REC_ID" | j '.exchange_count')

TLS_PORT=$(free_port)
TLS_SVC=$(api POST /services "{\"name\":\"e2e-tls\",\"listen_addr\":\"127.0.0.1:$TLS_PORT\",\"upstream\":\"$DEMO\",\"mode\":\"passthrough\",\"enabled\":true,\"tls\":{\"self_signed\":true}}")
TLS_ID=$(echo "$TLS_SVC" | j '.id')
wait_for "the TLS listener" 10 curl -sk "https://127.0.0.1:$TLS_PORT/health"
assert "a self-signed TLS service proxies to the upstream" test "$(curl -sk --max-time 5 -o /dev/null -w '%{http_code}' "https://127.0.0.1:$TLS_PORT/health")" = 200
assert "plain HTTP to the TLS port is refused" test "$(curl -s --max-time 5 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$TLS_PORT/health")" != 200
api DELETE "/services/$TLS_ID" >/dev/null

# A response larger than max_body_bytes is recorded truncated; virtualize must
# not serve the cut-off body as a complete 200.
TRUNC_PORT=$(free_port)
TRUNC_REC=$(api POST /recordings '{"name":"e2e-truncated"}' | j '.id')
TRUNC_ID=$(api POST /services "{\"name\":\"e2e-trunc\",\"listen_addr\":\"127.0.0.1:$TRUNC_PORT\",\"upstream\":\"$DEMO\",\"mode\":\"record\",\"recording_id\":\"$TRUNC_REC\",\"enabled\":true,\"max_body_bytes\":200,\"latency\":{\"mode\":\"none\"}}" | j '.id')
TRUNC_URL="http://127.0.0.1:$TRUNC_PORT/api/products"
wait_for "the truncating service" 10 curl -sf -o /dev/null "$TRUNC_URL"
FULL=$(curl -sS --max-time 10 "$TRUNC_URL" | wc -c | tr -d ' ')
assert "traffic itself is never truncated ($FULL bytes, cap 200)" num_gt "$FULL" 200
trunc_counted() { num_ge "$(api GET "/services/$TRUNC_ID" | j '.status.capture_truncated')" 1; }
wait_for "a truncated capture to be counted" 10 trunc_counted
TRUNC_WARN=$(api GET "/services/$TRUNC_ID" | j '.status.warning')
assert "record mode warns about max_body_bytes ($TRUNC_WARN)" grep -q 'max_body_bytes' <<<"$TRUNC_WARN"
api POST "/services/$TRUNC_ID/mode" "{\"mode\":\"virtualize\",\"recording_id\":\"$TRUNC_REC\"}" >/dev/null
code=$(curl -sS --max-time 10 -D "$TMP/trunc.hdr" -o "$TMP/trunc.body" -w '%{http_code}' "$TRUNC_URL")
assert "a truncated recording is answered 502, not a short 200 (got $code)" test "$code" = 502
assert "...with X-Mirage-Error: truncated" grep -qi '^x-mirage-error: truncated' "$TMP/trunc.hdr"
assert "...and a body saying how much was kept ($(j '.kept_bytes' <"$TMP/trunc.body") of $(j '.recorded_bytes' <"$TMP/trunc.body"))" \
  num_gt "$(j '.recorded_bytes' <"$TMP/trunc.body")" "$(j '.kept_bytes' <"$TMP/trunc.body")"
assert "the service counts the truncated match" num_ge "$(api GET "/services/$TRUNC_ID" | j '.status.virtual_truncated')" 1
# The fix the warning asks for: a bigger cap, and hybrid records the request again.
TRUNC_SVC=$(api GET "/services/$TRUNC_ID" | sed -E 's/"max_body_bytes":[0-9]+/"max_body_bytes":1048576/; s/"mode":"virtualize"/"mode":"hybrid"/')
api PUT "/services/$TRUNC_ID" "$TRUNC_SVC" >/dev/null
code=$(curl -sS --max-time 10 -o "$TMP/trunc2.body" -w '%{http_code}' "$TRUNC_URL")
assert "hybrid proxies a truncated-only match to the upstream (got $code, $(wc -c <"$TMP/trunc2.body" | tr -d ' ') bytes)" \
  test "$code" = 200 -a "$(wc -c <"$TMP/trunc2.body" | tr -d ' ')" = "$FULL"
sleep 1
api POST "/services/$TRUNC_ID/mode" "{\"mode\":\"virtualize\",\"recording_id\":\"$TRUNC_REC\"}" >/dev/null
code=$(curl -sS --max-time 10 -o "$TMP/trunc3.body" -w '%{http_code}' "$TRUNC_URL")
assert "virtualize now serves the complete recording (got $code, $(wc -c <"$TMP/trunc3.body" | tr -d ' ') of $FULL bytes)" \
  test "$code" = 200 -a "$(wc -c <"$TMP/trunc3.body" | tr -d ' ')" = "$FULL"
api DELETE "/services/$TRUNC_ID" >/dev/null
kill "$DEMO2_PID" 2>/dev/null || true

# ---------------------------------------------------------------------------
# 10. shutdown, persistence, restart
# ---------------------------------------------------------------------------
step "graceful shutdown and restart"
stop_mirage
assert "mirage exits 0 on SIGINT" test "$STOP_RC" = 0
assert "the log says shutdown completed" grep -q 'shutdown complete' "$TMP/mirage.log"
assert "the service port is released" bash -c "! (exec 3<>/dev/tcp/127.0.0.1/$SVC_PORT) 2>/dev/null"
AFTER=$("$BIN/mirage" recordings --data "$DB" | awk '$2 == "e2e" { gsub(",", "", $4); print $4 }')
assert "no exchange was lost at shutdown" test "$AFTER" = "$BEFORE"

# Restart without the config file: the stored service (virtualize) comes back.
start_mirage mirage2.log --admin "127.0.0.1:$ADMIN_PORT" --data "$DB"
SV=$(api GET "/services/$SVC_ID")
assert "the stored service is running again, virtualized" test "$(echo "$SV" | j '.status.running')/$(echo "$SV" | j '.status.mode')" = "true/virtualize"
code=$(curl -sS --max-time 10 -o "$TMP/virt2.body" -w '%{http_code}' "$PROBE_URL")
assert "...and answers the recorded request" test "$code" = 200
stop_mirage
assert "second shutdown is clean" test "$STOP_RC" = 0

# ---------------------------------------------------------------------------
# 11. admin token
# ---------------------------------------------------------------------------
step "admin token"
ADMIN_PORT=$(free_port)
ADMIN="http://127.0.0.1:$ADMIN_PORT"
start_mirage mirage3.log --admin "127.0.0.1:$ADMIN_PORT" --data "$TMP/data/auth.db" --admin-token s3cret
assert "health is open without a token" test "$(api_status GET /health)" = 200
assert "services need the token" test "$(api_status GET /services)" = 401
assert "a bearer token is accepted" test "$(curl -sS -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer s3cret' "$ADMIN/api/v1/services")" = 200
assert "access_token works for EventSource and downloads" test "$(curl -sS -o /dev/null -w '%{http_code}' "$ADMIN/api/v1/services?access_token=s3cret")" = 200
assert "a wrong token is refused" test "$(curl -sS -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer nope' "$ADMIN/api/v1/services")" = 401
stop_mirage
assert "clean shutdown with a token" test "$STOP_RC" = 0

printf '\n%se2e: PASSED%s (%d checks, %ds)\n' "$C_OK" "$C_OFF" "$CHECKS" "$((SECONDS - STARTED))"
