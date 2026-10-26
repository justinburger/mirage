// Services: list and detail pages.
import { h } from '../dom.js';
import { icon } from '../icons.js';
import { api } from '../api.js';
import { state, refreshServices, serviceById } from '../state.js';
import { navigate } from '../router.js';
import { loadHistory, pointToRow } from '../series.js';
import { trafficCharts, WINDOWS, savedWindow, saveWindow } from '../livecharts.js';
import { card, btn, iconBtn, dataTable, modeBadge, emptyState, popMenu, confirmDialog, toastOk, kvList, copyButton, alertBox, segmentedCtl, badge, dot, pageHeader, toastError } from '../ui.js';
import { openServiceForm, switchMode, MODES, MODE_HELP, durText } from '../service-form.js';
import { fmtRate, fmtPct, fmtMs, fmtInt, fmtBytes, relTime, portOf, DASH, fmtCompact } from '../format.js';

function stateCell(st, enabled) {
  if (st.running) return h('span', { class: 'status-text' }, dot('live'), 'Running');
  if (st.error) return h('span', { class: 'status-text', title: st.error, style: 'color:var(--err)' }, dot('err'), 'Error');
  return h('span', { class: 'status-text muted' }, dot('off'), enabled ? 'Stopped' : 'Disabled');
}

function modeButton(svc) {
  const b = h('button', { type: 'button', class: 'mode-btn', title: 'Change mode', 'aria-haspopup': 'menu' }, modeBadge(svc.mode), icon('chevron-down', 13));
  b.addEventListener('click', () => {
    popMenu(b, MODES.map((m) => ({
      label: m.label, disabled: m.value === svc.mode,
      onClick: () => switchMode(serviceById(svc.id) || svc, m.value),
    })));
  });
  return b;
}

async function toggleEnabled(svc) {
  if (svc.enabled) await api.disableService(svc.id); else await api.enableService(svc.id);
  await refreshServices();
  toastOk(`${svc.name} ${svc.enabled ? 'disabled' : 'enabled'}`);
}

async function reloadIndex(svc) {
  await api.reloadService(svc.id);
  await refreshServices();
  toastOk(`Reloaded the virtualization index for ${svc.name}`);
}

export async function deleteService(svc) {
  const ok = await confirmDialog({
    title: `Delete ${svc.name}?`, danger: true, confirmText: 'Delete service',
    message: h('div', null, 'The listener on ', h('code', null, svc.listen_addr), ' stops immediately. Its recording and metrics history are kept; clients pointed at it will get connection errors.'),
  });
  if (!ok) return false;
  await api.deleteService(svc.id);
  await refreshServices();
  toastOk(`Deleted ${svc.name}`);
  return true;
}

// ------------------------------------------------------------------ list
export async function list(ctx) {
  ctx.setCrumbs([{ label: 'Services' }]);
  const [, recs] = await Promise.all([refreshServices().catch(() => {}), api.recordings().catch(() => [])]);
  const recName = new Map(recs.map((r) => [r.id, r.name]));
  const root = h('div', { class: 'stack' });

  root.appendChild(pageHeader({
    subtitle: 'A service is a listener in front of one upstream API. Switch its mode to record, virtualize or just proxy.',
    actions: btn('New service', { icon: 'plus', kind: 'primary', onClick: () => openForm() }),
  }));

  const table = dataTable({
    rowKey: (r) => r.id, sortKey: 'name',
    onRowClick: (r) => navigate(`/services/${encodeURIComponent(r.id)}`),
    empty: () => emptyState({
      icon: 'services', title: 'No services yet',
      text: 'Create a service to put Mirage in front of an API. Start in passthrough mode to see traffic, then record it.',
      actions: [btn('Create a service', { icon: 'plus', kind: 'primary', onClick: () => openForm() })],
    }),
    columns: [
      { key: 'name', label: 'Service', sortValue: (r) => r.name.toLowerCase(), render: (r) => h('div', { class: 'cell-main' }, h('a', { class: 'cell-name', href: `#/services/${encodeURIComponent(r.id)}` }, r.name), h('span', { class: 'cell-sub mono', title: r.upstream }, r.upstream || 'no upstream')), sig: (r) => r.name + r.upstream },
      { key: 'mode', label: 'Mode', render: (r) => modeButton(r), sig: (r) => r.mode },
      { key: 'state', label: 'State', sortValue: (r) => (r.status.running ? 1 : r.status.error ? 2 : 0), render: (r) => stateCell(r.status, r.enabled), sig: (r) => `${r.status.running}|${r.status.error}|${r.enabled}` },
      { key: 'listen', label: 'Listen', cls: 'hide-md', render: (r) => h('code', null, r.listen_addr), sig: (r) => r.listen_addr },
      { key: 'rec', label: 'Recording', cls: 'hide-md', sortValue: (r) => recName.get(r.recording_id) || '', render: (r) => (r.recording_id ? h('a', { class: 'truncate', href: `#/recordings/${encodeURIComponent(r.recording_id)}` }, recName.get(r.recording_id) || r.recording_id) : h('span', { class: 'muted' }, DASH)), sig: (r) => r.recording_id },
      { key: 'rps', label: 'Req/s', align: 'right', sortValue: (r) => r._m.rps, render: (r) => fmtRate(r._m.rps) },
      { key: 'p95', label: 'p95', align: 'right', sortValue: (r) => r._m.p95, render: (r) => (r._m.has ? fmtMs(r._m.p95) : DASH) },
      { key: 'err', label: 'Errors', align: 'right', sortValue: (r) => r._m.err, render: (r) => (r._m.has ? h('span', { style: r._m.err > 0.02 ? 'color:var(--err)' : r._m.err > 0.005 ? 'color:var(--warn)' : null }, fmtPct(r._m.err)) : DASH) },
      {
        key: 'actions', label: '', sortable: false, cls: 'actions',
        render: (r) => h('div', { class: 'cell-actions' },
          iconBtn(r.enabled ? 'pause' : 'play', r.enabled ? 'Disable service' : 'Enable service', () => toggleEnabled(r)),
          r.mode === 'virtualize' || r.mode === 'hybrid' ? iconBtn('refresh', 'Reload virtualization index', () => reloadIndex(r)) : null,
          iconBtn('edit', 'Edit service', () => openForm(r)),
          iconBtn('trash', 'Delete service', () => deleteService(r))),
        sig: (r) => `${r.id}|${r.enabled}|${r.mode}`,
      },
    ],
  });
  root.appendChild(card({ flush: true, body: table.el }));

  function paint() {
    const rows = state.services.map((s) => {
      const snap = state.snapshots[`service:${s.id}`];
      const has = !!snap && s.status.running;
      return { ...s, _m: { has, rps: has ? snap.rps : 0, p95: has && snap.recent ? snap.recent.p95 : 0, err: has ? snap.error_rate10 : 0 } };
    });
    table.setRows(rows);
  }
  paint();
  ctx.on('services', paint);
  ctx.on('stats', paint);

  function openForm(svc) {
    openServiceForm({ service: svc || null, onSaved: (s) => { if (!svc) navigate(`/services/${encodeURIComponent(s.id)}`); } }).then((m) => {
      if (ctx.path === '/services/new') {
        history.replaceState(null, '', '#/services');
      }
      void m;
    });
  }
  if (ctx.path === '/services/new') setTimeout(() => openForm(), 0);
  return root;
}

// ---------------------------------------------------------------- detail
const WIN_KEY = 'mirage_service_window';

function clientHint(svc, st) {
  const port = portOf(st.addr) || portOf(svc.listen_addr);
  const host = (location.hostname.includes(':') ? `[${location.hostname}]` : location.hostname) || 'localhost';
  const scheme = svc.tls ? 'https' : 'http';
  return { url: `${scheme}://${host}:${port}`, loopback: /^(127\.|\[?::1)/.test(svc.listen_addr) };
}

export async function detail(ctx) {
  const id = ctx.params.id;
  let svc = await api.service(id);
  ctx.setCrumbs([{ label: 'Services', href: '#/services' }, { label: svc.name }]);
  const recs = await api.recordings().catch(() => []);
  const recById = () => new Map(recs.map((r) => [r.id, r]));
  let windowMs = savedWindow(WIN_KEY);

  const root = h('div', { class: 'stack' });
  const headHost = h('div');
  const errHost = h('div');
  const modeHost = h('div');
  const statusHost = h('div');
  const configHost = h('div');
  root.append(headHost, errHost, h('div', { class: 'cols-2' }, modeHost, h('div', { id: 'connect-host' })), statusHost);

  // --- header
  function paintHeader() {
    const st = svc.status;
    headHost.replaceChildren(h('div', { class: 'page-head', style: 'margin-bottom:0' },
      h('div', { class: 'page-titles' },
        h('h2', { class: 'page-title' }, svc.name, modeBadge(svc.mode), h('span', { class: 'title-meta' }, stateCell(st, svc.enabled))),
        h('p', { class: 'page-sub mono', style: 'font-size:13px' }, `${svc.listen_addr}  →  ${svc.upstream || '(no upstream)'}`)),
      h('div', { class: 'page-actions' },
        btn('Edit', { icon: 'edit', onClick: () => openServiceForm({ service: svc, onSaved: () => reload() }) }),
        btn(svc.enabled ? 'Disable' : 'Enable', { icon: svc.enabled ? 'pause' : 'play', onClick: async () => { await toggleEnabled(svc); await reload(); } }),
        svc.mode === 'virtualize' || svc.mode === 'hybrid' ? btn('Reload index', { icon: 'refresh', onClick: () => reloadIndex(svc) }) : null,
        btn('Delete', { icon: 'trash', kind: 'danger', onClick: async () => { if (await deleteService(svc)) navigate('/services'); } }))));
    errHost.replaceChildren(st.error ? alertBox('error', st.error, { title: 'This service could not start' }) : '');
  }

  // --- mode switcher + client hint
  function paintMode() {
    const seg = segmentedCtl(MODES, { value: svc.mode, label: 'Service mode' });
    seg.el.classList.add('block');
    seg.onChange(async (v) => {
      seg.set(svc.mode);
      try {
        const updated = await switchMode(svc, v);
        if (updated) await reload();
      } catch (e) { toastError(e); }
    });
    const rec = recById().get(svc.recording_id);
    modeHost.replaceChildren(card({
      title: 'Mode', subtitle: 'Switching takes effect immediately.',
      body: h('div', { class: 'stack-sm' }, seg.el,
        h('p', { class: 'explain' }, MODE_HELP[svc.mode]),
        svc.mode !== 'passthrough' ? h('div', { class: 'row' }, icon('recordings', 15), 'Recording: ',
          rec ? h('a', { href: `#/recordings/${encodeURIComponent(rec.id)}` }, rec.name) : h('span', { class: 'muted' }, svc.recording_id || 'none'),
          rec ? h('span', { class: 'muted' }, `· ${fmtInt(rec.exchange_count)} exchanges`) : null) : null),
    }));
    const hint = clientHint(svc, svc.status);
    const curl = `curl -i ${hint.url}/`;
    const host = modeHost.parentNode.querySelector('#connect-host');
    host.replaceChildren(card({
      title: 'Point your client here', subtitle: 'Send requests to this address instead of the real API.',
      body: h('div', { class: 'stack-sm' },
        h('div', { class: 'hint-box' }, h('code', { class: 'grow' }, hint.url), copyButton(hint.url, { title: 'Copy address' })),
        h('div', { class: 'hint-box' }, h('code', { class: 'grow' }, curl), copyButton(curl, { title: 'Copy curl command' })),
        hint.loopback ? h('p', { class: 'explain' }, `This listener is bound to loopback (${svc.listen_addr}); only clients on the Mirage host can reach it.`) : null,
        svc.tls && svc.tls.self_signed ? h('p', { class: 'explain' }, 'TLS uses a self-signed certificate generated at start-up. Use curl -k or trust it in your client.') : null),
    }));
  }

  // --- status grid
  function cell(k, v, s, tone) {
    return h('div', { class: 'stat-cell' }, h('div', { class: 'stat-k' }, k), h('div', { class: ['stat-v', tone] }, v), h('div', { class: 'stat-s' }, s || ' '));
  }
  function paintStatus() {
    const st = svc.status;
    const hits = st.virtual_hits, misses = st.virtual_misses;
    const total = hits + misses;
    const virt = svc.mode === 'virtualize' || svc.mode === 'hybrid';
    const rec = svc.mode === 'record' || svc.mode === 'hybrid';
    const snap = state.snapshots[`service:${svc.id}`];
    const cells = [
      cell('Status', st.running ? 'Running' : st.error ? 'Error' : 'Stopped', st.running && st.started_at ? `up ${relTime(st.started_at).replace(' ago', '')}` : '', st.running ? 'ok' : st.error ? 'err' : ''),
      cell('Listening on', st.addr || DASH, svc.listen_addr),
      cell('Requests / s', snap && st.running ? fmtRate(snap.rps) : DASH, snap ? `${fmtCompact(snap.requests)} total` : ''),
    ];
    if (rec) {
      const sample = svc.capture_sample > 0 && svc.capture_sample < 1 ? ` (${fmtPct(svc.capture_sample, 0)} sampled)` : '';
      cells.push(
        cell('Captured', fmtInt(st.captured), `exchanges written${sample}`),
        cell('Capture dropped', fmtInt(st.capture_dropped), st.capture_dropped ? 'queue was full' : 'none', st.capture_dropped ? 'warn' : ''),
        cell('Capture skipped', fmtInt(st.capture_skipped), st.capture_skipped ? 'client left early, sampled out or over the limit' : 'none', ''));
    }
    if (virt) {
      cells.push(
        cell('Virtual hits', fmtInt(hits), total ? `${fmtPct(hits / total)} hit rate` : ''),
        cell('Virtual misses', fmtInt(misses), svc.mode === 'hybrid' ? 'proxied and recorded' : `answered ${svc.match.miss_status}`, total && misses / total > 0.05 ? 'warn' : ''),
        cell('Errors injected', fmtInt(st.virtual_injected), svc.error_amplification !== 1 || svc.error_floor > 0 ? `by ${svc.error_amplification}× amplification${svc.error_floor > 0 ? ` and ${fmtPct(svc.error_floor)} floor` : ''}` : 'amplification is off', ''),
        cell('Indexed', fmtInt(st.indexed_exchanges), 'exchanges in index'));
    }
    statusHost.replaceChildren(h('div', { class: 'stat-grid cols-4' }, ...cells));
  }

  // --- charts
  const winSeg = segmentedCtl(WINDOWS, { value: windowMs, label: 'Time window' });
  winSeg.el.classList.add('window-seg');
  const charts = trafficCharts({ windowMs });
  ctx.onCleanup(() => charts.destroy());
  root.appendChild(h('div', { class: 'toolbar', style: 'margin:4px 0 -4px' }, h('h2', { class: 'section-title', style: 'margin:0' }, 'Live metrics'), h('div', { class: 'grow' }), winSeg.el));
  root.appendChild(charts.el);
  winSeg.onChange((v) => { windowMs = v; saveWindow(WIN_KEY, v); charts.setWindow(v); });
  loadHistory('service', svc.id, 3600 * 1000).then((rows) => { if (ctx.isActive()) charts.setRows(rows.slice()); }).catch(() => {});

  // --- endpoints table
  const epTable = dataTable({
    rowKey: (r) => r.endpoint, sortKey: 'count', sortDir: 'desc', dense: true,
    empty: () => h('div', { class: 'empty', style: 'padding:24px' }, h('div', { class: 'empty-text' }, 'No traffic seen yet. Endpoints appear here as requests arrive.')),
    columns: [
      { key: 'endpoint', label: 'Endpoint', render: (r) => h('code', { style: 'background:none;padding:0' }, r.endpoint) },
      { key: 'count', label: 'Requests', align: 'right', render: (r) => fmtInt(r.count) },
      { key: 'errors', label: 'Errors', align: 'right', render: (r) => fmtInt(r.errors) },
      { key: 'rate', label: 'Error rate', align: 'right', sortValue: (r) => (r.count ? r.errors / r.count : 0), render: (r) => h('span', { style: r.count && r.errors / r.count > 0.02 ? 'color:var(--err)' : null }, fmtPct(r.count ? r.errors / r.count : 0)) },
      { key: 'p50', label: 'p50', align: 'right', sortValue: (r) => r.latency.p50, render: (r) => fmtMs(r.latency.p50) },
      { key: 'p95', label: 'p95', align: 'right', sortValue: (r) => r.latency.p95, render: (r) => fmtMs(r.latency.p95) },
      { key: 'p99', label: 'p99', align: 'right', sortValue: (r) => r.latency.p99, render: (r) => fmtMs(r.latency.p99) },
    ],
  });
  root.appendChild(card({ title: 'Endpoints', subtitle: 'Cumulative since the listener started', flush: true, body: epTable.el }));
  const paintEndpoints = () => { const snap = state.snapshots[`service:${svc.id}`]; epTable.setRows(snap ? snap.endpoints || [] : []); };
  paintEndpoints();

  // --- configuration
  root.appendChild(configHost);
  function paintConfig() {
    const m = svc.match || {};
    const l = svc.latency || {};
    const virt = svc.mode === 'virtualize' || svc.mode === 'hybrid';
    const tag = (a) => (a && a.length ? h('span', { class: 'row' }, a.map((x) => badge(x, 'muted'))) : null);
    configHost.replaceChildren(card({
      title: 'Configuration', actions: btn('Edit', { size: 'sm', icon: 'edit', onClick: () => openServiceForm({ service: svc, onSaved: () => reload() }) }),
      body: h('div', { class: 'cols-2' },
        kvList([
          ['Upstream', svc.upstream, true],
          ['Listen address', svc.listen_addr, true],
          ['TLS', svc.tls ? (svc.tls.self_signed ? 'self-signed' : 'certificate files') : 'off'],
          ['Anonymize capture', svc.anonymize_capture ? `on (${svc.profile_id || 'default'})` : 'off'],
          ['Endpoint patterns', tag(svc.endpoint_patterns) || DASH],
          ['Timeout', durText(svc.timeout) || DASH],
          ['Max captured body', svc.max_body_bytes === -1 ? 'bodies not captured' : fmtBytes(svc.max_body_bytes)],
          ['Capture sampling', svc.capture_sample > 0 && svc.capture_sample < 1 ? `${fmtPct(svc.capture_sample, 0)} of requests` : 'every request'],
          ['Capture limit', svc.max_capture_bytes > 0 ? `${fmtBytes(svc.max_capture_bytes)} of bodies` : 'none'],
          ['Preserve Host', svc.preserve_host ? 'yes' : 'no'],
          ['Skip TLS verify', svc.insecure_skip_verify ? 'yes' : 'no'],
        ]),
        kvList([
          ['Query matching', m.query],
          ['Body matching', m.body],
          ['Path templates', m.path_templates ? 'on' : 'off'],
          ['Selection', m.selection],
          ['Miss status', m.miss_status],
          ['Ignored params', tag(m.ignore_params) || DASH],
          ['Latency', l.mode === 'fixed' ? `fixed ${durText(l.fixed)}` : l.mode === 'recorded' ? `recorded ×${l.multiplier}` : 'none'],
          ['Jitter', durText(l.jitter) || DASH],
          ['Error amplification', virt || svc.error_amplification !== 1 ? `${svc.error_amplification}×` : '1× (off)'],
          ['Error floor', fmtPct(svc.error_floor || 0)],
        ])),
    }));
  }

  function paintAll() { paintHeader(); paintMode(); paintStatus(); paintConfig(); }
  async function reload() {
    try { svc = await api.service(id); } catch (e) { toastError(e); return; }
    if (!ctx.isActive()) return;
    paintAll();
  }
  paintAll();
  // Live updates come from the stream: the `services` event carries the live
  // counters of every service once a second and `stats` the traffic series,
  // so the page needs no polling. A change of mode or of the running state
  // (enabled, disabled, a crashed listener) also means the configuration may
  // have changed, so it is fetched again.
  let seenMode = svc.status.mode;
  let seenRunning = svc.status.running;
  let reloading = false;
  ctx.on('stats', (evt) => {
    const p = evt.points[`service:${svc.id}`];
    if (p) charts.push(pointToRow(p));
    paintStatus();
    paintEndpoints();
  });
  // The recording's exchange count is refreshed (at most every 10 s) when the
  // service has captured something new, rather than on a timer.
  let recFetched = Date.now();
  let capturedAtFetch = svc.status.captured;
  async function refreshRecording() {
    if (!svc.recording_id) return;
    recFetched = Date.now();
    capturedAtFetch = svc.status.captured;
    try {
      const fresh = await api.recording(svc.recording_id);
      if (!ctx.isActive()) return;
      const i = recs.findIndex((r) => r.id === fresh.id);
      if (i >= 0) recs[i] = fresh; else recs.push(fresh);
      paintMode();
    } catch (_) { /* the recording may have been deleted meanwhile */ }
  }
  ctx.on('services', () => {
    const cur = serviceById(svc.id);
    if (!cur) return;
    svc = { ...svc, status: cur.status };
    paintHeader(); paintStatus();
    if (cur.status.captured !== capturedAtFetch && Date.now() - recFetched > 10000) refreshRecording();
    if (cur.status.mode !== seenMode || cur.status.running !== seenRunning) {
      seenMode = cur.status.mode; seenRunning = cur.status.running;
      if (!reloading) { reloading = true; reload().finally(() => { reloading = false; }); }
    }
  });

  return root;
}
