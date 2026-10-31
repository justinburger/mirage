// The standard set of live traffic charts shared by the dashboard and the
// service detail page.
import { TimeChart } from './chart.js';
import { fmtRate, fmtMsShort, fmtCompact, fmtPct, fmtMs } from './format.js';
import { h } from './dom.js';
import { card } from './ui.js';

const pctAxis = (v) => (Number.isInteger(v) || v >= 10 ? v.toFixed(0) : v >= 1 ? v.toFixed(1) : v.toFixed(2).replace(/0$/, '')) + '%';
const pctTip = (v) => fmtPct(v / 100);

export function trafficCharts({ windowMs = 15 * 60000, height = 190 } = {}) {
  const rps = new TimeChart({
    title: 'Requests per second', windowMs, height,
    series: [{ key: 'rps', label: 'req/s', color: 'c1', area: true }],
    yFormat: fmtRate, tipFormat: (v) => fmtCompact(v),
  });
  const lat = new TimeChart({
    title: 'Latency', windowMs, height,
    series: [{ key: 'p50', label: 'p50', color: 'c1' }, { key: 'p95', label: 'p95', color: 'c2' }, { key: 'p99', label: 'p99', color: 'c3' }],
    yFormat: fmtMsShort, tipFormat: fmtMs,
  });
  const err = new TimeChart({
    title: 'Error rate', windowMs, height,
    series: [{ key: 'err_pct', label: 'errors', color: 'err', area: true }],
    yFormat: pctAxis, tipFormat: pctTip, yCap: 100,
  });
  const inflight = new TimeChart({
    title: 'In flight', windowMs, height,
    series: [{ key: 'in_flight', label: 'requests', color: 'c4', area: true }],
    yFormat: fmtCompact, tipFormat: (v) => String(Math.round(v)),
  });
  const all = [rps, lat, err, inflight];
  // Across several services a percentile cannot be rebuilt from the
  // per-service ones (see series.js aggregateRows): the chart shows the
  // slowest service's p95 and p99, and says so.
  const latLabels = { p50: 'p50', p95: 'p95', p99: 'p99' };
  const latAggregate = {
    p50: ['p50 (avg)', 'p50 (average over services)'],
    p95: ['p95 (slowest)', 'p95 (slowest service)'],
    p99: ['p99 (slowest)', 'p99 (slowest service)'],
  };
  let aggregate = false;
  const el = h('div', { class: 'charts-2' },
    card({ body: rps.el }), card({ body: lat.el }), card({ body: err.el }), card({ body: inflight.el }));
  return {
    el, rps, lat, err, inflight, all,
    setRows: (rows) => all.forEach((c) => c.setRows(rows)),
    push: (row) => all.forEach((c) => c.push(row)),
    setWindow: (ms) => all.forEach((c) => c.setWindow(ms)),
    /** true while the rows combine several services (the dashboard's "All services"). */
    setAggregate: (on) => {
      if (on === aggregate) return;
      aggregate = on;
      lat.setSeriesLabels(on ? latAggregate : latLabels);
    },
    destroy: () => all.forEach((c) => c.destroy()),
  };
}

export const WINDOWS = [
  { value: 300000, label: '5m' },
  { value: 900000, label: '15m' },
  { value: 3600000, label: '1h' },
];

export function savedWindow(key, dflt = 900000) {
  try { const v = Number(localStorage.getItem(key)); if (WINDOWS.some((w) => w.value === v)) return v; } catch (_) { /* ignore */ }
  return dflt;
}
export function saveWindow(key, v) {
  try { localStorage.setItem(key, String(v)); } catch (_) { /* ignore */ }
}
