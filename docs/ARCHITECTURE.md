# Mirage architecture

Mirage is a single Go binary that sits between clients and an HTTP API. It
can pass traffic through, record it, serve it back without the real API
(service virtualization), inject faults, anonymize what it captured, and
replay that traffic as a load test at a multiple of its original rate or
error mix. A built-in web UI manages and monitors all of it.

```
                       ┌──────────────────────── mirage ───────────────────────────┐
 clients ──HTTP──▶ :9001 service listener                                          │
                       │  metrics ▸ faults ▸ mode:                                  │
                       │    passthrough ─────────────▶ reverse proxy ──────────────┼──▶ upstream API
                       │    record ──────────────────▶ reverse proxy + capture ────┼──▶
                       │    virtualize ─▶ virtual engine (recorded responses)       │
                       │    hybrid ─────▶ virtual engine, miss ▸ proxy + capture ──┼──▶
                       │                                                            │
                       │  capture queue ▸ decode ▸ anonymize ▸ batch ▸ SQLite      │
                       │                                                            │
                       │  replay engine ── schedule (timeline × speed, rate/ramp,  ─┼──▶ target (staging,
                       │                   max) × amplify × error amplification     │     or a mirage
                       │                                                            │     virtual service)
 browser ──▶ :8484 admin: web UI ▸ JSON API ▸ SSE live metrics ▸ /metrics         │
                       └────────────────────────────────────────────────────────────┘
```

## Packages

| Package | Responsibility | Imports (internal) |
|---|---|---|
| `internal/model` | Domain types, defaults, validation (shared contract) | — |
| `internal/store` | SQLite persistence (modernc.org/sqlite, pure Go) | model |
| `internal/metrics` | Histograms, collectors, 1 s series, registry, Prometheus | model |
| `internal/anonymize` | Deterministic anonymization engine and PII detectors | model |
| `internal/fault` | Fault rules: latency, errors, aborts, hangs, throttling | model |
| `internal/pathnorm` | Path → endpoint template (`/users/{id}`) | — |
| `internal/har` | HAR 1.2 import/export | model |
| `internal/virtual` | Recorded-response matching, latency simulation, error amplification | model, store, pathnorm |
| `internal/proxy` | Service listeners, reverse proxy, capture pipeline | model, store, metrics, fault, anonymize, pathnorm |
| `internal/replay` | Load generator and replay job manager | model, store, metrics, anonymize, pathnorm |
| `internal/ops` | Anonymized copies, HAR import/export over the store | model, store, anonymize, har, pathnorm, fault |
| `internal/api` | REST API, SSE, auth, static UI | model, store, metrics, fault, anonymize, har, pathnorm, ops |
| `internal/config` | YAML/JSON config file, applied at start-up | model, store, ops |
| `internal/app` | Wiring: builds every component, start-up and shutdown | everything |
| `cmd/mirage` | CLI: `serve`, `replay`, `anonymize`, `import`, `export`, `version` | app, … |
| `web` | Embedded UI (vanilla JS ES modules, no build step) | — |

Rules:

- `proxy` does not import `virtual`; it talks to it through
  `proxy.Virtualizer`, which `*virtual.Engine` implements. `api` does not
  import `proxy` or `replay`; it uses `api.ServiceController` and
  `api.ReplayController`. `internal/app` connects them. This keeps every
  package testable on its own with small fakes.
- `model` is the shared contract. Other packages may add exported API but
  never change `model` or another package's exported signatures without
  coordinating (report it).
- No global mutable state except `pathnorm.Default` and `version`.

## Conventions

- Go 1.26+, standard library first. External dependencies: only
  `modernc.org/sqlite` and `gopkg.in/yaml.v3`. The web UI has no
  dependencies and no build step.
- Errors wrap the sentinels in `model` (`ErrNotFound`, `ErrConflict`,
  `ErrValidation`/`*ValidationError`) so the API can map them to status
  codes. Use `fmt.Errorf("context: %w", err)`.
- Logging with `log/slog` passed in through `Deps` (never the global logger
  inside packages; tolerate a nil logger by using a discard logger).
- Every blocking operation takes a `context.Context`. Background goroutines
  stop on `Shutdown`/`Close` and are waited for; no goroutine leaks (tests
  check with `-race` and by waiting on shutdown).
- Tests: table-driven, `go test -race`, no sleeps longer than needed, no
  network beyond `httptest` and `127.0.0.1:0` listeners. Benchmarks for hot
  paths (`BenchmarkXxx`, `b.ReportAllocs()`).
- `gofmt` and `go vet` clean.
- Defaults and validation happen at the edges: the API, `config.Apply` and
  `replay.Manager.Create` call `WithDefaults` then `Validate` before every
  store write; the store does neither. Every consumer (`proxy.Apply`,
  `virtual.OptionsFromService`, `fault.SetRules`, `anonymize.New`,
  `replay.Run`) calls `WithDefaults` again; all `WithDefaults` are
  idempotent.
- A `ctx` passed to a manager method bounds that call only. Long-lived work
  (listeners, capture workers, replay jobs, the SSE broadcaster) runs on the
  owning component's root context, cancelled by its `Shutdown`.
- Exchanges are owned by one goroutine at a time. The store returns fresh
  objects and retains nothing after `AppendExchanges`; the virtual index
  treats indexed exchanges as immutable.

## Wiring (`internal/app`)

Start: open store → `config.Apply` → `ops.LoadFaults` → metrics registry
`Run` → `replay.Manager.Recover` → `proxy.Manager.Apply` for every enabled
service → `api.Server.Run` + admin HTTP server.

Shutdown (reverse): admin HTTP server → `replay.Manager.Shutdown` →
`proxy.Manager.Shutdown` (drains captures) → stop the registry → `store.Close`.

The `proxy.VirtualizerFactory` adapter returns an untyped nil interface on
error (typed-nil trap).

## Performance principles

- The request path never waits on disk, anonymization or metrics fan-out.
  Capture is a bounded queue; when full, captures are dropped and counted,
  traffic is never slowed.
- Hot-path counters are atomics; histograms are lock-free bucket arrays;
  per-endpoint maps are read-mostly.
- Pooled buffers (`sync.Pool`) for body capture and draining.
- Tuned `http.Transport`s (large idle pools, HTTP/2 when TLS, compression
  passthrough).
- Replay is an open-model scheduler (arrivals do not wait for responses)
  and reports both service time and coordinated-omission-corrected latency.

## Key behaviours

- **Determinism of anonymization.** `hash` and `fake` are keyed by an HMAC
  of the profile secret (and a salt for replay clones), so the same
  customer ID or email maps to the same token in every request and response.
  This keeps cache hit ratios, hot keys and request correlation realistic in
  replays.
- **Error amplification** exists in two places. In virtualize/hybrid mode
  (`Service.ErrorAmplification`, `ErrorFloor`) it raises the error rate the
  virtual service returns, per endpoint, using real recorded error responses.
  In replay (`ReplayConfig.ErrorAmplification`) it oversamples the recorded
  requests that produced errors, raising the share of error-inducing traffic
  sent to the target.
- **Traffic multiplication** in replay: `speed` compresses the recorded
  timeline (same shape, N× rate), `amplify` sends N copies of every request
  (optionally with per-copy identities via `vary_clones`), `rate`/`stages`
  drive an arbitrary arrival rate, `max` runs closed-loop at a concurrency.
