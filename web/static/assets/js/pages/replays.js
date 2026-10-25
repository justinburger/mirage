// Replays: job list, live job detail, and the side-by-side compare view.
import { h } from '../dom.js';
import { icon } from '../icons.js';
import { api } from '../api.js';
import { state, refreshReplays } from '../state.js';
import { navigate, replaceQuery } from '../router.js';
import { mergeHistory, getRows, pointToRow } from '../series.js';
import { TimeChart, statusBar, groupStatus } from '../chart.js';
import { card, btn, iconBtn, dataTable, emptyState, confirmDialog, toastOk, replayBadge, progressBar, kvList, alertBox, selectCtl, popMenu, badge, pageHeader, collapsible, toastError } from '../ui.js';
import { fmtInt, fmtCompact, fmtRate, fmtPct, fmtMs, fmtMsShort, fmtBytes, fmtDur, durMs, relTime, fmtHuman, DASH } from '../format.js';

export { create } from './replay-form.js';

const DONE = (s) => s === 'completed' || s === 'stopped' || s === 'failed';

async function startJob(job) {
  await api.startReplay(job.id);
  await refreshReplays();
  toastOk(`Started ${job.name}`);
}
async function stopJob(job) {
  await api.stopReplay(job.id);
  await refreshReplays();
  toastOk(`Stopped ${job.name}`);
}
async function deleteJob(job) {
  const ok = await confirmDialog({
    title: `Delete ${job.name}?`, danger: true, confirmText: 'Delete replay',
    message: 'The job, its summary and its per-second series are permanently removed.',
  });
  if (!ok) return false;
  await api.deleteReplay(job.id);
  await refreshReplays();
  toastOk(`Deleted ${job.name}`);
  return true;
}
async function cloneJob(job) {
  const copy = await api.cloneReplay(job.id, {});
  await refreshReplays();
  toastOk(`Cloned as ${copy.name}`);
  navigate(`/replays/${encodeURIComponent(copy.id)}`);
}

function progressInfo(job) {
  const s = job.summary;
  if (!s) return { frac: 0, text: job.status === 'pending' ? 'not started' : DASH, indeterminate: false };
  if (s.planned > 0) {
    const frac = Math.min(1, s.sent / s.planned);
    return { frac: DONE(job.status) && job.status === 'completed' ? 1 : frac, text: `${fmtCompact(s.sent)} / ${fmtCompact(s.planned)}`, indeterminate: false };
  }
  const d = durMs(job.config.duration);
  const el = durMs(s.elapsed);
  if (d > 0) return { frac: Math.min(1, el / d), text: `${fmtCompact(s.sent)} sent`, indeterminate: false };
  return { frac: 0, text: `${fmtCompact(s.sent)} sent`, indeterminate: job.status === 'running' };
}

// ------------------------------------------------------------------ list
export async function list(ctx) {
  ctx.setCrumbs([{ label: 'Replays' }]);
  const [, recs] = await Promise.all([refreshReplays(), api.recordings().catch(() => [])]);
  const recName = new Map(recs.map((r) => [r.id, r.name]));
  const root = h('div', { class: 'stack' });
  root.appendChild(pageHeader({
    subtitle: 'Replay recordings against a target as load tests: at 10x speed, along a ramp, or as fast as it will go.',
    actions: [btn('Compare', { icon: 'compare', href: '#/replays/compare' }), btn('New replay', { icon: 'plus', kind: 'primary', href: '#/replays/new' })],
  }));
  const bars = new Map();
  const getBar = (id) => {
    let b = bars.get(id);
    if (!b) {
      const bar = progressBar(0);
      const txt = h('span', { class: 'progress-txt' });
      b = { bar, txt, el: h('div', { class: 'progress-cell' }, bar.el, txt) };
      bars.set(id, b);
    }
    return b;
  };
  const table = dataTable({
    rowKey: (r) => r.id, sortKey: 'created', sortDir: 'desc',
    onRowClick: (r) => navigate(`/replays/${encodeURIComponent(r.id)}`),
    empty: () => emptyState({
      icon: 'replays', title: 'No replays yet',
      text: 'A replay sends a recording to a target at a chosen speed. Try 1x first to validate the setup, then ramp up.',
      actions: [btn('New replay', { icon: 'plus', kind: 'primary', href: '#/replays/new' })],
    }),
    columns: [
      { key: 'name', label: 'Replay', sortValue: (r) => r.name.toLowerCase(), render: (r) => h('div', { class: 'cell-main' }, h('a', { class: 'cell-name', href: `#/replays/${encodeURIComponent(r.id)}` }, r.name), h('span', { class: 'cell-sub' }, `${recName.get(r.config.recording_id) || r.config.recording_id} → ${r.config.target}`)), sig: (r) => r.name + r.config.target },
      { key: 'status', label: 'Status', render: (r) => replayBadge(r.status), sig: (r) => r.status },
      { key: 'mode', label: 'Load', cls: 'hide-md', render: (r) => h('span', { class: 'muted nowrap' }, modeSummary(r.config)), sig: (r) => modeSummary(r.config) },
      { key: 'progress', label: 'Progress', sortable: false, width: '170px', render: (r) => { const b = getBar(r.id); const p = progressInfo(r); b.bar.set(p.frac, { indeterminate: p.indeterminate, tone: r.status === 'failed' ? 'err' : r.status === 'completed' ? 'ok' : '' }); b.txt.textContent = p.text; return b.el; }, sig: (r) => `p${r.id}` },
      { key: 'rate', label: 'Achieved / target', align: 'right', sortValue: (r) => (r.summary ? r.summary.achieved_rps : 0), render: (r) => (r.summary && r.summary.sent ? `${fmtRate(r.summary.achieved_rps)} / ${r.summary.target_rps ? fmtRate(r.summary.target_rps) : 'max'}` : DASH) },
      { key: 'p95', label: 'p95', align: 'right', sortValue: (r) => (r.summary ? r.summary.latency.p95 : 0), render: (r) => (r.summary && r.summary.completed ? fmtMs(r.summary.latency.p95) : DASH) },
      { key: 'err', label: 'Errors', align: 'right', sortValue: (r) => (r.summary ? r.summary.error_rate : 0), render: (r) => (r.summary && r.summary.completed ? h('span', { style: r.summary.error_rate > 0.02 ? 'color:var(--err)' : r.summary.error_rate > 0.005 ? 'color:var(--warn)' : null }, fmtPct(r.summary.error_rate)) : DASH) },
      { key: 'created', label: 'Started', sortValue: (r) => Date.parse(r.started_at || r.created_at), render: (r) => h('div', { class: 'cell-main' }, h('span', null, r.started_at ? relTime(r.started_at) : 'not started'), r.summary && r.summary.elapsed ? h('span', { class: 'cell-sub' }, `ran ${fmtDur(r.summary.elapsed)}`) : null) },
      {
        key: 'actions', label: '', sortable: false, cls: 'actions',
        render: (r) => h('div', { class: 'cell-actions' },
          r.status === 'running' ? iconBtn('stop', 'Stop replay', () => stopJob(r)) : iconBtn('play', r.status === 'pending' ? 'Start replay' : 'Run again', () => startJob(r)),
          iconBtn('copyplus', 'Clone replay', () => cloneJob(r)),
          iconBtn('trash', 'Delete replay', () => deleteJob(r), { disabled: r.status === 'running' })),
        sig: (r) => `${r.id}|${r.status}`,
      },
    ],
  });
  root.appendChild(card({ flush: true, body: table.el }));
  const paint = () => table.setRows(state.replays.slice());
  paint();
  ctx.on('replays', paint);
  return root;
}

function modeSummary(c) {
  if (c.mode === 'timeline') return `timeline ${c.speed || 1}×${c.amplify > 1 ? ` · ${c.amplify} copies` : ''}`;
  if (c.mode === 'rate') return c.stages && c.stages.length ? `ramp, ${c.stages.length} stages` : `${fmtRate(c.rate)} req/s`;
  return `max, ${c.concurrency || 256} workers`;
}

// ---------------------------------------------------------------- detail
function targetFn(job) {
  const c = job.config;
  if (c.mode === 'rate') {
    if (c.stages && c.stages.length) {
      const st = c.stages.map((s) => ({ d: durMs(s.duration) / 1000, rate: s.rate }));
      return (sec) => { let acc = 0, prev = 0; for (const s of st) { if (sec <= acc + s.d) return prev + (s.rate - prev) * ((sec - acc) / s.d); acc += s.d; prev = s.rate; } return prev; };
    }
    return () => c.rate || 0;
  }
  return () => (job.summary && job.summary.target_rps) || 0;
}

const LAT_ROWS = [['min', 'Min'], ['mean', 'Mean'], ['p50', 'p50'], ['p90', 'p90'], ['p95', 'p95'], ['p99', 'p99'], ['p999', 'p99.9'], ['max', 'Max']];

export async function detail(ctx) {
  const id = ctx.params.id;
  let job = await api.replay(id);
  ctx.setCrumbs([{ label: 'Replays', href: '#/replays' }, { label: job.name }]);
  const rec = await api.recording(job.config.recording_id).catch(() => null);
  const name = `replay:${job.id}`;
  mergeHistory(name, job.series || []);

  const root = h('div', { class: 'stack' });
  const headHost = h('div');
  const bannerHost = h('div');
  const progHost = h('div');
  const statsHost = h('div');
  const latHost = h('div');
  const chartsHost = h('div');
  const statusHost = h('div');
  const epHost = h('div');
  const cfgHost = h('div');
  root.append(headHost, bannerHost, progHost, statsHost, latHost, chartsHost, statusHost, epHost, cfgHost);

  // ---- header
  function paintHeader() {
    const running = job.status === 'running';
    const menuBtn = iconBtn('more', 'More actions', () => {});
    menuBtn.addEventListener('click', () => popMenu(menuBtn, [
      { label: 'Edit as new replay', icon: 'edit', onClick: () => navigate('/replays/new', { from: job.id }) },
      { label: 'Compare with another run', icon: 'compare', onClick: () => navigate('/replays/compare', { a: job.id }) },
      { label: 'Open recording', icon: 'recordings', onClick: () => navigate(`/recordings/${encodeURIComponent(job.config.recording_id)}`) },
    ]));
    headHost.replaceChildren(h('div', { class: 'page-head', style: 'margin-bottom:0' },
      h('div', { class: 'page-titles' },
        h('h2', { class: 'page-title' }, job.name, replayBadge(job.status)),
        h('p', { class: 'page-sub' }, rec ? h('a', { href: `#/recordings/${encodeURIComponent(rec.id)}` }, rec.name) : job.config.recording_id, ' → ', h('code', null, job.config.target), ` · ${modeSummary(job.config)}`,
          job.started_at ? ` · started ${relTime(job.started_at)}` : '')),
      h('div', { class: 'page-actions' },
        running ? btn('Stop', { icon: 'stop', kind: 'danger', onClick: async () => { await stopJob(job); await reload(); } })
          : btn(job.status === 'pending' ? 'Start' : 'Run again', { icon: 'play', kind: 'primary', onClick: async () => { await startJob(job); await reload(true); } }),
        btn('Clone', { icon: 'copyplus', onClick: () => cloneJob(job) }),
        btn('Compare', { icon: 'compare', href: `#/replays/compare?a=${encodeURIComponent(job.id)}` }),
        menuBtn,
        btn('Delete', { icon: 'trash', kind: 'danger', disabled: running, onClick: async () => { if (await deleteJob(job)) navigate('/replays'); } }))));
    bannerHost.replaceChildren(job.error ? alertBox('error', job.error, { title: 'This replay failed' }) : '');
  }

  // ---- progress
  const bar = progressBar(0);
  const progText = h('div', { class: 'row-between', style: 'font-size:13px' });
  progHost.appendChild(card({ body: h('div', { class: 'stack-sm' }, progText, bar.el) }));
  bar.el.classList.add('progress-lg');
  function paintProgress() {
    const p = progressInfo(job);
    const s = job.summary;
    bar.set(p.frac, { indeterminate: p.indeterminate, tone: job.status === 'failed' ? 'err' : job.status === 'completed' ? 'ok' : job.status === 'stopped' ? 'warn' : '' });
    let eta = '';
    if (job.status === 'running' && s && s.planned > 0 && s.sent > 0 && durMs(s.elapsed) > 0) {
      const remain = ((s.planned - s.sent) / (s.sent / durMs(s.elapsed)));
      eta = ` · about ${fmtHuman(remain)} left`;
    }
    progText.replaceChildren(
      h('span', null, s ? `${fmtInt(s.sent)} sent${s.planned > 0 ? ` of ${fmtInt(s.planned)} planned (${fmtPct(p.frac, 0)})` : ' (open-ended run)'}${eta}` : 'Not started yet'),
      h('span', { class: 'muted' }, s ? `elapsed ${fmtDur(s.elapsed)}` : ''));
  }

  // ---- summary cards
  const cell = (k, v, sub, tone) => h('div', { class: 'stat-cell' }, h('div', { class: 'stat-k' }, k), h('div', { class: ['stat-v', tone] }, v), h('div', { class: 'stat-s' }, sub || ' '));
  function paintStats() {
    const s = job.summary;
    if (!s) { statsHost.replaceChildren(); return; }
    statsHost.replaceChildren(h('div', { class: 'stat-grid cols-4' },
      cell('Achieved rate', `${fmtRate(s.achieved_rps)} /s`, s.target_rps ? `target ${fmtRate(s.target_rps)} /s (${fmtPct(s.achieved_rps / s.target_rps, 0)})` : 'closed loop (no target)', s.target_rps && s.achieved_rps / s.target_rps < 0.9 ? 'warn' : ''),
      cell('Error rate', fmtPct(s.error_rate), `${fmtInt(s.errors)} errors`, s.error_rate > 0.05 ? 'err' : s.error_rate > 0.01 ? 'warn' : 'ok'),
      cell('Completed', fmtInt(s.completed), `${fmtInt(s.failed)} failed, ${fmtInt(s.timeouts)} timeouts`, s.failed ? 'warn' : ''),
      cell('Status mismatches', job.config.compare_status ? fmtInt(s.mismatches) : 'off', job.config.compare_status ? 'differ from the recording' : 'enable compare_status', job.config.compare_status && s.mismatches ? 'warn' : ''),
      cell('Max lag', fmtDur(s.max_lag), 'intended vs actual send time', durMs(s.max_lag) > 100 ? 'warn' : ''),
      cell('Sent', fmtBytes(s.bytes_sent), 'request bytes'),
      cell('Received', fmtBytes(s.bytes_received), 'response bytes'),
      cell('Elapsed', fmtDur(s.elapsed), job.ended_at ? `ended ${relTime(job.ended_at)}` : 'running')));
  }

  // ---- latency table
  function paintLatency() {
    const s = job.summary;
    if (!s || !s.completed) { latHost.replaceChildren(); return; }
    const tbl = h('table', { class: 'table table-dense lat-table' },
      h('thead', null, h('tr', null, h('th', null, 'Percentile'), h('th', { class: 'num' }, 'Service time'), h('th', { class: 'num' }, 'Corrected'), h('th', { class: 'num' }, 'Difference'))),
      h('tbody', null, LAT_ROWS.map(([k, label]) => {
        const a = s.latency[k], b = s.corrected_latency[k];
        const diff = b - a;
        const ratio = a > 0 ? b / a : 1;
        return h('tr', null, h('td', null, label), h('td', { class: 'num' }, fmtMs(a)), h('td', { class: 'num', style: 'font-weight:600' }, fmtMs(b)),
          h('td', { class: 'num', style: ratio > 1.2 && diff > 5 ? 'color:var(--warn)' : 'color:var(--text-3)' }, diff > 0.05 ? `+${fmtMs(diff)}` : '–'));
      })));
    const keys = ['p50', 'p95', 'p99'];
    const worst = Math.max(...keys.map((k) => (s.latency[k] > 0 ? s.corrected_latency[k] / s.latency[k] : 1)));
    const worstDiff = Math.max(...keys.map((k) => s.corrected_latency[k] - s.latency[k]));
    const queued = worst > 1.25 && worstDiff > 10;
    latHost.replaceChildren(card({
      title: 'Latency', subtitle: `${fmtInt(s.latency.count)} responses`,
      body: h('div', { class: 'cols-main' },
        h('div', { class: 'table-wrap' }, tbl),
        h('div', { class: 'stack-sm' },
          queued
            ? alertBox('warn', `Corrected latency is up to ${worst.toFixed(1)}× the service time (p99 ${fmtMs(s.corrected_latency.p99)} against ${fmtMs(s.latency.p99)}). The target, or the concurrency limit, could not keep up with the schedule, so requests waited before they were even sent.`, { title: 'Queueing detected' })
            : alertBox('success', 'Service time and corrected latency agree, so requests went out on schedule.', { title: 'On schedule' }),
          h('p', { class: 'explain' }, h('b', null, 'Service time'), ' runs from the moment a request was sent until its response was fully read.'),
          h('p', { class: 'explain' }, h('b', null, 'Corrected latency'), ' runs from when the request was ', h('i', null, 'supposed'), ' to be sent. When a slow target holds up the sender, plain service time hides that wait (coordinated omission: the slowest moments get the fewest samples). Corrected latency counts it, so it is the honest number for what a real user would have felt.'))),
    }));
  }

  // ---- charts
  const startedMs = job.started_at ? Date.parse(job.started_at) : 0;
  const tfn = targetFn(job);
  const showTarget = job.config.mode !== 'max';
  const mk = (title, series, yFormat, tipFormat) => new TimeChart({ title, series, windowMs: null, xMode: 'elapsed', xBase: startedMs, yFormat, tipFormat, height: 190, gapMs: 15000 });
  const cRps = mk('Achieved vs target rate', [{ key: 'rps', label: 'achieved', color: 'c1', area: true }, ...(showTarget ? [{ key: 'target', label: 'target', color: 'c5', dash: [6, 4], width: 1.5 }] : [])], fmtRate, (v) => `${fmtCompact(v)} /s`);
  const cLat = mk('Latency', [{ key: 'p50', label: 'p50', color: 'c1' }, { key: 'p95', label: 'p95', color: 'c2' }, { key: 'p99', label: 'p99', color: 'c3' }], fmtMsShort, fmtMs);
  const cErr = mk('Errors per second', [{ key: 'errors', label: 'errors/s', color: 'err', area: true }], fmtCompact, (v) => `${fmtCompact(v)} /s`);
  const cIn = mk('In flight', [{ key: 'in_flight', label: 'requests', color: 'c4', area: true }], fmtCompact, (v) => String(Math.round(v)));
  const allCharts = [cRps, cLat, cErr, cIn];
  ctx.onCleanup(() => allCharts.forEach((c) => c.destroy()));
  chartsHost.appendChild(h('div', { class: 'charts-2' }, card({ body: cRps.el }), card({ body: cLat.el }), card({ body: cErr.el }), card({ body: cIn.el })));
  const rowOf = (pt) => { const r = pointToRow(pt); r.target = tfn((r.t - startedMs) / 1000); return r; };
  function setChartRows(points) {
    const rows = points.map(rowOf);
    allCharts.forEach((c) => c.setRows(rows));
  }

  // ---- status codes + endpoints
  function paintStatus() {
    const s = job.summary;
    if (!s || !s.completed) { statusHost.replaceChildren(); return; }
    const codes = Object.entries(s.status_counts || {}).sort((a, b) => b[1] - a[1]);
    statusHost.replaceChildren(card({
      title: 'Responses by status', subtitle: `${fmtInt(s.completed)} responses${s.failed ? `, ${fmtInt(s.failed)} with no response` : ''}`,
      body: h('div', { class: 'stack-sm' }, statusBar({ ...groupStatus(s.status_counts), ...(s.failed ? { err: s.failed } : {}) }),
        h('div', { class: 'row', style: 'gap:6px 10px' }, codes.map(([c, n]) => h('span', { class: 'mono muted', style: 'font-size:12.5px' }, `${c} × ${fmtInt(n)}`)))),
    }));
  }
  const epTable = dataTable({
    rowKey: (r) => r.endpoint, sortKey: 'count', sortDir: 'desc', dense: true,
    empty: () => h('div', { class: 'empty', style: 'padding:24px' }, h('div', { class: 'empty-text' }, 'Endpoint results appear once requests complete.')),
    columns: [
      { key: 'endpoint', label: 'Endpoint', render: (r) => h('code', { style: 'background:none;padding:0' }, r.endpoint) },
      { key: 'count', label: 'Requests', align: 'right', render: (r) => fmtInt(r.count) },
      { key: 'errors', label: 'Errors', align: 'right', render: (r) => fmtInt(r.errors) },
      { key: 'rate', label: 'Error rate', align: 'right', sortValue: (r) => (r.count ? r.errors / r.count : 0), render: (r) => h('span', { style: r.count && r.errors / r.count > 0.02 ? 'color:var(--err)' : null }, fmtPct(r.count ? r.errors / r.count : 0)) },
      { key: 'p50', label: 'p50', align: 'right', sortValue: (r) => r.latency.p50, render: (r) => fmtMs(r.latency.p50) },
      { key: 'p95', label: 'p95', align: 'right', sortValue: (r) => r.latency.p95, render: (r) => fmtMs(r.latency.p95) },
      { key: 'p99', label: 'p99', align: 'right', sortValue: (r) => r.latency.p99, render: (r) => fmtMs(r.latency.p99) },
      { key: 'max', label: 'Max', align: 'right', sortValue: (r) => r.latency.max, render: (r) => fmtMs(r.latency.max) },
    ],
  });
  epHost.appendChild(card({ title: 'Endpoints', subtitle: 'Service time per endpoint', flush: true, body: epTable.el }));
  const paintEndpoints = () => epTable.setRows((job.summary && job.summary.endpoints) || []);

  // ---- config
  function paintConfig() {
    const c = job.config;
    const list = (a) => (a && a.length ? a.join(', ') : DASH);
    cfgHost.replaceChildren(card({
      title: 'Configuration', actions: btn('Edit as new', { size: 'sm', icon: 'edit', href: `#/replays/new?from=${encodeURIComponent(job.id)}` }),
      body: h('div', { class: 'stack-sm' },
        h('div', { class: 'cols-2' },
          kvList([
            ['Mode', c.mode], ['Speed', c.mode === 'timeline' ? `${c.speed}×` : undefined], ['Rate', c.mode === 'rate' && !(c.stages && c.stages.length) ? `${c.rate} req/s` : undefined],
            ['Stages', c.stages && c.stages.length ? c.stages.map((s) => `${fmtDur(s.duration)} → ${fmtCompact(s.rate)}/s`).join(', ') : undefined],
            ['Amplify', `${c.amplify || 1}×`], ['Error amplification', `${c.error_amplification || 1}×`], ['Concurrency', c.concurrency],
            ['Duration', c.duration ? fmtDur(c.duration) : DASH], ['Loop', c.loop ? 'yes' : 'no'], ['Max requests', c.max_requests || DASH], ['Timeout', fmtDur(c.timeout)],
          ]),
          kvList([
            ['Include', list(c.include)], ['Exclude', list(c.exclude)],
            ['Set headers', c.set_headers && Object.keys(c.set_headers).length ? Object.keys(c.set_headers).join(', ') : DASH], ['Remove headers', list(c.remove_headers)],
            ['Host header', c.host_header || DASH], ['Skip TLS verify', c.insecure_skip_verify ? 'yes' : 'no'],
            ['Anonymization', c.profile_id ? `${c.profile_id}${c.vary_clones ? ' (varied clones)' : ''}` : 'none'], ['Compare status', c.compare_status ? 'yes' : 'no'],
          ])),
        collapsible('Raw configuration', h('pre', { class: 'code' }, JSON.stringify(c, null, 2)))),
    }));
  }

  // ---- lifecycle
  function paintAll() {
    paintHeader(); paintProgress(); paintStats(); paintLatency(); paintStatus(); paintEndpoints(); paintConfig();
    const pending = job.status === 'pending';
    chartsHost.hidden = pending;
    epHost.hidden = !(job.summary && job.summary.completed);
  }
  async function reload(resetCharts) {
    try { job = await api.replay(id); } catch (e) { toastError(e); return; }
    if (!ctx.isActive()) return;
    mergeHistory(name, job.series || []);
    if (resetCharts || DONE(job.status)) setChartRows(job.series || []);
    paintAll();
  }
  paintAll();
  setChartRows(job.series || []);
  let lastStatus = job.status;
  let lastLive = Date.now();
  ctx.on('stats', (evt) => {
    const p = evt.points[name];
    if (p && job.status === 'running') { lastLive = Date.now(); allCharts.forEach((c) => c.push(rowOf(p))); }
  });
  // Fallback: if the stream carries no points for this job, poll its series.
  ctx.every(async () => {
    if (job.status !== 'running' || Date.now() - lastLive < 4000) return;
    const rows = getRows(name);
    const last = rows.length ? rows[rows.length - 1].t : 0;
    try {
      const pts = await api.replaySeries(id, last + 1);
      if (!ctx.isActive()) return;
      mergeHistory(name, pts);
      for (const p of pts) allCharts.forEach((c) => c.push(rowOf(p)));
    } catch (_) { /* transient */ }
  }, 2500);
  ctx.on('replays', () => {
    const cur = state.replays.find((r) => r.id === id);
    if (!cur) return;
    job = { ...job, status: cur.status, summary: cur.summary || job.summary };
    if (cur.status !== lastStatus) { lastStatus = cur.status; reload(true); return; }
    paintProgress(); paintStats(); paintLatency(); paintStatus(); paintEndpoints();
    if (job.status === 'running') paintHeader();
  });
void replaceQuery; void badge; void selectCtl; void icon;
  return root;
}

// --------------------------------------------------------------- compare
const BETTER_LOW = 'low';
const BETTER_HIGH = 'high';

export async function compare(ctx) {
  ctx.setCrumbs([{ label: 'Replays', href: '#/replays' }, { label: 'Compare' }]);
  await refreshReplays();
  const finished = () => state.replays.filter((r) => (r.status === 'completed' || r.status === 'stopped') && r.summary && r.summary.completed);
  const root = h('div', { class: 'stack' });
  root.appendChild(pageHeader({ subtitle: 'Pick two finished runs to see their results side by side, with the change from A to B.' }));

  const opts = (blank) => [{ value: '', label: blank }, ...finished().map((r) => ({ value: r.id, label: `${r.name}  ·  ${fmtRate(r.summary.achieved_rps)} req/s  ·  ${relTime(r.started_at || r.created_at)}` }))];
  const selA = selectCtl(opts('Choose run A (baseline)'), { value: ctx.query.a || '' });
  const selB = selectCtl(opts('Choose run B'), { value: ctx.query.b || '' });
  const swap = btn('Swap', { icon: 'compare', size: 'sm', onClick: () => { const a = selA.get(); selA.set(selB.get()); selB.set(a); go(); } });
  root.appendChild(card({ body: h('div', { class: 'row', style: 'align-items:flex-end' },
    h('div', { style: 'flex:1 1 280px' }, h('div', { class: 'field-label', style: 'margin-bottom:6px' }, h('span', { class: 'badge badge-info' }, 'A'), ' baseline'), selA.el),
    swap,
    h('div', { style: 'flex:1 1 280px' }, h('div', { class: 'field-label', style: 'margin-bottom:6px' }, h('span', { class: 'badge', style: 'color:var(--c3);border-color:var(--c3)' }, 'B'), ' comparison'), selB.el)) }));
  const out = h('div', { class: 'stack' });
  root.appendChild(out);
  const charts = [];
  ctx.onCleanup(() => charts.forEach((c) => c.destroy()));
  let seq = 0;

  async function go() {
    const a = selA.get(), b = selB.get();
    replaceQuery({ a, b });
    const my = ++seq;
    charts.splice(0).forEach((c) => c.destroy());
    out.replaceChildren();
    if (!a || !b) {
      out.appendChild(card({ body: emptyState({ icon: 'compare', title: finished().length < 2 ? 'You need two finished replays' : 'Choose two runs', text: finished().length < 2 ? 'Run a replay to completion (or stop it) and come back. Re-running with a different speed or amplification is the easiest way to get a pair.' : 'Pick a baseline and a comparison above.', actions: [btn('New replay', { href: '#/replays/new', kind: 'primary', icon: 'plus' })] }) }));
      return;
    }
    if (a === b) { out.appendChild(alertBox('info', 'Both selections are the same run. Pick two different runs to see differences.')); }
    let ja, jb;
    try { [ja, jb] = await Promise.all([api.replay(a), api.replay(b)]); } catch (e) { out.appendChild(alertBox('error', e.message)); return; }
    if (my !== seq || !ctx.isActive()) return;
    out.appendChild(summaryCompare(ja, jb));
    out.appendChild(configDiff(ja, jb));
    out.appendChild(chartCompare(ja, jb, charts));
    out.appendChild(endpointCompare(ja, jb));
  }
  selA.onChange(go); selB.onChange(go);
  await go();
  return root;
}

function deltaCell(a, b, better, fmt) {
  if (a == null || b == null) return h('td', { class: 'num' }, DASH);
  const diff = b - a;
  const pct = a !== 0 ? (diff / Math.abs(a)) * 100 : (b === 0 ? 0 : Infinity);
  let cls = 'flat';
  if (Math.abs(pct) >= 1 && better) cls = (better === BETTER_LOW ? diff < 0 : diff > 0) ? 'good' : 'bad';
  const sign = diff > 0 ? '+' : diff < 0 ? '−' : '';
  const abs = fmt(Math.abs(diff));
  return h('td', { class: 'num' }, h('span', { class: `delta ${cls}` }, Number.isFinite(pct) ? `${sign}${Math.abs(pct) >= 100 ? Math.abs(pct).toFixed(0) : Math.abs(pct).toFixed(1)}%` : '+∞'), h('span', { class: 'muted', style: 'margin-left:8px;font-size:12px' }, diff === 0 ? '' : `${sign}${abs}`));
}

function summaryCompare(ja, jb) {
  const A = ja.summary, B = jb.summary;
  const rows = [];
  const add = (label, a, b, fmt, better, o = {}) => rows.push({ label, a, b, fmt, better, ...o });
  add('Achieved rate', A.achieved_rps, B.achieved_rps, (v) => `${fmtRate(v)} /s`, BETTER_HIGH);
  add('Target rate', A.target_rps, B.target_rps, (v) => (v ? `${fmtRate(v)} /s` : 'max'), null);
  add('Error rate', A.error_rate, B.error_rate, fmtPct, BETTER_LOW);
  add('Status mismatches', A.mismatches, B.mismatches, fmtInt, BETTER_LOW);
  add('Failed (no response)', A.failed, B.failed, fmtInt, BETTER_LOW);
  add('Timeouts', A.timeouts, B.timeouts, fmtInt, BETTER_LOW);
  rows.push({ section: 'Service time' });
  for (const [k, l] of LAT_ROWS) add(l, A.latency[k], B.latency[k], fmtMs, BETTER_LOW);
  rows.push({ section: 'Corrected latency (from intended send time)' });
  for (const [k, l] of LAT_ROWS.filter(([k]) => ['p50', 'p95', 'p99', 'p999', 'max'].includes(k))) add(l, A.corrected_latency[k], B.corrected_latency[k], fmtMs, BETTER_LOW);
  rows.push({ section: 'Scheduling and volume' });
  add('Max lag', durMs(A.max_lag), durMs(B.max_lag), fmtMs, BETTER_LOW);
  add('Requests sent', A.sent, B.sent, fmtInt, null);
  add('Elapsed', durMs(A.elapsed), durMs(B.elapsed), (v) => fmtHuman(v), null);
  add('Bytes received', A.bytes_received, B.bytes_received, fmtBytes, null);
  const tbl = h('table', { class: 'table table-dense lat-table' },
    h('thead', null, h('tr', null, h('th', null, 'Metric'), h('th', { class: 'num' }, 'A'), h('th', { class: 'num' }, 'B'), h('th', { class: 'num' }, 'Change (B vs A)'))),
    h('tbody', null, rows.map((r) => (r.section
      ? h('tr', null, h('td', { colspan: 4, style: 'padding-top:14px;font-size:11.5px;text-transform:uppercase;letter-spacing:.06em;color:var(--text-3);font-weight:650' }, r.section))
      : h('tr', null, h('td', null, r.label), h('td', { class: 'num' }, r.fmt(r.a)), h('td', { class: 'num', style: 'font-weight:600' }, r.fmt(r.b)), deltaCell(r.a, r.b, r.better, r.fmt))))));
  return card({
    title: 'Summary',
    subtitle: h('span', null, h('a', { href: `#/replays/${encodeURIComponent(ja.id)}` }, ja.name), '  vs  ', h('a', { href: `#/replays/${encodeURIComponent(jb.id)}` }, jb.name)),
    flush: true, body: h('div', { class: 'table-wrap' }, tbl),
  });
}

function configDiff(ja, jb) {
  const keys = ['recording_id', 'target', 'mode', 'speed', 'rate', 'stages', 'amplify', 'error_amplification', 'concurrency', 'duration', 'loop', 'max_requests', 'timeout', 'include', 'exclude', 'profile_id', 'vary_clones', 'compare_status'];
  const fmtV = (v) => (v === undefined || v === '' ? DASH : typeof v === 'object' ? JSON.stringify(v) : String(v));
  const diffs = keys.filter((k) => JSON.stringify(ja.config[k]) !== JSON.stringify(jb.config[k]));
  return card({
    title: 'What changed between the runs',
    body: diffs.length
      ? h('table', { class: 'table table-dense' }, h('thead', null, h('tr', null, h('th', null, 'Setting'), h('th', null, 'A'), h('th', null, 'B'))),
        h('tbody', null, diffs.map((k) => h('tr', null, h('td', null, h('code', null, k)), h('td', { class: 'mono wrap-any' }, fmtV(ja.config[k])), h('td', { class: 'mono wrap-any' }, fmtV(jb.config[k]))))))
      : h('p', { class: 'muted' }, 'The two runs used identical settings.'),
  });
}

function chartCompare(ja, jb, store) {
  const toMap = (j) => {
    const t0 = j.started_at ? Date.parse(j.started_at) : (j.series[0] ? j.series[0].t : 0);
    const m = new Map();
    (j.series || []).forEach((p) => m.set(Math.round((p.t - t0) / 1000), pointToRow(p)));
    return m;
  };
  const ma = toMap(ja), mb = toMap(jb);
  const max = Math.max(...ma.keys(), ...mb.keys(), 0);
  const rows = [];
  for (let s = 0; s <= max; s++) {
    const a = ma.get(s), b = mb.get(s);
    if (!a && !b) continue;
    rows.push({ t: s * 1000, a_rps: a ? a.rps : undefined, b_rps: b ? b.rps : undefined, a_p95: a ? a.p95 : undefined, b_p95: b ? b.p95 : undefined, a_p99: a ? a.p99 : undefined, b_p99: b ? b.p99 : undefined, a_err: a ? a.errors : undefined, b_err: b ? b.errors : undefined });
  }
  const mk = (title, ka, kb, yf, tf) => {
    const c = new TimeChart({ title, series: [{ key: ka, label: 'A', color: 'c1' }, { key: kb, label: 'B', color: 'c3' }], windowMs: null, xMode: 'elapsed', xBase: 0, yFormat: yf, tipFormat: tf, height: 190, gapMs: 15000 });
    c.setRows(rows);
    store.push(c);
    return card({ body: c.el });
  };
  return h('div', { class: 'charts-2' },
    mk('Achieved rate', 'a_rps', 'b_rps', fmtRate, (v) => `${fmtCompact(v)} /s`),
    mk('Latency p95', 'a_p95', 'b_p95', fmtMsShort, fmtMs),
    mk('Latency p99', 'a_p99', 'b_p99', fmtMsShort, fmtMs),
    mk('Errors per second', 'a_err', 'b_err', fmtCompact, (v) => `${fmtCompact(v)} /s`));
}

function endpointCompare(ja, jb) {
  const ea = new Map((ja.summary.endpoints || []).map((e) => [e.endpoint, e]));
  const eb = new Map((jb.summary.endpoints || []).map((e) => [e.endpoint, e]));
  const names = Array.from(new Set([...ea.keys(), ...eb.keys()]));
  const rows = names.map((n) => ({ endpoint: n, a: ea.get(n), b: eb.get(n) }));
  rows.sort((x, y) => ((y.b || y.a).count) - ((x.b || x.a).count));
  const tbl = h('table', { class: 'table table-dense lat-table' },
    h('thead', null, h('tr', null, h('th', null, 'Endpoint'), h('th', { class: 'num' }, 'p95 A'), h('th', { class: 'num' }, 'p95 B'), h('th', { class: 'num' }, 'Change'), h('th', { class: 'num' }, 'Errors A'), h('th', { class: 'num' }, 'Errors B'))),
    h('tbody', null, rows.map((r) => h('tr', null,
      h('td', null, h('code', { style: 'background:none;padding:0' }, r.endpoint)),
      h('td', { class: 'num' }, r.a ? fmtMs(r.a.latency.p95) : DASH), h('td', { class: 'num' }, r.b ? fmtMs(r.b.latency.p95) : DASH),
      deltaCell(r.a ? r.a.latency.p95 : null, r.b ? r.b.latency.p95 : null, BETTER_LOW, fmtMs),
      h('td', { class: 'num' }, r.a ? fmtInt(r.a.errors) : DASH), h('td', { class: 'num' }, r.b ? fmtInt(r.b.errors) : DASH)))));
  return card({ title: 'Endpoints', subtitle: 'p95 service time per endpoint', flush: true, body: h('div', { class: 'table-wrap' }, tbl) });
}
