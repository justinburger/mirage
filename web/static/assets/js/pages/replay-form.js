// New replay form (#/replays/new). Explains every knob and shows a live
// plan summary next to the form.
import { h } from '../dom.js';
import { icon } from '../icons.js';
import { api, ApiError } from '../api.js';
import { navigate } from '../router.js';
import { rampChart } from '../chart.js';
import { card, btn, field, formSection, formGrid, textCtl, numCtl, durCtl, selectCtl, switchCtl, segmentedCtl, chipsCtl, linesCtl, kvCtl, sliderCtl, collapsible, errorBox, applyProblems, scrollToFirstError, alertBox, toastOk, badge, iconBtn, pageHeader } from '../ui.js';
import { fmtInt, fmtHuman, fmtPct, toGoDuration, DASH, fmtRate } from '../format.js';
import { durText, stamp } from '../service-form.js';

const MODE_TEXT = {
  timeline: 'Reproduces the recorded inter-arrival times, compressed by the speed factor. Speed 10 replays the recorded traffic shape ten times faster, with the same bursts and quiet periods.',
  rate: 'Sends at an arrival rate you choose, cycling through the recording in order. Open model: new requests keep arriving even when the target is slow, which is what exposes queueing. Use stages to ramp up.',
  max: 'Closed loop: each worker sends the next request as soon as the previous one finishes, so the target sets the pace. Finds the maximum throughput for a given concurrency.',
};

export async function create(ctx) {
  ctx.setCrumbs([{ label: 'Replays', href: '#/replays' }, { label: 'New replay' }]);
  const [recs, profiles] = await Promise.all([api.recordings(), api.profiles().catch(() => [])]);
  let base = null;
  let baseName = '';
  const fromId = ctx.query.from || ctx.query.clone;
  if (fromId) {
    try { const j = await api.replay(fromId); base = j.config; baseName = `${j.name} (edited)`; } catch (_) { /* ignore */ }
  }
  const cfg = base || {};

  const root = h('div', { class: 'stack' });
  root.appendChild(pageHeader({ subtitle: 'Replay recorded traffic against a target at a multiple of its original rate, error mix or concurrency.' }));
  if (!recs.length) {
    root.appendChild(card({ body: h('div', { class: 'empty' }, h('div', { class: 'empty-title' }, 'You need a recording first'), h('div', { class: 'empty-text' }, 'Replays send recorded requests to a target. Record traffic with a service, or import a HAR file.'), h('div', { class: 'empty-actions' }, btn('Go to recordings', { href: '#/recordings', kind: 'primary' }))) }));
    return root;
  }

  // ----------------------------------------------------------- controls
  const initialRec = ctx.query.recording || cfg.recording_id || recs[0].id;
  const name = textCtl({ value: baseName, placeholder: 'e.g. 10x checkout peak' });
  const recSel = selectCtl(recs.map((r) => ({ value: r.id, label: `${r.name}  (${fmtInt(r.exchange_count)} exchanges)` })), { value: recs.some((r) => r.id === initialRec) ? initialRec : recs[0].id });
  const target = textCtl({ value: cfg.target || '', placeholder: 'http://staging.internal:8080', mono: true });
  const mode = segmentedCtl([{ value: 'timeline', label: 'Timeline' }, { value: 'rate', label: 'Rate' }, { value: 'max', label: 'Max' }], { value: cfg.mode || 'timeline', label: 'Replay mode' });
  const speed = sliderCtl({ min: 0.1, max: 200, step: 0.1, log: true, value: cfg.speed || 1, presets: [1, 2, 5, 10, 25, 100], format: (v) => `${v}×`, numMin: 0.01, numMax: 100000 });
  const rateKind = segmentedCtl([{ value: 'constant', label: 'Constant rate' }, { value: 'stages', label: 'Stages (ramp)' }], { value: cfg.stages && cfg.stages.length ? 'stages' : 'constant', label: 'Rate shape', size: 'sm' });
  const rate = numCtl({ value: cfg.rate || 100, min: 0, step: 1, unit: 'req/s' });
  const rateDuration = durCtl({ value: durText(cfg.duration) || '2m', placeholder: '2m' });
  const maxConc = numCtl({ value: cfg.concurrency || 256, min: 1, step: 1, unit: 'workers' });
  const amplify = sliderCtl({ min: 1, max: 100, step: 1, log: true, value: cfg.amplify || 1, presets: [1, 2, 5, 10], format: (v) => `${v}×`, numMin: 1, numMax: 1000 });
  const errAmp = sliderCtl({ min: 0.1, max: 100, step: 0.1, log: true, value: cfg.error_amplification || 1, presets: [1, 2, 5, 10], format: (v) => `${v}×`, numMin: 0.01, numMax: 1000 });
  const concurrency = numCtl({ value: cfg.concurrency || 256, min: 1, step: 1, unit: 'in flight' });
  const duration = durCtl({ value: durText(cfg.duration), placeholder: 'optional, e.g. 5m' });
  const loop = switchCtl({ label: 'Loop the recording', help: 'Start over when the recording ends. Needs a duration or a request cap.', checked: !!cfg.loop });
  const maxReq = numCtl({ value: cfg.max_requests || '', min: 0, step: 1, placeholder: '0 = no cap' });
  const timeout = durCtl({ value: durText(cfg.timeout) || '30s', placeholder: '30s' });
  const include = linesCtl({ value: cfg.include || [], rows: 2, placeholder: '^GET /users\n/checkout', validate: regexCheck });
  const exclude = linesCtl({ value: cfg.exclude || [], rows: 2, placeholder: '^DELETE\n/admin/', validate: regexCheck });
  const setHeaders = kvCtl({ value: cfg.set_headers || {}, keyPlaceholder: 'Authorization', valuePlaceholder: 'Bearer staging-token', addLabel: 'Add header' });
  const removeHeaders = chipsCtl({ value: cfg.remove_headers || [], placeholder: 'e.g. Cookie' });
  const hostHeader = textCtl({ value: cfg.host_header || '', placeholder: 'default: the target host', mono: true });
  const skipVerify = switchCtl({ label: 'Skip TLS verification', help: 'Accept any certificate presented by the target.', checked: !!cfg.insecure_skip_verify });
  const profile = selectCtl([{ value: '', label: 'None (send as stored)' }, ...profiles.map((p) => ({ value: p.id, label: p.name }))], { value: cfg.profile_id || '' });
  const vary = switchCtl({ label: 'Vary clones', help: 'With amplify above 1x, give each copy its own consistent identities instead of identical requests hammering the same keys.', checked: !!cfg.vary_clones });
  const compare = switchCtl({ label: 'Compare statuses with the recording', help: 'Count responses whose status differs from the recorded one (shown as mismatches).', checked: cfg.compare_status ?? true });

  // stages editor
  const stagesHost = h('div', { class: 'stack-sm' });
  const stageRows = [];
  function addStage(d = '', r = '') {
    const dur = durCtl({ value: d, placeholder: '30s' });
    const rt = numCtl({ value: r, min: 0, step: 1, placeholder: '500', unit: 'req/s' });
    const num = h('div', { class: 'rule-num' });
    const row = h('div', { class: 'stage-row' }, num, field('Duration', dur), field('Target rate', rt),
      h('div', { style: 'margin-bottom:2px' }, iconBtn('x', 'Remove stage', () => { const i = stageRows.indexOf(entry); stageRows.splice(i, 1); row.remove(); renumber(); paintPlan(); })));
    const entry = { dur, rt, row, num };
    dur.onChange(paintPlan); rt.onChange(paintPlan);
    stageRows.push(entry);
    stagesHost.appendChild(row);
    renumber();
  }
  function renumber() { stageRows.forEach((s, i) => { s.num.textContent = String(i + 1); }); }
  if (cfg.stages && cfg.stages.length) cfg.stages.forEach((s) => addStage(durText(s.duration), s.rate));
  else { addStage('30s', 500); addStage('2m', 2000); }
  const rampHost = h('div');
  const stagesBlock = h('div', { class: 'stack-sm' }, stagesHost,
    h('div', { class: 'row-between' }, btn('Add stage', { icon: 'plus', size: 'sm', onClick: () => { const last = stageRows[stageRows.length - 1]; addStage('1m', last && last.rt.get() ? last.rt.get() : 1000); paintPlan(); } }),
      h('span', { class: 'muted', style: 'font-size:12px' }, 'The rate ramps linearly from the previous stage (the first ramps from 0).')),
    rampHost);

  const F = {
    name: field('Name', name, { help: 'Shown in the replay list. Optional.' }),
    recording_id: field('Recording', recSel, { required: true }),
    target: field('Target URL', target, { required: true, help: 'Base URL for the requests. Each recorded path and query is appended; a path on the target is used as a prefix.' }),
    speed: field('Speed', speed, { help: 'Speed 10 replays the recorded traffic shape ten times faster. 1× keeps the original timing.' }),
    rate: field('Rate', rate, { help: 'Requests per second, held constant.' }),
    duration_rate: field('Duration', rateDuration, { help: 'How long to hold the rate, e.g. 2m.' }),
    concurrency_max: field('Concurrency', maxConc, { help: 'Number of workers sending back-to-back. More workers means more load.' }),
    amplify: field('Amplify (copies)', amplify, { help: 'Send each scheduled request this many times. Amplify 10 is ten times the recorded traffic, spread evenly between requests instead of firing in a burst.' }),
    error_amplification: field('Error amplification', errAmp, { help: 'Oversamples requests whose recorded response was an error (5xx or 429). 10× sends them about ten times as often, so the target sees many more error-inducing requests.' }),
    concurrency: field('Max in flight', concurrency, { help: 'Caps concurrent requests. In max mode this is the number of workers.' }),
    duration: field('Stop after', duration, { help: 'Optional run time limit. Leave empty for one pass through the recording.' }),
    max_requests: field('Request cap', maxReq, { help: 'Stop after this many requests (0 = no cap).' }),
    timeout: field('Request timeout', timeout, { help: 'Per request. Slow responses past this count as timeouts.' }),
    include: field('Only include requests matching', include, { help: 'RE2 regex per line, matched against "METHOD /path?query". Empty includes everything.' }),
    exclude: field('Exclude requests matching', exclude, { help: 'Same syntax. Excludes win over includes.' }),
    set_headers: field('Set headers on every request', setHeaders, { help: 'Typically credentials that are valid for the target environment.' }),
    remove_headers: field('Remove headers', removeHeaders, { help: 'Deleted from every request before sending, e.g. recorded Cookie headers.' }),
    host_header: field('Host header override', hostHeader, { help: 'Useful when the target is reached through a load balancer or IP address.' }),
    profile_id: field('Anonymize on the fly', profile, { help: 'Apply a profile to every request before sending. Needed when replaying a raw recording against shared environments.' }),
  };

  // ----------------------------------------------------------- layout
  const modeText = h('p', { class: 'explain' });
  const timelineBlock = h('div', { class: 'stack-sm' }, F.speed);
  const rateBlock = h('div', { class: 'stack' }, rateKind.el,
    h('div', { id: 'rate-const' }, formGrid(F.rate, F.duration_rate)), stagesBlock);
  const rateConstHost = rateBlock.querySelector('#rate-const');
  const maxBlock = h('div', null, F.concurrency_max);
  const recInfo = h('div', { class: 'row', style: 'font-size:12.5px' });
  const rawWarn = h('div');
  const shape = formSection('Load shape', null, mode.el, modeText, timelineBlock, rateBlock, maxBlock);
  const advanced = collapsible('Advanced', h('div', { class: 'stack' },
    formGrid(F.include, F.exclude), F.set_headers, formGrid(F.remove_headers, F.host_header), skipVerify.el,
    formGrid(F.profile_id), vary.el, compare.el), { hint: 'filters, headers, anonymization, status comparison' });
  const errHost = h('div');
  const form = h('div', null,
    errHost,
    formSection('What to replay', null, formGrid(F.name, F.recording_id), recInfo, rawWarn, F.target),
    shape,
    formSection('Multiply the traffic', 'Independent knobs on top of the load shape.', formGrid(F.amplify, F.error_amplification)),
    formSection('Limits', null, formGrid(F.concurrency, F.timeout), formGrid(F.duration, F.max_requests), loop.el),
    advanced);

  // ------------------------------------------------------- plan summary
  const planHost = h('div');
  const rampMount = rampHost;
  function recording() { return recs.find((r) => r.id === recSel.get()); }
  function stageList() {
    return stageRows.map((s) => ({ durationMs: s.dur.ms() || 0, rate: s.rt.get() || 0 })).filter((s) => s.durationMs > 0);
  }
  function paintPlan() {
    const m = mode.get();
    const rec = recording();
    modeText.textContent = MODE_TEXT[m];
    timelineBlock.hidden = m !== 'timeline';
    rateBlock.hidden = m !== 'rate';
    maxBlock.hidden = m !== 'max';
    rateConstHost.hidden = rateKind.get() !== 'constant';
    stagesBlock.hidden = rateKind.get() !== 'stages';
    F.concurrency.hidden = m === 'max';
    vary.setDisabled(!profile.get() || (amplify.get() || 1) <= 1);
    if (rec) {
      recInfo.replaceChildren(
        badge(rec.anonymized ? 'anonymized' : 'raw', rec.anonymized ? 'ok' : 'warn', { dot: true }),
        h('span', { class: 'muted' }, `${fmtInt(rec.exchange_count)} exchanges, ${fmtInt(rec.error_count)} errors, ${rec.first_at && rec.last_at ? fmtHuman(Date.parse(rec.last_at) - Date.parse(rec.first_at)) + ' of traffic' : 'empty'}`));
      rawWarn.replaceChildren(!rec.anonymized && !profile.get() ? alertBox('warn', 'This recording is raw: requests are sent exactly as captured. Pick an anonymization profile under Advanced to anonymize on the fly.') : '');
    }
    const amp = amplify.get() || 1;
    const items = [];
    let big = DASH, bigSub = '';
    const spanSec = rec && rec.first_at && rec.last_at ? Math.max(1, (Date.parse(rec.last_at) - Date.parse(rec.first_at)) / 1000) : 0;
    const n = rec ? rec.exchange_count : 0;
    if (m === 'timeline') {
      const sp = speed.get() || 1;
      const avg = spanSec ? (n / spanSec) * sp * amp : 0;
      big = fmtRate(avg) + ' req/s';
      bigSub = 'estimated average arrival rate';
      items.push(['Recorded traffic', spanSec ? `${fmtRate(n / spanSec)} req/s over ${fmtHuman(spanSec * 1000)}` : DASH]);
      items.push(['Run time', spanSec ? (loop.get() && duration.ms() ? fmtHuman(duration.ms()) : fmtHuman((spanSec / sp) * 1000)) : DASH]);
      items.push(['Requests sent', loop.get() && !maxReq.get() ? 'until the time limit' : fmtInt((maxReq.get() > 0 ? Math.min(maxReq.get(), n * amp) : n * amp))]);
    } else if (m === 'rate') {
      if (rateKind.get() === 'constant') {
        const r = rate.get() || 0;
        const d = rateDuration.ms() || 0;
        big = fmtRate(r) + ' req/s';
        bigSub = 'constant arrival rate';
        items.push(['Run time', d ? fmtHuman(d) : DASH]);
        items.push(['Requests sent', d ? fmtInt(Math.round(r * (d / 1000))) : DASH]);
      } else {
        const st = stageList();
        const total = st.reduce((a, s) => a + s.durationMs, 0);
        let prev = 0, count = 0;
        for (const s of st) { count += ((prev + s.rate) / 2) * (s.durationMs / 1000); prev = s.rate; }
        const peak = Math.max(0, ...st.map((s) => s.rate));
        big = `peak ${fmtRate(peak)} req/s`;
        bigSub = `${st.length} stage${st.length === 1 ? '' : 's'} over ${fmtHuman(total)}`;
        items.push(['Run time', fmtHuman(total)]);
        items.push(['Requests sent', fmtInt(Math.round(count))]);
        rampMount.replaceChildren(st.length ? rampChart(st, { width: 600, height: 120 }) : '');
      }
    } else {
      big = `${fmtInt(maxConc.get() || 0)} workers`;
      bigSub = 'closed loop, as fast as the target allows';
      items.push(['Run time', duration.ms() ? fmtHuman(duration.ms()) : 'one pass through the recording']);
      items.push(['Requests', maxReq.get() > 0 ? fmtInt(maxReq.get()) : fmtInt(n * amp)]);
    }
    if (m !== 'rate' || rateKind.get() !== 'stages') rampMount.replaceChildren();
    if (rec && rec.exchange_count) {
      const base0 = rec.error_count / rec.exchange_count;
      const ea = errAmp.get() || 1;
      const share = Math.min(1, (base0 * ea) / (1 - base0 + base0 * ea));
      items.push(['Error-inducing requests', `${fmtPct(share)}${ea !== 1 ? ` (recorded ${fmtPct(base0)})` : ''}`]);
    }
    items.push(['Copies per request', `${amp}×`]);
    items.push(['Target', target.get().trim() || DASH]);
    planHost.replaceChildren(
      h('div', { class: 'plan-big' }, big),
      h('div', { class: 'muted', style: 'margin-bottom:10px' }, bigSub),
      h('ul', { class: 'plan' }, items.map(([k, v]) => h('li', null, h('span', null, k), h('span', { class: 'mono wrap-any' }, v)))));
  }
  [mode, rateKind, rate, rateDuration, maxConc, amplify, errAmp, concurrency, duration, loop, maxReq, speed, recSel, target, profile].forEach((c) => c.onChange(paintPlan));
  paintPlan();

  // ------------------------------------------------------------- submit
  function collect() {
    for (const f of Object.values(F)) f.setError('');
    const errs = [];
    const bail = (k, msg) => { F[k].setError(msg); errs.push(msg); };
    const m = mode.get();
    const tg = target.get().trim();
    let okT = false;
    try { const u = new URL(tg); okT = (u.protocol === 'http:' || u.protocol === 'https:') && !!u.host; } catch (_) { okT = false; }
    if (!okT) bail('target', 'Enter an absolute http:// or https:// URL.');
    if (!recSel.get()) bail('recording_id', 'Pick a recording.');
    if (m === 'timeline' && !((speed.get() || 0) > 0)) bail('speed', 'Speed must be greater than 0.');
    const config = {
      recording_id: recSel.get(), target: tg, mode: m,
      amplify: Math.round(amplify.get() || 1), error_amplification: errAmp.get() || 1,
      compare_status: compare.get(),
    };
    const am = config.amplify;
    if (am < 1 || am > 1000) bail('amplify', 'Amplify must be between 1 and 1000.');
    if (!((errAmp.get() || 0) > 0 && errAmp.get() <= 1000)) bail('error_amplification', 'Must be greater than 0 and at most 1000.');
    if (m === 'timeline') config.speed = speed.get() || 1;
    if (m === 'rate') {
      if (rateKind.get() === 'constant') {
        if (!((rate.get() || 0) > 0)) bail('rate', 'Rate must be greater than 0.');
        if (!rateDuration.valid() || !rateDuration.ms()) bail('duration_rate', 'Enter a duration like 2m.');
        config.rate = rate.get();
        config.duration = toGoDuration(rateDuration.ms() || 0);
      } else {
        const st = stageRows.map((s, i) => ({ i, d: s.dur.ms(), r: s.rt.get() }));
        if (!st.length) errs.push('Add at least one stage.');
        for (const s of st) {
          if (!s.d || s.d <= 0 || s.r === null || s.r < 0) { stageRows[s.i].row.classList.add('has-error'); errs.push(`Stage ${s.i + 1} needs a duration and a rate.`); }
        }
        config.stages = st.map((s) => ({ duration: toGoDuration(s.d || 0), rate: s.r || 0 }));
      }
    }
    const conc = m === 'max' ? maxConc.get() : concurrency.get();
    if (!conc || conc < 1) bail(m === 'max' ? 'concurrency_max' : 'concurrency', 'Must be at least 1.');
    else config.concurrency = Math.round(conc);
    if (!duration.valid()) bail('duration', 'Use a duration like 5m.');
    else if (duration.ms() && m !== 'rate') config.duration = toGoDuration(duration.ms());
    else if (duration.ms() && m === 'rate' && rateKind.get() === 'stages') config.duration = toGoDuration(duration.ms());
    if (!timeout.valid()) bail('timeout', 'Use a duration like 30s.');
    else if (timeout.ms()) config.timeout = toGoDuration(timeout.ms());
    if (loop.get()) {
      config.loop = true;
      if (!config.duration && !(maxReq.get() > 0)) bail('duration', 'Loop needs a duration or a request cap.');
    }
    if (maxReq.get() > 0) config.max_requests = Math.round(maxReq.get());
    if (include.validate()) bail('include', 'Fix the invalid regular expression.');
    if (exclude.validate()) bail('exclude', 'Fix the invalid regular expression.');
    if (include.get().length) config.include = include.get();
    if (exclude.get().length) config.exclude = exclude.get();
    const sh = setHeaders.get();
    if (Object.keys(sh).length) config.set_headers = sh;
    if (removeHeaders.get().length) config.remove_headers = removeHeaders.get();
    if (hostHeader.get().trim()) config.host_header = hostHeader.get().trim();
    if (skipVerify.get()) config.insecure_skip_verify = true;
    if (profile.get()) { config.profile_id = profile.get(); if (vary.get()) config.vary_clones = true; }
    if (errs.length) {
      errHost.replaceChildren(alertBox('error', 'Fix the highlighted fields and try again.'), h('div', { style: 'height:14px' }));
      scrollToFirstError(form);
      return null;
    }
    return config;
  }
  async function submit(start) {
    const config = collect();
    if (!config) return;
    errHost.replaceChildren();
    const rec = recording();
    const body = { name: name.get().trim() || `${rec ? rec.name : 'replay'} ${stamp()}`, config, start };
    try {
      const job = await api.createReplay(body);
      toastOk(start ? `Started ${job.name}` : `Saved ${job.name}`);
      navigate(`/replays/${encodeURIComponent(job.id)}`);
    } catch (e) {
      if (!(e instanceof ApiError)) throw e;
      applyProblems({ target: F.target, recording_id: F.recording_id, speed: F.speed, rate: F.rate, 'stage': F.duration_rate, amplify: F.amplify, error_amplification: F.error_amplification, concurrency: F.concurrency, duration: F.duration, 'max_requests': F.max_requests, timeout: F.timeout, 'profile_id': F.profile_id }, e);
      errHost.replaceChildren(errorBox(e), h('div', { style: 'height:14px' }));
      scrollToFirstError(form);
    }
  }

  const actions = h('div', { class: 'form-actions', style: 'margin-top:8px' },
    btn('Cancel', { kind: 'ghost', href: '#/replays' }),
    btn('Save without starting', { onClick: () => submit(false) }),
    btn('Start replay', { icon: 'play', kind: 'primary', onClick: () => submit(true) }));
  root.appendChild(h('div', { class: 'cols-main-wide' },
    h('div', null, card({ body: h('div', null, form, actions) })),
    h('div', { class: 'sticky' }, card({ title: 'Plan', subtitle: 'What this replay will do', body: planHost }))));
  void icon;
  return root;
}

function regexCheck(line) {
  try { new RegExp(line); return null; } catch (e) { return e.message.replace(/^Invalid regular expression: /, ''); }
}
