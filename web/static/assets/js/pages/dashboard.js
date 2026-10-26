// Dashboard: live overview of all traffic, services and replay jobs.
import { h } from '../dom.js';
import { icon } from '../icons.js';
import { state, refreshServices, refreshReplays, statusNotice } from '../state.js';
import { loadHistory, aggregateRows, aggregateEventPoints, pointToRow, getRows } from '../series.js';
import { trafficCharts, WINDOWS, savedWindow, saveWindow } from '../livecharts.js';
import { sparkline } from '../chart.js';
import { card, statTile, dataTable, modeBadge, btn, selectCtl, segmentedCtl, emptyState, progressBar, dot, replayBadge } from '../ui.js';
import { fmtRate, fmtPct, fmtMs, fmtInt, fmtCompact, DASH } from '../format.js';
import { replaceQuery } from '../router.js';

const WIN_KEY = 'mirage_dash_window';

export default async function dashboard(ctx) {
  ctx.setCrumbs([{ label: 'Dashboard' }]);
  await Promise.all([
    state.servicesLoaded ? null : refreshServices().catch(() => {}),
    state.replaysLoaded ? null : refreshReplays().catch(() => {}),
  ]);

  const root = h('div', { class: 'stack' });
  let live = null; // current live view {destroy, ...}
  let hadServices = state.services.length > 0;

  function build() {
    if (live) live.destroy();
    root.replaceChildren();
    if (!state.services.length) {
      live = buildEmpty(root);
    } else {
      live = buildLive(ctx, root);
    }
  }
  build();

  ctx.on('services', () => {
    const has = state.services.length > 0;
    if (has !== hadServices) { hadServices = has; build(); }
  });
  ctx.onCleanup(() => { if (live) live.destroy(); });
  return root;
}

// ---------------------------------------------------------------- empty
function buildEmpty(root) {
  const step = (n, title, text, icn) => h('div', { class: 'step' }, h('div', { class: 'step-title' }, icon(icn, 16), title), h('div', { class: 'step-text' }, text));
  root.appendChild(card({
    title: 'Getting started',
    subtitle: 'Mirage sits between your clients and an API. Four steps take you from live traffic to a 10x load test.',
    body: h('div', { class: 'stack' },
      h('div', { class: 'steps' },
        step(1, 'Put Mirage in front of your API', 'Create a service with a listen address and your API as the upstream. Point a client at it. Passthrough mode just proxies and measures.', 'services'),
        step(2, 'Record', 'Switch the service to Record. Every request and response is captured into a recording, with bodies and timing.', 'recordings'),
        step(3, 'Anonymize', 'Capture with an anonymization profile, or create an anonymized copy later. Emails, cards and tokens become consistent fakes.', 'shield'),
        step(4, 'Virtualize or replay', 'Serve the recording as a stand-in for the real API (add latency and errors), or replay it against staging at 10x the recorded rate.', 'replays')),
      h('div', { class: 'row' },
        btn('Create your first service', { icon: 'plus', kind: 'primary', href: '#/services/new' }),
        btn('Import a HAR file', { icon: 'upload', href: '#/recordings' }),
        btn('Read the quick tour', { icon: 'info', href: '#/settings' }))),
  }));
  return { destroy() {} };
}

// ----------------------------------------------------------------- live
function buildLive(ctx, root) {
  let scope = ctx.query.scope || 'all';
  let windowMs = savedWindow(WIN_KEY);
  const cleanups = [];

  // --- toolbar
  const scopeOptions = () => {
    const opts = [{ value: 'all', label: 'All services' }];
    for (const s of state.services) opts.push({ value: `service:${s.id}`, label: s.name });
    for (const r of state.replays) if (r.status === 'running') opts.push({ value: `replay:${r.id}`, label: `Replay: ${r.name}` });
    if (!opts.some((o) => o.value === scope)) opts.push({ value: scope, label: scope });
    return opts;
  };
  const scopeSel = selectCtl(scopeOptions(), { value: scope });
  const winSeg = segmentedCtl(WINDOWS, { value: windowMs, label: 'Time window' });
  winSeg.el.classList.add('window-seg');
  const liveNote = h('span', { class: 'live-note' }, dot('live'), 'Updating every second');
  root.appendChild(h('div', { class: 'toolbar' },
    h('div', { style: 'min-width:220px' }, scopeSel.el), winSeg.el, h('div', { class: 'grow' }), liveNote));

  // --- tiles
  const tRps = statTile({ label: 'Requests / s', value: DASH, sub: ' ', spark: null });
  const tErr = statTile({ label: 'Error rate', value: DASH, sub: ' ' });
  const tP95 = statTile({ label: 'p95 latency', value: DASH, sub: ' ' });
  const tSvc = statTile({ label: 'Active services', value: DASH, sub: ' ' });
  const tRep = statTile({ label: 'Running replays', value: DASH, sub: ' ' });
  const sparkRps = sparkline([], { width: 90, height: 28, color: 'c1' });
  tRps.el.appendChild(h('div', { class: 'tile-spark' }, sparkRps.el));
  root.appendChild(h('div', { class: 'tiles' }, tRps.el, tErr.el, tP95.el, tSvc.el, tRep.el));

  // --- charts
  const charts = trafficCharts({ windowMs });
  root.appendChild(charts.el);
  cleanups.push(() => charts.destroy());

  // --- services table + replays
  const spark = new Map();
  const getSpark = (id) => {
    let s = spark.get(id);
    if (!s) spark.set(id, (s = sparkline([], { width: 84, height: 24, color: 'c1' })));
    return s;
  };
  const svcTable = dataTable({
    rowKey: (r) => r.id,
    sortKey: 'name',
    onRowClick: (r) => { location.hash = `#/services/${encodeURIComponent(r.id)}`; },
    empty: () => emptyState({ icon: 'services', title: 'No services', text: 'Create a service to start proxying traffic.' }),
    columns: [
      { key: 'name', label: 'Service', sortValue: (r) => r.name.toLowerCase(), render: (r) => h('div', { class: 'cell-main' }, h('span', { class: 'row', style: 'gap:8px;flex-wrap:nowrap' }, stateDot(r), h('a', { class: 'cell-name', href: `#/services/${encodeURIComponent(r.id)}` }, r.name)), h('span', { class: 'cell-sub mono' }, r.upstream || 'no upstream')), sig: (r) => `${r.name}|${r.upstream}|${r.status.running}|${r.status.error}|${r.enabled}` },
      { key: 'mode', label: 'Mode', render: (r) => modeBadge(r.mode), sig: (r) => r.mode },
      { key: 'rps', label: 'Req/s', align: 'right', sortValue: (r) => r._m.rps, render: (r) => fmtRate(r._m.rps) },
      { key: 'p95', label: 'p95', align: 'right', sortValue: (r) => r._m.p95, render: (r) => (r._m.has ? fmtMs(r._m.p95) : DASH) },
      { key: 'err', label: 'Errors', align: 'right', sortValue: (r) => r._m.err, render: (r) => (r._m.has ? h('span', { class: r._m.err > 0.02 ? 'tone-err-text' : null, style: r._m.err > 0.02 ? 'color:var(--err)' : r._m.err > 0.005 ? 'color:var(--warn)' : null }, fmtPct(r._m.err)) : DASH) },
      { key: 'trend', label: 'Last 60 s', sortable: false, render: (r) => getSpark(r.id).el, sig: (r) => 'spark' },
    ],
  });
  const replaysHost = h('div', { class: 'stack-sm' });
  const replayRows = new Map();
  root.appendChild(h('div', { class: 'cols-main' },
    card({ title: 'Services', subtitle: 'Live per-second metrics', flush: true, actions: btn('Manage', { size: 'sm', kind: 'ghost', href: '#/services', iconRight: 'chevron-right' }), body: svcTable.el }),
    card({ title: 'Replays', subtitle: 'Running load tests', actions: btn('All replays', { size: 'sm', kind: 'ghost', href: '#/replays', iconRight: 'chevron-right' }), body: replaysHost })));

  // ------------------------------------------------------- data wiring
  const nameOf = (n) => n.split(':')[0];
  function metricsFor(svc) {
    const snap = state.snapshots[`service:${svc.id}`];
    if (!snap || !svc.status.running) return { rps: 0, p95: 0, err: 0, has: false };
    return { rps: snap.rps || 0, p95: snap.recent ? snap.recent.p95 : 0, err: snap.error_rate10 || 0, has: true };
  }
  function paintServices() {
    const rows = state.services.map((s) => ({ ...s, _m: metricsFor(s) }));
    svcTable.setRows(rows);
    for (const s of state.services) {
      const vals = getRows(`service:${s.id}`).slice(-60).map((r) => r.rps);
      getSpark(s.id).update(vals);
    }
  }

  function aggregateTiles() {
    let rps = 0, errW = 0, p95Sum = 0, p95W = 0, active = 0;
    const snaps = [];
    if (scope === 'all') {
      for (const s of state.services) {
        const sn = state.snapshots[`service:${s.id}`];
        if (sn && s.status.running) snaps.push(sn);
      }
    } else if (state.snapshots[scope]) snaps.push(state.snapshots[scope]);
    for (const sn of snaps) {
      rps += sn.rps || 0;
      errW += (sn.rps10 || 0) * (sn.error_rate10 || 0);
      const w = sn.recent ? sn.recent.count : 0;
      p95Sum += (sn.recent ? sn.recent.p95 : 0) * w;
      p95W += w;
    }
    const rps10 = snaps.reduce((a, s) => a + (s.rps10 || 0), 0);
    const errRate = rps10 > 0 ? errW / rps10 : 0;
    active = state.services.filter((s) => s.status.running).length;
    return { rps, errRate, p95: p95W > 0 ? p95Sum / p95W : 0, has: snaps.length > 0, active };
  }

  function paintTiles() {
    const t = aggregateTiles();
    tRps.set(t.has ? fmtRate(t.rps) : DASH, scope === 'all' ? `across ${fmtInt(state.services.filter((s) => s.status.running).length)} running services` : 'selected collector');
    tErr.set(t.has ? fmtPct(t.errRate) : DASH, t.has ? 'last 10 seconds' : ' ', !t.has ? '' : t.errRate > 0.05 ? 'err' : t.errRate > 0.01 ? 'warn' : 'ok');
    tP95.set(t.has ? fmtMs(t.p95) : DASH, t.has ? 'last 10 seconds' : ' ', '');
    const running = state.services.filter((s) => s.status.running).length;
    // A listener that failed is an error; a running service that reports a note (capture limit reached) is only a warning.
    const kinds = state.services.map((s) => (statusNotice(s.status) || {}).kind);
    const errored = kinds.filter((k) => k === 'failed').length;
    const noted = kinds.filter((k) => k === 'capture-limit' || k === 'runtime').length;
    tSvc.set(`${running} / ${state.services.length}`,
      errored ? `${errored} with errors` : noted ? `${noted} with warnings` : running === state.services.length ? 'all running' : `${state.services.length - running} stopped`,
      errored ? 'err' : noted ? 'warn' : '');
    const rr = state.replays.filter((r) => r.status === 'running');
    tRep.set(String(rr.length), `${fmtInt(state.replays.length)} total jobs`, '');
    sparkRps.update(rowsBuf.slice(-60).map((r) => r.rps));
  }

  function paintReplays() {
    const running = state.replays.filter((r) => r.status === 'running');
    const seen = new Set();
    for (const r of running) {
      seen.add(r.id);
      let v = replayRows.get(r.id);
      if (!v) {
        const bar = progressBar(0);
        const line = h('div', { class: 'muted tnum', style: 'font-size:12.5px' });
        const el = h('div', { class: 'stack-sm', style: 'padding-bottom:10px;border-bottom:1px solid var(--border)' },
          h('div', { class: 'row-between' }, h('a', { class: 'cell-name', href: `#/replays/${encodeURIComponent(r.id)}` }, r.name), replayBadge('running')),
          bar.el, line);
        v = { el, bar, line };
        replayRows.set(r.id, v);
        replaysHost.appendChild(el);
      }
      const s = r.summary;
      if (s) {
        const frac = s.planned > 0 ? s.sent / s.planned : 0;
        v.bar.set(frac, { indeterminate: s.planned <= 0 });
        // The live snapshot of the job's collector carries the corrected
        // latency: shown when queueing makes it differ from service time.
        const snap = state.snapshots[`replay:${r.id}`];
        const lat = snap && snap.latency && snap.latency.count ? snap.latency.p99 : 0;
        const cor = snap && snap.corrected_latency && snap.corrected_latency.count ? snap.corrected_latency.p99 : 0;
        const queued = lat > 0 && cor > lat * 1.25 && cor - lat > 10;
        v.line.textContent = `${fmtCompact(s.sent)}${s.planned > 0 ? ' / ' + fmtCompact(s.planned) : ''} requests · ${fmtRate(s.achieved_rps)}${s.target_rps ? ' of ' + fmtRate(s.target_rps) : ''} req/s · ${fmtPct(s.error_rate)} errors${lat ? ` · p99 ${fmtMs(lat)}${queued ? ` (${fmtMs(cor)} corrected)` : ''}` : ''}${s.saturated && r.config.mode !== 'max' ? ' · sender saturated' : ''}`;
      }
    }
    for (const [id, v] of replayRows) if (!seen.has(id)) { v.el.remove(); replayRows.delete(id); }
    if (!running.length) {
      if (!replaysHost.querySelector('.empty-mini')) {
        replaysHost.replaceChildren(h('div', { class: 'empty-mini muted', style: 'padding:6px 0 2px' },
          'No replay is running.', h('div', { style: 'margin-top:10px' }, btn('New replay', { icon: 'play', size: 'sm', kind: 'primary', href: '#/replays/new' }))));
      }
    } else {
      const m = replaysHost.querySelector('.empty-mini');
      if (m) m.remove();
    }
  }

  // rows for the active scope (chart data) -------------------------------
  let rowsBuf = [];
  async function loadScope() {
    const myScope = scope;
    let names;
    if (scope === 'all') names = state.services.map((s) => ['service', s.id]);
    else { const [k, id] = scope.split(/:(.*)/); names = [[k, id]]; }
    const lists = await Promise.all(names.map(([k, id]) => loadHistory(k, id, 3600 * 1000).catch(() => [])));
    if (myScope !== scope || !ctx.isActive()) return;
    rowsBuf = scope === 'all' ? aggregateRows(lists) : (lists[0] || []).slice();
    charts.setRows(rowsBuf);
    paintTiles();
  }

  const sub = (topic, fn) => cleanups.push(ctx.on(topic, fn));
  sub('stats', (evt) => {
    let row = null;
    if (scope === 'all') row = aggregateEventPoints(evt.points, (n) => n.startsWith('service:'));
    else if (evt.points[scope]) row = pointToRow(evt.points[scope]);
    if (row) {
      if (!rowsBuf.length || row.t > rowsBuf[rowsBuf.length - 1].t) rowsBuf.push(row);
      if (rowsBuf.length > 4000) rowsBuf.shift();
      charts.push(row);
    }
    paintTiles();
    paintServices();
    paintReplays();
  });
  sub('services', () => { paintServices(); paintTiles(); refreshScopeOptions(); });
  sub('replays', () => { paintReplays(); paintTiles(); });

  function refreshScopeOptions() {
    scopeSel.setOptions(scopeOptions(), true);
    scopeSel.set(scope);
  }
  scopeSel.onChange((v) => {
    scope = v;
    replaceQuery(v === 'all' ? {} : { scope: v });
    rowsBuf = [];
    charts.setRows([]);
    loadScope();
  });
  winSeg.onChange((v) => { windowMs = v; saveWindow(WIN_KEY, v); charts.setWindow(v); });

  paintServices();
  paintTiles();
  paintReplays();
  loadScope();
  void nameOf;

  return { destroy() { cleanups.forEach((f) => f()); } };
}

function stateDot(r) {
  const st = r.status;
  const n = statusNotice(st);
  if (st.running) return n ? dot('warn', `Running. ${n.text}`) : dot('live', 'Running');
  if (n) return dot('err', n.text);
  return dot('off', r.enabled ? 'Stopped' : 'Disabled');
}
