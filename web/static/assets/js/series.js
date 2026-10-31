// Client-side time-series buffers fed by the history endpoints and the SSE
// `stats` event. A series "name" is the collector name: "service:<id>" or
// "replay:<id>". Rows are chart-ready (see pointToRow).
import { api } from './api.js';

const MAX_ROWS = 4000; // a bit over an hour at 1 point/second
const buffers = new Map(); // name -> {rows: Row[], loaded: Promise|null}
const subs = new Set();

/** SeriesPoint -> chart row. */
export function pointToRow(p) {
  const req = p.requests || 0;
  const err = p.errors || 0;
  return {
    t: p.t,
    rps: req,
    errors: err,
    err_pct: req > 0 ? (err / req) * 100 : 0,
    p50: p.p50 || 0,
    p95: p.p95 || 0,
    p99: p.p99 || 0,
    max: p.max || 0,
    in_flight: p.in_flight || 0,
    bytes_in: p.bytes_in || 0,
    bytes_out: p.bytes_out || 0,
    // Replay jobs only: latency from each request's intended send time. Left
    // undefined for services so charts skip the series.
    cp95: p.corrected_p95 != null ? p.corrected_p95 : undefined,
    cp99: p.corrected_p99 != null ? p.corrected_p99 : undefined,
  };
}

function buf(name) {
  let b = buffers.get(name);
  if (!b) buffers.set(name, (b = { rows: [], loaded: null }));
  return b;
}

function insertRow(b, row) {
  const rows = b.rows;
  const n = rows.length;
  if (!n || row.t > rows[n - 1].t) rows.push(row);
  else if (row.t === rows[n - 1].t) rows[n - 1] = row;
  else {
    // out-of-order or backfill: binary search
    let lo = 0, hi = n;
    while (lo < hi) { const mid = (lo + hi) >> 1; if (rows[mid].t < row.t) lo = mid + 1; else hi = mid; }
    if (rows[lo] && rows[lo].t === row.t) rows[lo] = row;
    else rows.splice(lo, 0, row);
  }
  if (rows.length > MAX_ROWS) rows.splice(0, rows.length - MAX_ROWS);
}

/** Called by sse.js for every stats event. */
export function ingestStats(evt) {
  const pts = evt && evt.points;
  if (!pts) return;
  for (const name of Object.keys(pts)) {
    const row = pointToRow(pts[name]);
    insertRow(buf(name), row);
    for (const fn of subs) fn(name, row);
  }
}

export function getRows(name) {
  return buf(name).rows;
}

export function subscribe(fn) {
  subs.add(fn);
  return () => subs.delete(fn);
}

/** Replace/merge a whole history (from GET .../series or a replay job). */
export function mergeHistory(name, points) {
  const b = buf(name);
  for (const p of points || []) insertRow(b, pointToRow(p));
}

/**
 * Loads history once per name (sinceMs ago). Resolves to the row array.
 * kind: 'service' | 'replay'.
 */
export function loadHistory(kind, id, sinceMs = 3600 * 1000) {
  const name = `${kind}:${id}`;
  const b = buf(name);
  if (!b.loaded) {
    const since = Date.now() - sinceMs;
    const call = kind === 'service' ? api.serviceSeries(id, since) : api.replaySeries(id, since);
    b.loaded = call.then((pts) => {
      mergeHistory(name, pts);
      return b.rows;
    }).catch((e) => { b.loaded = null; throw e; });
  }
  return b.loaded.then(() => b.rows);
}

export function resetSeries(name) {
  buffers.delete(name);
}

/**
 * Combines the rows of several series at equal timestamps (for "all
 * services"): counts add up. A percentile of the combined traffic cannot be
 * rebuilt from per-series percentiles, and a request-weighted mean of p95s is
 * not a p95 (one service at 800 ms with half the traffic and one at 100 ms
 * average to about 450 ms, while the combined p95 is about 800 ms). So p95 and p99
 * are the worst series' value, an upper bound that names the slowest service;
 * p50 stays a request-weighted mean, a typical value. The charts say so.
 */
export function aggregateRows(rowLists) {
  const byT = new Map();
  for (const rows of rowLists) {
    for (const r of rows) {
      let a = byT.get(r.t);
      if (!a) byT.set(r.t, (a = { t: r.t, rps: 0, errors: 0, w: 0, p50: 0, p95: 0, p99: 0, max: 0, in_flight: 0, bytes_in: 0, bytes_out: 0 }));
      addInto(a, r);
    }
  }
  const out = Array.from(byT.values()).sort((x, y) => x.t - y.t);
  for (const a of out) finishAgg(a);
  return out;
}

export function addInto(a, r) {
  a.rps += r.rps;
  a.errors += r.errors;
  a.in_flight += r.in_flight;
  a.bytes_in += r.bytes_in;
  a.bytes_out += r.bytes_out;
  const w = r.rps || 0;
  a.w += w;
  a.p50 += r.p50 * w;
  if (w > 0) {
    a.p95 = Math.max(a.p95, r.p95);
    a.p99 = Math.max(a.p99, r.p99);
  }
  a.max = Math.max(a.max, r.max);
  return a;
}

/** Turn the weighted p50 sum into a mean; p95 and p99 already hold the worst series. */
export function finishAgg(a) {
  if (a.w > 0) a.p50 /= a.w;
  else a.p50 = a.p95 = a.p99 = 0;
  a.err_pct = a.rps > 0 ? (a.errors / a.rps) * 100 : 0;
  delete a.w;
  return a;
}

/** Aggregate one SSE event's service points into a single row. */
export function aggregateEventPoints(points, filter) {
  let a = null;
  for (const name of Object.keys(points || {})) {
    if (filter && !filter(name)) continue;
    const row = pointToRow(points[name]);
    if (!a) a = { t: row.t, rps: 0, errors: 0, w: 0, p50: 0, p95: 0, p99: 0, max: 0, in_flight: 0, bytes_in: 0, bytes_out: 0 };
    addInto(a, row);
  }
  return a ? finishAgg(a) : null;
}
