# Mirage UI development kit

The management UI in `web/static` is plain ES modules and CSS: there is no
build step and no dependencies. This folder holds what you need to work on it
without the Go backend (the mock server below) and to check it against the
real one (`real/up.sh` and the `real-*.mjs` scenarios, at the end of this
file).

## Mock server

```sh
node web/dev/mock-server.mjs          # http://127.0.0.1:8485
open http://127.0.0.1:8485
```

It serves `web/static` (with the same security headers as the real server,
including `Content-Security-Policy: default-src 'self'`, so inline scripts or
remote assets fail here too) and implements **every endpoint in
`docs/API.md`** with stateful, in-memory data:

* 4 services (record, virtualize, hybrid, disabled passthrough) with live,
  evolving metrics, endpoint tables and counters.
* 4 recordings with several hundred realistic exchanges each, containing
  emails, card numbers, SSNs, JWTs, binary bodies and a truncated body.
  A real anonymization engine (detectors, JSONPath rules, hash/fake/mask/
  redact/remove) backs the preview, anonymized copies, HAR import and export.
* 3 profiles, 4 fault rules (fire counts grow), 4 replay jobs: two completed,
  one running that progresses in real time (it finishes after ~6.5 minutes;
  use "Run again"), one failed. New replays are simulated (saturation shows up
  as queueing, so corrected latency diverges from service time).
* `GET /api/v1/stream` emitting `stats` every second, `services` on status
  changes and `replays` while jobs run.
* The newer API fields: service `capture_sample` / `max_capture_bytes`, status
  `capture_skipped` / `virtual_injected`, profile `opaque_bodies`, replay
  `max_gap` / `seed` / stage `hold`, summary `max_in_flight` / `saturated` and the
  `corrected_p95` / `corrected_p99` series.
* Like the Go API: values of credential-like replay `set_headers` (a fixed
  rule: `Authorization`, `Cookie`, `X-Api-Key`, anything with token, secret,
  password, ... in the name) are returned as `"***"` on every read and a
  posted `"***"` for one is a 400, and a record
  or hybrid service whose recording holds `max_capture_bytes` of bodies stays
  running with `status.warning: "capture limit reached"` (not `error`). `target_rps` of a
  replay summary is 0 until its first second has been scheduled.
* Validation errors in the documented `{error, problems[]}` shape (400),
  duplicate names or ports (409), unknown JSON fields rejected (400), like the
  Go server.

Environment variables:

| Variable | Default | Meaning |
|---|---|---|
| `PORT` | `8485` | listen port |
| `HOST` | `127.0.0.1` | listen address |
| `MOCK_TOKEN` | empty | enable admin-token auth (Bearer, `mirage_token` cookie or `access_token` query) to test the 401 flow |
| `MOCK_EMPTY` | empty | `1` starts with no services, recordings, jobs or rules (first-run experience) |
| `MOCK_LATENCY` | `12` | extra milliseconds per API call |
| `MOCK_QUIET` | empty | `1` silences the request log |

Dev-only helpers:

* `POST /__mock/sse-drop?ms=6000` ends every live stream and refuses new ones
  for a while, to exercise the "Reconnecting" indicator.
* `POST /__mock/restart?ms=2500` is a server restart: the same outage, and every
  replay that was running is marked failed (`interrupted: mirage restarted`)
  without any stream event announcing it, like the Go server.
* `POST /__mock/rename?id=svc_x&name=new` renames a service the way a rename-only
  PUT does on the Go server: no listener restart, so no `ServiceStatus` field
  changes and nothing is streamed.

`restart-mock.sh` restarts the server in the background (state resets).

## Headless browser checks

`cdp_check.mjs` is a dependency-free Chrome DevTools Protocol runner (Node 22+,
Google Chrome; copied from another project). It launches headless Chrome,
runs a JSON scenario and prints one JSON report. `run.mjs` wraps it and prints
only failed steps, console errors and exceptions. A scenario is a JSON array
or an `.mjs` file whose default export is an array (or a function returning one).

```sh
./web/dev/restart-mock.sh                                       # fresh mock state
IGNORE_HTTP=1 node web/dev/run.mjs web/dev/scenarios/interactions.mjs
./web/dev/restart-mock.sh
IGNORE_HTTP=1 node web/dev/run.mjs web/dev/scenarios/interactions2.mjs
IGNORE_HTTP=1 node web/dev/run.mjs web/dev/scenarios/qa-fixes.mjs  # QA regressions (restart, p95, probability, wrapping, toasts)
node web/dev/run.mjs web/dev/scenarios/overflow.mjs             # no page-level horizontal overflow: 15 routes x 5 widths
node web/dev/run.mjs web/dev/scenarios/hover.mjs                # chart tooltip, legend toggle, window switch
node web/dev/run.mjs web/dev/scenarios/screenshots.mjs --height 1180   # regenerates web/dev/screenshots/*.png
```

`IGNORE_HTTP=1` hides the expected 4xx responses of the validation tests (and the
503s of the stream while a simulated outage refuses it).

| Scenario | What it covers |
|---|---|
| `interactions.mjs` | modals (Escape, backdrop, focus, inert), invalid and conflicting service creation, mode switching, disable and delete with confirmation, HAR import, exchange drawer and anonymization preview, anonymized copy, replay form validation and start/stop/clone, fault toggle and preset, profile creation with the test panel, theme toggle |
| `interactions2.mjs` | editing a service (PUT), creating a recording from the service form, 409 handling for recordings and profiles, regenerate-secret flow, editing and deleting faults, dashboard scope and window, HAR export, settings (token, theme) |
| `review-fixes.mjs` | regressions from the UI review: Edit as new never posts a masked `***` secret (and says which one to re-enter), a running service at its capture limit is an amber notice and not "could not start", the compare view plots corrected latency and diffs seed and headers, the service form follows the mode (Virtualization open and first, Capture folded) and opens sections that hold an error, replay chart legends show the target while running and the whole run afterwards. Changes mock data: restart the mock after it |
| `qa-fixes.mjs` | regressions from the QA run: after a stream outage the services, replays and info are fetched again (a replay the restarted server marked failed stops showing Running; a service disabled, renamed or edited elsewhere updates its rows and page; returning to a tab that was hidden refetches), the "all services" p95 tile and chart show the slowest service's p95 and say so, a fault probability keeps its precision and is never rewritten untouched (same for error floor and capture sampling), 200-character names, long paths, headers and bodies wrap in tables, pages and the drawer (1440 and 390 px), `#/services/%` is "not found" and not a URIError, a service that saved but could not start gets a warning toast. Run with `IGNORE_HTTP=1`; `QA_CONTINUE=1` lists every failing check instead of stopping at the first. Creates and removes its own data (about a minute) |
| `overflow.mjs` | page-level horizontal overflow at 1440, 1280, 1024, 768 and 390 px |
| `hover.mjs` | crosshair and tooltip, legend toggle, 5/15/60 minute window |
| `reconnect.mjs` | live indicator through a stream outage (`/__mock/sse-drop`); the 503s it logs are expected |
| `auth.mjs` | 401 token prompt, wrong token, stored token, `access_token` on export links. Needs `PORT=8487 MOCK_TOKEN=s3cret node web/dev/mock-server.mjs` and `IGNORE_HTTP=1` (the 401s are the point) |
| `empty.mjs` | first-run empty states. Needs `PORT=8486 MOCK_EMPTY=1 node web/dev/mock-server.mjs` |
| `screenshots.mjs` | the README screenshots (run right after a restart so the "running" replay is mid-flight) |

Scenario steps (all in `cdp_check.mjs`): `goto`, `wait`, `waitFor`, `viewport`,
`media` (e.g. `prefers-color-scheme`), `screenshot` (optional `clip`), `click`,
`hover`, `key`, `insertText` (+ `into`), `eval` (+ `as`), `text`, `assert`.
`goto` waits for the load event, so navigate inside the app with
`eval: "location.hash='#/services'"`.

## Screenshots

`screenshots/` holds 1440 px captures (dark theme unless noted) generated by
`scenarios/screenshots.mjs`, `hover.mjs`, `auth.mjs` and `empty.mjs`:
`dashboard`, `dashboard-light`, `empty-dashboard` (getting started), `services`,
`service-detail`, `service-form`, `recordings`, `recording-drawer`,
`recording-drawer-preview`, `replays`, `replay-new`, `replay-running`,
`replay-completed` (queueing detected), `replay-compare`, `profile-editor`
(with the test panel), `faults`, `chart-hover`, `auth-prompt`.

## Layout of the UI code

```
web/static/index.html          shell, loads /assets/app.css and /assets/app.js
web/static/assets/app.js       bootstrap: theme, shell, router, SSE, auth prompt
web/static/assets/app.css      design tokens (dark/light) and all components
web/static/assets/js/
  dom.js        h()/svg() helpers; never innerHTML for data
  api.js        fetch wrapper, ApiError(problems), token, access_token URLs
  sse.js        EventSource with backoff, refetch after a gap; state.js pub/sub + shared state
  series.js     per-collector time-series buffers (history + live points)
  router.js     hash router with lazy page modules
  chart.js      canvas TimeChart, sparkline, status bar, ramp preview
  ui.js         buttons, modals/drawers, toasts, form controls, tables
  bodyview.js   JSON/hex body viewers, headers, exchange sections
  pages/        dashboard, services, recordings, replays (+form), profiles, faults, settings
```

## Against the real server

The mock can drift from the Go server, so the UI is also checked against the
real thing. `real/up.sh` builds the binaries, starts demo-api and `mirage
serve --config examples/mirage.yaml`, fills the recording with 35 s of traffic,
and `real/seed.mjs` adds a virtual service, a latency fault and six replay jobs
of every shape (timeline, staged ramp with a step, a saturated one with too few
workers, error amplification, closed loop and one still running). Background
traffic keeps the live charts moving. State goes to `$REAL_DIR` (default
`$TMPDIR/mirage-ui-real`), including `ids.json` for the scenarios.

```sh
web/dev/real/up.sh                                    # http://127.0.0.1:8484
node web/dev/run.mjs web/dev/scenarios/real.mjs       # every route: console errors, failed requests, error pages
IGNORE_HTTP=1 node web/dev/run.mjs web/dev/scenarios/real-interactions.mjs
OUT=docs/screenshots node web/dev/run.mjs web/dev/scenarios/real-screenshots.mjs --height 1180
web/dev/real/down.sh
```

| Scenario | What it covers |
|---|---|
| `real.mjs` | opens all 23 routes (with real ids) and fails on console errors, uncaught exceptions, 4xx/5xx responses, and pages that ended in an error state |
| `real-interactions.mjs` | the service page follows the stream (no polling of `GET /services/{id}`, counters move, an external mode change shows up), editing a service keeps fields the form does not show, the replay form in rate and timeline mode (stage steps, amplify only in timeline, `max_gap`, seed, server default concurrency), honest progress and the saturation warning, profile `opaque_bodies`. It changes demo data, so run `up.sh` again afterwards |
| `real-screenshots.mjs` | the README screenshots in `docs/screenshots` |

The interaction scenario drives replay jobs against the virtual service on
`:9002`, so it also proves the whole path UI, API, replay engine, virtualizer.
