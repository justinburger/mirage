// Fault injection rules.
import { h } from '../dom.js';
import { icon } from '../icons.js';
import { api, ApiError } from '../api.js';
import { state, refreshServices } from '../state.js';
import { card, btn, iconBtn, dataTable, emptyState, confirmDialog, toastOk, badge, field, formSection, formGrid, textCtl, textareaCtl, selectCtl, switchCtl, segmentedCtl, numCtl, durCtl, kvCtl, chipsCtl, sliderCtl, openModal, errorBox, alertBox, applyProblems, scrollToFirstError, pageHeader, toastError } from '../ui.js';
import { fmtInt, fmtPct, fmtBytes, fmtDur, toGoDuration, DASH } from '../format.js';
import { durText } from '../service-form.js';

const TYPES = [
  { value: 'error', label: 'Error response' },
  { value: 'latency', label: 'Latency' },
  { value: 'abort', label: 'Connection reset' },
  { value: 'timeout', label: 'Hung request' },
  { value: 'throttle', label: 'Throttle' },
];
const TYPE_BADGE = { error: 'err', latency: 'info', abort: 'err', timeout: 'warn', throttle: 'warn' };
const TYPE_LABEL = Object.fromEntries(TYPES.map((t) => [t.value, t.label]));

const PRESETS = [
  { label: '10% 503 errors', icon: 'alert', rule: { name: '10% 503 errors', type: 'error', probability: 0.1, status: 503, body: '{"error":"service unavailable"}', response_headers: { 'Retry-After': '1' } } },
  { label: '+250ms latency ±50ms', icon: 'clock', rule: { name: '+250ms latency (±50ms)', type: 'latency', probability: 1, delay: '250ms', jitter: '50ms' } },
  { label: '1% connection resets', icon: 'x', rule: { name: '1% connection resets', type: 'abort', probability: 0.01 } },
  { label: '5% hung requests (30s)', icon: 'pause', rule: { name: '5% hung requests (30s)', type: 'timeout', probability: 0.05, delay: '30s' } },
  { label: 'Throttle to 50 KB/s', icon: 'gauge', rule: { name: 'Throttle to 50 KB/s', type: 'throttle', probability: 1, bytes_per_second: 51200 } },
];

function detail(r) {
  switch (r.type) {
    case 'error': return `HTTP ${r.status || 503}`;
    case 'latency': return `+${fmtDur(r.delay)}${r.jitter && r.jitter !== '0s' ? ` ±${fmtDur(r.jitter)}` : ''}`;
    case 'abort': return 'drop connection';
    case 'timeout': return `hang ${fmtDur(r.delay)}`;
    case 'throttle': return `${fmtBytes(r.bytes_per_second)}/s`;
    default: return DASH;
  }
}

function payloadOf(r, patch = {}) {
  const { created_at, updated_at, fired, ...rest } = r;
  return { ...rest, ...patch };
}

export default async function faults(ctx) {
  ctx.setCrumbs([{ label: 'Faults' }]);
  if (!state.servicesLoaded) await refreshServices().catch(() => {});
  let rules = await api.faults();
  const svcName = (id) => { const s = state.services.find((x) => x.id === id); return s ? s.name : id; };

  const root = h('div', { class: 'stack' });
  root.appendChild(pageHeader({
    subtitle: 'Inject failures into live traffic to see how clients behave. Rules apply in every service mode, before the request is proxied or virtualized, and fire independently with their probability.',
    actions: btn('New rule', { icon: 'plus', kind: 'primary', onClick: () => openFaultForm({ onSaved: reload }) }),
  }));
  root.appendChild(card({
    title: 'Quick presets', subtitle: 'Opens the editor pre-filled so you can pick a service first',
    body: h('div', { class: 'preset-row' }, PRESETS.map((p) => h('button', { type: 'button', class: 'preset-card', onclick: () => openFaultForm({ preset: p.rule, onSaved: reload }) }, icon(p.icon, 15), p.label))),
  }));

  const toggles = new Map();
  const getToggle = (r) => {
    let t = toggles.get(r.id);
    if (!t) {
      const sw = switchCtl({ checked: r.enabled });
      sw.input.setAttribute('aria-label', `Enable ${r.name}`);
      sw.onChange(async (v) => {
        try {
          await api.updateFault(r.id, payloadOf(t.rule, { enabled: v }));
          t.rule.enabled = v;
          toastOk(`${t.rule.name} ${v ? 'enabled' : 'disabled'}`);
        } catch (e) { sw.set(!v); toastError(e); }
      });
      t = { sw, rule: r };
      toggles.set(r.id, t);
    }
    t.rule = r;
    if (t.sw.get() !== r.enabled) t.sw.set(r.enabled);
    return t.sw.el;
  };
  const table = dataTable({
    rowKey: (r) => r.id, sortKey: 'order',
    onRowClick: (r) => openFaultForm({ rule: r, onSaved: reload }),
    empty: () => emptyState({ icon: 'faults', title: 'No fault rules', text: 'Start from a preset above, for example 10% 503 errors on a service, to rehearse failure handling.', actions: [btn('New rule', { icon: 'plus', kind: 'primary', onClick: () => openFaultForm({ onSaved: reload }) })] }),
    columns: [
      { key: 'enabled', label: 'On', sortValue: (r) => (r.enabled ? 1 : 0), render: (r) => getToggle(r), sig: (r) => `${r.id}|${r.enabled}`, width: '64px' },
      { key: 'name', label: 'Rule', sortValue: (r) => r.name.toLowerCase(), render: (r) => h('div', { class: 'cell-main' }, h('span', { class: 'cell-name' }, r.name), h('span', { class: 'cell-sub' }, [r.methods && r.methods.length ? r.methods.join(', ') : 'any method', r.path_pattern || 'any path'].join('  ·  '))), sig: (r) => r.name + (r.methods || []).join() + r.path_pattern },
      { key: 'type', label: 'Fault', render: (r) => h('div', { class: 'cell-main' }, badge(TYPE_LABEL[r.type] || r.type, TYPE_BADGE[r.type] || 'muted'), h('span', { class: 'cell-sub' }, detail(r))), sig: (r) => r.type + detail(r) },
      { key: 'probability', label: 'Probability', align: 'right', render: (r) => fmtPct(r.probability), sig: (r) => String(r.probability) },
      { key: 'scope', label: 'Scope', sortValue: (r) => (r.service_id ? svcName(r.service_id) : ''), render: (r) => (r.service_id ? h('a', { href: `#/services/${encodeURIComponent(r.service_id)}` }, svcName(r.service_id)) : h('span', { class: 'muted' }, 'All services')) },
      { key: 'fired', label: 'Fired', align: 'right', render: (r) => fmtInt(r.fired || 0) },
      {
        key: 'actions', label: '', sortable: false, cls: 'actions',
        render: (r) => h('div', { class: 'cell-actions' }, iconBtn('edit', 'Edit rule', () => openFaultForm({ rule: r, onSaved: reload })),
          iconBtn('trash', 'Delete rule', async () => {
            const ok = await confirmDialog({ title: `Delete ${r.name}?`, danger: true, confirmText: 'Delete rule', message: 'The rule stops firing immediately.' });
            if (!ok) return;
            await api.deleteFault(r.id);
            toastOk(`Deleted ${r.name}`);
            await reload();
          })),
        sig: (r) => r.id,
      },
    ],
  });
  root.appendChild(card({ title: 'Rules', subtitle: h('span', { class: 'live-note' }, 'Fire counts refresh every 2 seconds'), flush: true, body: table.el }));
  const paint = () => table.setRows(rules.slice());
  paint();
  async function reload() {
    try { rules = await api.faults(); } catch (_) { return; }
    if (ctx.isActive()) paint();
  }
  ctx.every(reload, 2000);
  ctx.on('services', paint);
  return root;
}

// ----------------------------------------------------------------- form
export async function openFaultForm({ rule = null, preset = null, onSaved } = {}) {
  if (!state.servicesLoaded) await refreshServices().catch(() => {});
  const editing = !!rule;
  const r = rule || preset || {};
  const name = textCtl({ value: r.name || '', placeholder: 'e.g. 10% 503s on checkout' });
  const enabled = switchCtl({ label: 'Enabled', help: 'Rules start firing as soon as they are saved while enabled.', checked: r.enabled ?? true });
  const scope = selectCtl([{ value: '', label: 'All services' }, ...state.services.map((s) => ({ value: s.id, label: s.name }))], { value: r.service_id || '' });
  const methods = chipsCtl({ value: r.methods || [], placeholder: 'Any method', validate: (v) => (/^[A-Za-z]+$/.test(v) ? null : 'Use a method name such as GET'), lower: false });
  const methodBtns = h('div', { class: 'row', style: 'gap:6px' }, ['GET', 'POST', 'PUT', 'PATCH', 'DELETE'].map((m) => h('button', { type: 'button', class: 'preset', onclick: () => { const cur = methods.get(); methods.set(cur.includes(m) ? cur.filter((x) => x !== m) : [...cur, m]); } }, m)));
  const path = textCtl({ value: r.path_pattern || '', placeholder: '/v1/charges/**', mono: true });
  const headers = kvCtl({ value: r.headers || {}, keyPlaceholder: 'Header', valuePlaceholder: 'exact value', addLabel: 'Add header match' });
  const prob = sliderCtl({ min: 0, max: 100, step: 0.5, value: Math.round((r.probability ?? 0.1) * 1000) / 10, presets: [1, 5, 10, 25, 50, 100], format: (v) => `${v}%`, unit: '%', numMin: 0, numMax: 100 });
  const order = numCtl({ value: r.order ?? 0, step: 1 });
  const type = segmentedCtl(TYPES, { value: r.type || 'error', label: 'Fault type' });
  // type specific
  const delay = durCtl({ value: durText(r.delay), placeholder: '250ms' });
  const jitter = durCtl({ value: durText(r.jitter), placeholder: '50ms' });
  const status = numCtl({ value: r.status || 503, min: 100, max: 599, step: 1 });
  const statusPresets = h('div', { class: 'presets' }, [429, 500, 502, 503, 504].map((c) => h('button', { type: 'button', class: 'preset', onclick: () => status.set(c) }, String(c))));
  const body = textareaCtl({ value: r.body || '', rows: 3, placeholder: '{"error":"overloaded"}' });
  const respHeaders = kvCtl({ value: r.response_headers || {}, keyPlaceholder: 'Retry-After', valuePlaceholder: '1', addLabel: 'Add response header' });
  const bps = numCtl({ value: r.bytes_per_second || 51200, min: 1, step: 1, unit: 'B/s' });
  const bpsPresets = h('div', { class: 'presets' }, [[10240, '10 KB/s'], [51200, '50 KB/s'], [102400, '100 KB/s'], [1048576, '1 MB/s']].map(([v, l]) => h('button', { type: 'button', class: 'preset', onclick: () => bps.set(v) }, l)));
  const hang = durCtl({ value: durText(r.delay) || '30s', placeholder: '30s' });

  const F = {
    name: field('Name', name, { required: true }),
    service_id: field('Applies to', scope, { help: 'Limit the rule to one service, or hit all of them.' }),
    methods: field('Methods', methods, { help: 'Empty means every method.' }),
    path_pattern: field('Path pattern', path, { help: '"*" matches one path segment, "**" any number: /api/**, /users/*/orders. Prefix with re: for a regular expression. Empty matches every path.' }),
    headers: field('Required request headers', headers, { help: 'The request must carry these headers with exactly these values.' }),
    probability: field('Probability', prob, { help: 'Share of matching requests that get the fault. Rules fire independently.' }),
    order: field('Order', order, { help: 'Ascending. Matters when several terminal faults (error, reset, hang) match: the first wins.' }),
    delay: field('Added delay', delay, { help: 'Latency faults add up when several match.' }),
    jitter: field('Jitter', jitter, { help: 'Uniform random offset of up to ± this much.' }),
    status: field('Status code', status),
    body: field('Response body', body),
    response_headers: field('Response headers', respHeaders),
    bytes_per_second: field('Bandwidth limit', bps, { help: 'Response bodies are streamed no faster than this. The lowest throttle wins.' }),
    hang: field('Hold the request for', hang, { help: 'Holds the request open for this long (or until the client gives up), then aborts.' }),
  };
  const blocks = {
    latency: h('div', null, formGrid(F.delay, F.jitter)),
    error: h('div', { class: 'stack-sm' }, formGrid(F.status), statusPresets, F.body, F.response_headers),
    abort: h('p', { class: 'explain' }, 'Drops the connection without sending a response: a connection reset for HTTP/1.1, a stream reset for HTTP/2. Good for testing client retries.'),
    timeout: h('div', null, F.hang),
    throttle: h('div', { class: 'stack-sm' }, F.bytes_per_second, bpsPresets),
  };
  const paint = () => { for (const [k, el] of Object.entries(blocks)) el.hidden = k !== type.get(); };
  type.onChange(paint); paint();
  const errHost = h('div');

  const save = btn(editing ? 'Save rule' : 'Create rule', {
    kind: 'primary',
    onClick: async () => {
      for (const f of Object.values(F)) f.setError('');
      errHost.replaceChildren();
      const t = type.get();
      const errs = [];
      const bail = (k, m) => { F[k].setError(m); errs.push(m); };
      if (!name.get().trim()) bail('name', 'Name is required.');
      if (path.get().trim() && !path.get().trim().startsWith('/') && !path.get().trim().startsWith('re:')) bail('path_pattern', 'Must start with / or re:.');
      if (path.get().trim().startsWith('re:')) { try { new RegExp(path.get().trim().slice(3)); } catch (e) { bail('path_pattern', 'Invalid regular expression.'); } }
      const p = prob.get();
      if (p === null || p < 0 || p > 100) bail('probability', 'Use a value from 0 to 100.');
      const payload = {
        ...(editing ? { id: rule.id } : {}),
        name: name.get().trim(), enabled: enabled.get(), order: order.get() || 0, service_id: scope.get(),
        methods: methods.get().map((m) => m.toUpperCase()), path_pattern: path.get().trim(), headers: headers.get(), probability: (p || 0) / 100, type: t,
      };
      if (t === 'latency') {
        if (!delay.valid() || !jitter.valid()) bail('delay', 'Use durations like 250ms.');
        if (!delay.ms() && !jitter.ms()) bail('delay', 'Set a delay or a jitter.');
        payload.delay = toGoDuration(delay.ms() || 0); payload.jitter = toGoDuration(jitter.ms() || 0);
      } else if (t === 'error') {
        const sc = status.get();
        if (!sc || sc < 100 || sc > 599) bail('status', 'Use a valid HTTP status (100-599).');
        payload.status = sc || 503; payload.body = body.get(); payload.response_headers = respHeaders.get();
      } else if (t === 'timeout') {
        if (!hang.valid() || !hang.ms()) bail('hang', 'Enter how long to hold the request, e.g. 30s.');
        payload.delay = toGoDuration(hang.ms() || 0);
      } else if (t === 'throttle') {
        if (!(bps.get() > 0)) bail('bytes_per_second', 'Must be greater than 0.');
        payload.bytes_per_second = Math.round(bps.get() || 0);
      }
      if (errs.length) { errHost.replaceChildren(alertBox('error', 'Fix the highlighted fields and try again.'), h('div', { style: 'height:12px' })); scrollToFirstError(m.body); return; }
      try {
        const saved = editing ? await api.updateFault(rule.id, payload) : await api.createFault(payload);
        toastOk(editing ? `Saved ${saved.name}` : `Created ${saved.name}`);
        m.close(saved);
        if (onSaved) onSaved(saved);
      } catch (e) {
        if (!(e instanceof ApiError)) throw e;
        applyProblems({ name: F.name, probability: F.probability, status: F.status, path_pattern: F.path_pattern, bytes_per_second: F.bytes_per_second, delay: F.delay }, e);
        errHost.replaceChildren(errorBox(e), h('div', { style: 'height:12px' }));
        scrollToFirstError(m.body);
      }
    },
  });
  const m = openModal({
    title: editing ? `Edit ${rule.name}` : preset ? 'New rule from preset' : 'New fault rule', size: 'lg',
    subtitle: 'Faults are injected on the service listener, before the request is proxied or virtualized.',
    content: h('div', null, errHost,
      formSection('Rule', null, formGrid(F.name, F.service_id), enabled.el),
      formSection('What to hit', null, F.methods, methodBtns, F.path_pattern, F.headers),
      formSection('Fault', null, F.probability, F.order, type.el, blocks.latency, blocks.error, blocks.abort, blocks.timeout, blocks.throttle)),
    footer: [btn('Cancel', { onClick: () => m.close(null) }), save],
    initialFocus: name.input,
  });

  return m;
}
