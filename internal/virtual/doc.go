// Package virtual serves recorded responses in place of a real upstream
// (service virtualization), with realistic latency and controllable error
// rates.
//
// CONTRACT (implemented by the virtual agent):
//
// Index. Build streams the recording (store.IterateExchanges with request
// and response bodies) into an in-memory index with three levels (see
// model.MatchConfig):
//
//	exact    method + path + query key + body key + selected header values
//	path     method + path
//	template method + endpoint template (only if Match.PathTemplates)
//
// Query key per Match.Query: exact → params sorted by name then value,
// minus IgnoreParams; ignore → empty; subset → empty at the exact level, and
// candidates found at the "path" level are then filtered to those whose
// recorded params (minus IgnoreParams) are all present with equal values in
// the request. Body key per Match.Body: ignore → empty; exact → hash of
// bytes; json → hash of canonical JSON (keys sorted, insignificant
// whitespace removed, numbers kept as literals) or of the bytes when the
// body is not JSON. Use a fast non-cryptographic hash (hash/maphash with a
// per-engine seed). Exchanges whose Error is set (no upstream response) are
// not served as normal matches but do count toward error rates. Candidate
// lists (and sequential cursors) follow (StartedAt, Seq) order, because Seq
// is completion order, not arrival order. Template-level keys and endpoint
// pools use the stored Exchange.Endpoint when non-empty (it was computed from
// the raw path) and compute it with the Normalizer only otherwise. A stored
// endpoint that is not one of the Normalizer's own patterns may come from
// another normalizer (HAR import, endpoint_patterns edited after recording),
// so the exchange is also indexed under the Normalizer's template of its path;
// a stored endpoint that is one of the Normalizer's patterns is authoritative.
//
// Ownership and immutability: Add takes ownership of the exchange it is
// given (callers do not touch it afterwards; Build's exchanges come fresh
// from the store). Indexed exchanges are immutable: Lookup and Respond never
// mutate ex.Header or ex.Body (Date, Content-Length and X-Mirage-* go to the
// response header map only). The gzip cache is built at most once per
// exchange (sync.Once). Add after Close is a no-op. An exchange that Build
// already read from the store (ID not above the highest ID Build saw) is
// ignored by Add, so a capture stored while the engine was being built is not
// indexed twice.
//
// The index keeps a trimmed copy of each exchange: Match.Exchange has no
// request headers, no request body and no response header map (the response
// headers live in the item, shared between items that have identical sets).
// The exchange passed to Add is not modified.
//
// Lookup is the hot path (target ≥ 50k lookups/s per core, no allocation
// beyond key building). Avoid one global RWMutex: shard the index (e.g. 64
// shards by key hash, one RWMutex each; Add takes only its shard's write
// lock) or use copy-on-write snapshots behind an atomic pointer. Selection among candidates
// per Match.Selection: sequential keeps an atomic cursor per key and cycles;
// random picks uniformly; first always returns the first recorded.
//
// Error amplification. At Build, for each recorded endpoint compute the
// error rate e (model.Exchange.IsError) and keep pools of its error and
// success exchanges. Target rate t = min(1, max(e × ErrorAmplification,
// ErrorFloor)). When a matched response is a success and t > e, flip it to an
// error with probability (t − e) / (1 − e): serve a random recorded error
// exchange of the same endpoint, or if there is none a synthetic 503 with
// JSON body {"error":"service unavailable (injected by mirage)"}. When the
// match is an error and t < e, flip to a random recorded success with
// probability 1 − t/e (when a success exists). Mark the Match Injected.
//
// Writing a response (Respond / Serve):
//   - Latency per model.LatencyConfig: recorded → wait TTFB × Multiplier,
//     write and flush the headers, wait the rest of (Duration − TTFB) ×
//     Multiplier, then write the body; fixed → wait Fixed, then respond;
//     Jitter adds uniform ±Jitter to the total (never negative). Waits stop
//     early if the request context is cancelled (then return without writing).
//   - Copy recorded headers except hop-by-hop headers (Connection,
//     Keep-Alive, Proxy-*, Te, Trailer, Transfer-Encoding, Upgrade),
//     Content-Length (recomputed) and Date (set to now).
//   - If the recorded response had ContentEncoding "gzip" and the request
//     accepts gzip, serve it gzip-compressed with Content-Encoding: gzip,
//     compressing each exchange's body once and caching the result.
//   - Add "X-Mirage-Match: exact|path|template" and, for amplified errors,
//     "X-Mirage-Injected: error".
//   - HEAD requests get headers only.
//
// Misses: Serve returns false and writes nothing; WriteMiss writes
// Match.MissStatus with a JSON body explaining the miss
// ({"error":"no recorded response matches this request","method":…,
// "path":…,"recording_id":…}) and "X-Mirage-Match: miss".
//
// Truncated recordings. A recording whose Response.BodyTruncated is set holds
// only part of its body (it exceeded max_body_bytes at capture, or was cut in
// an imported HAR). It is never served as if it were complete: a prefix with
// a matching Content-Length would look like the real response. Such
// exchanges stay in the index, so the request still matches, but when an
// entry holds any, selection picks among its complete candidates only (the
// rotation behaves as if the truncated ones were absent) and falls back to a
// truncated one only when nothing else matches. They are left out of the
// error-amplification pools. With Options.TruncatedAsMiss (hybrid services) a
// truncated-only match is a miss, so the proxy forwards the request and
// records it again. Otherwise Respond answers 502 with
// "X-Mirage-Error: truncated", the match level, and a JSON body ({"error":…,
// "method":…,"path":…,"recording_id":…,"exchange_id":…,"recorded_bytes":…,
// "recorded_bytes_exact":…,"kept_bytes":…}); Stats.Truncated counts these. A
// recording that kept no body at all (max_body_bytes -1, a HAR entry without
// content) is served as recorded, empty.
//
// Memory. If the recording's captured bytes exceed MaxResidentBytes, keep
// only the index and request-side keys in memory and load response bodies on
// demand with store.GetExchange through an LRU cache bounded by
// MaxResidentBytes. Below the threshold everything stays resident.
//
// Hybrid mode: Add inserts a newly captured exchange into all levels and
// the endpoint pools while serving continues (write lock).
//
// # Implementation notes
//
// Decisions the contract leaves open, all covered by tests:
//
//   - Index. Keys are byte strings built on the stack
//     ("e"+method+path+query+body+headers, "p"+method+path, "t"+endpoint) and
//     looked up with m[string(key)], which does not allocate. The index is 64
//     shards chosen by a maphash of the key, each with its own RWMutex and
//     map. A candidate list is replaced, never edited in place, once a reader
//     can see it: Lookup copies the slice header under the read lock and
//     works on it lock-free. Add appends in the common case (a newer exchange
//     sorts last) and copies the list only to insert in the middle. Build
//     appends without ordering and sorts each list once at the end.
//   - Subset queries filter the candidates of every level (exact, path and
//     template), not only "path": the exact-level key has no query part, so
//     without the filter "subset" would behave like "ignore".
//   - When the path level would be identical to the exact level (query is not
//     "exact", body is "ignore" and no headers are matched) it is not indexed
//     twice, and such matches are reported as "exact".
//   - A HEAD request without a recorded HEAD exchange is answered from the GET
//     recording, headers only.
//   - Sequential cursors live on the index entry of the key the candidates
//     were found under and advance on every hit there. Under a subset filter
//     the cursor selects among the candidates that pass the filter.
//   - Counters are striped by key hash and padded, so concurrent Lookups
//     of different keys never share a cache line.
//   - Random choices use math/rand/v2 unless Options.Seed is set; a seeded
//     engine draws from a counter-based splitmix64 stream, so a single
//     goroutine sees a reproducible sequence.
//   - Error amplification pools are per endpoint (the stored endpoint of the
//     matched exchange) with atomic counters, so it costs no lock. Both
//     directions (adding and removing errors) mark the Match Injected;
//     Stats.Injected counts only the injected errors (the responses that carry
//     X-Mirage-Injected: error), the errors swapped for successes are counted
//     by Engine.Suppressed. An injected synthetic 503 reuses the latency of the
//     exchange that was matched.
//   - Build asks the store only for what it needs: request bodies when the
//     body is matched on, response bodies when they stay resident. With
//     Match.Body "ignore" the exchanges in Match.Exchange therefore carry no
//     request body.
//   - Lazy mode (large recordings, or an engine whose resident bodies would
//     outgrow MaxResidentBytes in hybrid mode) keeps a trimmed copy of each
//     exchange without bodies and loads a response body by ID the first time
//     it is served. Loads of one body are shared between concurrent requests.
//
// Limits: a "subset" query scans the candidate list of the key it matches
// (cost proportional to its length); Add inserts in the middle of a list by
// copying it (appending the newest exchange, the usual case, does not copy);
// bodies cut by the capture limit are served cut, with a Content-Length that
// matches what is sent; Len counts exchanges that failed without a response
// although they are never served.
package virtual
