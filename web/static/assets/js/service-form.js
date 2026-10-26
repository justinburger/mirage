// Create / edit service modal and the mode-switch dialog.
import { h } from './dom.js';
import { api, ApiError } from './api.js';
import { refreshServices } from './state.js';
import { openModal, btn, field, formSection, formGrid, textCtl, numCtl, durCtl, selectCtl, switchCtl, segmentedCtl, chipsCtl, linesCtl, sliderCtl, collapsible, errorBox, applyProblems, scrollToFirstError, toastOk, alertBox, modeLabel } from './ui.js';
import { parseDuration, toGoDuration } from './format.js';

export const MODES = [
  { value: 'passthrough', label: 'Passthrough', cls: 'm-passthrough', title: 'Proxy to the upstream; metrics and faults still apply, nothing is recorded' },
  { value: 'record', label: 'Record', cls: 'm-record', title: 'Proxy to the upstream and capture every exchange into a recording' },
  { value: 'virtualize', label: 'Virtualize', cls: 'm-virtualize', title: 'Answer from the recording; never contact the upstream' },
  { value: 'hybrid', label: 'Hybrid', cls: 'm-hybrid', title: 'Answer from the recording; proxy and record the misses' },
];

export const MODE_HELP = {
  passthrough: 'Requests are proxied straight to the upstream. Metrics and fault injection still apply, but nothing is recorded.',
  record: 'Requests are proxied to the upstream and every request/response pair is captured into the service recording.',
  virtualize: 'Requests are answered from the recording and the upstream is never contacted. Unmatched requests get the miss status.',
  hybrid: 'Requests are answered from the recording when possible. Misses go to the upstream and are recorded and added to the live index.',
};

const NAME_RE = /^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$/;

function pad(n) { return String(n).padStart(2, '0'); }
export function stamp(d = new Date()) {
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

/** Normalised duration for inputs: "1m0s" -> "1m", "" stays "". */
export const durText = (v) => {
  if (v == null || v === '') return '';
  const ms = parseDuration(v);
  return ms == null ? String(v) : toGoDuration(ms);
};

// ------------------------------------------------------------------------
export async function openServiceForm({ service = null, onSaved } = {}) {
  const editing = !!service;
  const [recordings, profiles] = await Promise.all([api.recordings().catch(() => []), api.profiles().catch(() => [])]);
  const s = service || {};
  const match = s.match || {};
  const latency = s.latency || {};

  // --- general
  const name = textCtl({ value: s.name || '', placeholder: 'payments' });
  const listen = textCtl({ value: s.listen_addr || '', placeholder: ':9001', mono: true });
  const upstream = textCtl({ value: s.upstream || '', placeholder: 'https://api.example.com', mono: true });
  const mode = segmentedCtl(MODES, { value: s.mode || 'passthrough', label: 'Mode' });
  const enabled = switchCtl({ label: 'Enabled', help: 'A disabled service keeps its configuration but does not listen.', checked: s.enabled ?? true });

  // --- recording
  const hasRecs = recordings.length > 0;
  const recSource = segmentedCtl([
    { value: 'existing', label: 'Use existing' },
    { value: 'new', label: 'Create new' },
  ], { value: s.recording_id || hasRecs ? 'existing' : 'new', label: 'Recording source' });
  const recSelect = selectCtl(recordings.map((r) => ({ value: r.id, label: `${r.name}  (${r.exchange_count} exchanges)` })), { value: s.recording_id || (recordings[0] && recordings[0].id) || '' });
  const recName = textCtl({ value: '', placeholder: `${s.name || 'service'} ${stamp()}` });
  if (!hasRecs) recSelect.setOptions([{ value: '', label: 'No recordings yet' }], false);

  // --- capture
  const anon = switchCtl({ label: 'Anonymize before writing to disk', help: 'Raw values never touch the database.', checked: !!s.anonymize_capture });
  const profile = selectCtl(profiles.length ? profiles.map((p) => ({ value: p.id, label: p.name })) : [{ value: 'default', label: 'Default' }], { value: s.profile_id || 'default' });
  const patterns = linesCtl({
    value: s.endpoint_patterns || [], rows: 3, placeholder: '/v1/customers/{id}\n/orders/{order_id}/items',
    validate: (l) => (l.startsWith('/') ? null : 'must start with /'),
  });
  const timeout = durCtl({ value: durText(s.timeout) || '60s', placeholder: '60s' });
  const maxBody = numCtl({ value: s.max_body_bytes ?? 1048576, step: 1, unit: 'bytes', placeholder: '1048576' });
  const sample = numCtl({ value: s.capture_sample > 0 ? Math.round(s.capture_sample * 10000) / 100 : 100, step: 'any', min: 0, max: 100, unit: '%' });
  const capLimit = numCtl({ value: s.max_capture_bytes || 0, step: 1, min: 0, unit: 'bytes', placeholder: '0 = no limit' });

  // --- network
  const tlsMode = segmentedCtl([
    { value: 'off', label: 'Off' }, { value: 'self', label: 'Self-signed' }, { value: 'files', label: 'Certificate files' },
  ], { value: s.tls ? (s.tls.self_signed ? 'self' : 'files') : 'off', label: 'TLS' });
  const certFile = textCtl({ value: (s.tls && s.tls.cert_file) || '', placeholder: '/etc/mirage/tls.crt', mono: true });
  const keyFile = textCtl({ value: (s.tls && s.tls.key_file) || '', placeholder: '/etc/mirage/tls.key', mono: true });
  const preserveHost = switchCtl({ label: 'Preserve Host header', help: 'Send the client Host header to the upstream instead of the upstream host.', checked: !!s.preserve_host });
  const skipVerify = switchCtl({ label: 'Skip TLS verification', help: 'Accept any upstream certificate. For lab environments only.', checked: !!s.insecure_skip_verify });

  // --- virtualization
  const mQuery = selectCtl([{ value: 'exact', label: 'exact' }, { value: 'ignore', label: 'ignore' }, { value: 'subset', label: 'subset' }], { value: match.query || 'exact' });
  const mIgnore = chipsCtl({ value: match.ignore_params || [], placeholder: 'e.g. _ts, utm_source' });
  const mBody = selectCtl([{ value: 'json', label: 'json (semantic)' }, { value: 'exact', label: 'exact (bytes)' }, { value: 'ignore', label: 'ignore' }], { value: match.body || 'json' });
  const mHeaders = chipsCtl({ value: match.headers || [], placeholder: 'Header names that must match' });
  const mTemplates = switchCtl({ label: 'Match path templates', help: 'GET /users/7 may answer with the recording of GET /users/42.', checked: !!match.path_templates });
  const mSelection = selectCtl([{ value: 'sequential', label: 'sequential' }, { value: 'random', label: 'random' }, { value: 'first', label: 'first' }], { value: match.selection || 'sequential' });
  const mMiss = numCtl({ value: match.miss_status || 404, step: 1, min: 100, max: 599 });
  const latMode = segmentedCtl([{ value: 'none', label: 'None' }, { value: 'recorded', label: 'Recorded' }, { value: 'fixed', label: 'Fixed' }], { value: latency.mode || 'recorded', label: 'Latency mode' });
  const latMult = numCtl({ value: latency.multiplier ?? 1, step: 0.1, min: 0, unit: '×' });
  const latFixed = durCtl({ value: durText(latency.fixed), placeholder: 'e.g. 40ms' });
  const latJitter = durCtl({ value: durText(latency.jitter), placeholder: 'e.g. 5ms' });
  const errAmp = sliderCtl({ min: 0.1, max: 100, step: 0.1, value: s.error_amplification || 1, log: true, presets: [1, 2, 5, 10], format: (v) => `${v}×`, numMin: 0, numMax: 1000 });
  const errFloor = numCtl({ value: (s.error_floor || 0) * 100, step: 0.1, min: 0, max: 100, unit: '%' });

  // --- fields (wrappers)
  const F = {
    name: field('Name', name, { required: true, help: 'Letters, digits, ".", "_" and "-".' }),
    listen_addr: field('Listen address', listen, { required: true, help: 'Where clients connect, e.g. :9001 or 127.0.0.1:9001.' }),
    upstream: field('Upstream', upstream, { help: 'The real API. Optional when only virtualizing.' }),
    mode: field('Mode', mode),
    recording_id: field('Recording', recSelect, { help: 'Captured traffic is stored here (record, hybrid) and served from here (virtualize, hybrid).' }),
    new_recording_name: field('New recording name', recName),
    profile_id: field('Anonymization profile', profile),
    endpoint_patterns: field('Endpoint patterns', patterns, { help: 'One path template per line. They group requests into endpoints for metrics and template matching.' }),
    timeout: field('Upstream timeout', timeout, { help: 'How long to wait for the upstream\'s response headers, e.g. 30s; the body then streams until it ends. 0 uses the 60s default.' }),
    max_body_bytes: field('Max captured body', maxBody, { help: '0 = 1 MiB, -1 = do not capture bodies. Traffic is never truncated, only the captured copy.' }),
    capture_sample: field('Capture sampling', sample, { help: 'Share of requests that are captured (100 = all). Decided before anything is buffered, so skipped requests cost nothing extra.' }),
    max_capture_bytes: field('Capture limit', capLimit, { help: 'Stop capturing once the recording holds this many body bytes (0 = no limit). Traffic keeps flowing; the service status then reads "capture limit reached".' }),
    tls: field('TLS', tlsMode, { help: 'Serve this listener over HTTPS.' }),
    cert_file: field('Certificate file', certFile),
    key_file: field('Key file', keyFile),
    'match.query': field('Query matching', mQuery, { help: 'exact: all params must match, order ignored. ignore: drop the query. subset: every recorded param must be present.' }),
    ignore_params: field('Ignored query params', mIgnore, { help: 'Never take part in matching (timestamps, tracking ids).' }),
    'match.body': field('Body matching', mBody, { help: 'json compares semantically (key order and whitespace ignored).' }),
    'match.headers': field('Headers that must match', mHeaders),
    'match.selection': field('Selection', mSelection, { help: 'How to pick when several recorded exchanges match.' }),
    'match.miss_status': field('Miss status', mMiss, { help: 'Returned for unmatched requests in virtualize mode.' }),
    'latency.mode': field('Latency', latMode, { help: 'Recorded replays the original upstream timing (times the multiplier).' }),
    'latency.multiplier': field('Multiplier', latMult, { help: '2 = twice as slow as recorded.' }),
    'latency.fixed': field('Fixed delay', latFixed),
    'latency.jitter': field('Jitter', latJitter, { help: 'Uniform random offset of up to ± this much.' }),
    error_amplification: field('Error amplification', errAmp, { help: 'Multiplies the recorded per-endpoint error rate. 10× means ten times as many errors, using real recorded error responses where they exist.' }),
    error_floor: field('Error floor', errFloor, { help: 'Minimum error rate after amplification, so endpoints that never failed can still fail.' }),
  };

  // --- dynamic visibility
  const recBlock = h('div', { class: 'stack-sm' },
    field('Recording', recSource, { help: 'Captured traffic is stored here (record, hybrid) and served from here (virtualize, hybrid).' }),
    F.recording_id, F.new_recording_name);
  const recSection = formSection('Recording', null, recBlock);
  const profileRow = h('div', null, F.profile_id);
  const certRow = formGrid(F.cert_file, F.key_file);
  const virt = collapsible('Virtualization', h('div', { class: 'stack' },
    formGrid(F['match.query'], F['match.body']),
    formGrid(F.ignore_params, F['match.headers']),
    formGrid(F['match.selection'], F['match.miss_status']),
    h('div', null, mTemplates.el),
    formGrid(F['latency.mode'], F['latency.multiplier'], F['latency.fixed'], F['latency.jitter']),
    formGrid(F.error_amplification, F.error_floor)), { open: (s.mode === 'virtualize' || s.mode === 'hybrid'), hint: 'used in virtualize and hybrid modes' });

  function paint() {
    const m = mode.get();
    recSection.hidden = m === 'passthrough';
    const isNew = recSource.get() === 'new';
    F.recording_id.hidden = isNew;
    F.new_recording_name.hidden = !isNew;
    if (m === 'virtualize') { recSource.set('existing'); F.recording_id.hidden = false; F.new_recording_name.hidden = true; }
    profileRow.hidden = !anon.get();
    certRow.hidden = tlsMode.get() !== 'files';
    F['latency.multiplier'].hidden = latMode.get() !== 'recorded';
    F['latency.fixed'].hidden = latMode.get() !== 'fixed';
    F.upstream.querySelector('.field-help').textContent = m === 'virtualize' ? 'Optional: the virtualized API never contacts it.' : 'The real API. Required for passthrough, record and hybrid.';
  }
  [mode, recSource, anon, tlsMode, latMode].forEach((c) => c.onChange(paint));
  paint();
  if (!editing && !name.get()) name.input.addEventListener('input', () => { recName.input.placeholder = `${name.get() || 'service'} ${stamp()}`; });

  const errHost = h('div');
  const body = h('div', null,
    errHost,
    formSection('General', null,
      formGrid(F.name, F.listen_addr),
      F.upstream,
      F.mode,
      h('div', { class: 'muted', id: 'mode-help', style: 'font-size:12.5px;margin-top:-6px' }),
      enabled.el),
    recSection,
    formSection('Capture', 'What is stored when traffic is recorded.', anon.el, profileRow, F.endpoint_patterns, formGrid(F.timeout, F.max_body_bytes), formGrid(F.capture_sample, F.max_capture_bytes)),
    formSection('Network', null, F.tls, certRow, h('div', { class: 'fgrid' }, preserveHost.el, skipVerify.el)),
    virt);
  const modeHelp = body.querySelector('#mode-help');
  const paintHelp = () => { modeHelp.textContent = MODE_HELP[mode.get()]; };
  mode.onChange(paintHelp); paintHelp();

  // --- submit
  let createdRecId = '';
  const saveBtn = btn(editing ? 'Save changes' : 'Create service', {
    kind: 'primary',
    onClick: async () => {
      const result = collect();
      if (!result) return;
      errHost.replaceChildren();
      try {
        if (result.newRec && !createdRecId) {
          const rec = await api.createRecording({ name: result.newRec });
          createdRecId = rec.id;
        }
        const payload = result.payload;
        if (result.newRec) payload.recording_id = createdRecId;
        const saved = editing ? await api.updateService(s.id, payload) : await api.createService(payload);
        await refreshServices().catch(() => {});
        toastOk(editing ? `Saved ${saved.name}` : `Created ${saved.name}`);
        m.close(saved);
        if (onSaved) onSaved(saved);
      } catch (e) {
        if (!(e instanceof ApiError)) throw e;
        applyProblems(F, e);
        errHost.replaceChildren(errorBox(e), h('div', { style: 'height:14px' }));
        scrollToFirstError(m.body);
      }
    },
  });

  function collect() {
    for (const f of Object.values(F)) f.setError('');
    const errs = [];
    const bail = (key, msg) => { F[key].setError(msg); errs.push(msg); };
    const md = mode.get();
    const nm = name.get().trim();
    if (!NAME_RE.test(nm)) bail('name', 'Use 1-64 letters, digits, ".", "_" or "-" (must start with a letter or digit).');
    const la = listen.get().trim();
    if (!/^(\[[^\]]*\]|[^:[\]]*):\d+$/.test(la)) bail('listen_addr', 'Use host:port or :port, e.g. :9001.');
    const up = upstream.get().trim();
    if (md !== 'virtualize' || up) {
      let ok = false;
      try { const u = new URL(up); ok = (u.protocol === 'http:' || u.protocol === 'https:') && !!u.host; } catch (_) { ok = false; }
      if (!ok) bail('upstream', 'Enter an absolute http:// or https:// URL.');
    }
    let recordingId = '';
    let newRec = '';
    if (md !== 'passthrough') {
      if (recSource.get() === 'new' && md !== 'virtualize') {
        newRec = recName.get().trim() || recName.input.placeholder;
      } else {
        recordingId = recSelect.get();
        if (!recordingId) bail('recording_id', md === 'virtualize' ? 'Virtualize needs an existing recording. Record or import one first.' : 'Pick a recording or create a new one.');
      }
    } else recordingId = s.recording_id || '';
    if (!timeout.valid()) bail('timeout', 'Use a duration like 30s or 1m30s.');
    const tmo = timeout.ms();
    const mb = maxBody.get();
    if (mb !== null && mb < -1) bail('max_body_bytes', 'Must be -1, 0 or positive.');
    const smp = sample.get();
    if (smp === null || !(smp > 0) || smp > 100) bail('capture_sample', 'Use a percentage above 0 and up to 100.');
    const cap = capLimit.get();
    if (cap !== null && cap < 0) bail('max_capture_bytes', 'Must be 0 (no limit) or positive.');
    const tm = tlsMode.get();
    if (tm === 'files' && (!certFile.get().trim() || !keyFile.get().trim())) bail('cert_file', 'Both certificate and key files are required.');
    const pats = patterns.get();
    if (patterns.validate()) bail('endpoint_patterns', 'Every pattern must start with /.');
    const miss = mMiss.get();
    if (miss !== null && (miss < 100 || miss > 599)) bail('match.miss_status', 'Use a valid HTTP status (100-599).');
    if (!latFixed.valid()) bail('latency.fixed', 'Use a duration like 40ms.');
    if (!latJitter.valid()) bail('latency.jitter', 'Use a duration like 5ms.');
    const amp = errAmp.get();
    if (amp === null || amp < 0 || amp > 1000) bail('error_amplification', 'Must be between 0 and 1000.');
    const floor = errFloor.get();
    if (floor !== null && (floor < 0 || floor > 100)) bail('error_floor', 'Must be between 0% and 100%.');
    if (errs.length) {
      errHost.replaceChildren(alertBox('error', 'Fix the highlighted fields and try again.'), h('div', { style: 'height:14px' }));
      scrollToFirstError(m.body);
      return null;
    }
    // Start from the stored service so that fields this form does not edit
    // survive the full-object PUT; the conditional ones are reset below.
    const { status: _status, created_at: _created, updated_at: _updated, tls: _tls, profile_id: _profile, recording_id: _rec, ...kept } = s;
    void _status; void _created; void _updated; void _tls; void _profile; void _rec;
    const payload = {
      ...kept,
      ...(editing ? { id: s.id } : {}),
      name: nm, listen_addr: la, upstream: up, mode: md, enabled: enabled.get(),
      anonymize_capture: anon.get(),
      endpoint_patterns: pats,
      preserve_host: preserveHost.get(), insecure_skip_verify: skipVerify.get(),
      timeout: tmo ? toGoDuration(tmo) : '0s',
      max_body_bytes: mb === null ? 0 : mb,
      capture_sample: smp >= 100 ? 1 : smp / 100,
      max_capture_bytes: cap === null ? 0 : cap,
      match: {
        query: mQuery.get(), body: mBody.get(), path_templates: mTemplates.get(), selection: mSelection.get(), miss_status: miss || 404,
        ...(mIgnore.get().length ? { ignore_params: mIgnore.get() } : {}),
        ...(mHeaders.get().length ? { headers: mHeaders.get() } : {}),
      },
      latency: {
        mode: latMode.get(), multiplier: latMult.get() ?? 1,
        fixed: latFixed.get() ? toGoDuration(parseDuration(latFixed.get())) : '0s',
        jitter: latJitter.get() ? toGoDuration(parseDuration(latJitter.get())) : '0s',
      },
      error_amplification: amp,
      error_floor: floor === null ? 0 : floor / 100,
    };
    if (recordingId) payload.recording_id = recordingId;
    if (anon.get()) payload.profile_id = profile.get();
    if (tm === 'self') payload.tls = { self_signed: true };
    else if (tm === 'files') payload.tls = { cert_file: certFile.get().trim(), key_file: keyFile.get().trim() };
    return { payload, newRec };
  }

  const m = openModal({
    title: editing ? `Edit ${s.name}` : 'New service',
    subtitle: editing ? 'Changes apply immediately; the listener restarts if needed.' : 'A service is a listener that sits in front of your API.',
    size: 'lg',
    content: body,
    footer: [btn('Cancel', { onClick: () => m.close(null) }), saveBtn],
    initialFocus: editing ? undefined : name.input,
  });
  return m;
}

// ------------------------------------------------------------------------
/**
 * Switches a service mode, asking which recording to use when needed.
 * Resolves to the updated service, or null when cancelled.
 */
export async function switchMode(svc, newMode) {
  if (svc.mode === newMode) return null;
  const needsRecording = newMode !== 'passthrough';
  if (!needsRecording || (newMode !== 'record' && svc.recording_id)) {
    const updated = await api.setMode(svc.id, { mode: newMode, ...(svc.recording_id && needsRecording ? { recording_id: svc.recording_id } : {}) });
    await refreshServices().catch(() => {});
    toastOk(`${svc.name} is now in ${modeLabel(newMode)} mode`);
    return updated;
  }
  return pickRecording(svc, newMode);
}

function pickRecording(svc, newMode) {
  return new Promise(async (resolve) => {
    const recordings = await api.recordings().catch(() => []);
    const allowNew = newMode !== 'virtualize';
    const hasExisting = recordings.length > 0;
    const source = segmentedCtl([
      ...(allowNew ? [{ value: 'new', label: 'New recording' }] : []),
      { value: 'existing', label: 'Existing recording' },
    ], { value: allowNew && (newMode === 'record' || !hasExisting) ? 'new' : 'existing', label: 'Recording source' });
    const nameC = textCtl({ value: `${svc.name} ${stamp()}`, placeholder: 'Recording name' });
    const sel = selectCtl(recordings.length ? recordings.map((r) => ({ value: r.id, label: `${r.name}  (${r.exchange_count} exchanges)` })) : [{ value: '', label: 'No recordings yet' }], { value: svc.recording_id || (recordings[0] && recordings[0].id) || '' });
    const nameF = field('Recording name', nameC);
    const selF = field('Recording', sel, { help: newMode === 'record' ? 'New exchanges are appended to it.' : undefined });
    const errHost = h('div');
    const paint = () => { const isNew = source.get() === 'new'; nameF.hidden = !isNew; selF.hidden = isNew; };
    source.onChange(paint); paint();
    const intro = {
      record: 'Choose where captured traffic goes. A fresh recording keeps this run separate and easy to replay later.',
      virtualize: 'Virtualize answers from an existing recording. Pick the one to serve.',
      hybrid: 'Hybrid serves from a recording and records the misses into it.',
    }[newMode];
    let done = false;
    const confirm = btn(`Switch to ${modeLabel(newMode)}`, {
      kind: 'primary',
      onClick: async () => {
        const isNew = source.get() === 'new';
        const body = { mode: newMode };
        if (isNew) {
          const nm = nameC.get().trim();
          if (!nm) { nameF.setError('Give the recording a name.'); return; }
          body.new_recording_name = nm;
        } else {
          if (!sel.get()) { selF.setError('Pick a recording. Record or import one first.'); return; }
          body.recording_id = sel.get();
        }
        try {
          const updated = await api.setMode(svc.id, body);
          await refreshServices().catch(() => {});
          toastOk(`${svc.name} is now in ${modeLabel(newMode)} mode`);
          done = true;
          modal.close(updated);
        } catch (e) {
          if (!(e instanceof ApiError)) throw e;
          errHost.replaceChildren(errorBox(e));
        }
      },
    });
    const modal = openModal({
      title: `Switch ${svc.name} to ${modeLabel(newMode)}`, size: 'sm',
      content: h('div', { class: 'stack-sm' }, h('p', { class: 'muted' }, intro), errHost, source.el, nameF, selF),
      footer: [btn('Cancel', { onClick: () => modal.close(null) }), confirm],
      onClose: (r) => resolve(done ? r : null),
    });
  });
}

