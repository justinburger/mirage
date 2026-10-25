// Shared helpers for the Mirage mock API server (dev only).
import { createHmac } from 'node:crypto';

// ---- deterministic randomness -------------------------------------------
export function mulberry32(seed) {
  let a = seed >>> 0;
  return function rng() {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

/** Stable pseudo-noise in [0,1) from two numbers. */
export function hash01(a, b = 0) {
  let h = (Math.imul(a | 0, 374761393) + Math.imul(b | 0, 668265263)) | 0;
  h = Math.imul(h ^ (h >>> 13), 1274126177);
  h ^= h >>> 16;
  return (h >>> 0) / 4294967296;
}

export function strHash(s) {
  let h = 2166136261;
  for (let i = 0; i < s.length; i++) { h ^= s.charCodeAt(i); h = Math.imul(h, 16777619); }
  return h >>> 0;
}

export const pick = (rng, arr) => arr[Math.floor(rng() * arr.length)];
export const randInt = (rng, lo, hi) => lo + Math.floor(rng() * (hi - lo + 1));
export function weighted(rng, items) {
  const total = items.reduce((s, i) => s + i.w, 0);
  let r = rng() * total;
  for (const i of items) { r -= i.w; if (r <= 0) return i; }
  return items[items.length - 1];
}
/** Log-normal-ish positive sample with the given median. */
export function lognorm(rng, median, sigma = 0.5) {
  let u = 0, v = 0;
  while (u === 0) u = rng();
  while (v === 0) v = rng();
  const z = Math.sqrt(-2 * Math.log(u)) * Math.cos(2 * Math.PI * v);
  return median * Math.exp(sigma * z);
}

// ---- ids -------------------------------------------------------------------
const B32 = 'abcdefghijklmnopqrstuvwxyz234567';
export function newId(prefix) {
  let s = '';
  for (let i = 0; i < 16; i++) s += B32[Math.floor(Math.random() * 32)];
  return `${prefix}_${s}`;
}
export function newSecret() {
  let s = '';
  for (let i = 0; i < 64; i++) s += '0123456789abcdef'[Math.floor(Math.random() * 16)];
  return s;
}

export function hmacHex(secret, value) {
  return createHmac('sha256', secret).update(String(value)).digest('hex');
}

// ---- Go durations --------------------------------------------------------
function trim(x, digits) {
  return String(Number(x.toFixed(digits)));
}

/** milliseconds -> Go duration string ("12.1ms", "1m30s"). */
export function fmtGoDuration(ms) {
  if (!Number.isFinite(ms) || ms <= 0) return '0s';
  if (ms < 0.001) return trim(ms * 1e6, 0) + 'ns';
  if (ms < 1) return trim(ms * 1000, 3) + 'µs';
  if (ms < 1000) return trim(ms, 6) + 'ms';
  let s = ms / 1000;
  const h = Math.floor(s / 3600); s -= h * 3600;
  const m = Math.floor(s / 60); s -= m * 60;
  let out = '';
  if (h) out += h + 'h';
  if (h || m) out += m + 'm';
  out += trim(s, 6) + 's';
  return out;
}

const UNITS = { ns: 1e-6, us: 1e-3, 'µs': 1e-3, 'μs': 1e-3, ms: 1, s: 1000, m: 60000, h: 3600000 };
/** Go duration string or number (ms) -> ms, or null when invalid. "" -> 0. */
export function parseGoDuration(v) {
  if (v === null || v === undefined || v === '') return 0;
  if (typeof v === 'number') return Number.isFinite(v) ? v : null;
  if (typeof v !== 'string') return null;
  const s = v.trim();
  if (s === '') return 0;
  if (/^[+-]?\d+(\.\d+)?$/.test(s)) return parseFloat(s);
  let sign = 1;
  let str = s;
  if (str[0] === '-') { sign = -1; str = str.slice(1); } else if (str[0] === '+') str = str.slice(1);
  const re = /(\d+(?:\.\d+)?)(ns|us|µs|μs|ms|s|m|h)/gy;
  let total = 0, pos = 0, m;
  while (pos < str.length && (m = re.exec(str))) { total += parseFloat(m[1]) * UNITS[m[2]]; pos = re.lastIndex; }
  if (pos !== str.length || pos === 0) return null;
  return sign * total;
}

export const iso = (ms) => new Date(ms).toISOString();

// ---- percentiles ---------------------------------------------------------
export function percentile(sorted, p) {
  if (!sorted.length) return 0;
  const i = Math.min(sorted.length - 1, Math.max(0, Math.ceil((p / 100) * sorted.length) - 1));
  return sorted[i];
}

export function latencySummary(values) {
  const n = values.length;
  if (!n) return { count: 0, min: 0, mean: 0, p50: 0, p90: 0, p95: 0, p99: 0, p999: 0, max: 0 };
  const s = values.slice().sort((a, b) => a - b);
  const sum = s.reduce((a, b) => a + b, 0);
  return { count: n, min: s[0], mean: sum / n, p50: percentile(s, 50), p90: percentile(s, 90), p95: percentile(s, 95), p99: percentile(s, 99), p999: percentile(s, 99.9), max: s[n - 1] };
}

export class HttpError extends Error {
  constructor(status, message, problems) {
    super(message);
    this.status = status;
    this.problems = problems;
  }
}
export const bad = (message, problems) => new HttpError(400, message, problems);

export function round(x, d = 2) {
  const f = 10 ** d;
  return Math.round(x * f) / f;
}

/** Smooth value noise in [0,1): interpolates hash01 at integer steps of `period` seconds. */
export function smooth01(ts, seed, period = 6) {
  const a = Math.floor(ts / period);
  const f = (ts / period) - a;
  const u = f * f * (3 - 2 * f);
  return hash01(a, seed) * (1 - u) + hash01(a + 1, seed) * u;
}
