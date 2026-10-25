// Number, duration, size and time formatting helpers.

const nf0 = new Intl.NumberFormat('en-US', { maximumFractionDigits: 0 });
const nf1 = new Intl.NumberFormat('en-US', { maximumFractionDigits: 1 });
const nf2 = new Intl.NumberFormat('en-US', { maximumFractionDigits: 2 });

export const DASH = '–';

const isNum = (n) => typeof n === 'number' && Number.isFinite(n);

/** 1234567 -> "1,234,567" */
export function fmtInt(n) {
  return isNum(n) ? nf0.format(Math.round(n)) : DASH;
}

/** Compact: 1.2k, 3.4M, 950. Keeps 1 decimal below 10 of the unit. */
export function fmtCompact(n) {
  if (!isNum(n)) return DASH;
  const a = Math.abs(n);
  if (a < 1000) return a < 10 && a !== Math.floor(a) ? nf1.format(n) : nf0.format(n);
  const units = [[1e12, 'T'], [1e9, 'B'], [1e6, 'M'], [1e3, 'k']];
  for (const [v, u] of units) {
    if (a >= v) {
      const x = n / v;
      return (Math.abs(x) < 10 ? nf1.format(x) : nf0.format(x)) + u;
    }
  }
  return nf0.format(n);
}

/** Requests per second: "1.2k", "48", "0.4" */
export function fmtRate(n) {
  if (!isNum(n)) return DASH;
  if (n === 0) return '0';
  if (Math.abs(n) < 1) return nf2.format(n);
  return fmtCompact(n);
}

/** 0.0123 -> "1.23%" ; handles tiny rates gracefully. */
export function fmtPct(frac, digits) {
  if (!isNum(frac)) return DASH;
  const p = frac * 100;
  if (digits != null) return p.toFixed(digits) + '%';
  if (p === 0) return '0%';
  const a = Math.abs(p);
  if (a >= 100) return nf0.format(p) + '%';
  if (a >= 10) return nf1.format(p) + '%';
  if (a >= 1) return nf2.format(p) + '%';
  if (a >= 0.01) return p.toFixed(2) + '%';
  return '<0.01%';
}

export function fmtBytes(n) {
  if (!isNum(n)) return DASH;
  const a = Math.abs(n);
  if (a < 1024) return nf0.format(n) + ' B';
  const units = ['KB', 'MB', 'GB', 'TB', 'PB'];
  let x = n / 1024;
  let i = 0;
  while (Math.abs(x) >= 1024 && i < units.length - 1) { x /= 1024; i++; }
  return (Math.abs(x) < 10 ? nf2.format(x) : Math.abs(x) < 100 ? nf1.format(x) : nf0.format(x)) + ' ' + units[i];
}

/** Latency in milliseconds with sensible precision. */
export function fmtMs(ms) {
  if (!isNum(ms)) return DASH;
  const a = Math.abs(ms);
  if (a === 0) return '0 ms';
  if (a < 0.01) return '<0.01 ms';
  if (a < 1) return ms.toFixed(2) + ' ms';
  if (a < 10) return ms.toFixed(1) + ' ms';
  if (a < 1000) return nf0.format(ms) + ' ms';
  if (a < 10000) return (ms / 1000).toFixed(2) + ' s';
  if (a < 60000) return (ms / 1000).toFixed(1) + ' s';
  return fmtHuman(ms);
}

/** Axis-friendly latency: "12ms", "1.5s" */
export function fmtMsShort(ms) {
  if (!isNum(ms)) return '';
  const a = Math.abs(ms);
  if (a === 0) return '0';
  if (a < 1) return ms.toFixed(2) + 'ms';
  if (a < 10) return (Math.round(ms * 10) / 10) + 'ms';
  if (a < 1000) return Math.round(ms) + 'ms';
  if (a < 60000) return (Math.round(ms / 100) / 10) + 's';
  return fmtHuman(ms);
}

/** Human duration: 90000 -> "1m 30s", 3700000 -> "1h 1m". */
export function fmtHuman(ms) {
  if (!isNum(ms)) return DASH;
  if (ms < 0) return '-' + fmtHuman(-ms);
  if (ms < 1000) return fmtMs(ms);
  let s = Math.round(ms / 1000);
  const d = Math.floor(s / 86400); s -= d * 86400;
  const hh = Math.floor(s / 3600); s -= hh * 3600;
  const m = Math.floor(s / 60); s -= m * 60;
  const parts = [];
  if (d) parts.push(d + 'd');
  if (hh) parts.push(hh + 'h');
  if (m) parts.push(m + 'm');
  if (s || !parts.length) parts.push(s + 's');
  return parts.slice(0, 2).join(' ');
}

const UNIT_MS = { ns: 1e-6, us: 1e-3, 'µs': 1e-3, 'μs': 1e-3, ms: 1, s: 1000, m: 60000, h: 3600000 };

/**
 * Parse a Go duration string ("1m30s", "250ms", "1.5s") to milliseconds.
 * A bare number means milliseconds (matches the API). Returns null if invalid.
 */
export function parseDuration(str) {
  if (str == null) return null;
  if (typeof str === 'number') return Number.isFinite(str) ? str : null;
  let s = String(str).trim();
  if (s === '') return null;
  if (/^[+-]?\d+(\.\d+)?$/.test(s)) return parseFloat(s);
  let sign = 1;
  if (s[0] === '-') { sign = -1; s = s.slice(1); } else if (s[0] === '+') s = s.slice(1);
  const re = /(\d+(?:\.\d+)?)(ns|us|µs|μs|ms|s|m|h)/gy;
  let total = 0;
  let pos = 0;
  let m;
  while (pos < s.length && (m = re.exec(s))) {
    total += parseFloat(m[1]) * UNIT_MS[m[2]];
    pos = re.lastIndex;
  }
  if (pos !== s.length || pos === 0) return null;
  return sign * total;
}

/** API duration string -> ms, tolerant ("" / null -> 0). */
export function durMs(v) {
  if (v == null || v === '') return 0;
  const n = parseDuration(v);
  return n == null ? 0 : n;
}

/** API duration string -> display ("12.1ms" -> "12.1 ms", "1m30s" -> "1m 30s"). */
export function fmtDur(v) {
  if (v == null || v === '') return DASH;
  const ms = parseDuration(v);
  return ms == null ? String(v) : fmtMs(ms);
}

/** ms -> Go duration string for requests: 1500 -> "1.5s", 90000 -> "1m30s". */
export function toGoDuration(ms) {
  if (!isNum(ms) || ms <= 0) return '0s';
  if (ms < 1000) return trimNum(ms) + 'ms';
  let s = ms / 1000;
  let out = '';
  const h = Math.floor(s / 3600); s -= h * 3600;
  const m = Math.floor(s / 60); s -= m * 60;
  if (h) out += h + 'h';
  if (m) out += m + 'm';
  if (s > 0 || !out) out += trimNum(s) + 's';
  return out;
}

function trimNum(x) {
  return String(Math.round(x * 1000) / 1000);
}

export function toDate(v) {
  if (v == null || v === '') return null;
  if (v instanceof Date) return v;
  const d = new Date(v);
  // Go's zero time (0001-01-01) means "unset"
  return Number.isNaN(d.getTime()) || d.getFullYear() < 1971 ? null : d;
}

export function relTime(v, now = Date.now()) {
  const d = toDate(v);
  if (!d) return DASH;
  const diff = (now - d.getTime()) / 1000;
  const a = Math.abs(diff);
  const fut = diff < 0;
  let s;
  if (a < 5) return 'just now';
  if (a < 60) s = Math.round(a) + ' sec';
  else if (a < 3600) s = Math.round(a / 60) + ' min';
  else if (a < 86400) { const x = Math.round(a / 3600); s = x + ' hr'; }
  else if (a < 86400 * 30) { const x = Math.round(a / 86400); s = x + (x === 1 ? ' day' : ' days'); }
  else if (a < 86400 * 365) { const x = Math.round(a / 86400 / 30); s = x + (x === 1 ? ' month' : ' months'); }
  else { const x = Math.round(a / 86400 / 365); s = x + (x === 1 ? ' year' : ' years'); }
  return fut ? 'in ' + s : s + ' ago';
}

const dtf = new Intl.DateTimeFormat(undefined, { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit' });
const tf = new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false });
const tfm = new Intl.DateTimeFormat(undefined, { hour: '2-digit', minute: '2-digit', hour12: false });

export function fmtDateTime(v) {
  const d = toDate(v);
  return d ? dtf.format(d) : DASH;
}
export function fmtTime(v, withSeconds = true) {
  const d = toDate(v);
  return d ? (withSeconds ? tf : tfm).format(d) : DASH;
}

/** elapsed seconds -> "m:ss" or "h:mm:ss" (for relative x axes). */
export function fmtClock(sec) {
  sec = Math.max(0, Math.round(sec));
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  const pad = (x) => String(x).padStart(2, '0');
  return h ? `${h}:${pad(m)}:${pad(s)}` : `${m}:${pad(s)}`;
}

export function plural(n, one, many) {
  return `${fmtInt(n)} ${n === 1 ? one : (many || one + 's')}`;
}

/** Signed delta text: "+12.3%" / "-4.1%". */
export function fmtDeltaPct(a, b) {
  if (!isNum(a) || !isNum(b)) return DASH;
  if (a === 0) return b === 0 ? '0%' : '+∞';
  const d = (b - a) / Math.abs(a) * 100;
  const s = Math.abs(d) >= 100 ? nf0.format(d) : Math.abs(d) >= 10 ? nf1.format(d) : nf2.format(d);
  return (d > 0 ? '+' : '') + s + '%';
}

/** Extract the TCP port from "[::]:9001", ":9001", "127.0.0.1:9001". */
export function portOf(addr) {
  if (!addr) return '';
  const m = String(addr).match(/:(\d+)$/);
  return m ? m[1] : '';
}

export function statusClass(status) {
  if (!status) return 'err';
  return String(Math.floor(status / 100)) + 'xx';
}

export function clamp(x, lo, hi) {
  return Math.min(hi, Math.max(lo, x));
}
