// Anonymization profiles: list and editor with a live test panel.
import { h, debounce } from '../dom.js';
import { icon } from '../icons.js';
import { api, ApiError } from '../api.js';
import { navigate } from '../router.js';
import { card, btn, iconBtn, dataTable, emptyState, confirmDialog, toastOk, badge, field, formGrid, formSection, textCtl, textareaCtl, selectCtl, switchCtl, checkCtl, chipsCtl, segmentedCtl, numCtl, errorBox, alertBox, collapsible, pageHeader, applyProblems, scrollToFirstError, loading, toastError } from '../ui.js';
import { exchangeSections, marksFromChanges } from '../bodyview.js';
import { fmtInt, relTime } from '../format.js';

const STRATEGIES = [
  { value: 'fake', label: 'fake', help: 'Format-preserving fake data, consistent per value (an email stays an email, a card stays Luhn-valid).' },
  { value: 'hash', label: 'hash', help: 'A deterministic token such as h_3f9a1c0b7d2e4a51. Same input, same token.' },
  { value: 'mask', label: 'mask', help: 'Keep a prefix and/or suffix and replace the rest with *.' },
  { value: 'redact', label: 'redact', help: 'Replace with a fixed string ([REDACTED] by default).' },
  { value: 'remove', label: 'remove', help: 'Delete the value (header, parameter, cookie or JSON member).' },
];
const TARGETS = [
  { value: 'json', label: 'JSON body', ph: '$.user.email', help: 'JSONPath subset: $.user.email, $.items[*].card, $.items[0].id, $..email (any depth by key), $.*' },
  { value: 'header', label: 'Header', ph: 'X-Customer-Id', help: 'Header name, case-insensitive.' },
  { value: 'query', label: 'Query parameter', ph: 'email', help: 'Query string parameter name.' },
  { value: 'cookie', label: 'Cookie', ph: 'session', help: 'Cookie name, matched in Cookie and Set-Cookie.' },
  { value: 'form', label: 'Form field', ph: 'card_number', help: 'Field name in application/x-www-form-urlencoded bodies.' },
  { value: 'regex', label: 'Regex', ph: '(?i)account[:=]\\s*(\\d+)', help: 'RE2 pattern over all text (bodies, query and header values). With a capture group, only the first group is replaced.' },
  { value: 'path', label: 'URL path', ph: '/users/{id}/orders/{order_id}', help: 'Path template; the {named} segments are anonymized.' },
];
const FAKE_KINDS = ['auto', 'email', 'name', 'phone', 'uuid', 'digits', 'ipv4', 'credit_card', 'string'];
const SCOPES = [{ value: 'both', label: 'Request and response' }, { value: 'request', label: 'Request only' }, { value: 'response', label: 'Response only' }];
const COMMON_HEADERS = ['Authorization', 'Cookie', 'Set-Cookie', 'X-Api-Key', 'X-Auth-Token', 'X-Forwarded-For'];

// ------------------------------------------------------------------ list
export async function list(ctx) {
  ctx.setCrumbs([{ label: 'Anonymization' }]);
  let profiles = await api.profiles();
  const root = h('div', { class: 'stack' });
  root.appendChild(pageHeader({
    subtitle: 'Profiles decide what is replaced when traffic is captured, copied, exported or replayed. Values are mapped consistently, so the same customer keeps the same fake identity everywhere.',
    actions: btn('New profile', { icon: 'plus', kind: 'primary', href: '#/profiles/new' }),
  }));
  const table = dataTable({
    rowKey: (r) => r.id, sortKey: 'name',
    onRowClick: (r) => navigate(`/profiles/${encodeURIComponent(r.id)}`),
    empty: () => emptyState({ icon: 'shield', title: 'No profiles', text: 'Create a profile to control anonymization.', actions: [btn('New profile', { href: '#/profiles/new', kind: 'primary', icon: 'plus' })] }),
    columns: [
      { key: 'name', label: 'Profile', sortValue: (r) => r.name.toLowerCase(), render: (r) => h('div', { class: 'cell-main' }, h('span', { class: 'row', style: 'gap:8px' }, h('a', { class: 'cell-name', href: `#/profiles/${encodeURIComponent(r.id)}` }, r.name), r.id === 'default' ? badge('built-in', 'info') : null), r.description ? h('span', { class: 'cell-sub', style: 'max-width:60ch', title: r.description }, r.description) : null) },
      { key: 'detectors', label: 'Detectors', align: 'right', sortValue: (r) => (r.detectors || []).length, render: (r) => fmtInt((r.detectors || []).length) },
      { key: 'strategy', label: 'Strategy', render: (r) => badge(r.detector_strategy, 'muted') },
      { key: 'rules', label: 'Rules', align: 'right', sortValue: (r) => (r.rules || []).length, render: (r) => { const all = r.rules || []; const on = all.filter((x) => x.enabled).length; return `${on}${all.length !== on ? ` of ${all.length}` : ''}`; } },
      { key: 'headers', label: 'Redacted headers', align: 'right', sortValue: (r) => (r.redact_headers || []).length, render: (r) => fmtInt((r.redact_headers || []).length) },
      { key: 'updated', label: 'Updated', sortValue: (r) => Date.parse(r.updated_at), render: (r) => relTime(r.updated_at) },
      {
        key: 'actions', label: '', sortable: false, cls: 'actions',
        render: (r) => h('div', { class: 'cell-actions' },
          iconBtn('copyplus', 'Duplicate profile', async () => {
            const full = await api.profile(r.id);
            const { id, created_at, updated_at, has_secret, ...rest } = full;
            const copy = await api.createProfile({ ...rest, name: `${r.name} (copy)` });
            toastOk(`Created ${copy.name}`);
            navigate(`/profiles/${encodeURIComponent(copy.id)}`);
          }),
          iconBtn('edit', 'Edit profile', () => navigate(`/profiles/${encodeURIComponent(r.id)}`)),
          iconBtn('trash', r.id === 'default' ? 'The default profile cannot be deleted' : 'Delete profile', async () => {
            if (await deleteProfile(r)) { profiles = await api.profiles(); table.setRows(profiles); }
          }, { disabled: r.id === 'default' })),
        sig: (r) => r.id,
      },
    ],
  });
  root.appendChild(card({ flush: true, body: table.el }));
  table.setRows(profiles);
  return root;
}

async function deleteProfile(p) {
  const ok = await confirmDialog({ title: `Delete ${p.name}?`, danger: true, confirmText: 'Delete profile', message: 'Services and replays that reference this profile will need another one. Anonymized recordings keep their data.' });
  if (!ok) return false;
  try { await api.deleteProfile(p.id); } catch (e) { if (e instanceof ApiError && e.status === 409) { toastError(e); return false; } throw e; }
  toastOk(`Deleted ${p.name}`);
  return true;
}

// ---------------------------------------------------------------- editor
let ruleSeq = 0;

export async function editor(ctx) {
  const isNew = ctx.path === '/profiles/new';
  const [detectors, recordings] = await Promise.all([api.detectors().catch(() => []), api.recordings().catch(() => [])]);
  let prof = isNew ? null : await api.profile(ctx.params.id);
  ctx.setCrumbs([{ label: 'Anonymization', href: '#/profiles' }, { label: isNew ? 'New profile' : prof.name }]);
  const init = prof || { name: '', description: '', detectors: ['email', 'credit_card', 'ssn', 'phone', 'ipv4', 'jwt'], detector_strategy: 'fake', redact_headers: ['Authorization', 'Cookie', 'Set-Cookie', 'X-Api-Key'], rules: [] };

  // --- general
  const name = textCtl({ value: init.name, placeholder: 'e.g. Strict (payments)' });
  const desc = textareaCtl({ value: init.description || '', rows: 2, placeholder: 'What this profile is for', mono: false });
  const F = { name: field('Name', name, { required: true }), description: field('Description', desc) };

  // --- detectors
  const detChecks = detectors.map((d) => ({ d, c: checkCtl({ label: d.name, desc: d.description, example: d.example, checked: (init.detectors || []).includes(d.name) }) }));
  const detGrid = h('div', { class: 'checks' }, detChecks.map((x) => x.c.el));
  const detStrategy = selectCtl(STRATEGIES.map((s) => ({ value: s.value, label: s.label })), { value: init.detector_strategy || 'fake' });
  const detHelp = h('div', { class: 'field-help' });
  const paintDetHelp = () => { const s = STRATEGIES.find((x) => x.value === detStrategy.get()); detHelp.textContent = s ? s.help : ''; };
  detStrategy.onChange(paintDetHelp); paintDetHelp();
  const setAllDetectors = (fn) => { detChecks.forEach((x) => { x.c.set(fn(x.d)); }); markChanged(); };

  // --- opaque bodies
  const OPAQUE_HELP = {
    keep: 'Bodies the engine cannot inspect (binary content, bodies that are still compressed, binary multipart parts) are stored unchanged. Pick this when such bodies carry no personal data.',
    fill: 'Bodies the engine cannot inspect are replaced by deterministic filler of the same length, so nothing readable survives and sizes stay realistic.',
  };
  const opaque = selectCtl([{ value: 'keep', label: 'keep unchanged' }, { value: 'fill', label: 'fill with same-length filler' }], { value: init.opaque_bodies || 'keep' });
  const opaqueHelp = h('div', { class: 'field-help' });
  const paintOpaqueHelp = () => { opaqueHelp.textContent = OPAQUE_HELP[opaque.get()] || ''; };
  opaque.onChange(paintOpaqueHelp); paintOpaqueHelp();

  // --- headers
  const headers = chipsCtl({ value: init.redact_headers || [], placeholder: 'Add a header name and press Enter' });
  const headerSuggest = h('div', { class: 'row', style: 'gap:6px' }, h('span', { class: 'muted', style: 'font-size:12px' }, 'Quick add:'),
    COMMON_HEADERS.map((n) => h('button', { type: 'button', class: 'preset', onclick: () => { const cur = headers.get(); if (!cur.some((x) => x.toLowerCase() === n.toLowerCase())) { headers.set([...cur, n]); markChanged(); } } }, n)));

  // --- rules
  const rules = [];
  const rulesHost = h('div', { class: 'stack-sm' });
  const noRules = h('div', { class: 'muted', style: 'padding:6px 2px' }, 'No rules. Detectors above already catch common PII; add rules for identifiers they cannot know about, such as customer IDs.');
  function makeRule(r = {}) {
    const id = r.id || `r${++ruleSeq}-${Math.random().toString(36).slice(2, 6)}`;
    const enabled = switchCtl({ checked: r.enabled ?? true });
    const rname = textCtl({ value: r.name || '', placeholder: 'Rule name (optional)' });
    const target = selectCtl(TARGETS.map((t) => ({ value: t.value, label: t.label })), { value: r.target || 'json' });
    const selector = textCtl({ value: r.selector || '', placeholder: '$.user.email', mono: true });
    const scope = selectCtl(SCOPES, { value: r.scope || 'both' });
    const strategy = selectCtl(STRATEGIES.map((s) => ({ value: s.value, label: s.label })), { value: r.strategy || 'hash' });
    const fake = selectCtl(FAKE_KINDS, { value: r.fake || 'auto' });
    const keepPrefix = numCtl({ value: r.keep_prefix || 0, min: 0, step: 1, unit: 'chars' });
    const keepSuffix = numCtl({ value: r.keep_suffix || 0, min: 0, step: 1, unit: 'chars' });
    const replacement = textCtl({ value: r.replacement || '', placeholder: '[REDACTED]' });
    const fSel = field('Selector', selector, { help: '' });
    const fFake = field('Fake kind', fake, { help: 'auto detects the format from the value.' });
    const fPre = field('Keep first', keepPrefix);
    const fSuf = field('Keep last', keepSuffix);
    const fRep = field('Replacement', replacement);
    const num = h('span', { class: 'rule-num' });
    const up = iconBtn('arrow-up', 'Move rule up', () => move(rule, -1));
    const down = iconBtn('arrow-down', 'Move rule down', () => move(rule, 1));
    const del = iconBtn('trash', 'Remove rule', () => { rules.splice(rules.indexOf(rule), 1); paintRules(); markChanged(); });
    const el = h('div', { class: 'rule-card' },
      h('div', { class: 'rule-head' }, num, enabled.el, h('div', { style: 'flex:1;min-width:0' }, rname.el), up, down, del),
      h('div', { class: 'rule-grid' }, field('Applies to', target), fSel, field('Scope', scope), field('Strategy', strategy)),
      h('div', { class: 'rule-grid' }, fFake, fPre, fSuf, fRep));
    const rule = { id, el, num, up, down, enabled, rname, target, selector, scope, strategy, fake, keepPrefix, keepSuffix, replacement, fSel };
    const paint = () => {
      const t = TARGETS.find((x) => x.value === target.get());
      selector.input.placeholder = t.ph;
      fSel.querySelector('.field-help').textContent = t.help;
      const st = strategy.get();
      fFake.hidden = st !== 'fake'; fPre.hidden = st !== 'mask'; fSuf.hidden = st !== 'mask'; fRep.hidden = st !== 'redact';
      el.classList.toggle('is-off', !enabled.get());
    };
    [target, strategy, enabled].forEach((c) => c.onChange(paint));
    [enabled, rname, target, selector, scope, strategy, fake, keepPrefix, keepSuffix, replacement].forEach((c) => c.onChange(markChanged));
    paint();
    return rule;
  }
  function move(rule, d) {
    const i = rules.indexOf(rule);
    const j = i + d;
    if (j < 0 || j >= rules.length) return;
    rules.splice(i, 1); rules.splice(j, 0, rule);
    paintRules(); markChanged();
  }
  function paintRules() {
    rulesHost.replaceChildren(...rules.map((r) => r.el));
    rules.forEach((r, i) => { r.num.textContent = String(i + 1); r.up.disabled = i === 0; r.down.disabled = i === rules.length - 1; });
    if (!rules.length) rulesHost.replaceChildren(noRules);
  }
  (init.rules || []).forEach((r) => rules.push(makeRule(r)));
  paintRules();

  // --- secret
  let regenerate = false;
  const secretState = h('div');
  function paintSecret() {
    secretState.replaceChildren(
      isNew ? h('p', { class: 'explain' }, 'A random secret is generated when the profile is created. It keys the hash and fake strategies so the same input always maps to the same output.')
        : h('div', { class: 'stack-sm' },
          h('div', { class: 'row' }, badge(prof.has_secret ? 'secret set' : 'no secret', prof.has_secret ? 'ok' : 'warn', { dot: true }), regenerate ? badge('will be regenerated when you save', 'warn') : null),
          h('p', { class: 'explain' }, 'The secret is never shown. Regenerating it changes every hash and fake token this profile produces, so recordings anonymized earlier will no longer line up with new captures of the same customers.'),
          btn(regenerate ? 'Keep current secret' : 'Regenerate secret', {
            icon: 'refresh', kind: regenerate ? 'secondary' : 'danger', size: 'sm',
            onClick: async () => {
              if (regenerate) { regenerate = false; paintSecret(); markChanged(); return; }
              const ok = await confirmDialog({ title: 'Regenerate the secret?', danger: true, confirmText: 'Regenerate on save', message: h('div', null, 'All hash and fake tokens produced by this profile will change. Existing anonymized recordings keep their old tokens, so the same customer will get a ', h('b', null, 'different'), ' identity in new captures. The new secret is applied when you save.') });
              if (ok) { regenerate = true; paintSecret(); markChanged(); }
            },
          })));
  }
  paintSecret();

  // --- build the profile object from the form
  function build({ forTest = false } = {}) {
    const p = {
      ...(prof ? { id: prof.id } : {}),
      name: name.get().trim() || (forTest ? 'preview' : ''),
      description: desc.get().trim(),
      detectors: detChecks.filter((x) => x.c.get()).map((x) => x.d.name),
      detector_strategy: detStrategy.get(),
      redact_headers: headers.get(),
      opaque_bodies: opaque.get(),
      rules: rules.filter((r) => !forTest || r.selector.get().trim()).map((r) => {
        const st = r.strategy.get();
        return {
          id: r.id, name: r.rname.get().trim(), enabled: r.enabled.get(), target: r.target.get(), selector: r.selector.get().trim(), scope: r.scope.get(), strategy: st,
          ...(st === 'fake' ? { fake: r.fake.get() } : {}),
          ...(st === 'mask' ? { keep_prefix: r.keepPrefix.get() || 0, keep_suffix: r.keepSuffix.get() || 0 } : {}),
          ...(st === 'redact' && r.replacement.get() ? { replacement: r.replacement.get() } : {}),
        };
      }),
    };
    if (!forTest && regenerate) p.regenerate_secret = true;
    return p;
  }

  function validate() {
    for (const f of Object.values(F)) f.setError('');
    let ok = true;
    if (!name.get().trim()) { F.name.setError('Name is required.'); ok = false; }
    rules.forEach((r, i) => {
      r.fSel.setError('');
      const sel = r.selector.get().trim();
      const t = r.target.get();
      let msg = '';
      if (!sel) msg = 'Selector is required.';
      else if (t === 'json' && !sel.startsWith('$')) msg = 'A JSON selector must start with $.';
      else if (t === 'path' && !sel.startsWith('/')) msg = 'A path selector must start with /.';
      else if (t === 'regex') { try { new RegExp(sel); } catch (e) { msg = 'Invalid regular expression.'; } }
      if (msg) { r.fSel.setError(`Rule ${i + 1}: ${msg}`); ok = false; }
    });
    return ok;
  }

  // --- test scheduling (declared early: markChanged and the test panel use it)
  let ranOnce = false;
  let testFn = () => {};
  const scheduleTest = debounce(() => { if (ranOnce && ctx.isActive()) testFn(); }, 600);

  // --- save
  const errHost = h('div');
  const dirty = h('span', { class: 'muted', style: 'font-size:12.5px' });
  let changed = false;
  function markChanged() { changed = true; dirty.textContent = 'Unsaved changes'; scheduleTest(); }
  const saveBtn = btn(isNew ? 'Create profile' : 'Save profile', {
    kind: 'primary',
    onClick: async () => {
      errHost.replaceChildren();
      if (!validate()) { errHost.replaceChildren(alertBox('error', 'Fix the highlighted fields and try again.'), h('div', { style: 'height:12px' })); scrollToFirstError(root); return; }
      const payload = build();
      try {
        const saved = isNew ? await api.createProfile(payload) : await api.updateProfile(prof.id, payload);
        toastOk(isNew ? `Created ${saved.name}` : `Saved ${saved.name}`);
        changed = false;
        if (isNew) navigate(`/profiles/${encodeURIComponent(saved.id)}`);
        else { prof = saved; regenerate = false; paintSecret(); dirty.textContent = ''; ctx.setCrumbs([{ label: 'Anonymization', href: '#/profiles' }, { label: saved.name }]); }
      } catch (e) {
        if (!(e instanceof ApiError)) throw e;
        applyProblems({ name: F.name }, e);
        errHost.replaceChildren(errorBox(e), h('div', { style: 'height:12px' }));
        scrollToFirstError(root);
      }
    },
  });

  // ------------------------------------------------------------- test panel
  const testHost = h('div', { class: 'stack-sm' });
  const out = h('div', { class: 'stack-sm' });
  const srcSeg = segmentedCtl([{ value: 'exchange', label: 'Recorded exchange' }, { value: 'sample', label: 'Paste a sample' }], { value: 'exchange', label: 'Sample source', size: 'sm' });
  const recSel = selectCtl(recordings.length ? recordings.map((r) => ({ value: r.id, label: r.name })) : [{ value: '', label: 'No recordings' }], { value: (recordings.find((r) => !r.anonymized) || recordings[0] || {}).id || '' });
  const exSel = selectCtl([{ value: '', label: 'Loading…' }], { value: '' });
  const exBlock = h('div', { class: 'stack-sm' }, field('Recording', recSel), field('Exchange', exSel));
  const sMethodPath = textCtl({ value: 'POST /v1/customers?email=jane.doe@acme.io', mono: true });
  const sReqHeaders = textareaCtl({ value: 'Content-Type: application/json\nAuthorization: Bearer sk_live_51Hx9kd2eZvKYlo2C\nX-Customer-Id: cus_8f3k2m9d', rows: 3 });
  const sReqBody = textareaCtl({ value: '{"customer_id":"cus_8f3k2m9d","name":"Jane Doe","card":{"number":"4242424242424242","last4":"4242"},"contact":{"email":"jane.doe@acme.io","phone":"+1 (503) 555-0142"}}', rows: 4 });
  const sStatus = numCtl({ value: 200, min: 100, max: 599, step: 1 });
  const sResHeaders = textareaCtl({ value: 'Content-Type: application/json\nSet-Cookie: session=9f8c2d71aa04; Path=/; HttpOnly', rows: 2 });
  const sResBody = textareaCtl({ value: '{"id":"cus_8f3k2m9d","email":"jane.doe@acme.io","ssn":"123-45-6789","ip":"203.0.113.45","note":"Call Jane on 503-555-0142"}', rows: 4 });
  const sampleBlock = h('div', { class: 'stack-sm' },
    field('Request line', sMethodPath, { help: 'METHOD /path?query' }), field('Request headers', sReqHeaders, { help: 'One "Name: value" per line' }), field('Request body', sReqBody),
    formGrid(field('Response status', sStatus)), field('Response headers', sResHeaders), field('Response body', sResBody));
  srcSeg.onChange(paintSource);
  function paintSource() { const ex = srcSeg.get() === 'exchange'; exBlock.hidden = !ex; sampleBlock.hidden = ex; scheduleTest(); }
  paintSource();

  async function loadExchanges() {
    if (!recSel.get()) { exSel.setOptions([{ value: '', label: 'No exchanges' }], false); return; }
    exSel.setOptions([{ value: '', label: 'Loading…' }], false);
    try {
      const res = await api.exchanges(recSel.get(), { limit: 80 });
      exSel.setOptions(res.items.map((e) => ({ value: String(e.id), label: `#${e.seq}  ${e.method} ${e.path}${e.raw_query ? '?' + e.raw_query : ''}  ${e.status}` })), false);
      if (res.items.length) exSel.set(String((res.items.find((e) => e.raw_query || e.req_bytes > 0) || res.items[0]).id));
      scheduleTest();
    } catch (e) { exSel.setOptions([{ value: '', label: 'Could not load exchanges' }], false); }
  }
  recSel.onChange(loadExchanges);
  exSel.onChange(() => scheduleTest());
  [sMethodPath, sReqHeaders, sReqBody, sStatus, sResHeaders, sResBody].forEach((c) => c.onChange(() => scheduleTest()));

  const parseHeaders = (txt) => {
    const o = {};
    for (const line of txt.split('\n')) {
      const i = line.indexOf(':');
      if (i > 0) { const k = line.slice(0, i).trim(); (o[k] ||= []).push(line.slice(i + 1).trim()); }
    }
    return o;
  };
  const ctOf = (hd) => { const k = Object.keys(hd).find((x) => x.toLowerCase() === 'content-type'); return k ? hd[k][0] : ''; };
  function sampleExchange() {
    const line = sMethodPath.get().trim();
    const m = /^([A-Za-z]+)\s+(\S+)/.exec(line);
    const method = m ? m[1].toUpperCase() : 'GET';
    const uri = m ? m[2] : line;
    const qi = uri.indexOf('?');
    const reqH = parseHeaders(sReqHeaders.get()), resH = parseHeaders(sResHeaders.get());
    const rb = sReqBody.get(), sb = sResBody.get();
    const body = (t, hd) => (t ? { encoding: 'text', data: t, size: new TextEncoder().encode(t).length, content_type: ctOf(hd) } : { encoding: 'none', size: 0 });
    return {
      id: 0, recording_id: '', seq: 0, started_at: new Date().toISOString(), duration: '0s', ttfb: '0s', endpoint: `${method} ${qi >= 0 ? uri.slice(0, qi) : uri}`,
      request: { method, host: 'sample.local', path: qi >= 0 ? uri.slice(0, qi) : uri, ...(qi >= 0 ? { raw_query: uri.slice(qi + 1) } : {}), header: reqH, body: body(rb, reqH) },
      response: { status: sStatus.get() || 200, header: resH, body: body(sb, resH) }, anonymized: false,
    };
  }

  let testSeq = 0;
  async function runTest(manual) {
    const body = { profile: build({ forTest: true }) };
    if (srcSeg.get() === 'exchange') {
      if (!exSel.get()) { out.replaceChildren(h('div', { class: 'muted' }, 'Pick a recording with exchanges to test against.')); return; }
      body.exchange_id = Number(exSel.get());
    } else body.exchange = sampleExchange();
    const my = ++testSeq;
    if (manual) out.replaceChildren(loading('Running…'));
    try {
      const res = await api.preview(body);
      if (my !== testSeq || !ctx.isActive()) return;
      paintResult(res);
    } catch (e) {
      if (my !== testSeq || !ctx.isActive()) return;
      out.replaceChildren(e instanceof ApiError ? errorBox(e) : alertBox('error', String(e.message || e)));
    }
  }
  testFn = () => runTest(false);
  ctx.onCleanup(() => scheduleTest.cancel());
  function paintResult(res) {
    const ch = res.changes || [];
    const byCount = {};
    for (const c of ch) byCount[c.source] = (byCount[c.source] || 0) + 1;
    out.replaceChildren(
      h('div', { class: 'row-between' },
        h('strong', null, `${fmtInt(ch.length)} ${ch.length === 1 ? 'value' : 'values'} changed`),
        h('span', { class: 'live-note' }, 'updates as you edit')),
      ch.length ? h('div', { class: 'row', style: 'gap:6px' }, Object.entries(byCount).map(([s, n]) => badge(`${s} × ${n}`, s.startsWith('detector') ? 'info' : 'muted'))) : h('p', { class: 'muted' }, 'Nothing matched. Try enabling more detectors or adding a rule.'),
      h('div', { class: 'stack-sm' }, ch.map((c) => h('div', { class: 'chg' },
        h('div', { class: 'chg-loc mono' }, c.location),
        h('div', { class: 'chg-vals' }, h('span', { class: 'chg-before mono' }, c.before), h('span', { class: 'chg-arrow' }, icon('arrow-right', 12)), h('span', { class: 'chg-after mono' }, c.after)),
        h('div', { class: 'chg-src' }, c.source)))),
      collapsible('Anonymized exchange', exchangeSections(res.exchange, { marks: marksFromChanges(ch, 'after') }), { open: false }));
  }
  const runBtn = btn('Test profile', { icon: 'play', kind: 'primary', onClick: async () => { ranOnce = true; await runTest(true); } });
  testHost.append(srcSeg.el, exBlock, sampleBlock, h('div', { class: 'row' }, runBtn, h('span', { class: 'muted', style: 'font-size:12px' }, 'Uses the unsaved profile; nothing is stored.')), out);
  loadExchanges();

  // ------------------------------------------------------------- layout
  const root = h('div', { class: 'stack' });
  root.appendChild(pageHeader({
    title: isNew ? 'New profile' : prof.name,
    subtitle: isNew ? 'Choose what to detect, which headers to redact and any custom rules.' : (prof.id === 'default' ? 'The built-in profile used when a service does not name one.' : `Last updated ${relTime(prof.updated_at)}`),
    actions: [dirty, btn('Back', { kind: 'ghost', href: '#/profiles' }), saveBtn],
  }));
  const detBtns = h('div', { class: 'row', style: 'gap:6px' },
    btn('All', { size: 'sm', kind: 'ghost', onClick: () => setAllDetectors(() => true) }),
    btn('None', { size: 'sm', kind: 'ghost', onClick: () => setAllDetectors(() => false) }),
    btn('Recommended', { size: 'sm', kind: 'ghost', onClick: () => setAllDetectors((d) => d.name !== 'uuid') }));
  detChecks.forEach((x) => x.c.onChange(markChanged));
  [name, desc, detStrategy, headers, opaque].forEach((c) => c.onChange(markChanged));
  const form = h('div', null,
    errHost,
    formSection('General', null, F.name, F.description),
    formSection('Detectors', 'Scan every textual value (bodies, query values, header and cookie values that are not already redacted) and transform what they find.',
      detBtns, detGrid, field('Detector strategy', detStrategy), detHelp),
    formSection('Redacted headers', 'These header values are always replaced with deterministic tokens. Authentication schemes such as "Bearer " are kept. Cookie and Set-Cookie are handled per cookie.', field('Headers', headers), headerSuggest),
    formSection('Opaque bodies', 'What happens to bodies the engine cannot look inside.', field('Opaque bodies', opaque), opaqueHelp),
    formSection('Rules', 'Run before the detectors, in order. Use them for identifiers detectors cannot recognise.',
      rulesHost,
      btn('Add rule', { icon: 'plus', size: 'sm', onClick: () => { const r = makeRule({}); rules.push(r); paintRules(); r.selector.input.focus(); markChanged(); } })),
    formSection('Secret', null, secretState));
  root.appendChild(h('div', { class: 'cols-main-wide' },
    card({ body: form }),
    h('div', { class: 'sticky' }, card({ title: 'Test', subtitle: 'See what this profile does to a real or pasted exchange', body: testHost }))));

  return root;
}
