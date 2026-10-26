# Benchmarks

Measured on 2026-10-25 on the development laptop: Apple M3 Pro (12 cores: 6
performance, 6 efficiency), 18 GB, macOS 14.1, Go 1.27.3, loopback networking.
The laptop was in ordinary use (browser, video decoding in the background),
so treat the numbers as indicative and repeat runs vary by a few percent.

Everything below runs on **one machine**: the load generator, the trivial
upstream and Mirage share the same 12 cores. A throughput figure is therefore
what that whole setup delivers, not what Mirage alone could do, and the
fastest rows are limited by the load generator and the upstream, not by
Mirage (see "Reading the numbers").

## How to reproduce

```sh
scripts/bench.sh                       # about 5 minutes; BENCH_DURATION=3s for a quick run
BENCH_OUT=docs/bench-run.md scripts/bench.sh
```

The script builds `bin/mirage` and `bin/benchtool` ([`examples/benchtool`](../examples/benchtool/main.go))
and runs, against a temporary data directory and free ports:

```sh
# the trivial upstream: every GET answers the same 256-byte JSON document
bin/benchtool upstream -addr 127.0.0.1:$UP &

# Mirage with four services in front of it (generated config, see below)
bin/mirage serve --admin 127.0.0.1:$ADMIN --data $TMP/bench.db --config $TMP/mirage.yaml --log-level warn &

# for each target (the upstream itself, then each Mirage service):
bin/benchtool load -json -d 10s -c 64 -paths 100 -url $TARGET              # throughput, closed loop
bin/benchtool load -json -d 10s -c 1  -paths 100 -url $TARGET              # latency of a lightly loaded path
bin/benchtool load -json -d 10s -c 8 -rate 5000 -paths 100 -url $TARGET    # paced at 5,000 req/s

# the recording for the virtual service is filled by the "seed" service, then it is switched
bin/benchtool load -json -d 2s -warmup 0s -c 4 -rate 800 -paths 100 -url http://127.0.0.1:$P_SEED
curl -X POST $ADMIN/api/v1/services/$SEED/mode -d '{"mode":"virtualize","recording_id":"..."}'   # only mode and recording change
curl -X PUT  $ADMIN/api/v1/services/$SEED -d "$SERVICE_JSON"   # the same service with "latency":{"mode":"none",...}

# replay as the load generator
bin/mirage replay --data $TMP/bench.db --recording bench-virt --target $TARGET \
    --mode max --concurrency 256 --loop --duration 10s --quiet --json replay.json
```

`benchtool load` warms up for 2 s, then measures for the given duration and
reports requests per second and latency percentiles. `-paths 100` cycles
through `/item/1` to `/item/100`, so a recording made with it has 100
distinct request keys. The generated Mirage config:

```yaml
recordings: [{name: bench}, {name: bench-anon}, {name: bench-virt}]
services:
  - {name: passthrough, mode: passthrough, upstream: <upstream>, listen_addr: 127.0.0.1:<port>, enabled: true}
  - {name: record,      mode: record,      recording: bench,      anonymize_capture: false, ...}
  - {name: record-anon, mode: record,      recording: bench-anon, anonymize_capture: true,  ...}   # built-in default profile
  - {name: seed,        mode: record,      recording: bench-virt, ...}   # later switched to virtualize (mode call), then latency none (PUT)
```

## Results

### Throughput (64 connections, closed loop)

| Target | req/s | p50 | p99 | errors | captured | dropped |
|---|---:|---:|---:|---:|---:|---:|
| upstream (direct) | 206,759 | 249 us | 1,385 us | 0 | - | - |
| passthrough | 76,624 | 640 us | 4,311 us | 0 | 0 | 0 |
| record | 72,477 | 640 us | 4,904 us | 0 | 361,816 | 505,548 |
| record + anonymize | 71,443 | 647 us | 5,141 us | 0 | 335,332 | 512,252 |
| virtualize | 203,365 | 258 us | 1,327 us | 0 | - | - |

"captured" and "dropped" cover the 2 s warm-up and the 10 s measurement
together, measured after the queue drained. At full speed the capture pipeline
writes about 25,000 exchanges per second to SQLite on top of the 65,536 the
queue holds; what the queue cannot absorb is dropped and
counted (`capture_dropped` in the service status, shown in the UI) and the
request is still served at full speed. See the paced table for a rate the
pipeline sustains.

### Latency on a lightly loaded path (1 connection) and added latency

| Target | p50 | p90 | p99 | added p50 | added p99 |
|---|---:|---:|---:|---:|---:|
| upstream (direct) | 34 us | 40 us | 73 us | - | - |
| passthrough | 73 us | 81 us | 129 us | +39 us | +56 us |
| record | 74 us | 82 us | 165 us | +40 us | +92 us |
| record + anonymize | 75 us | 83 us | 164 us | +41 us | +91 us |
| virtualize | 34 us | 39 us | 50 us | +0 us | -23 us |

### Paced at 5,000 req/s (8 connections)

| Target | achieved req/s | p50 | p99 | added p50 | captured | dropped |
|---|---:|---:|---:|---:|---:|---:|
| upstream (direct) | 4,999 | 57 us | 142 us | - | - | - |
| passthrough | 4,999 | 77 us | 161 us | +19 us | 0 | 0 |
| record | 4,999 | 79 us | 174 us | +21 us | 60,010 | 0 |
| record + anonymize | 4,999 | 79 us | 177 us | +22 us | 60,010 | 0 |
| virtualize | 4,999 | 67 us | 131 us | +9 us | - | - |

Every request was captured (60,010 = 5,000 req/s over the 12 s run) with
nothing dropped.

### `mirage replay --mode max` (256 workers, looping a 100-request recording)

| Target | req/s | p50 | p99 | errors |
|---|---:|---:|---:|---:|
| replay -> upstream (the trivial server) | 164,087 | 989 us | 4,997 us | 0 |
| replay -> virtualize | 149,051 | 1,077 us | 6,569 us | 0 |

## Reading the numbers

- **Added latency.** A request through Mirage costs about 40 microseconds
  more at the median on an otherwise idle path (passthrough and record), and
  about 20 microseconds when 5,000 requests per second keep the connections
  warm. Recording adds about 1 to 2 microseconds at the median over passthrough
  because the request never waits for the capture: bodies are copied into a
  pooled buffer and the rest happens on worker goroutines. Anonymizing at
  capture does not show on the request path.
- **Throughput.** A proxy handles each request on two connections (client to
  Mirage, Mirage to the upstream) with the generator and the upstream on the
  same cores, so passthrough reaches 37 percent of what the generator gets from
  the upstream directly (76,600 against 206,800 requests per second). Recording
  costs another 5 percent.
- **Virtualize.** The 203,000 requests per second equal the upstream's own
  figure: one generator process cannot go faster against anything on this
  machine, so this row is a lower bound for the virtual engine, not its
  ceiling. (A trial with three generators at once gave lower totals, about
  167,000 requests per second against the upstream directly, because all 12
  cores were busy.) The engine itself is measured in-process below: a lookup is 110
  nanoseconds and a complete served response, headers and body, about 350
  nanoseconds without sockets, which is 2.9 million per second for one
  handler.
- **Capture.** The pipeline sustains about 25,000 exchanges per second to disk
  when traffic saturates the machine, and the 5,000 requests per second
  measured in the paced table (the only rate besides the 1-connection run
  that was tried) were all captured. A service that sees more than the
  pipeline can write should set `capture_sample` below 1; otherwise the
  surplus is dropped and counted.
- **Replay.** Replay used as a load generator in max mode delivers about
  164,000 requests per second against a trivial server, with no errors. (A
  start-up burst of 256 simultaneous dials used to make about 120 of 1.8
  million requests fail with connection resets on macOS, whose listen queue is
  128; the replay engine now opens at most 32 connections at a time.)

## Go benchmarks

The in-process numbers, run with `go test -run='^$' -bench=<name> -benchmem -benchtime=1s <package>`
(same machine, same session):

| Benchmark | Result |
|---|---|
| `internal/proxy` `BenchmarkDirect` (client and server, no Mirage) | 168,196 req/s, 5.9 us/op |
| `internal/proxy` `BenchmarkPassthrough` | 84,816 req/s, 11.8 us/op, 153 allocs/op |
| `internal/proxy` `BenchmarkRecord` | 72,092 req/s, 0% dropped |
| `internal/proxy` `BenchmarkRecordAnonymized` | 71,031 req/s, 0% dropped |
| `internal/proxy` `BenchmarkVirtualize` (over sockets) | 170,223 req/s |
| `internal/proxy` `BenchmarkHybridHit` | 178,444 req/s |
| `internal/proxy` `BenchmarkVirtualizeHandler` (no sockets) | 348 ns/op, 2,873,103 req/s, 6 allocs/op |
| `internal/virtual` `BenchmarkLookupExactHit` | 110 ns/op, 0 allocs |
| `internal/virtual` `BenchmarkLookupPathFallback` | 97 ns/op, 0 allocs |
| `internal/virtual` `BenchmarkLookupTemplateFallback` | 158 ns/op, 1 alloc |
| `internal/virtual` `BenchmarkLookupMiss` | 109 ns/op, 0 allocs |
| `internal/virtual` `BenchmarkLookupParallel` | 50 ns/op over 12 threads |
| `internal/virtual` `BenchmarkServe` (into an `httptest.ResponseRecorder`) | 614 ns/op, 9 allocs |
| `internal/virtual` `BenchmarkServeNopWriter` | 298 ns/op, 1 alloc |
| `internal/replay` `BenchmarkRunMax/concurrency=64` | 201,867 req/s against a local server |
| `internal/replay` `BenchmarkRunMax/concurrency=256` | 188,059 req/s |
| `internal/replay` `BenchmarkRunRate` (10,000 req/s target) | 9,883 req/s, max lag 0.31 ms |
| `internal/store` `BenchmarkAppendExchanges/file` | 78,089 exchanges/s (94,030 in memory) |
| `internal/store` `BenchmarkAppendExchangesParallel` | 63,603 exchanges/s |
| `internal/anonymize` `BenchmarkExchangeDefaultProfile` | 13.5 us/exchange, 160 MB/s |
| `internal/anonymize` `BenchmarkExchangeWithRules` | 19.0 us/exchange, 113 MB/s |

The commands are one `go test` line per package, for example

```sh
go test -run='^$' -bench='^(BenchmarkDirect|BenchmarkPassthrough|BenchmarkRecord|BenchmarkRecordAnonymized|BenchmarkVirtualize|BenchmarkHybridHit|BenchmarkVirtualizeHandler)$' -benchmem -benchtime=1s ./internal/proxy
go test -run='^$' -bench='^(BenchmarkLookup.*|BenchmarkServe.*)$' -benchmem -benchtime=1s ./internal/virtual
go test -run='^$' -bench='^(BenchmarkRunMax|BenchmarkRunRate)$' -benchmem -benchtime=1s ./internal/replay
go test -run='^$' -bench='^(BenchmarkAppendExchanges.*)$' -benchmem -benchtime=1s ./internal/store
go test -run='^$' -bench='^(BenchmarkExchangeDefaultProfile|BenchmarkExchangeWithRules)$' -benchmem -benchtime=1s ./internal/anonymize
```

`make bench` runs all of them (`BENCH`, `PKGS` and `BENCHTIME` select which).

## Caveats

- One machine, loopback only: no network latency, and the cores are shared by
  the generator, the upstream and Mirage. A dedicated load-generation host
  would raise every throughput figure and lower the latency figures.
- The upstream is trivial (one fixed 256-byte body, no latency). Real
  services spend their time elsewhere, so the *added* latency is the portable
  number, the absolute throughput is not.
- Bodies are small. Large bodies shift the cost to copying and, when recording,
  to the SQLite write volume (bodies over 1 KiB are gzipped in the store).
