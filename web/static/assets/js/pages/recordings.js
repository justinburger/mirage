// Recordings: list, import/export, detail with endpoint stats and an
// exchange browser with a request/response drawer.
import { h, debounce } from '../dom.js';
import { icon } from '../icons.js';
import { api, apiUrl, request, ApiError } from '../api.js';
import { serviceName } from '../state.js';
import { navigate, replaceQuery } from '../router.js';
import { card, btn, iconBtn, dataTable, emptyState, popMenu, confirmDialog, toastOk, badge, field, textCtl, textareaCtl, selectCtl, switchCtl, segmentedCtl, openModal, errorBox, alertBox, pageHeader, statusPill, methodTag, kvCtl, loading, toastError } from '../ui.js';
import { groupStatus } from '../chart.js';
import { exchangeSections, timingStrip, changesTable, marksFromChanges } from '../bodyview.js';
import { fmtInt, fmtBytes, fmtDur, fmtPct, fmtDateTime, fmtTime, relTime, fmtHuman, durMs, DASH, toDate } from '../format.js';

const SOURCE_LABEL = { proxy: 'captured', har: 'HAR import', copy: 'anonymized copy' };

function spanText(rec) {
  const a = toDate(rec.first_at), b = toDate(rec.last_at);
  if (!a || !b) return DASH;
  return `${fmtDateTime(a)} → ${fmtTime(b, false)} (${fmtHuman(b - a)})`;
}

function anonBadge(rec) {
  return rec.anonymized
    ? badge('anonymized', 'ok', { dot: true, title: 'Every exchange has been through an anonymization profile' })
    : badge('raw', 'warn', { dot: true, title: 'Contains values exactly as captured. Anonymize before sharing.' });
}

export function exportHref(rec, profileId) {
  return apiUrl(`/recordings/${encodeURIComponent(rec.id)}/export.har`, profileId ? { profile_id: profileId } : null);
}

async function exportMenu(anchor, rec) {
  const profiles = await api.profiles().catch(() => []);
  popMenu(anchor, [
    { label: 'Download HAR (as stored)', icon: 'download', href: exportHref(rec), download: `${rec.name}.har` },
    '-',
    ...profiles.map((p) => ({ label: `Anonymize on export: ${p.name}`, icon: 'shield', href: exportHref(rec, p.id), download: `${rec.name}.anon.har` })),
  ]);
}

// ------------------------------------------------------------- modals
export async function openImportModal({ onDone } = {}) {
  const profiles = await api.profiles().catch(() => []);
  let file = null;
  const fileInput = h('input', { type: 'file', accept: '.har,.json,application/json', class: 'sr-only', id: 'har-file', tabindex: -1 });
  const fileLabel = h('div', { class: 'dz-file' }, 'Drop a .har file here or click to choose');
  const sizeLabel = h('div', { class: 'muted', style: 'font-size:12.5px;margin-top:4px' }, 'HTTP Archive 1.2, e.g. exported from browser DevTools or another proxy');
  const zone = h('label', { class: 'dropzone', for: 'har-file' }, icon('upload', 22), fileLabel, sizeLabel);
  const nameC = textCtl({ placeholder: 'Defaults to the file name' });
  const anon = switchCtl({ label: 'Anonymize while importing', help: 'Raw values are never stored when a profile is applied.', checked: true });
  const profile = selectCtl(profiles.length ? profiles.map((p) => ({ value: p.id, label: p.name })) : [{ value: 'default', label: 'Default' }], { value: 'default' });
  const profF = field('Anonymization profile', profile);
  anon.onChange(() => { profF.hidden = !anon.get(); });
  const errHost = h('div');
  function setFile(f) {
    file = f;
    fileLabel.textContent = f ? f.name : 'Drop a .har file here or click to choose';
    sizeLabel.textContent = f ? fmtBytes(f.size) : 'HTTP Archive 1.2, e.g. exported from browser DevTools or another proxy';
    if (f && !nameC.get()) nameC.set(f.name.replace(/\.(har|json)$/i, ''));
  }
  fileInput.addEventListener('change', () => setFile(fileInput.files[0] || null));
  zone.addEventListener('dragover', (e) => { e.preventDefault(); zone.classList.add('is-over'); });
  zone.addEventListener('dragleave', () => zone.classList.remove('is-over'));
  zone.addEventListener('drop', (e) => { e.preventDefault(); zone.classList.remove('is-over'); if (e.dataTransfer.files[0]) setFile(e.dataTransfer.files[0]); });
  const go = btn('Import', {
    kind: 'primary',
    onClick: async () => {
      errHost.replaceChildren();
      if (!file) { errHost.replaceChildren(alertBox('error', 'Choose a HAR file first.')); return; }
      const fd = new FormData();
      fd.append('file', file, file.name);
      try {
        const rec = await request('POST', '/recordings/import', { body: fd, query: { name: nameC.get().trim(), profile_id: anon.get() ? profile.get() : '' } });
        toastOk(`Imported ${fmtInt(rec.exchange_count)} exchanges into ${rec.name}`);
        m.close(rec);
        if (onDone) onDone(rec); else navigate(`/recordings/${encodeURIComponent(rec.id)}`);
      } catch (e) {
        if (!(e instanceof ApiError)) throw e;
        errHost.replaceChildren(errorBox(e));
      }
    },
  });
  const m = openModal({
    title: 'Import HAR', subtitle: 'Create a recording from a HAR file.', size: 'md',
    content: h('div', { class: 'stack' }, errHost, zone, fileInput, field('Recording name', nameC), anon.el, profF),
    footer: [btn('Cancel', { onClick: () => m.close(null) }), go],
  });
  return m;
}

export function openNewRecordingModal({ onDone } = {}) {
  const nameC = textCtl({ placeholder: 'e.g. checkout rehearsal' });
  const descC = textareaCtl({ rows: 3, placeholder: 'Optional notes', mono: false });
  const nameF = field('Name', nameC, { required: true });
  const errHost = h('div');
  const go = btn('Create recording', {
    kind: 'primary',
    onClick: async () => {
      nameF.setError('');
      if (!nameC.get().trim()) { nameF.setError('Name is required.'); return; }
      try {
        const rec = await api.createRecording({ name: nameC.get().trim(), description: descC.get().trim() });
        toastOk(`Created ${rec.name}`);
        m.close(rec);
        if (onDone) onDone(rec);
      } catch (e) { if (!(e instanceof ApiError)) throw e; errHost.replaceChildren(errorBox(e)); }
    },
  });
  const m = openModal({ title: 'New empty recording', size: 'sm', content: h('div', { class: 'stack-sm' }, errHost, nameF, field('Description', descC)), footer: [btn('Cancel', { onClick: () => m.close(null) }), go] });
  return m;
}

export async function openAnonymizeCopyModal(rec) {
  const profiles = await api.profiles().catch(() => []);
  const profile = selectCtl(profiles.map((p) => ({ value: p.id, label: p.name })), { value: 'default' });
  const nameC = textCtl({ value: `${rec.name} (anon)` });
  const errHost = h('div');
  const desc = h('p', { class: 'explain' });
  const paint = () => { const p = profiles.find((x) => x.id === profile.get()); desc.textContent = p ? (p.description || `${(p.detectors || []).length} detectors, ${(p.rules || []).length} rules.`) : ''; };
  profile.onChange(paint); paint();
  const go = btn('Create anonymized copy', {
    kind: 'primary',
    onClick: async () => {
      errHost.replaceChildren();
      try {
        const copy = await api.anonymizeRecording(rec.id, { profile_id: profile.get(), name: nameC.get().trim() || `${rec.name} (anon)` });
        toastOk(`Created ${copy.name}`);
        m.close(copy);
        navigate(`/recordings/${encodeURIComponent(copy.id)}`);
      } catch (e) { if (!(e instanceof ApiError)) throw e; errHost.replaceChildren(errorBox(e)); }
    },
  });
  const m = openModal({
    title: 'Create anonymized copy', subtitle: `Source: ${rec.name}`, size: 'sm',
    content: h('div', { class: 'stack-sm' }, errHost, field('Profile', profile), desc, field('Name of the copy', nameC),
      h('p', { class: 'explain' }, 'The original stays untouched. Hash and fake values are keyed by the profile secret, so the same customer or email maps to the same token everywhere in the copy.')),
    footer: [btn('Cancel', { onClick: () => m.close(null) }), go],
  });
  return m;
}

export function openEditRecordingModal(rec, onSaved) {
  const nameC = textCtl({ value: rec.name });
  const descC = textareaCtl({ value: rec.description || '', rows: 3, mono: false });
  const labels = kvCtl({ value: rec.labels || {}, keyPlaceholder: 'label', valuePlaceholder: 'value', addLabel: 'Add label' });
  const nameF = field('Name', nameC, { required: true });
  const errHost = h('div');
  const go = btn('Save', {
    kind: 'primary',
    onClick: async () => {
      nameF.setError('');
      if (!nameC.get().trim()) { nameF.setError('Name is required.'); return; }
      try {
        const updated = await api.patchRecording(rec.id, { name: nameC.get().trim(), description: descC.get(), labels: labels.get() });
        toastOk('Recording updated');
        m.close(updated);
        if (onSaved) onSaved(updated);
      } catch (e) { if (!(e instanceof ApiError)) throw e; errHost.replaceChildren(errorBox(e)); }
    },
  });
  const m = openModal({ title: 'Edit recording', size: 'sm', content: h('div', { class: 'stack-sm' }, errHost, nameF, field('Description', descC), field('Labels', labels)), footer: [btn('Cancel', { onClick: () => m.close(null) }), go] });
  return m;
}

export async function deleteRecording(rec) {
  const ok = await confirmDialog({
    title: `Delete ${rec.name}?`, danger: true, confirmText: 'Delete recording',
    message: h('div', null, `All ${fmtInt(rec.exchange_count)} exchanges (${fmtBytes(rec.bytes)}) are permanently removed. Services that use this recording must be switched first; replays that reference it will fail.`),
  });
  if (!ok) return false;
  try {
    await api.deleteRecording(rec.id);
  } catch (e) {
    if (e instanceof ApiError && e.status === 409) { toastError(e); return false; }
    throw e;
  }
  toastOk(`Deleted ${rec.name}`);
  return true;
}

// ------------------------------------------------------------------ list
export async function list(ctx) {
  ctx.setCrumbs([{ label: 'Recordings' }]);
  let recs = await api.recordings();
  const root = h('div', { class: 'stack' });
  root.appendChild(pageHeader({
    subtitle: 'Captured and imported traffic. Anonymize it, serve it as a virtual service or replay it as a load test.',
    actions: [btn('New recording', { icon: 'plus', onClick: () => openNewRecordingModal({ onDone: (rec) => navigate(`/recordings/${encodeURIComponent(rec.id)}`) }) }), btn('Import HAR', { icon: 'upload', kind: 'primary', onClick: () => openImportModal() })],
  }));
  const table = dataTable({
    rowKey: (r) => r.id, sortKey: 'created', sortDir: 'desc',
    onRowClick: (r) => navigate(`/recordings/${encodeURIComponent(r.id)}`),
    empty: () => emptyState({
      icon: 'recordings', title: 'No recordings yet',
      text: 'Record traffic by switching a service to Record mode, or import a HAR file from your browser or another tool.',
      actions: [btn('Import a HAR file', { icon: 'upload', kind: 'primary', onClick: () => openImportModal() }), btn('Go to services', { href: '#/services' })],
    }),
    columns: [
      { key: 'name', label: 'Recording', sortValue: (r) => r.name.toLowerCase(), render: (r) => h('div', { class: 'cell-main' }, h('a', { class: 'cell-name', href: `#/recordings/${encodeURIComponent(r.id)}` }, r.name), r.description ? h('span', { class: 'cell-sub', title: r.description }, r.description) : null) },
      { key: 'source', label: 'Source', render: (r) => h('span', { class: 'row', style: 'gap:6px' }, badge(SOURCE_LABEL[r.source] || r.source, 'muted'), r.service_id ? h('a', { href: `#/services/${encodeURIComponent(r.service_id)}`, class: 'muted' }, serviceName(r.service_id)) : null) },
      { key: 'anonymized', label: 'Privacy', sortValue: (r) => (r.anonymized ? 1 : 0), render: (r) => anonBadge(r) },
      { key: 'exchange_count', label: 'Exchanges', align: 'right', render: (r) => fmtInt(r.exchange_count) },
      { key: 'error_count', label: 'Errors', align: 'right', sortValue: (r) => r.error_count, render: (r) => h('span', { style: r.error_count ? 'color:var(--warn)' : null }, fmtInt(r.error_count), r.exchange_count ? h('span', { class: 'muted' }, ` (${fmtPct(r.error_count / r.exchange_count)})`) : null) },
      { key: 'bytes', label: 'Size', align: 'right', render: (r) => fmtBytes(r.bytes) },
      { key: 'span', label: 'Time span', cls: 'hide-md', sortValue: (r) => (r.first_at ? Date.parse(r.first_at) : 0), render: (r) => h('span', { class: 'muted', style: 'font-size:12.5px' }, spanText(r)) },
      { key: 'created', label: 'Created', cls: 'hide-md', sortValue: (r) => Date.parse(r.created_at), render: (r) => relTime(r.created_at) },
      {
        key: 'actions', label: '', sortable: false, cls: 'actions',
        render: (r) => h('div', { class: 'cell-actions' },
          (() => { const b = iconBtn('download', 'Export HAR', () => {}); b.addEventListener('click', () => exportMenu(b, r)); return b; })(),
          iconBtn('replays', 'Replay this recording', () => navigate('/replays/new', { recording: r.id })),
          iconBtn('trash', 'Delete recording', async () => { if (await deleteRecording(r)) reloadList(); })),
        sig: (r) => r.id,
      },
    ],
  });
  root.appendChild(card({ flush: true, body: table.el }));
  table.setRows(recs);
  async function reloadList() {
    recs = await api.recordings();
    if (ctx.isActive()) table.setRows(recs);
  }
  ctx.every(() => { reloadList().catch(() => {}); }, 5000);
  return root;
}

// ---------------------------------------------------------------- detail
const PAGE_SIZES = [50, 100, 200];

export async function detail(ctx) {
  const id = ctx.params.id;
  let rec = await api.recording(id);
  ctx.setCrumbs([{ label: 'Recordings', href: '#/recordings' }, { label: rec.name }]);
  const [endpoints, profiles] = await Promise.all([api.endpoints(id).catch(() => []), api.profiles().catch(() => [])]);

  const root = h('div', { class: 'stack' });
  const headHost = h('div');
  const statsHost = h('div');
  root.append(headHost, statsHost);

  function paintHead() {
    const exportBtn = btn('Export HAR', { icon: 'download', iconRight: 'chevron-down' });
    exportBtn.addEventListener('click', () => exportMenu(exportBtn, rec));
    headHost.replaceChildren(
      h('div', { class: 'page-head', style: 'margin-bottom:0' },
        h('div', { class: 'page-titles' },
          h('h2', { class: 'page-title' }, rec.name, anonBadge(rec), badge(SOURCE_LABEL[rec.source] || rec.source, 'muted')),
          rec.description ? h('p', { class: 'page-sub' }, rec.description) : null,
          Object.keys(rec.labels || {}).length ? h('div', { class: 'row', style: 'margin-top:8px' }, Object.entries(rec.labels).map(([k, v]) => badge(`${k}: ${v}`, 'muted'))) : null),
        h('div', { class: 'page-actions' },
          btn('Replay', { icon: 'replays', kind: 'primary', href: `#/replays/new?recording=${encodeURIComponent(rec.id)}` }),
          btn('Anonymized copy', { icon: 'shield', onClick: () => openAnonymizeCopyModal(rec) }),
          exportBtn,
          btn('Edit', { icon: 'edit', onClick: () => openEditRecordingModal(rec, (u) => { rec = u; ctx.setCrumbs([{ label: 'Recordings', href: '#/recordings' }, { label: rec.name }]); paintHead(); }) }),
          btn('Delete', { icon: 'trash', kind: 'danger', onClick: async () => { if (await deleteRecording(rec)) navigate('/recordings'); } }))));
  }
  function cell(k, v, s) { return h('div', { class: 'stat-cell' }, h('div', { class: 'stat-k' }, k), h('div', { class: 'stat-v' }, v), h('div', { class: 'stat-s' }, s || ' ')); }
  function paintStats() {
    const errRate = rec.exchange_count ? rec.error_count / rec.exchange_count : 0;
    statsHost.replaceChildren(
      !rec.anonymized && rec.exchange_count ? alertBox('warn', 'This recording holds values exactly as captured (emails, card numbers, tokens). Create an anonymized copy before sharing it or replaying it against shared environments.', { title: 'Raw data' }) : '',
      h('div', { style: 'height:12px' }),
      h('div', { class: 'stat-grid' },
        cell('Exchanges', fmtInt(rec.exchange_count), `${endpoints.length} endpoints`),
        cell('Errors', fmtInt(rec.error_count), `${fmtPct(errRate)} of traffic`),
        cell('Captured size', fmtBytes(rec.bytes), 'request + response bodies'),
        cell('Time span', rec.first_at && rec.last_at ? fmtHuman(Date.parse(rec.last_at) - Date.parse(rec.first_at)) : DASH, rec.first_at ? fmtDateTime(rec.first_at) : ''),
        cell('Source', rec.service_id ? h('a', { href: `#/services/${encodeURIComponent(rec.service_id)}` }, serviceName(rec.service_id)) : SOURCE_LABEL[rec.source] || rec.source, `created ${relTime(rec.created_at)}`)));
  }
  paintHead(); paintStats();

  // --- endpoints
  const epFilter = { endpoint: ctx.query.endpoint || '' };
  const epTable = dataTable({
    rowKey: (r) => r.endpoint, sortKey: 'count', sortDir: 'desc', dense: true,
    onRowClick: (r) => { setFilter({ endpoint: r.endpoint }); document.getElementById('exchanges-card').scrollIntoView({ behavior: 'smooth', block: 'start' }); },
    empty: () => h('div', { class: 'empty', style: 'padding:24px' }, h('div', { class: 'empty-text' }, 'No exchanges recorded yet.')),
    columns: [
      { key: 'endpoint', label: 'Endpoint', render: (r) => h('code', { style: 'background:none;padding:0' }, r.endpoint) },
      { key: 'count', label: 'Count', align: 'right', render: (r) => fmtInt(r.count) },
      { key: 'error_rate', label: 'Errors', align: 'right', render: (r) => h('span', { style: r.error_rate > 0.02 ? 'color:var(--err)' : r.error_rate > 0 ? 'color:var(--warn)' : null }, fmtPct(r.error_rate)) },
      { key: 'avg', label: 'Avg', align: 'right', sortValue: (r) => durMs(r.avg), render: (r) => fmtDur(r.avg) },
      { key: 'p50', label: 'p50', align: 'right', sortValue: (r) => durMs(r.p50), render: (r) => fmtDur(r.p50) },
      { key: 'p95', label: 'p95', align: 'right', sortValue: (r) => durMs(r.p95), render: (r) => fmtDur(r.p95) },
      { key: 'p99', label: 'p99', align: 'right', sortValue: (r) => durMs(r.p99), render: (r) => fmtDur(r.p99) },
      { key: 'max', label: 'Max', align: 'right', sortValue: (r) => durMs(r.max), render: (r) => fmtDur(r.max) },
      {
        key: 'mix', label: 'Status mix', sortable: false, width: '170px',
        render: (r) => {
          const g = groupStatus(r.status_counts);
          const total = Object.values(g).reduce((a, b) => a + b, 0) || 1;
          const bar = h('div', { class: 'statusbar', style: 'height:8px;min-width:110px' });
          for (const c of ['1xx', '2xx', '3xx', '4xx', '5xx', 'err']) if (g[c]) bar.appendChild(h('div', { class: `statusbar-seg sc-${c}`, style: { flex: `${g[c]} 1 0`, minWidth: '3px' }, title: `${c}: ${g[c]} (${((g[c] / total) * 100).toFixed(1)}%)` }));
          return bar;
        },
        sig: (r) => JSON.stringify(r.status_counts),
      },
    ],
  });
  epTable.setRows(endpoints);
  root.appendChild(card({ title: 'Endpoints', subtitle: 'Click an endpoint to filter the exchanges below', flush: true, body: epTable.el }));

  // --- exchanges
  const q = ctx.query;
  const f = {
    method: q.method || '', status_class: q.status_class || '', path: q.path || '', endpoint: epFilter.endpoint,
    errors: q.errors === '1', desc: q.desc === '1', offset: Number(q.offset) || 0, limit: Number(q.limit) || 50,
  };
  const methodSel = selectCtl([{ value: '', label: 'Any method' }, ...['GET', 'POST', 'PUT', 'PATCH', 'DELETE'].map((m) => ({ value: m, label: m }))], { value: f.method });
  const classSel = selectCtl([{ value: '', label: 'Any status' }, { value: '2', label: '2xx success' }, { value: '3', label: '3xx redirect' }, { value: '4', label: '4xx client error' }, { value: '5', label: '5xx server error' }], { value: f.status_class });
  const pathIn = textCtl({ value: f.path, placeholder: 'Search path or query' });
  const epSel = selectCtl([{ value: '', label: 'Any endpoint' }, ...endpoints.map((e) => ({ value: e.endpoint, label: e.endpoint }))], { value: f.endpoint });
  const errSw = switchCtl({ label: 'Errors only', checked: f.errors });
  const orderSeg = segmentedCtl([{ value: 'asc', label: 'Oldest first' }, { value: 'desc', label: 'Newest first' }], { value: f.desc ? 'desc' : 'asc', size: 'sm' });
  const clearBtn = btn('Clear', { kind: 'ghost', size: 'sm', onClick: () => setFilter({ method: '', status_class: '', path: '', endpoint: '', errors: false }) });
  const filters = h('div', { class: 'filters' },
    field('Method', methodSel), field('Status', classSel), field('Path', pathIn), field('Endpoint', epSel),
    h('div', { style: 'padding-bottom:6px' }, errSw.el), h('div', { style: 'padding-bottom:3px' }, clearBtn));
  const info = h('span');
  const exTable = dataTable({
    rowKey: (r) => r.id, dense: true,
    onRowClick: (r) => openDrawer(r.id),
    empty: () => emptyState({ icon: 'search', title: 'No exchanges match', text: 'Try clearing a filter.' }),
    columns: [
      { key: 'seq', label: '#', align: 'right', sortable: false, render: (r) => h('span', { class: 'muted' }, String(r.seq)) },
      { key: 'time', label: 'Time', sortable: false, render: (r) => h('span', { class: 'mono muted', style: 'font-size:12px' }, fmtTime(r.started_at)) },
      { key: 'method', label: 'Method', sortable: false, render: (r) => methodTag(r.method) },
      { key: 'path', label: 'Path', sortable: false, render: (r) => h('div', { class: 'cell-main' }, h('span', { class: 'mono truncate', style: 'max-width:46ch', title: r.path + (r.raw_query ? '?' + r.raw_query : '') }, r.path + (r.raw_query ? '?' + r.raw_query : '')), r.error ? h('span', { class: 'cell-sub', style: 'color:var(--err)' }, r.error) : null) },
      { key: 'status', label: 'Status', sortable: false, render: (r) => statusPill(r.status) },
      { key: 'endpoint', label: 'Endpoint', sortable: false, render: (r) => h('code', { style: 'background:none;padding:0;color:var(--text-3)' }, r.endpoint) },
      { key: 'duration', label: 'Duration', align: 'right', sortable: false, render: (r) => fmtDur(r.duration) },
      { key: 'resp', label: 'Response', align: 'right', sortable: false, render: (r) => fmtBytes(r.resp_bytes) },
    ],
  });
  const exWrap = h('div', { class: 'table-wrap scroll-y' }, exTable.el);
  const pager = h('div', { class: 'pager' });
  const pageSel = selectCtl(PAGE_SIZES.map((n) => ({ value: String(n), label: `${n} per page` })), { value: String(f.limit) });
  const exCard = card({ title: 'Exchanges', subtitle: h('span', null, info), actions: [orderSeg.el], body: h('div', null, filters, exWrap, pager) });
  exCard.id = 'exchanges-card';
  root.appendChild(exCard);

  let total = 0;
  let items = [];
  let loadSeq = 0;
  async function load() {
    const my = ++loadSeq;
    const query = { method: f.method, status_class: f.status_class, path: f.path, endpoint: f.endpoint, errors: f.errors, desc: f.desc, offset: f.offset, limit: f.limit };
    exWrap.style.opacity = '.6';
    let res;
    try { res = await api.exchanges(id, query); } catch (e) { if (my === loadSeq) { exWrap.style.opacity = ''; toastError(e); } return; }
    if (my !== loadSeq || !ctx.isActive()) return;
    exWrap.style.opacity = '';
    items = res.items || [];
    total = res.total || 0;
    exTable.setRows(items);
    info.textContent = `${fmtInt(total)} matching ${total === 1 ? 'exchange' : 'exchanges'}${f.endpoint ? ` in ${f.endpoint}` : ''}`;
    paintPager();
    const qs = {};
    for (const k of ['method', 'status_class', 'path', 'endpoint']) if (f[k]) qs[k] = f[k];
    if (f.errors) qs.errors = '1';
    if (f.desc) qs.desc = '1';
    if (f.offset) qs.offset = f.offset;
    if (f.limit !== 50) qs.limit = f.limit;
    replaceQuery(qs);
  }
  function paintPager() {
    const from = total ? f.offset + 1 : 0;
    const to = Math.min(total, f.offset + f.limit);
    pager.replaceChildren(
      h('span', null, `${fmtInt(from)}–${fmtInt(to)} of ${fmtInt(total)}`),
      h('div', { class: 'row' },
        pageSel.el,
        iconBtn('chevron-left', 'Previous page', () => { f.offset = Math.max(0, f.offset - f.limit); load(); }, { disabled: f.offset === 0 }),
        iconBtn('chevron-right', 'Next page', () => { f.offset += f.limit; load(); }, { disabled: f.offset + f.limit >= total })));
    pageSel.el.style.width = '130px';
  }
  function setFilter(patch) {
    Object.assign(f, patch, { offset: 0 });
    methodSel.set(f.method); classSel.set(f.status_class); pathIn.set(f.path); epSel.set(f.endpoint); errSw.set(f.errors);
    load();
  }
  methodSel.onChange((v) => setFilter({ method: v }));
  classSel.onChange((v) => setFilter({ status_class: v }));
  epSel.onChange((v) => setFilter({ endpoint: v }));
  errSw.onChange((v) => setFilter({ errors: v }));
  orderSeg.onChange((v) => setFilter({ desc: v === 'desc' }));
  pageSel.onChange((v) => { f.limit = Number(v); f.offset = 0; load(); });
  const debouncedPath = debounce((v) => setFilter({ path: v.trim() }), 300);
  pathIn.onChange(debouncedPath);
  ctx.onCleanup(() => debouncedPath.cancel());

  function openDrawer(exchangeId) {
    openExchangeDrawer({ exchangeId, ids: items.map((x) => x.id), profiles, onNavigate: (x) => openDrawer(x) });
  }
  await load();
  if (ctx.query.x) openDrawer(Number(ctx.query.x));

  return root;
}

// ---------------------------------------------------------------- drawer
let currentDrawer = null;

export async function openExchangeDrawer({ exchangeId, ids = [], profiles = [], onNavigate }) {
  if (currentDrawer) { const d = currentDrawer; currentDrawer = null; d.close(); }
  const idx = ids.indexOf(exchangeId);
  const body = h('div', null, loading());
  const title = h('span', null, `Exchange #${exchangeId}`);
  const m = openModal({
    title: title, drawer: true, size: 'lg',
    content: body,
    footer: [
      h('span', { class: 'spacer muted', style: 'font-size:12.5px' }, ids.length && idx >= 0 ? `${idx + 1} of ${ids.length} on this page` : ''),
      btn('Previous', { icon: 'chevron-left', disabled: idx <= 0, onClick: () => { m.close(); onNavigate && onNavigate(ids[idx - 1]); } }),
      btn('Next', { iconRight: 'chevron-right', disabled: idx < 0 || idx >= ids.length - 1, onClick: () => { m.close(); onNavigate && onNavigate(ids[idx + 1]); } }),
    ],
    onClose: () => { if (currentDrawer === m) currentDrawer = null; },
  });
  currentDrawer = m;
  let ex;
  try { ex = await api.exchange(exchangeId); } catch (e) { body.replaceChildren(errorBox(e)); return m; }
  m.setTitle(`${ex.request.method} ${ex.request.path}`);
  body.replaceChildren();

  const preview = { data: null };
  const view = h('div');
  const tabsHost = h('div', { style: 'margin-bottom:14px' });
  const profSel = selectCtl(profiles.length ? profiles.map((p) => ({ value: p.id, label: p.name })) : [{ value: 'default', label: 'Default' }], { value: 'default' });
  profSel.el.style.maxWidth = '220px';
  let which = 'original';
  function paint() {
    view.replaceChildren();
    if (which === 'original') view.appendChild(exchangeSections(ex, { marks: preview.data ? marksFromChanges(preview.data.changes, 'before') : [] }));
    else if (which === 'anonymized') view.appendChild(exchangeSections(preview.data.exchange, { marks: marksFromChanges(preview.data.changes, 'after') }));
    else view.appendChild(changesTable(preview.data.changes));
  }
  function paintTabs() {
    tabsHost.replaceChildren();
    if (!preview.data) return;
    const seg = segmentedCtl([
      { value: 'original', label: 'Original' },
      { value: 'anonymized', label: 'Anonymized' },
      { value: 'changes', label: `Changes (${preview.data.changes.length})` },
    ], { value: which, label: 'Preview view' });
    seg.onChange((v) => { which = v; paint(); });
    tabsHost.appendChild(h('div', { class: 'row-between' }, seg.el, btn('Close preview', { kind: 'ghost', size: 'sm', onClick: () => { preview.data = null; which = 'original'; paintTabs(); paint(); } })));
  }
  const previewBtn = btn('Preview anonymization', {
    icon: 'shield',
    onClick: async () => {
      const res = await api.preview({ profile_id: profSel.get(), exchange_id: ex.id });
      preview.data = res;
      which = 'changes';
      paintTabs(); paint();
    },
  });
  body.append(
    h('div', { class: 'row', style: 'margin-bottom:14px' }, statusPill(ex.response.status), h('span', { class: 'mono muted' }, `#${ex.seq} in recording`), ex.anonymized ? badge('anonymized', 'ok', { dot: true }) : badge('raw', 'warn', { dot: true }),
      ex.error ? badge(ex.error, 'err') : null,
      h('div', { class: 'grow' }), profSel.el, previewBtn),
    timingStrip(ex), tabsHost, view);
  paint();
  return m;
}
void field; void icon;
