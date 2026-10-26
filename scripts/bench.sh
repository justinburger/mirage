#!/usr/bin/env bash
# Throughput and added-latency benchmarks of the running product: real binaries
# over loopback, a trivial upstream, and examples/benchtool as the load
# generator. Everything runs on one machine, so the load generator, the
# upstream and Mirage share its cores; the numbers are what that setup
# delivers, not what Mirage alone could do.
#
#   scripts/bench.sh                    10 s per measurement (about 5 minutes)
#   BENCH_DURATION=3s scripts/bench.sh  quick run
#   BENCH_OUT=file.md scripts/bench.sh  also write the result tables to a file
#
# Measurements per target (the upstream directly, then Mirage in each mode):
#   throughput   64 connections, closed loop, as fast as the target answers
#   latency      1 connection, closed loop: the latency of a lightly loaded path
#   paced        8 connections at 5,000 req/s: latency and capture at a rate
#                the capture pipeline can sustain
# and `mirage replay --mode max` against the upstream and a virtual service.
#
# Requires: go, curl, jq.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
DUR="${BENCH_DURATION:-10s}"
CONNS="${BENCH_CONNS:-64}"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/mirage-bench.XXXXXX")"
PIDS=()
cleanup() {
  local pid
  for pid in ${PIDS[@]+"${PIDS[@]}"}; do kill "$pid" 2>/dev/null || true; done
  for pid in ${PIDS[@]+"${PIDS[@]}"}; do wait "$pid" 2>/dev/null || true; done
  [ "${BENCH_KEEP:-0}" = 1 ] && echo "bench: temp dir kept at $TMP" >&2 || rm -rf "$TMP"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

step() { printf '\n== %s\n' "$*" >&2; }

# j PATH < json: a dotted path through the JSON on stdin
j() { jq -r ".$(printf '%s' "$1" | sed -E 's/\.([A-Za-z0-9_-]+)/["\1"]/g; s/^([A-Za-z0-9_-]+)/["\1"]/')"; }

port_in_use() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
free_port() { local p; while :; do p=$((20000 + RANDOM % 20000)); port_in_use "$p" || { echo "$p"; return; }; done; }
api() { curl -sS --max-time 30 -X "$1" ${3:+-H 'Content-Type: application/json' --data-binary "$3"} "$ADMIN/api/v1$2"; }

command -v jq >/dev/null || { echo "bench: jq is required" >&2; exit 1; }
step "build"
mkdir -p bin
go build -o bin/mirage ./cmd/mirage
go build -o bin/benchtool ./examples/benchtool

UP=$(free_port); ADMIN_PORT=$(free_port); P_PASS=$(free_port); P_REC=$(free_port); P_ANON=$(free_port); P_SEED=$(free_port)
ADMIN="http://127.0.0.1:$ADMIN_PORT"
UPSTREAM="http://127.0.0.1:$UP"
DB="$TMP/bench.db"

./bin/benchtool upstream -addr "127.0.0.1:$UP" 2>"$TMP/upstream.log" &
PIDS+=($!)
cat >"$TMP/mirage.yaml" <<YAML
recordings:
  - name: bench
  - name: bench-anon
  - name: bench-virt
services:
  - {name: passthrough, listen_addr: "127.0.0.1:$P_PASS", upstream: "$UPSTREAM", mode: passthrough, enabled: true}
  - {name: record, listen_addr: "127.0.0.1:$P_REC", upstream: "$UPSTREAM", mode: record, enabled: true, recording: bench, anonymize_capture: false}
  - {name: record-anon, listen_addr: "127.0.0.1:$P_ANON", upstream: "$UPSTREAM", mode: record, enabled: true, recording: bench-anon, anonymize_capture: true}
  - {name: seed, listen_addr: "127.0.0.1:$P_SEED", upstream: "$UPSTREAM", mode: record, enabled: true, recording: bench-virt}
YAML
./bin/mirage serve --admin "127.0.0.1:$ADMIN_PORT" --data "$DB" --config "$TMP/mirage.yaml" --log-level warn >"$TMP/mirage.log" 2>&1 &
PIDS+=($!)
for _ in $(seq 1 100); do curl -sf "$ADMIN/api/v1/health" >/dev/null 2>&1 && break; sleep 0.1; done
curl -sf "$ADMIN/api/v1/health" >/dev/null || { cat "$TMP/mirage.log"; exit 1; }

svc_id() { api GET /services | jq -r ".[] | select(.name==\"$1\") | .id"; }
LOAD="./bin/benchtool load -json -d $DUR"
RES="$TMP/results.jsonl"; : >"$RES"

# measure LABEL URL [service-name]: throughput, latency and paced runs
measure() {
  local label=$1 url=$2 svc=${3-}
  local id='' before after
  [ -n "$svc" ] && id=$(svc_id "$svc")
  step "$label"
  for kind in throughput latency paced; do
    case $kind in
      throughput) args="-c $CONNS -paths 100" ;;
      latency)    args="-c 1 -paths 100" ;;
      paced)      args="-c 8 -rate 5000 -paths 100" ;;
    esac
    [ -n "$id" ] && before=$(api GET "/services/$id")
    out=$($LOAD $args -url "$url")
    cap='{}'
    if [ -n "$id" ]; then
      sleep 2.5 # let the capture queue drain
      after=$(api GET "/services/$id")
      cap=$(printf '{"captured":%s,"dropped":%s,"skipped":%s}' \
        "$(( $(echo "$after" | j status.captured) - $(echo "$before" | j status.captured) ))" \
        "$(( $(echo "$after" | j status.capture_dropped) - $(echo "$before" | j status.capture_dropped) ))" \
        "$(( $(echo "$after" | j status.capture_skipped) - $(echo "$before" | j status.capture_skipped) ))")
    fi
    printf '%s\n' "$out" | jq -c --arg label "$label" --arg kind "$kind" --argjson cap "$cap" '. + {label: $label, kind: $kind, capture: $cap}' >>"$RES"
    printf '%s\n' "$out" | jq -r '"  \(.connections) conns: \(.rps|floor) req/s  p50 \(.p50_us|floor)us  p99 \(.p99_us|floor)us"' >&2
  done
}

measure "upstream (direct)" "$UPSTREAM"
measure "passthrough" "http://127.0.0.1:$P_PASS" passthrough
measure "record" "http://127.0.0.1:$P_REC" record
measure "record + anonymize" "http://127.0.0.1:$P_ANON" record-anon

# fill the virtual recording through the seed service, then serve it
step "seed the virtual recording"
./bin/benchtool load -json -d 2s -warmup 0s -c 4 -rate 800 -paths 100 -url "http://127.0.0.1:$P_SEED" >/dev/null
sleep 2
SEED=$(svc_id seed); REC=$(api GET "/services/$SEED" | j recording_id)
api POST "/services/$SEED/mode" "{\"mode\":\"virtualize\",\"recording_id\":\"$REC\"}" >/dev/null
SEED_JSON=$(api GET "/services/$SEED" | sed -E 's/"latency":\{"mode":"[a-z]+"/"latency":{"mode":"none"/')
api PUT "/services/$SEED" "$SEED_JSON" >/dev/null
echo "  recording $REC holds $(api GET "/recordings/$REC" | j exchange_count) exchanges" >&2
measure "virtualize" "http://127.0.0.1:$P_SEED" ""

step "mirage replay --mode max"
replay() { # replay LABEL TARGET
  ./bin/mirage replay --data "$DB" --recording bench-virt --target "$2" --mode max --concurrency 256 --loop --duration "$DUR" --quiet --json "$TMP/replay.json" >/dev/null
  jq -c --arg label "$1" '{label: $label, kind: "replay", rps: .achieved_rps, requests: .sent, errors: .errors, p50_us: (.latency.p50*1000), p99_us: (.latency.p99*1000)}' "$TMP/replay.json" >>"$RES"
  jq -r '"  \(.achieved_rps|floor) req/s, p50 \(.latency.p50*1000|floor)us, p99 \(.latency.p99*1000|floor)us, errors \(.errors)"' "$TMP/replay.json" >&2
}
echo "  against the trivial upstream" >&2; replay "replay -> upstream" "$UPSTREAM"
echo "  against the virtual service" >&2; replay "replay -> virtualize" "http://127.0.0.1:$P_SEED"

# ------------------------------------------------------------ report
report() {
  local f
  echo "### Throughput ($CONNS connections, closed loop)"
  echo
  echo "| Target | req/s | p50 | p99 | errors | captured | dropped |"
  echo "|---|---:|---:|---:|---:|---:|---:|"
  jq -r 'select(.kind=="throughput") | "| \(.label) | \(.rps|floor) | \(.p50_us|floor) us | \(.p99_us|floor) us | \(.errors) | \(if .capture.captured then .capture.captured else "-" end) | \(if .capture.dropped then .capture.dropped else "-" end) |"' "$RES"
  echo
  echo "### Latency on a lightly loaded path (1 connection) and added latency"
  echo
  echo "| Target | p50 | p90 | p99 | added p50 | added p99 |"
  echo "|---|---:|---:|---:|---:|---:|"
  jq -rs 'def delta($x): ($x|round) as $r | (if $r >= 0 then "+" else "-" end) + ($r|fabs|tostring) + " us"; (map(select(.kind=="latency" and .label=="upstream (direct)"))[0]) as $b | .[] | select(.kind=="latency") | "| \(.label) | \(.p50_us|floor) us | \(.p90_us|floor) us | \(.p99_us|floor) us | \(if .label==$b.label then "-" else delta(.p50_us-$b.p50_us) end) | \(if .label==$b.label then "-" else delta(.p99_us-$b.p99_us) end) |"' "$RES"
  echo
  echo "### Paced at 5,000 req/s (8 connections)"
  echo
  echo "| Target | achieved req/s | p50 | p99 | added p50 | captured | dropped |"
  echo "|---|---:|---:|---:|---:|---:|---:|"
  jq -rs 'def delta($x): ($x|round) as $r | (if $r >= 0 then "+" else "-" end) + ($r|fabs|tostring) + " us"; (map(select(.kind=="paced" and .label=="upstream (direct)"))[0]) as $b | .[] | select(.kind=="paced") | "| \(.label) | \(.rps|floor) | \(.p50_us|floor) us | \(.p99_us|floor) us | \(if .label==$b.label then "-" else delta(.p50_us-$b.p50_us) end) | \(if .capture.captured then .capture.captured else "-" end) | \(if .capture.dropped then .capture.dropped else "-" end) |"' "$RES"
  echo
  echo "### mirage replay --mode max (256 workers)"
  echo
  echo "| Target | req/s | p50 | p99 | errors |"
  echo "|---|---:|---:|---:|---:|"
  jq -r 'select(.kind=="replay") | "| \(.label) | \(.rps|floor) | \(.p50_us|floor) us | \(.p99_us|floor) us | \(.errors) |"' "$RES"
}
echo >&2
report | tee "${BENCH_OUT:-/dev/null}"
