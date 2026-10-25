# Mirage management API (v1)

The admin server (default `127.0.0.1:8484`) serves the web UI at `/`, this
JSON API under `/api/v1`, and Prometheus metrics at `/metrics`.

Object shapes are the JSON forms of the types in `internal/model`; field
names below are exact. Durations are ASCII Go duration strings (`"250ms"`,
`"1m30s"`, `"125us"`); on input a JSON number is also accepted and means
milliseconds.
Timestamps are RFC 3339. Series timestamps (`t`) are Unix milliseconds.

## Conventions

- Errors: `{"error": "message", "problems": ["field x ..."]}` with 400
  (validation or malformed JSON), 401 (auth), 404, 409 (conflict, e.g. a
  duplicate service name or listen address), 500.
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
{"service_id":"svc_...","running":true,"addr":"[::]:9001","mode":"record","error":"",
 "started_at":"...","captured":1520,"capture_dropped":0,"capture_skipped":0,
 "virtual_hits":0,"virtual_misses":0,"virtual_injected":0,"indexed_exchanges":0}
```

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

`opaque_bodies`: `keep` (default) stores bodies the engine cannot inspect
(binary, still-compressed, binary multipart parts) unchanged; `fill` replaces
them with deterministic filler of the same length. Regenerating a secret
makes new captures produce different tokens than existing recordings.

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
| GET | `/api/v1/replays/{id}` | | `ReplayJob` with `series` |
| POST | `/api/v1/replays/{id}/start` | | `ReplayJob` (re-runs a finished job) |
| POST | `/api/v1/replays/{id}/stop` | | `ReplayJob` |
| POST | `/api/v1/replays/{id}/clone` | `{"name"?}` | 201 new pending `ReplayJob` with the same config |
| DELETE | `/api/v1/replays/{id}` | | 204 |
| GET | `/api/v1/replays/{id}/series?since=<unix ms>` | | `[SeriesPoint]` (live while running) |

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
or 64 (max). `seed` 0 means the server picks one and stores it, so re-runs
and clones replay the same schedule. Values of sensitive `set_headers`
(Authorization, Cookie, X-Api-Key, …) are returned masked as `"***"`.

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

`saturated` means every concurrency slot was busy at least once, so lag and
corrected latency include client-side queueing (raise `concurrency`).
`corrected_latency` is measured from each request's intended send time and
corrects coordinated omission.

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

SSE events on `/api/v1/stream` (each `data:` is one JSON document):

- `event: stats` every second:
  `{"time":"...","snapshots":[StatsSnapshot],"points":{"service:svc_x":SeriesPoint,...}}`
- `event: services` every second (and immediately on connect): `[ServiceStatus]`
- `event: replays` every second while any job runs:
  `[{"id","status","summary":ReplaySummary}]`, and once more when a job finishes.
- `: ping` comment every 15 s.
