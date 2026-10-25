# Mirage UI development kit

The management UI in `web/static` is plain ES modules and CSS: there is no
build step and no dependencies. This folder holds what you need to work on it
without the Go backend.

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

Dev-only helper: `POST /__mock/sse-drop?ms=6000` ends every live stream and
refuses new ones for a while, to exercise the "Reconnecting" indicator.

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
node web/dev/run.mjs web/dev/scenarios/overflow.mjs             # no page-level horizontal overflow: 15 routes x 5 widths
node web/dev/run.mjs web/dev/scenarios/hover.mjs                # chart tooltip, legend toggle, window switch
node web/dev/run.mjs web/dev/scenarios/screenshots.mjs --height 1180   # regenerates web/dev/screenshots/*.png
```

`IGNORE_HTTP=1` hides the expected 4xx responses of the validation tests.

| Scenario | What it covers |
|---|---|
| `interactions.mjs` | modals (Escape, backdrop, focus, inert), invalid and conflicting service creation, mode switching, disable and delete with confirmation, HAR import, exchange drawer and anonymization preview, anonymized copy, replay form validation and start/stop/clone, fault toggle and preset, profile creation with the test panel, theme toggle |
| `interactions2.mjs` | editing a service (PUT), creating a recording from the service form, 409 handling for recordings and profiles, regenerate-secret flow, editing and deleting faults, dashboard scope and window, HAR export, settings (token, theme) |
| `overflow.mjs` | page-level horizontal overflow at 1440, 1280, 1024, 768 and 390 px |
| `hover.mjs` | crosshair and tooltip, legend toggle, 5/15/60 minute window |
| `reconnect.mjs` | live indicator through a stream outage (`/__mock/sse-drop`); the 503s it logs are expected |
| `auth.mjs` | 401 token prompt, wrong token, stored token, `access_token` on export links. Needs `PORT=8487 MOCK_TOKEN=s3cret node web/dev/mock-server.mjs` |
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
  sse.js        EventSource with backoff; state.js pub/sub + shared state
  series.js     per-collector time-series buffers (history + live points)
  router.js     hash router with lazy page modules
  chart.js      canvas TimeChart, sparkline, status bar, ramp preview
  ui.js         buttons, modals/drawers, toasts, form controls, tables
  bodyview.js   JSON/hex body viewers, headers, exchange sections
  pages/        dashboard, services, recordings, replays (+form), profiles, faults, settings
```
