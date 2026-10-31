# Mirage management API (v1)

The admin server (default `127.0.0.1:8484`) serves the web UI at `/`, this
JSON API under `/api/v1`, and Prometheus metrics at `/metrics`.

Object shapes are the JSON forms of the types in `internal/model`; field
names below are exact. Durations are ASCII Go duration strings (`"250ms"`,
`"1m30s"`, `"125us"`); on input a JSON number is also accepted and means
milliseconds.
Timestamps are RFC 3339. Series timestamps (`t`) are Unix milliseconds.

## Conventions

- Errors: `{"error": "message", "problems": ["field x ..."]}` with:
  - 400: validation, malformed JSON, or a malformed query parameter;
  - 401: authentication;
  - 403: the browser protection refused the request: the `Host` is a DNS name
    other than `localhost` or `*.localhost` (IP addresses always work) that is
    not listed with `--allowed-host` (or `server.allowed_hosts`), on a
    server without an admin token, or a state-changing request carries another
    origin (`Origin` that is not the server's own host, or `Sec-Fetch-Site:
    cross-site`). `curl` and scripts send neither and are not affected;
  - 404: an unknown object, or an unknown path under `/api/v1`;
  - 405: a known path with the wrong method (the `Allow` header lists the
    methods that path accepts);
  - 409: a conflict, for example a duplicate service name (compared ignoring
    case and surrounding spaces, like profile names) or listen address,
    starting a replay that is already running, or deleting something in use;
  - 413: a JSON request body over 10 MiB, or a HAR upload over 1 GiB;
  - 415: a POST, PUT, PATCH or DELETE whose body is not declared as JSON. Send
    `Content-Type: application/json` (`+json` types work too) with every
    request body; the HAR import also takes `multipart/form-data`. Requests
    without a body need no content type;
  - 500: anything unexpected (the detail is in the server log, not the body);
  - 503: the service or replay controller is not available (a server started
    without one);
  - 504: the operation timed out.
- Optional fields are omitted from responses when they are empty or zero (Go
  `omitempty`), so the full objects shown below list every field but a real
  response can leave some out. Read an absent field as its zero value (`""`,
  `0`, `false`, no list, `null`). Examples: a service without a recording has
  no `recording_id`, a replay that has not ended has no `ended_at`, a
  `latency` fault has no `status`, and `ServiceStatus.error` appears only
  when there is one. Required fields such as `id`, `name`, `mode`, `status` and
  `created_at` are always present.
- Messages never name internal packages (`store:`, `ops:`) and name an object
  once, however many layers the error passed through. That includes
  `ServiceStatus.error`, also in the live stream: `capture: recording "rec_x":
  not found`, not `capture: store: store: recording "rec_x": not found`.
- Unknown JSON fields in request bodies are rejected (400), except read-only
  echo fields, which are accepted and ignored so a fetched object can be PUT
  back: `status`, `fired`, `has_secret`, `created_at`, `updated_at`. The id
  in the URL wins over an id in the body.
- `profile_id` absent means the `default` profile for anonymize preview and
  anonymized copies, and no anonymization for HAR export/import.
- Auth (only when the server runs with `--admin-token`): send
  `Authorization: Bearer <token>`, or the cookie `mirage_token`, or the query
  parameter `access_token` (for EventSource and download links).
  `GET /api/v1/health` is always open.
- Lists are plain JSON arrays unless they are paginated (`{items, total}`).

## System

| Method | Path | Response |
|---|---|---|
| GET | `/api/v1/health` | `{"status":"ok","version":"v0.1.0","uptime_s":12.3}` |
| GET | `/api/v1/info` | `{"version","go_version","data_path","started_at","auth":bool,"detectors":[DetectorInfo]}` |
| GET | `/metrics` | Prometheus text format |

`DetectorInfo` = `{"name":"email","description":"...","example":"jane@acme.io"}`.

## Services

`ServiceWithStatus` = every `Service` field plus `"status": ServiceStatus`.

| Method | Path | Body | Response |
|---|---|---|---|
| GET | `/api/v1/services` | | `[ServiceWithStatus]` sorted by name |
| POST | `/api/v1/services` | `Service` (id optional) | 201 `ServiceWithStatus` |
| GET | `/api/v1/services/{id}` | | `ServiceWithStatus` |
| PUT | `/api/v1/services/{id}` | `Service` (full object) | `ServiceWithStatus` |
| DELETE | `/api/v1/services/{id}` | | 204 |
| POST | `/api/v1/services/{id}/mode` | `{"mode":"record","recording_id":"rec_x"}` or `{"mode":"record","new_recording_name":"checkout run 3"}` | `ServiceWithStatus` |
| POST | `/api/v1/services/{id}/enable` | | `ServiceWithStatus` (sets enabled=true, applies) |
| POST | `/api/v1/services/{id}/disable` | | `ServiceWithStatus` |
| POST | `/api/v1/services/{id}/reload` | | `ServiceWithStatus` (rebuild virtual index) |
| GET | `/api/v1/services/{id}/stats` | | `StatsSnapshot` |
| GET | `/api/v1/services/{id}/series?since=<unix ms>` | | `[SeriesPoint]` (default: last 15 min) |

Every mode except `passthrough` (`record`, `hybrid`, `virtualize`) needs a
recording: saving a service in one of them without `recording_id` is a 400
(`mode record needs a recording_id`). The `mode` endpoint keeps the service's
current recording unless it is given `recording_id` or `new_recording_name`
(which creates an empty recording and points the service at it); the two are
mutually exclusive, and it is a 400 when the service would still have none.

Service names are unique ignoring case and surrounding spaces (`Payments` and
` payments ` clash): a duplicate is a 409 on create, update and rename. A
database that already holds two names differing only in case still opens and
lists; one of them has to be renamed before it can be saved again.

**Virtualizing an anonymized recording.** A service with `anonymize_capture`
stores anonymized paths, queries, bodies and headers, so the raw requests of a
client (an email in the path, a card number in the body) would never match
what was recorded. In `virtualize` and `hybrid` mode such a service therefore
anonymizes each incoming request with its `profile_id` (the same way capture
does: gzip/deflate bodies are decoded first) and looks that up; the request as
received is tried too, which still matches traffic that is already anonymized,
such as a replay of the recording. Both forms are tried level by level, most
specific first (exact, path, template), so a broad template match never hides
an exact one, and the template level uses the endpoint the capture stored. The
request itself is never modified (the upstream of a `hybrid` miss gets the raw
request). Editing the profile, or regenerating its secret, rebuilds the
virtual index of every service using it; if the rebuild fails the old one
keeps serving and the error is shown in `status.error` until a later rebuild
succeeds. Regenerating a secret makes new captures differ from older
recordings, so requests that matched the older tokens stop matching. A
service without `anonymize_capture` looks up the request as received, with no
extra work.

Virtual responses are the recorded ones: a recording without a `Content-Type`
is served without one (no `text/plain` is invented); the proxy modes do not
add a `Content-Type` the upstream did not send either. A `HEAD` request
answered from a recorded `HEAD` exchange carries the `Content-Length` that
exchange announced, and one answered from a `GET` recording the length that
`GET` body would be served with.

`Service`:

```json
{
  "id": "svc_ab12cd34ef56gh78",
  "name": "payments",
  "listen_addr": ":9001",
  "upstream": "https://payments.internal:8443",
  "mode": "record",
  "enabled": true,
  "recording_id": "rec_...",
  "anonymize_capture": true,
  "profile_id": "default",
  "endpoint_patterns": ["/v1/customers/{id}"],
  "preserve_host": false,
  "insecure_skip_verify": false,
  "tls": {"self_signed": true},
  "timeout": "60s",
  "max_body_bytes": 1048576,
  "capture_sample": 1,
  "max_capture_bytes": 0,
  "match": {"query": "exact", "ignore_params": ["_ts"], "body": "json", "headers": [],
            "path_templates": true, "selection": "sequential", "miss_status": 404},
  "latency": {"mode": "recorded", "multiplier": 1, "fixed": "0s", "jitter": "0s"},
  "error_amplification": 1,
  "error_floor": 0,
  "created_at": "...", "updated_at": "..."
}
```

`ServiceStatus`:

```json
{"service_id":"svc_...","running":true,"addr":"[::]:9001","mode":"record","error":"","warning":"",
 "started_at":"...","config_updated_at":"...","captured":1520,"capture_dropped":0,
 "capture_skipped":0,"capture_truncated":0,"virtual_hits":0,"virtual_misses":0,
 "virtual_injected":0,"virtual_truncated":0,"indexed_exchanges":0}
```

`config_updated_at` is the `updated_at` of the configuration last applied to
the service, also when it is stopped or applying it failed. It changes with
every saved edit, so a client that follows the `services` stream event can
tell that its copy of a service is stale even when nothing else in the status
changed (a rename, say).

`error` says why a service is not running (it could not start, or its listener
died) or reports a failure found while serving, such as a capture write that
failed. `warning` is a note about something that leaves the service working;
it is `capture limit reached` (the recording holds `max_capture_bytes` of
bodies: traffic keeps flowing, only capturing stopped) or a note mentioning
`max_body_bytes` when recorded responses were truncated (see below). While the
capture limit is reached, `capture limit reached` is shown instead of a note
about truncated captures.
It appears only on a running service. Both are omitted when empty and cleared
the next time the service is applied, for example by raising the limit.
`mode` is the mode of the running listener, or the saved service's mode when
nothing is running. `virtual_injected` counts injected errors only, the
responses that carry `X-Mirage-Injected`.

`capture_truncated` counts stored exchanges whose response body was larger than
`max_body_bytes` and was cut (only the beginning is kept; the traffic itself is
never truncated). It is not counted when `max_body_bytes` is `-1` (bodies are
not captured on purpose), it resets when `max_body_bytes` is raised or the
recording changes, and lowering the cap does not reset it.

A recording that holds only part of a response body is never served as if it
were complete:

- If the same request was also recorded completely, a complete recording is
  served; `selection` rotates over the complete recordings as if the truncated
  ones did not exist.
- In `hybrid` mode a request whose only recordings are truncated is a miss: it
  is proxied to the upstream and recorded again, on every such request until
  `max_body_bytes` is large enough to record it whole (like any other miss
  that hybrid does not learn).
- In `virtualize` mode it is answered `502` with `X-Mirage-Error: truncated`,
  the `X-Mirage-Match` level and a JSON body with `error`, `method`, `path`,
  `recording_id`, `exchange_id`, `recorded_bytes`, `recorded_bytes_exact`
  (false when the body was cut while being decompressed, so `recorded_bytes`
  is only a lower bound) and `kept_bytes`. `virtual_truncated` counts these
  answers since the virtual index was built.
- A recording that kept no body at all (`max_body_bytes: -1`, or a HAR entry
  without response content) is served as recorded, with an empty body.

The truncation `warning` is derived from these counters: it names
`max_body_bytes` and is shown while they are non-zero. Raise `max_body_bytes`
and record the affected requests again (switch to `record` or `hybrid`;
`hybrid` does it by itself).

**Saving a service that cannot start is not an HTTP error.** Create, update,
`enable` and `mode` persist the service and then apply it to the listener. If
the listener cannot start (a port taken by another process, say) the service
is still saved and the response is still 2xx (200, or 201 on create), with
`status.running: false` and the reason in `status.error`. Always read `status`
after these calls, not just the HTTP status. Only a clash with another Mirage
service (name or listen address) is a 409.

Deleting a recording and saving a service that uses it are ordered, so a
service can never be left enabled on a recording that is gone: whichever
request gets in first wins. A service save (create, update, `enable`, `mode`)
that comes after the delete is a 400 (`recording_id "rec_x" does not exist`);
a delete that comes while a service on that recording is being saved or is
already enabled is a 409. A very large recording takes a while to purge, and
service changes wait for it.

## Recordings and exchanges

| Method | Path | Body / query | Response |
|---|---|---|---|
| GET | `/api/v1/recordings` | | `[Recording]` newest first |
| POST | `/api/v1/recordings` | `{"name","description"?,"labels"?}` | 201 `Recording` |
| GET | `/api/v1/recordings/{id}` | | `Recording` |
| PATCH | `/api/v1/recordings/{id}` | `{"name"?,"description"?,"labels"?}` | `Recording` |
| DELETE | `/api/v1/recordings/{id}` | | 204 (409 if an enabled service uses it or a replay on it is pending/running) |
| GET | `/api/v1/recordings/{id}/endpoints` | | `[EndpointStat]` |
| GET | `/api/v1/recordings/{id}/exchanges` | `method, status, status_class, path, endpoint, errors=1, offset, limit, desc=1` | `{"items":[ExchangeSummary],"total":N}` |
| GET | `/api/v1/exchanges/{id}` | | `ExchangeView` |
| POST | `/api/v1/recordings/{id}/anonymize` | `{"profile_id":"default","name":"payments (anon)"}` | 201 `Recording` (the new copy) |
| GET | `/api/v1/recordings/{id}/export.har` | `profile_id` (optional: anonymize on export) | HAR download (`Content-Disposition: attachment`) |
| POST | `/api/v1/recordings/import` | raw HAR JSON body, or `multipart/form-data` with field `file`; query `name`, `profile_id` (optional) | 201 `Recording` |

In a multipart upload `name` and `profile_id` may also be form fields, but the
upload is streamed, so they are read only when they come **before** the `file`
part; a field after it is ignored. The query parameters win over form fields.
Without a name the file's name (without `.har`/`.json`) is used.

**Anonymized copies, exports and imports.** `POST .../anonymize`, an export
with `profile_id` and an import with `profile_id` all run every exchange
through the same steps as capture:

- *Compressed bodies are decoded first.* gzip, x-gzip, zlib and raw deflate
  bodies, also stacked (`gzip, gzip`, `deflate, gzip`), are inflated (up to
  256 MiB per body) so the profile can see what is inside; the anonymized
  artifact then holds the plain body without `Content-Encoding`. A stream that
  was cut by the capture cap yields its readable prefix. A body the engine
  cannot decode (`br`, `zstd`, a stream that gives nothing) is dropped and
  marked `truncated`, never kept as it was: it would be raw personal data
  that anyone can decompress. A HAR import decodes the same codings (up to
  1 MiB per entry) when it reads the file, whether or not it anonymizes.
  HAR files normally hold bodies decoded next to the original
  `Content-Encoding` header, so reading one decodes a body only when it is
  plainly compressed: the gzip magic number, a zlib stream, or a raw deflate
  stream that uses the whole body. Anonymizing errs the other way: a body
  recorded as decoded that may still be compressed (under `deflate`, a zlib
  header or a stream that uses the whole body, complete or cut; under `br`
  or `zstd`, bytes that are not valid UTF-8) is decoded or dropped, so a
  decoded binary body under such a coding can occasionally be lost from the
  anonymized artifact. A body that was decoded is anonymized like any other,
  text in a legacy charset such as ISO-8859-1 included.
- *Endpoints are rebuilt from the anonymized path*, so a raw segment
  (`/ssn/123-45-6789`) never survives in endpoint statistics or matching. An
  endpoint that came from a service's `endpoint_patterns` is kept when it has
  no literal segment the anonymizer changed.
- *An exchange the profile cannot process is left out*, never written in any
  form. The copy or import still succeeds and its `description` says `Left out
  N exchanges that could not be anonymized.`; only when every exchange fails
  is the request an error (and nothing is kept). An export writes the
  document without them, and fails (the document is left unfinished) only if
  none could be written.
- *A HAR import repairs start times that cannot be stored.* An entry whose
  `startedDateTime` is missing, the zero time, the Unix epoch, or outside the
  years 1678 to 2261 takes the start time of the previous entry (leading
  entries wait for the first usable one; the import time is used when there is
  none), so a timeline replay does not wait for years. The count is in the
  `description` (`N entries had a missing start time ...`). The store itself
  rejects a batch with such a time (400, naming the exchanges), so no other
  path can write one.
- *Skipped entries.* HAR entries that are not HTTP or HTTPS requests (`data:`
  URLs and the like) are skipped and counted in the `description`
  (`Skipped N unsupported entries ...`); any other entry that cannot be
  converted fails the import with a 400 that names it.

Query parameters of the exchange list (all optional, combined with AND):

| Parameter | Format |
|---|---|
| `method` | HTTP method, case-insensitive (`GET`) |
| `status` | exact status code, 1 to 599 (`503`); 0 or absent means any |
| `status_class` | an integer from 1 to 5, the hundreds digit: `5` means 500 to 599. The form `5xx` is rejected with 400 |
| `path` | case-sensitive substring of the path plus `?query` (`/users/4`, `expand=orders`), not a pattern |
| `endpoint` | exact endpoint template (`GET /users/{id}`) |
| `errors` | `1` or `true` for error exchanges only: status 5xx or 429, no response at all (status 0), or a recorded transport error |
| `offset` | rows to skip, default 0 |
| `limit` | page size, default 100 (also for 0 or absent), silently capped at 1000: a larger value returns 1000 rows, not an error |
| `desc` | `1` or `true` for newest first (default: oldest first, by `seq`) |

A value that is not an integer, or is out of range, is a 400 that names the
parameter.

`total` is the number of exchanges matching the filters, not the page size, so
page with `offset` until it reaches `total`.

`Recording`:

```json
{"id":"rec_...","name":"checkout run 3","description":"","service_id":"svc_...",
 "source":"proxy","anonymized":true,"labels":{},
 "exchange_count":15230,"error_count":41,"bytes":48211230,
 "first_at":"...","last_at":"...","created_at":"...","updated_at":"..."}
```

`EndpointStat`:

```json
{"endpoint":"GET /users/{id}","count":5120,"errors":12,"error_rate":0.0023,
 "avg":"12.1ms","p50":"9ms","p95":"31ms","p99":"80ms","max":"412ms",
 "status_counts":{"200":5108,"503":12}}
```

`ExchangeSummary`:

```json
{"id":881,"seq":12,"started_at":"...","duration":"14.2ms","method":"GET",
 "path":"/users/42","raw_query":"expand=orders","status":200,
 "endpoint":"GET /users/{id}","req_bytes":0,"resp_bytes":1840,
 "content_type":"application/json","error":""}
```

`ExchangeView` (bodies as `BodyView`):

```json
{"id":881,"recording_id":"rec_...","seq":12,"service_id":"svc_...",
 "started_at":"...","duration":"14.2ms","ttfb":"11ms","endpoint":"GET /users/{id}",
 "request":{"method":"GET","scheme":"http","host":"localhost:9001","path":"/users/42",
            "raw_query":"expand=orders","proto":"HTTP/1.1",
            "header":{"Accept":["application/json"]},
            "body":{"encoding":"none","size":0}},
 "response":{"status":200,"proto":"HTTP/1.1","header":{"Content-Type":["application/json"]},
             "body":{"encoding":"text","data":"{\"id\":42}","size":9,"content_type":"application/json"},
             "content_encoding":"gzip"},
 "error":"","anonymized":true}
```

`BodyView.encoding` is `"text"`, `"base64"` or `"none"`; `truncated` is true
when the capture cap cut the body.

## Anonymization

| Method | Path | Body | Response |
|---|---|---|---|
| GET | `/api/v1/profiles` | | `[AnonymizationProfile]` (secret omitted, `has_secret` set) |
| POST | `/api/v1/profiles` | `AnonymizationProfile` | 201 profile |
| GET | `/api/v1/profiles/{id}` | | profile |
| PUT | `/api/v1/profiles/{id}` | profile, plus optional `"regenerate_secret": true` | profile |
| DELETE | `/api/v1/profiles/{id}` | | 204 (409 for `default` or if a service or replay uses it) |
| GET | `/api/v1/anonymize/detectors` | | `[DetectorInfo]` |
| POST | `/api/v1/anonymize/preview` | `{"profile": AnonymizationProfile}` or `{"profile_id":"default"}`, plus `{"exchange_id": 881}` or `{"exchange": ExchangeView}` | `{"exchange": ExchangeView, "changes": [AnonChange]}` |

`AnonymizationProfile`:

```json
{"id":"prof_...","name":"Strict","description":"",
 "detectors":["email","credit_card","ssn","phone","ipv4","ipv6","jwt","aws_key","iban"],
 "detector_strategy":"fake",
 "redact_headers":["Authorization","Cookie","Set-Cookie","X-Api-Key"],
 "opaque_bodies":"keep",
 "rules":[{"id":"r1","name":"customer id","enabled":true,"target":"json",
           "selector":"$..customer_id","scope":"both","strategy":"hash"},
          {"id":"r2","name":"card last4","enabled":true,"target":"json",
           "selector":"$.card.last4","scope":"response","strategy":"mask","keep_suffix":2}],
 "has_secret":true,"created_at":"...","updated_at":"..."}
```

Rule `target`: `header | query | cookie | json | form | regex | path`.
`strategy`: `redact | remove | hash | fake | mask`. `fake`: `auto | email |
name | phone | uuid | digits | ipv4 | credit_card | string`. `scope`:
`both | request | response`.

How rules match:

- *Names ignore case, `_` and `-`.* A `header`, `query`, `cookie` or `form`
  selector (and a multipart part name) and a `$..name` descent in a `json`
  rule compare names with those characters ignored, so a `password` rule
  also covers `Password`, `PASSWORD`, `?API_KEY=` for `api-key` and
  `userData` for `user_data`; `user[password]` in a form also matches by its
  last bracketed part. Absolute JSONPath (`$.a.b`) and `path`/`regex` rules are
  unchanged.
- *The first matching rule wins.* A value is transformed once: when several
  rules select it the first in the profile applies and later ones leave it
  alone (a `remove` listed after a `hash` rule that matches the same value does
  not delete it). A `regex` rule only takes the stretches of text it matches,
  so a later rule never rewrites what an earlier one wrote, and a selector
  rule listed after a `regex` rule that took part of a value leaves that value
  alone.
- *JSON inside JSON strings is rewritten.* A string leaf that holds a JSON
  object or array (a queue message, a webhook envelope, a log event) is parsed
  and rewritten up to three levels deep. Only `$..` descents, `regex` rules and
  the detectors apply inside it (absolute paths are relative to the outer
  document), the inner document is written back compactly if something in it
  changed, and a string that does not parse, or that a rule selects as a
  whole, is treated as a plain string. Changes inside it are reported with the
  concrete path, for example `response.body $.payload <json> $.password`.

When a release extends the built-in `default` profile, a database opened by
it gains the new rules and headers, appended after the ones it has. Rules you
edited, switched off or deleted, and headers you removed, stay as you left
them.

`opaque_bodies`: `keep` (default) stores bodies the engine cannot inspect
(binary, still-compressed, binary multipart parts) unchanged; `fill` replaces
them with deterministic filler of the same length. Regenerating a secret
makes new captures produce different tokens than existing recordings.

`secret` is write-only: no response contains it, `has_secret` says whether one
is stored. Leave it out when creating and the server generates one; on `PUT` an
empty `secret` keeps the stored one and `"regenerate_secret": true` (`PUT`
only) replaces it.

`AnonChange` = `{"location":"response.body $.user.email","source":"detector:email","before":"jane@acme.io","after":"mason.reed42@example.com"}`.

## Fault injection

| Method | Path | Body | Response |
|---|---|---|---|
| GET | `/api/v1/faults` | | `[FaultRuleWithCount]` (rule fields plus `"fired": N`) |
| POST | `/api/v1/faults` | `FaultRule` | 201 rule |
| GET | `/api/v1/faults/{id}` | | rule |
| PUT | `/api/v1/faults/{id}` | `FaultRule` | rule |
| DELETE | `/api/v1/faults/{id}` | | 204 |

`FaultRule`:

```json
{"id":"flt_...","name":"10% 503s on checkout","enabled":true,"order":0,
 "service_id":"","methods":["POST"],"path_pattern":"/checkout/**","headers":{},
 "probability":0.1,"type":"error","status":503,"body":"{\"error\":\"overloaded\"}",
 "response_headers":{"Retry-After":"1"},
 "delay":"0s","jitter":"0s","bytes_per_second":0,"created_at":"...","updated_at":"..."}
```

`type`: `latency` (uses `delay`, `jitter`), `error` (`status`, `body`,
`response_headers`), `abort`, `timeout` (`delay`), `throttle`
(`bytes_per_second`).

## Replay (load tests)

| Method | Path | Body | Response |
|---|---|---|---|
| GET | `/api/v1/replays` | | `[ReplayJob]` newest first, no `series` |
| POST | `/api/v1/replays` | `{"name":"10x checkout","config":ReplayConfig,"start":true}` | 201 `ReplayJob` |
| GET | `/api/v1/replays/{id}` | | `ReplayJob` with `series` (also while it runs: the live series so far) |
| POST | `/api/v1/replays/{id}/start` | | `ReplayJob` (re-runs a finished job) |
| POST | `/api/v1/replays/{id}/stop` | | `ReplayJob` |
| POST | `/api/v1/replays/{id}/clone` | `{"name"?}` | 201 new pending `ReplayJob` with the same config |
| DELETE | `/api/v1/replays/{id}` | | 204 |
| GET | `/api/v1/replays/{id}/series?since=<unix ms>` | | `[SeriesPoint]` (live while running) |

`start` runs a pending job, or runs a finished one again and replaces its
results; on a job that is running it answers 409. `stop` is idempotent: it
returns the job with 200 whether it was running (it waits for the result to be
saved, with a summary of what was sent), or was pending, stopped or completed
already.

`ReplayConfig`:

```json
{"recording_id":"rec_...","target":"http://staging.internal:8080","mode":"timeline",
 "speed":10,"max_gap":"5s","rate":0,
 "stages":[{"duration":"30s","rate":500},{"duration":"2m","rate":2000,"hold":true}],
 "amplify":1,"error_amplification":10,"concurrency":2048,"duration":"5m","loop":true,
 "max_requests":0,"timeout":"30s","include":["^GET /users"],"exclude":[],
 "set_headers":{"Authorization":"Bearer staging-token"},"remove_headers":["Cookie"],
 "host_header":"","insecure_skip_verify":false,
 "profile_id":"","vary_clones":false,"compare_status":true,"seed":0}
```

`mode`: `timeline` (uses `speed`, `max_gap`, `amplify`, `loop`), `rate` (uses
`rate` or `stages`; a stage ramps linearly from the previous rate unless
`hold` is true, which steps straight to its rate), `max` (closed loop at
`concurrency` workers). `amplify` > 1 is only valid in timeline mode (in
rate mode raise the rate). `concurrency` defaults to 2048 (timeline, rate)
or 64 (max). `seed` 0 means the server picks one (an integer from 1 to
2^53-1, so JavaScript clients round-trip it) and stores it, so re-runs and
clones replay the same schedule. Values of credential-like `set_headers`
(`Authorization`, `Cookie`, `X-Api-Key`, and any name containing `token`,
`secret`, `password`, `session`, `signature`, ending in `-key`, …; a fixed
rule) are returned masked as `"***"`. Posting `"***"` as the value of such a
header is a 400, so a fetched job cannot be sent back with the mask as its
credential; send the real value, or clone the replay to reuse the stored one.

`ReplayJob`:

```json
{"id":"rpl_...","name":"10x checkout","config":{...},"status":"running","error":"",
 "created_at":"...","started_at":"...","ended_at":null,
 "summary":ReplaySummary,"series":[SeriesPoint]}
```

`status`: `pending | running | completed | stopped | failed`.

`ReplaySummary`:

```json
{"planned":152300,"sent":40211,"completed":40102,"failed":109,"timeouts":3,
 "errors":512,"mismatches":88,"status_counts":{"200":39590,"503":403,"404":109},
 "latency":LatencySummary,"corrected_latency":LatencySummary,
 "target_rps":1000,"achieved_rps":998.2,"error_rate":0.0127,
 "bytes_sent":1203311,"bytes_received":88123011,"elapsed":"40.3s","max_lag":"3.1ms",
 "max_in_flight":412,"saturated":false,"endpoints":[EndpointSummary]}
```

`saturated` applies to the open-loop modes (`timeline` and `rate`): every
concurrency slot was busy at least once, so lag and corrected latency include
client-side queueing (raise `concurrency`). It is always `false` in `max` mode,
which is closed loop and keeps every slot busy by design.
`corrected_latency` is measured from each request's intended send time and
corrects coordinated omission.

`latency` and `corrected_latency` also include requests that failed after a
connection was made (a timeout counts as at least `timeout` long); requests
that never reached the target (connection refused, DNS or certificate
failure) are not measured, so a dead target has `latency.count` 0 rather than
a fast p99. `achieved_rps` is measured over the time requests were being sent,
not the drain of slow in-flight ones. `planned` is exact for a single pass and
for looping timelines, an estimate with fractional `error_amplification` on a
recording that has errors (such an estimate is raised to `sent` if the run
overshoots it), and `-1` when unknown.

`LatencySummary` (milliseconds) =
`{"count","min","mean","p50","p90","p95","p99","p999","max"}`.
`EndpointSummary` = `{"endpoint","count","errors","latency":LatencySummary}`.

## Live metrics

| Method | Path | Response |
|---|---|---|
| GET | `/api/v1/stats` | `[StatsSnapshot]` for every collector |
| GET | `/api/v1/stream` | Server-Sent Events (below) |

`StatsSnapshot`:

```json
{"name":"service:svc_...","kind":"service","id":"svc_...","since":"...",
 "requests":120331,"errors":220,"bytes_in":1203311,"bytes_out":98123011,"in_flight":14,
 "status_classes":{"2xx":119800,"4xx":311,"5xx":220,"err":0},
 "rps":1022,"rps10":998.4,"error_rate10":0.0018,
 "latency":LatencySummary,"recent":LatencySummary,
 "corrected_latency":LatencySummary,"endpoints":[EndpointSummary]}
```

`corrected_latency` is present only for replay collectors. `endpoints` in
snapshots carries the 20 busiest endpoints.

`SeriesPoint` =
`{"t":1761430000000,"requests":1022,"errors":2,"p50":8.1,"p95":30.2,"p99":71.9,"max":180.4,"in_flight":14,"bytes_in":10233,"bytes_out":812331,"corrected_p95":41.0,"corrected_p99":95.2}`
(`corrected_*` only for replay collectors).

The per-service series are what the dashboard's "all services" view
aggregates: requests, errors, in-flight and bytes are summed, `p50` is the
request-weighted mean of the services' p50, and `p95` and `p99` are the
largest value of any service (a mean of percentiles is not a percentile, and
would hide a slow service behind a fast busy one). The dashboard labels them
`p50 (avg)`, `p95 (slowest)` and `p99 (slowest)`, and its p95 tile reads `p95
(slowest service)` with the service named in its tooltip; the view of a single
service or replay shows the plain values.

SSE events on `/api/v1/stream` (each `data:` is one JSON document):

- `event: stats` every second:
  `{"time":"...","snapshots":[StatsSnapshot],"points":{"service:svc_x":SeriesPoint,...}}`
- `event: services` every second (and immediately on connect): `[ServiceStatus]`
- `event: replays` every second while any job runs:
  `[{"id","status","summary":ReplaySummary}]`, and once more when a job finishes.
- `: ping` comment every 15 s.
