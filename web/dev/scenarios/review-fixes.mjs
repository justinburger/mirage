// Regression checks for the UI review findings (masked replay secrets, runtime
// service notices, honest compare view, service form layout, chart legends).
// Runs against the mock server, which masks sensitive set_headers and reports
// "capture limit reached" like the Go server does:
//
//   PORT=8485 node web/dev/mock-server.mjs
//   node web/dev/run.mjs web/dev/scenarios/review-fixes.mjs
//
// BASE overrides the origin. It changes mock data (jobs, one service), so
// restart the mock afterwards.
const BASE = process.env.BASE || 'http://127.0.0.1:8485';
const sleep = (ms) => ({ wait: ms });
const run = (name, code) => ({ name, eval: `(async()=>{ ${code} ; return true; })()` });
const ok = (name, expr) => ({ name, assert: expr, message: name });
const waitFor = (name, expr, timeout = 6000) => ({ name, waitFor: expr, timeout });
const go = (hash) => ({ name: `go ${hash}`, eval: `location.hash='${hash}'; true` });

import { HELPERS } from './helpers.mjs';

// In-page helpers: API calls, a hook that records the bodies of POST /replays,
// and readers for the pieces of the pages under test.
const PAGE = `
window.__api = async (method, path, body) => {
  const res = await fetch('/api/v1' + path, { method, headers: body ? { 'content-type': 'application/json' } : {}, body: body ? JSON.stringify(body) : undefined });
  const text = await res.text();
  if (!res.ok) throw new Error(method + ' ' + path + ' -> ' + res.status + ' ' + text);
  return text ? JSON.parse(text) : null;
};
window.__posts = [];
if (!window.__hooked) {
  window.__hooked = true;
  const real = window.fetch;
  window.fetch = (u, o) => {
    if (o && o.method === 'POST' && /\\/api\\/v1\\/replays$/.test(String(u))) window.__posts.push(JSON.parse(o.body));
    return real(u, o);
  };
}
window.__kv = () => [...document.querySelectorAll('.kv-row')].map((r) => ({ k: r.children[0].value, v: r.children[1].value, row: r }));
window.__legend = (chartTitle) => {
  const c = [...document.querySelectorAll('.chart')].find((x) => x.querySelector('.chart-title').firstChild.nodeValue.trim() === chartTitle);
  if (!c) throw new Error('no chart ' + chartTitle);
  return [...c.querySelectorAll('.legend-item')].map((b) => ({ label: b.querySelector('.legend-label').textContent, val: b.querySelector('.legend-val').textContent }));
};
`;

export default function steps() {
  return [
    { media: { 'prefers-color-scheme': 'dark' } },
    { viewport: { w: 1440, h: 1100 } },
    { goto: `${BASE}/#/replays`, settle: 1200 },
    run('install helpers', HELPERS + PAGE),

    // ===== replay-edit-as-new-masks-secret / edit-as-new-resubmits-masked-secrets
    run('create a job with a secret header and a visible one', `
      const j = await __api('POST', '/replays', { name: 'with secret', config: { recording_id: 'rec_paymentsrun3000', target: 'http://127.0.0.1:19999', mode: 'timeline', speed: 2, set_headers: { Authorization: 'Bearer abc123', 'X-Test': 'visible' } } });
      window.__secretJob = j.id; window.__masked = j.config.set_headers.Authorization; window.__posts.length = 0;`),
    ok('the API masks the secret on read (premise)', `window.__masked === '***'`),
    run('open Edit as new for that job', `location.hash = '#/replays/new?from=' + window.__secretJob`),
    waitFor('the header editor is filled', `__kv().length === 2`),
    ok('the masked secret is not put into the form', `__kv().every((r) => r.v !== '***')`),
    ok('the visible header is prefilled as is', `__kv().some((r) => r.k === 'X-Test' && r.v === 'visible')`),
    ok('the secret row asks for the value again', `__kv().some((r) => r.k === 'Authorization' && r.v === '' && /re-enter/i.test(r.row.children[1].placeholder))`),
    ok('the masked row is marked', `__kv().find((r) => r.k === 'Authorization').row.classList.contains('is-masked')`),
    ok('a note explains that the secret is hidden', `/hidden/i.test(document.querySelector('.kv-note') ? document.querySelector('.kv-note').innerText : '')`),
    ok('Advanced is open so the rows are visible', `[...document.querySelectorAll('details.collapsible')].find((d) => /Advanced/.test(d.innerText.slice(0, 40))).open`),
    run('start without re-entering the secret', `__t.click('button', '^\\\\s*Start replay')`), sleep(500),
    ok('nothing was posted', `window.__posts.length === 0`),
    ok('the field says which secret is missing', `/Authorization/.test(document.querySelector('.field.has-error') ? document.querySelector('.field.has-error').innerText : '')`),
    ok('we stay on the form', `location.hash.startsWith('#/replays/new')`),
    run('type the literal placeholder', `const r = __kv().find((x) => x.k === 'Authorization'); r.row.children[1].value = '***'; __t.fire(r.row.children[1], 'input')`),
    run('start again', `__t.click('button', '^\\\\s*Start replay')`), sleep(400),
    ok('the placeholder is refused too', `window.__posts.length === 0 && /\\*\\*\\*|placeholder/.test(document.querySelector('.field.has-error') ? document.querySelector('.field.has-error').innerText : '')`),
    run('re-enter the secret', `const r = __kv().find((x) => x.k === 'Authorization'); r.row.children[1].value = 'Bearer fresh-token'; __t.fire(r.row.children[1], 'input')`),
    run('start', `__t.click('button', '^\\\\s*Start replay')`),
    waitFor('the new job page opens', `/^#\\/replays\\/rpl_/.test(location.hash)`),
    ok('exactly one create call', `window.__posts.length === 1`),
    ok('it carries the re-entered secret', `window.__posts[0].config.set_headers.Authorization === 'Bearer fresh-token'`),
    ok('it carries the visible header', `window.__posts[0].config.set_headers['X-Test'] === 'visible'`),
    ok('the placeholder was never sent', `!JSON.stringify(window.__posts).includes('"***"')`),

    // removing the masked row sends no Authorization header at all
    run('reset the hook', `window.__posts.length = 0`),
    run('open Edit as new again', `location.hash = '#/replays/new?from=' + window.__secretJob`),
    waitFor('the header editor is filled', `__kv().length === 2`),
    run('remove the masked row', `__kv().find((r) => r.k === 'Authorization').row.querySelector('button').click()`),
    ok('the note is gone with the row', `!document.querySelector('.kv-note') || document.querySelector('.kv-note').hidden`),
    run('save without starting', `__t.click('button', 'Save without starting')`),
    waitFor('saved', `window.__posts.length === 1`),
    ok('only the visible header was sent', `JSON.stringify(window.__posts[0].config.set_headers) === '{"X-Test":"visible"}'`),

    // ===== capture-limit-shown-as-could-not-start
    run('cap the recording of the payments service at 1 byte', `
      const svc = await __api('GET', '/services/svc_payments0000001');
      const { status, created_at, updated_at, ...rest } = svc;
      window.__svcBackup = rest;
      await __api('PUT', '/services/svc_payments0000001', { ...rest, max_capture_bytes: 1 });
      window.__st = (await __api('GET', '/services/svc_payments0000001')).status;`),
    ok('the service runs and reports the limit (premise)', `window.__st.running === true && window.__st.warning === 'capture limit reached' && !window.__st.error`),
    go('#/services/svc_payments0000001'), sleep(1200),
    ok('no "could not start" title', `!/could not start/i.test(document.querySelector('main').innerText)`),
    ok('an amber notice names the stopped capture', `!!document.querySelector('.alert-warn') && /Capture stopped/i.test(document.querySelector('.alert-warn').innerText)`),
    ok('it says the limit in bytes', `/1 B|1 byte/i.test(document.querySelector('.alert-warn').innerText)`),
    ok('there is no red alert on a running service', `!document.querySelector('.alert-error')`),
    ok('offers to raise the limit', `[...document.querySelectorAll('.alert-warn button, .alert-warn a')].some((b) => /raise the limit/i.test(b.innerText))`),
    ok('the Captured cell is flagged', `/capture limit reached|limit reached/i.test(([...document.querySelectorAll('.stat-cell')].find((c) => /^Captured/i.test(c.innerText.trim())) || { innerText: '' }).innerText)`),
    go('#/services'), sleep(900),
    ok('the list row still says Running', `/Running/.test([...document.querySelectorAll('tbody tr')].find((r) => /payments/.test(r.innerText)).innerText)`),
    ok('the list row flags the full capture', `/Capture full|Capture limit/i.test([...document.querySelectorAll('tbody tr')].find((r) => /payments/.test(r.innerText)).innerText)`),
    go('#/'), sleep(1200),
    ok('the dashboard counts it as a warning, not an error', `(() => { const t = [...document.querySelectorAll('.tile')].find((x) => /services/i.test(x.querySelector('.tile-label').innerText)); return /1 with warnings?/.test(t.innerText) && !/with errors/.test(t.innerText); })()`),
    run('raise the limit from the notice', `location.hash = '#/services/svc_payments0000001'`), sleep(900),
    run('click Raise the limit', `__t.click('.alert-warn button, .alert-warn a', 'raise the limit')`), sleep(500),
    ok('the edit form opened', `!!__t.modal() && /Edit payments/.test(__t.modal().innerText)`),
    ok('focus is on the capture limit', `document.activeElement === __t.field('Capture limit', __t.modal())`),
    run('close the form', `__t.click('.modal-foot button', 'Cancel')`), sleep(300),
    run('restore the service', `await __api('PUT', '/services/svc_payments0000001', window.__svcBackup)`),
    run('a service that really fails to start', `await __api('POST', '/services', { name: 'clash', listen_addr: ':9999', upstream: 'http://x.internal:8080', mode: 'passthrough', enabled: true }); window.__clash = (await __api('GET', '/services')).find((s) => s.name === 'clash').id`),
    run('open it', `location.hash = '#/services/' + window.__clash`), sleep(1000),
    ok('a start failure keeps the red "could not start" banner', `!!document.querySelector('.alert-error') && /could not start/i.test(document.querySelector('.alert-error').innerText)`),
    run('remove the clash service', `await __api('DELETE', '/services/' + window.__clash)`),

    // ===== new-service-form-virtualize-layout
    go('#/services'), sleep(500),
    run('open the new service form', `__t.click('button', '^\\\\s*New service')`), sleep(600),
    ok('Virtualization starts closed in passthrough mode', `!__t.modal().querySelector('details.collapsible:nth-of-type(1)') || ![...__t.modal().querySelectorAll('details.collapsible')].find((d) => /Virtualization/.test(d.innerText.slice(0, 40))).open`),
    run('choose Virtualize', `__t.click('.modal .seg-btn', '^\\\\s*Virtualize')`), sleep(300),
    ok('Virtualization opens', `[...__t.modal().querySelectorAll('details.collapsible')].find((d) => /Virtualization/.test(d.innerText.slice(0, 40))).open`),
    ok('Capture collapses', `![...__t.modal().querySelectorAll('details.collapsible')].find((d) => /Capture/.test(d.innerText.slice(0, 30))).open`),
    ok('Virtualization comes before Capture', `(() => { const ds = [...__t.modal().querySelectorAll('details.collapsible')]; const v = ds.findIndex((d) => /Virtualization/.test(d.innerText.slice(0, 40))); const c = ds.findIndex((d) => /Capture/.test(d.innerText.slice(0, 30))); return v >= 0 && c >= 0 && v < c; })()`),
    ok('the recording help is shown once', `(__t.modal().innerText.match(/Captured traffic is stored here/g) || []).length === 1`),
    run('close Virtualization by hand, then toggle an unrelated switch', `const d = [...__t.modal().querySelectorAll('details.collapsible')].find((x) => /Virtualization/.test(x.innerText.slice(0, 40))); d.open = false; const sw = __t.modal().querySelector('.switch input, input[type=checkbox]'); if (sw) sw.click()`), sleep(200),
    ok('a hand-closed section stays closed when other controls change', `![...__t.modal().querySelectorAll('details.collapsible')].find((d) => /Virtualization/.test(d.innerText.slice(0, 40))).open`),
    run('choose Record', `__t.click('.modal .seg-btn', '^\\\\s*Record')`), sleep(300),
    ok('Capture opens again in record mode', `[...__t.modal().querySelectorAll('details.collapsible')].find((d) => /Capture/.test(d.innerText.slice(0, 30))).open`),
    ok('Virtualization closes in record mode', `![...__t.modal().querySelectorAll('details.collapsible')].find((d) => /Virtualization/.test(d.innerText.slice(0, 40))).open`),
    run('choose Virtualize and put an invalid sampling value in the collapsed section', `
      __t.click('.modal .seg-btn', '^\\\\s*Record');
      __t.set('Capture sampling', '500', __t.modal());
      __t.click('.modal .seg-btn', '^\\\\s*Virtualize')`), sleep(300),
    run('submit', `__t.set('Name', 'formcheck', __t.modal()); __t.set('Listen address', ':9444', __t.modal()); __t.click('.modal-foot button', 'Create service')`), sleep(500),
    ok('a validation error inside a collapsed section opens it', `[...__t.modal().querySelectorAll('details.collapsible')].find((d) => /Capture/.test(d.innerText.slice(0, 30))).open`),
    { key: 'Escape' }, sleep(300),

    // ===== compare-ignores-corrected-latency-and-config-keys
    run('two short runs that differ in seed, headers and compare_status', `
      const base = { recording_id: 'rec_paymentsrun3000', target: 'http://127.0.0.1:19999', mode: 'timeline', speed: 4 };
      const a = await __api('POST', '/replays', { name: 'cmp A', config: { ...base, seed: 111, compare_status: false, set_headers: { 'X-Env': 'a' } }, start: true });
      const b = await __api('POST', '/replays', { name: 'cmp B', config: { ...base, seed: 222, compare_status: true, set_headers: { 'X-Env': 'b' }, remove_headers: ['Cookie'] }, start: true });
      window.__cmp = [a.id, b.id];`),
    sleep(3500),
    run('stop both', `for (const id of window.__cmp) await __api('POST', '/replays/' + id + '/stop')`),
    run('open the comparison', `location.hash = '#/replays/compare?a=' + window.__cmp[0] + '&b=' + window.__cmp[1]`),
    waitFor('the comparison renders', `document.querySelectorAll('.chart').length >= 4`),
    ok('the diff lists the seed', `[...document.querySelectorAll('main code')].some((c) => c.textContent === 'seed')`),
    ok('the diff lists set_headers', `[...document.querySelectorAll('main code')].some((c) => c.textContent === 'set_headers')`),
    ok('the diff lists remove_headers', `[...document.querySelectorAll('main code')].some((c) => c.textContent === 'remove_headers')`),
    ok('so it does not claim identical settings', `!/identical settings/.test(document.querySelector('main').innerText)`),
    ok('p95 chart plots corrected latency too', `(() => { const l = __legend('Latency p95').map((x) => x.label); return l.includes('A corrected') && l.includes('B corrected'); })()`),
    ok('p99 chart plots corrected latency too', `(() => { const l = __legend('Latency p99').map((x) => x.label); return l.includes('A corrected') && l.includes('B corrected'); })()`),
    ok('mismatches are n/a when compare_status was off', `(() => { const r = [...document.querySelectorAll('main tbody tr')].find((x) => /^Status mismatches/.test(x.innerText)); const c = r.querySelectorAll('td'); return /n\\/a/i.test(c[1].innerText) && !/%/.test(c[3].innerText); })()`),
    run('clean up the comparison jobs', `for (const id of window.__cmp) await __api('DELETE', '/replays/' + id)`),

    // ===== replay-chart-legend-live-values
    run('a pending job and its page', `const j = await __api('POST', '/replays', { name: 'legend live', config: { recording_id: 'rec_paymentsrun3000', target: 'http://127.0.0.1:19999', mode: 'timeline', speed: 5 } }); window.__live = j.id; location.hash = '#/replays/' + j.id`),
    sleep(800),
    run('start it while the page is open', `await __api('POST', '/replays/' + window.__live + '/start')`),
    sleep(5500),
    ok('the target is known while it runs', `(() => { const t = __legend('Achieved vs target rate').find((x) => x.label === 'target'); return !!t && t.val !== '' && !/^0(\\.0)?\\s*\\/s$/.test(t.val.trim()); })()`),
    ok('the target is about the one on the card', `(() => { const num = (t) => { const m = String(t).match(/([\\d.]+)\\s*(k?)/); return m ? parseFloat(m[1]) * (m[2] ? 1000 : 1) : NaN; }; const card = ([...document.querySelectorAll('.stat-cell')].find((c) => /Achieved rate/i.test(c.innerText)) || { innerText: '' }).innerText.match(/target ([\\d.]+k?) \\/s/); const t = __legend('Achieved vs target rate').find((x) => x.label === 'target').val; return !!card && Math.abs(num(t) - num(card[1])) <= 0.15 * num(card[1]); })()`),
    run('stop it', `await __api('POST', '/replays/' + window.__live + '/stop')`),
    waitFor('the page notices it is done', `/stopped/i.test(document.querySelector('.page-title').innerText)`),
    sleep(600),
    ok('after the run the achieved legend is the run average of the card', `(() => { const card = ([...document.querySelectorAll('.stat-cell')].find((c) => /Achieved rate/i.test(c.innerText)) || { innerText: '' }).innerText.split('\\n').filter(Boolean)[1]; const a = __legend('Achieved vs target rate').find((x) => x.label === 'achieved').val; return card.replace(/\\s+/g, '') === a.replace(/\\s+/g, ''); })()`),
    ok('the target legend is not zero after the run', `(() => { const t = __legend('Achieved vs target rate').find((x) => x.label === 'target').val.trim(); return t !== '' && !/^0(\\.0)?\\s*\\/s$/.test(t); })()`),
    ok('the legend says it shows the whole run', `/whole run|run total|run average/i.test(document.querySelector('.chart-legend-note') ? document.querySelector('.chart-legend-note').textContent : '')`),
    run('delete it', `await __api('DELETE', '/replays/' + window.__live)`),
  ];
}
