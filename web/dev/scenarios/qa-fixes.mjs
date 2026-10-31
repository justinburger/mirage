// Regression checks for the QA findings on the UI:
//   #9  resync after the stream reconnects, services follow outside changes
//   #10 the "all services" p95 is the slowest service's, not a weighted mean
//   #11 fault probability keeps its precision and is never rewritten untouched
//   #22 long unbroken strings wrap, '#/services/%' is not found (no URIError),
//       percentages are not shown with float noise, a service that saved but
//       could not start gets a warning toast
// Runs against the mock server (it needs the /__mock/restart and
// /__mock/rename helpers):
//
//   PORT=8485 node web/dev/mock-server.mjs
//   node web/dev/run.mjs web/dev/scenarios/qa-fixes.mjs
//
// BASE overrides the origin. The scenario removes what it creates and restores
// what it changes; the mock's "running" demo replay is not touched.
const BASE = process.env.BASE || 'http://127.0.0.1:8485';
// QA_CONTINUE=1 keeps going after a failed step, to see every check fail
// against the UI before the fixes (BASE=a mock serving the old files).
const more = process.env.QA_CONTINUE ? { continueOnError: true } : {};
const sleep = (ms) => ({ wait: ms });
const run = (name, code) => ({ name, eval: `(async()=>{ ${code} ; return true; })()`, ...more });
const ok = (name, expr) => ({ name, assert: expr, message: name, ...more });
const waitFor = (name, expr, timeout = 6000) => ({ name, waitFor: expr, timeout, ...more });
// an assertion that needs await: code is a function body returning true or false
const okAsync = (name, code) => ({ name, eval: `(async()=>{ if (!(await (async()=>{ ${code} })())) throw new Error(${JSON.stringify('assertion failed: ' + name)}); return true; })()`, ...more });
const go = (hash) => ({ name: `go ${hash}`, eval: `location.hash='${hash}'; true` });

import { HELPERS } from './helpers.mjs';

const PAGE = `
window.__api = async (method, path, body) => {
  const res = await fetch('/api/v1' + path, { method, headers: body ? { 'content-type': 'application/json' } : {}, body: body ? JSON.stringify(body) : undefined });
  const text = await res.text();
  if (!res.ok) throw new Error(method + ' ' + path + ' -> ' + res.status + ' ' + text);
  return text ? JSON.parse(text) : null;
};
window.__mock = (path) => fetch('/__mock' + path, { method: 'POST' }).then((r) => r.json());
window.__puts = [];
if (!window.__hooked) {
  window.__hooked = true;
  const real = window.fetch;
  window.fetch = (u, o) => {
    if (o && o.method === 'PUT') window.__puts.push({ url: String(u), body: JSON.parse(o.body) });
    return real(u, o);
  };
}
window.__st = () => window.__mirage.state;
window.__fmt = (name) => import('/assets/js/format.js').then((m) => m[name]);
window.__row = (re) => [...document.querySelectorAll('tbody tr')].find((r) => re.test(r.innerText));
window.__toasts = () => [...document.querySelectorAll('.toast')].map((t) => ({ kind: t.className.replace(/.*toast-(\\w+).*/, '$1'), text: t.innerText }));
window.__fits = (el) => !!el && el.scrollWidth <= el.clientWidth + 1;
`;

// 200 characters without a break opportunity, and a long path like them
const LONG = 'Q' + 'abcdefghij'.repeat(19) + 'Z';
const LONG_PATH = '/v1/' + 'segment0123456789'.repeat(14) + '/' + 'x'.repeat(60);
const HAR = JSON.stringify({ log: { entries: [
  { startedDateTime: new Date().toISOString(), time: 30, request: { method: 'POST', url: 'http://api.example.com' + LONG_PATH + '?q=' + 'verylongquery'.repeat(12), headers: [{ name: 'X-Long', value: 'v1234567890'.repeat(20) }, { name: 'Content-Type', value: 'application/json' }], postData: { mimeType: 'application/json', text: JSON.stringify({ blob: '0123456789abcdef'.repeat(30), nested: { k: 'Z'.repeat(300) } }) } },
    response: { status: 200, headers: [{ name: 'Content-Type', value: 'application/json' }], content: { mimeType: 'application/json', text: JSON.stringify({ ok: true }) } } },
  { startedDateTime: new Date(Date.now() + 1000).toISOString(), time: 12, request: { method: 'GET', url: 'http://api.example.com/plain', headers: [] }, response: { status: 404, headers: [{ name: 'Content-Type', value: 'text/plain' }], content: { mimeType: 'text/plain', text: 'plaintextwithoutspaces'.repeat(30) } } },
] } });

export default function steps() {
  return [
    { media: { 'prefers-color-scheme': 'dark' } },
    { viewport: { w: 1440, h: 1100 } },
    { goto: `${BASE}/#/replays`, settle: 1500 },
    run('install helpers', HELPERS + PAGE),
    waitFor('the stream is live', `document.getElementById('conn').classList.contains('is-connected')`, 8000),

    // ===== #9 resync: a replay the restarted server marked failed stops showing Running
    run('start a long replay', `
      const j = await __api('POST', '/replays', { name: 'qa restart victim', start: true, config: { recording_id: 'rec_paymentsrun3000', target: 'http://127.0.0.1:19999', mode: 'rate', rate: 20, duration: '30m' } });
      window.__rj = j.id; window.__svcBefore = __st().services.length;`),
    waitFor('the list knows it is running', `(__st().replays.find((r) => r.id === window.__rj) || {}).status === 'running'`),
    run('open its page', `location.hash = '#/replays/' + window.__rj`),
    waitFor('the page says Running', `/Running/i.test(document.querySelector('.page-head, main').innerText)`),
    ok('no failure banner yet', `!document.querySelector('.alert-error')`),
    run('restart the server', `window.__restart = await __mock('/restart?ms=2500')`),
    ok('the restart marked the job failed on the server (premise)', `window.__restart.failed.includes(window.__rj)`),
    waitFor('the indicator shows the outage', `document.getElementById('conn').classList.contains('is-reconnecting')`, 4000),
    waitFor('the stream comes back', `document.getElementById('conn').classList.contains('is-connected')`, 25000),
    waitFor('the shared list now has the job failed', `(__st().replays.find((r) => r.id === window.__rj) || {}).status === 'failed'`, 6000),
    waitFor('its page shows the failure', `!!document.querySelector('.alert-error') && /interrupted: mirage restarted/.test(document.querySelector('.alert-error').innerText)`, 6000),
    ok('its page no longer says Running', `!/\\bRunning\\b/.test(document.querySelector('.page-title').innerText)`),
    go('#/replays'), sleep(700),
    ok('the list row says Failed', `/Failed/.test(__row(/qa restart victim/).innerText) && !/Running/.test(__row(/qa restart victim/).innerText)`),
    ok('the sidebar counts no running replay for it', `__st().replays.filter((r) => r.status === 'running' && r.name === 'qa restart victim').length === 0`),
    run('remove the job', `await __api('DELETE', '/replays/' + window.__rj)`),

    // ----- services edited elsewhere (curl, another tab) show up
    go('#/services'), sleep(900),
    ok('orders is running and offers Disable (premise)', `/Running/.test(__row(/orders/).innerText) && !!__row(/orders/).querySelector('[aria-label="Disable service"]')`),
    run('disable it behind the UI\'s back', `await __api('POST', '/services/svc_orders00000001/disable')`),
    waitFor('the row says Disabled, not Stopped', `(() => { const r = __row(/orders/); return !!r && /Disabled/.test(r.innerText) && !!r.querySelector('[aria-label="Enable service"]'); })()`, 5000),
    run('enable it again', `await __api('POST', '/services/svc_orders00000001/enable')`),
    waitFor('the row is Running with Disable again', `(() => { const r = __row(/orders/); return !!r && /Running/.test(r.innerText) && !!r.querySelector('[aria-label="Disable service"]'); })()`, 5000),
    run('rename it behind the UI\'s back (listener restarts, like a PUT that moves it)', `
      const s = await __api('GET', '/services/svc_orders00000001'); const { status, created_at, updated_at, ...rest } = s;
      await __api('PUT', '/services/svc_orders00000001', { ...rest, name: 'orders-renamed' })`),
    waitFor('the new name is in the list', `!!__row(/orders-renamed/)`, 5000),
    run('rename it back', `
      const s = await __api('GET', '/services/svc_orders00000001'); const { status, created_at, updated_at, ...rest } = s;
      await __api('PUT', '/services/svc_orders00000001', { ...rest, name: 'orders' })`),
    waitFor('and back', `!!__row(/orders/) && !__row(/orders-renamed/)`, 5000),
    // a rename that changes nothing else in the status is announced by its
    // config_updated_at, on the list and on the detail page
    run('rename only, behind the UI\'s back, while on the list', `await __mock('/rename?id=svc_orders00000001&name=orders-live')`),
    waitFor('the list shows the new name live', `!!__row(/orders-live/)`, 5000),
    go('#/services/svc_orders00000001'), sleep(900),
    run('rename only, while on the detail page', `await __mock('/rename?id=svc_orders00000001&name=orders-live2')`),
    waitFor('the detail page shows it live', `/orders-live2/.test(document.querySelector('.page-title').innerText)`, 5000),
    run('rename back', `await __mock('/rename?id=svc_orders00000001&name=orders')`),
    waitFor('the detail page says orders again', `/^orders$/.test(document.querySelector('.page-title').firstChild.nodeValue.trim())`, 5000),
    // a rename the stream cannot announce (a server without config_updated_at)
    // is seen after a resync: returning to the tab
    run('quiet rename on the server', `await __mock('/rename?id=svc_orders00000001&name=orders-silent&quiet=1')`),
    sleep(1500),
    ok('nothing announces it: the page still says orders', `/^orders$/.test(document.querySelector('.page-title').firstChild.nodeValue.trim())`),
    run('leave the tab for more than 5 s, then come back', `
      const set = (v) => { Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => v }); document.dispatchEvent(new Event('visibilitychange')); };
      set('hidden'); await new Promise((r) => setTimeout(r, 5400)); set('visible');`),
    waitFor('the page now shows the new name', `/orders-silent/.test(document.querySelector('.page-title').innerText)`, 5000),
    ok('so does the shared list', `__st().services.some((s) => s.name === 'orders-silent')`),
    run('rename it back for good', `await __mock('/rename?id=svc_orders00000001&name=orders&quiet=1'); await __st().services.length; const { refreshServices } = await import('/assets/js/state.js'); await refreshServices()`),

    // ===== #10 the all-services p95 is the slowest service's p95
    run('the arithmetic on rows: 49% at 800 ms, 51% at 104 ms', `
      const { aggregateRows, aggregateEventPoints } = await import('/assets/js/series.js');
      const row = (rps, p95) => ({ t: 1000, rps, errors: 0, p50: p95 / 4, p95, p99: p95 * 1.2, max: p95 * 2, in_flight: 0, bytes_in: 0, bytes_out: 0 });
      const [r] = aggregateRows([[row(49, 800)], [row(51, 104)]]);
      window.__agg = { p95: r.p95, p99: r.p99, p50: r.p50, rps: r.rps };
      const pt = (req, p95) => ({ t: 1000, requests: req, errors: 0, p50: p95 / 4, p95, p99: p95 * 1.2, max: p95 * 2 });
      const e = aggregateEventPoints({ 'service:a': pt(49, 800), 'service:b': pt(51, 104), 'replay:x': pt(500, 9000) }, (n) => n.startsWith('service:'));
      window.__aggEvt = e.p95; window.__quiet = aggregateRows([[row(0, 0)], [row(0, 0)]])[0].p95`),
    ok('p95 is 800, not the request-weighted mean (445)', `window.__agg.p95 === 800 && window.__aggEvt === 800`),
    ok('p99 follows the same rule, so p95 never exceeds p99', `window.__agg.p99 >= window.__agg.p95`),
    ok('requests still add up and a quiet second is 0', `window.__agg.rps === 100 && window.__quiet === 0`),
    go('#/'), sleep(2500),
    ok('the tile says whose p95 it is', `/slowest service/i.test(([...document.querySelectorAll('.tile')].find((t) => /p95/i.test(t.querySelector('.tile-label').innerText))).querySelector('.tile-label').innerText)`),
    run('compare the tile with the live snapshots (one task: the same stats event)', `
      const fmtMs = await __fmt('fmtMs');
      const st = __st();
      const tile = [...document.querySelectorAll('.tile')].find((t) => /p95/i.test(t.querySelector('.tile-label').innerText));
      const v = st.services.filter((s) => s.status.running).map((s) => st.snapshots['service:' + s.id]).filter((sn) => sn && sn.recent && sn.recent.count > 0);
      const max = Math.max(...v.map((sn) => sn.recent.p95));
      const w = v.reduce((a, sn) => a + sn.recent.p95 * sn.recent.count, 0) / v.reduce((a, sn) => a + sn.recent.count, 0);
      window.__tile = { tile: tile.querySelector('.tile-value').textContent, max: fmtMs(max), weighted: fmtMs(w), n: v.length, hover: tile.title }`),
    ok('the tile shows the largest per-service p95', `window.__tile && window.__tile.tile === window.__tile.max`),
    ok('and not the weighted mean (the services differ, premise)', `window.__tile.n > 1 && window.__tile.max !== window.__tile.weighted`),
    ok('hovering names the slowest service', `/Slowest|slowest/.test(window.__tile.hover)`),
    ok('the chart legend says the lines are the slowest service', `(() => { const l = [...document.querySelectorAll('.chart')].find((c) => /Latency/.test(c.querySelector('.chart-title').firstChild.nodeValue)); return [...l.querySelectorAll('.legend-label')].map((x) => x.textContent).join('|') === 'p50 (avg)|p95 (slowest)|p99 (slowest)'; })()`),
    run('the chart reads the same as the services', `
      const fmtMs = await __fmt('fmtMs');
      const { getRows } = await import('/assets/js/series.js');
      const lat = [...document.querySelectorAll('.chart')].find((c) => /Latency/.test(c.querySelector('.chart-title').firstChild.nodeValue));
      const shown = [...lat.querySelectorAll('.legend-item')].find((b) => /p95/.test(b.querySelector('.legend-label').textContent)).querySelector('.legend-val').textContent;
      const rows = __st().services.filter((s) => s.status.running).map((s) => getRows('service:' + s.id)).map((r) => r[r.length - 1]);
      const t = Math.max(...rows.map((r) => r.t));
      window.__chart = { shown, max: fmtMs(Math.max(...rows.filter((r) => r.t === t).map((r) => r.p95))) }`),
    ok('the latest p95 on the chart is the largest service p95', `window.__chart.shown === window.__chart.max`),
    run('pick one service', `const sel = document.querySelector('.toolbar select'); sel.value = 'service:svc_payments0000001'; __t.fire(sel, 'change')`), sleep(1800),
    ok('a per-service tile is the plain p95', `(() => { const t = [...document.querySelectorAll('.tile')].find((x) => /p95/i.test(x.querySelector('.tile-label').innerText)); return /^p95 latency$/i.test(t.querySelector('.tile-label').innerText.trim()); })()`),
    ok('and the chart legend is plain too', `(() => { const l = [...document.querySelectorAll('.chart')].find((c) => /Latency/.test(c.querySelector('.chart-title').firstChild.nodeValue)); return [...l.querySelectorAll('.legend-label')].map((x) => x.textContent).join('|') === 'p50|p95|p99'; })()`),
    run('back to all services', `const sel = document.querySelector('.toolbar select'); sel.value = 'all'; __t.fire(sel, 'change')`), sleep(1500),
    ok('and the slowest labels are back', `/slowest/i.test(document.querySelector('.tiles').innerText)`),

    // ===== #11 fault probability keeps its precision
    run('rules with a tiny and a long-tailed probability', `
      window.__f1 = (await __api('POST', '/faults', { name: 'qa tiny', type: 'error', status: 503, probability: 0.0001 })).id;
      window.__f2 = (await __api('POST', '/faults', { name: 'qa tail', type: 'error', status: 503, probability: 0.1234567890123456 })).id;
      window.__puts.length = 0`),
    go('#/faults'), sleep(900),
    run('edit the tiny rule', `__t.click('button', 'Edit rule', __row(/qa tiny/))`), sleep(500),
    ok('the form shows 0.01, not 0', `__t.field('Probability', __t.modal()).value === '0.01'`),
    run('save without touching it', `__t.click('.modal-foot button', 'Save rule')`), sleep(900),
    ok('the saved probability is exactly the stored one', `window.__puts.length === 1 && window.__puts[0].body.probability === 0.0001`),
    ok('the table does not show 0', `/0\\.01%/.test(__row(/qa tiny/).innerText)`),
    run('edit the long-tailed rule', `window.__puts.length = 0; __t.click('button', 'Edit rule', __row(/qa tail/))`), sleep(500),
    ok('the form shows many digits', `/^12\\.3456789012/.test(__t.field('Probability', __t.modal()).value)`),
    run('save without touching it', `__t.click('.modal-foot button', 'Save rule')`), sleep(900),
    ok('nothing was rounded away', `window.__puts.length === 1 && window.__puts[0].body.probability === 0.1234567890123456`),
    run('edit the tiny rule and type a new value', `window.__puts.length = 0; __t.click('button', 'Edit rule', __row(/qa tiny/))`), sleep(500),
    run('type 0.25', `__t.set('Probability', '0.25', __t.modal())`),
    run('save', `__t.click('.modal-foot button', 'Save rule')`), sleep(900),
    ok('an edited value is saved as typed', `window.__puts.length === 1 && window.__puts[0].body.probability === 0.0025`),
    run('a preset still gives 10%', `__t.click('button', '10% 503 errors')`), sleep(500),
    ok('the preset form shows 10', `__t.field('Probability', __t.modal()).value === '10'`),
    run('create it', `window.__posts = []; __t.set('Name', 'qa preset', __t.modal()); __t.click('.modal-foot button', 'Create rule')`), sleep(900),
    okAsync('10% is saved as 0.1', `return (await __api('GET', '/faults')).find((f) => f.name === 'qa preset').probability === 0.1`),
    run('remove the rules', `for (const f of await __api('GET', '/faults')) if (/^qa /.test(f.name)) await __api('DELETE', '/faults/' + f.id)`),

    // ===== #22 long unbroken strings wrap
    run('data with long names, paths and bodies', `
      window.__lrec = (await fetch('/api/v1/recordings/import?name=' + encodeURIComponent(${JSON.stringify(LONG)}), { method: 'POST', headers: { 'content-type': 'application/json' }, body: ${JSON.stringify(HAR)} }).then((r) => r.json())).id;
      window.__lsvc = (await __api('POST', '/services', { name: 'S' + 'x'.repeat(63), listen_addr: ':9111', upstream: 'http://' + 'upstream-host-'.repeat(14) + '.internal:8080', mode: 'passthrough', enabled: true })).id;
      window.__lrpl = (await __api('POST', '/replays', { name: ${JSON.stringify(LONG)}, config: { recording_id: window.__lrec, target: 'http://127.0.0.1:19999/' + 'p'.repeat(150), mode: 'timeline', speed: 1 } })).id;
      window.__lprof = (await __api('POST', '/profiles', { name: ${JSON.stringify(LONG)}, description: 'd'.repeat(200), detectors: ['email'] })).id;
      window.__lflt = (await __api('POST', '/faults', { name: ${JSON.stringify(LONG)}, type: 'error', probability: 0.1, status: 503, path_pattern: '/' + 'pathpattern'.repeat(25) })).id;
      window.__pageFits = () => document.documentElement.scrollWidth <= document.documentElement.clientWidth;
      window.__tablesFit = () => [...document.querySelectorAll('.table-wrap')].every((w) => w.scrollWidth <= w.clientWidth + 1)`),
    go('#/recordings'), sleep(1100),
    ok('recordings: the page and its table fit at 1440', `__pageFits() && __tablesFit()`),
    ok('recordings: the 200-character name wraps in its cell', `(() => { const c = __row(/Qabcdef/).querySelector('.cell-name'); return c.getBoundingClientRect().height > 30 && __fits(c.closest('td')); })()`),
    run('open the recording', `location.hash = '#/recordings/' + window.__lrec`), sleep(1300),
    ok('the recording page fits, title included', `__pageFits() && __fits(document.querySelector('.page-title')) && __tablesFit()`),
    run('open the first exchange', `document.querySelector('#exchanges-card tbody tr').click()`), sleep(900),
    ok('the drawer does not scroll sideways', `(() => { const d = document.querySelector('.modal.drawer'); return !!d && __fits(d) && __fits(d.querySelector('.modal-body')) && __fits(d.querySelector('.modal-title')); })()`),
    ok('the long path and the long header value wrap', `(() => { const d = document.querySelector('.modal.drawer'); const bad = [...d.querySelectorAll('.xline-path, .hdrs dd')].filter((e) => !__fits(e)); return bad.length === 0 && d.querySelectorAll('.hdrs dd').length > 2; })()`),
    ok('a long body starts wrapped (and the toggle says so)', `(() => { const d = document.querySelector('.modal.drawer'); const pre = [...d.querySelectorAll('pre.code')].find((p) => /ZZZZZZ/.test(p.textContent)); return !!pre && pre.classList.contains('wrap') && __fits(pre); })()`),
    ok('a short body keeps its lines', `[...document.querySelectorAll('.modal.drawer pre.code')].some((p) => !p.classList.contains('wrap'))`),
    { key: 'Escape' }, sleep(300),
    go('#/replays'), sleep(1100),
    ok('replays: the page fits and the name cell wraps', `__pageFits() && __fits(__row(/Qabcdef/).querySelector('.cell-name').closest('td'))`),
    run('open the job', `location.hash = '#/replays/' + window.__lrpl`), sleep(1300),
    ok('the replay page fits', `__pageFits() && __fits(document.querySelector('.page-title'))`),
    go('#/profiles'), sleep(1100),
    ok('profiles: the page and table fit', `__pageFits() && __tablesFit()`),
    run('open the profile', `location.hash = '#/profiles/' + window.__lprof`), sleep(1300),
    ok('the profile page fits', `__pageFits()`),
    go('#/faults'), sleep(1100),
    ok('faults: the page and table fit', `__pageFits() && __tablesFit()`),
    go('#/services'), sleep(1100),
    ok('services: the page and table fit with a 64-character name', `__pageFits() && __tablesFit() && !!__row(/Sxxxx/)`),
    run('open the service', `location.hash = '#/services/' + window.__lsvc`), sleep(1300),
    ok('the service page fits', `__pageFits() && __fits(document.querySelector('.page-title')) && __tablesFit()`),
    go('#/'), sleep(2200),
    ok('the dashboard fits with the long service name', `__pageFits() && __tablesFit()`),
    run('delete the long data', `
      await __api('DELETE', '/faults/' + window.__lflt); await __api('DELETE', '/profiles/' + window.__lprof);
      await __api('DELETE', '/replays/' + window.__lrpl); await __api('DELETE', '/services/' + window.__lsvc); await __api('DELETE', '/recordings/' + window.__lrec)`),

    // ----- narrow screens
    { viewport: { w: 390, h: 900, mobile: true } },
    run('same data again', `
      window.__lrec = (await fetch('/api/v1/recordings/import?name=' + encodeURIComponent(${JSON.stringify(LONG)}), { method: 'POST', headers: { 'content-type': 'application/json' }, body: ${JSON.stringify(HAR)} }).then((r) => r.json())).id;
      window.__lsvc = (await __api('POST', '/services', { name: 'S' + 'x'.repeat(63), listen_addr: ':9111', upstream: 'http://' + 'upstream-host-'.repeat(14) + '.internal:8080', mode: 'passthrough', enabled: true })).id`),
    run('recording page', `location.hash = '#/recordings/' + window.__lrec`), sleep(1300),
    ok('390 px: the recording page does not scroll sideways', `__pageFits() && __fits(document.querySelector('.page-title'))`),
    run('open the first exchange', `document.querySelector('#exchanges-card tbody tr').click()`), sleep(900),
    ok('390 px: the drawer does not scroll sideways', `(() => { const d = document.querySelector('.modal.drawer'); return __pageFits() && __fits(d) && __fits(d.querySelector('.modal-body')); })()`),
    { key: 'Escape' }, sleep(300),
    run('service page', `location.hash = '#/services/' + window.__lsvc`), sleep(1300),
    ok('390 px: the service page fits', `__pageFits() && __fits(document.querySelector('.page-title'))`),
    go('#/'), sleep(2200),
    ok('390 px: the dashboard fits', `__pageFits()`),
    run('delete the long data', `await __api('DELETE', '/services/' + window.__lsvc); await __api('DELETE', '/recordings/' + window.__lrec)`),
    { viewport: { w: 1440, h: 1100 } },

    // ----- '#/services/%' is not found, not an exception
    go('#/services/%'), sleep(600),
    ok('a malformed escape shows the not-found page', `/Page not found/.test(document.querySelector('main').innerText) && /There is no page at/.test(document.querySelector('main').innerText)`),
    ok('the sidebar is still there', `!!document.querySelector('.sidebar')`),
    go('#/recordings/%E0%A4%A'), sleep(500),
    ok('another one too', `/Page not found/.test(document.querySelector('main').innerText)`),
    go('#/services/svc_payments0000001'), sleep(900),
    ok('and a good link still opens', `/payments/.test(document.querySelector('.page-title').innerText)`),

    // ----- percentages are shown without float noise and saved as they were
    run('a service with error_floor 0.07 and a tiny capture_sample', `
      const s = await __api('GET', '/services/svc_catalog00000001'); const { status, created_at, updated_at, ...rest } = s;
      window.__catalog = rest;
      await __api('PUT', '/services/svc_catalog00000001', { ...rest, error_floor: 0.07, capture_sample: 0.00001 }); window.__puts.length = 0`),
    go('#/services'), sleep(900),
    run('edit it', `__t.click('button', 'Edit service', __row(/catalog/))`), sleep(600),
    ok('error floor reads 7', `__t.field('Error floor', __t.modal()).value === '7'`),
    ok('capture sampling is not 0', `__t.field('Capture sampling', __t.modal()).value === '0.001'`),
    run('save without touching them', `__t.click('.modal-foot button', 'Save changes')`), sleep(900),
    ok('both are saved as they were', `window.__puts.length === 1 && window.__puts[0].body.error_floor === 0.07 && window.__puts[0].body.capture_sample === 0.00001`),
    go('#/services/svc_catalog00000001'), sleep(900),
    ok('the configuration says 7%, not a long float', `/Error floor\\s*7(\\.0+)?%/.test(document.querySelector('main').innerText.replace(/\\n+/g, ' '))`),
    ok('and capture sampling is not 0%', `!/Capture sampling\\s*0%/.test(document.querySelector('main').innerText.replace(/\\n+/g, ' '))`),
    run('type new values', `window.__puts.length = 0; __t.click('.page-actions button', '^\\\\s*Edit'); await new Promise((r) => setTimeout(r, 500)); __t.set('Error floor', '0.7', __t.modal()); __t.set('Capture sampling', '12.5', __t.modal()); __t.click('.modal-foot button', 'Save changes')`), sleep(900),
    ok('typed values are saved as fractions', `window.__puts.length === 1 && window.__puts[0].body.error_floor === 0.007 && window.__puts[0].body.capture_sample === 0.125`),
    run('restore the service', `await __api('PUT', '/services/svc_catalog00000001', window.__catalog)`),

    // ----- a service that saved but could not start is a warning, not "Saved"
    go('#/services'), sleep(900),
    run('create one on a port in use', `__t.click('button', '^\\\\s*New service')`), sleep(400),
    run('fill', `__t.set('Name', 'qa-clash', __t.modal()); __t.set('Listen address', ':9999', __t.modal()); __t.set('Upstream', 'http://x.internal:8080', __t.modal())`),
    run('create', `__t.click('.modal-foot button', 'Create service')`),
    waitFor('a warning toast appears', `__toasts().some((t) => t.kind === 'warn')`, 4000),
    ok('it names the service and the reason', `__toasts().some((t) => t.kind === 'warn' && /qa-clash/.test(t.text) && /address already in use/.test(t.text))`),
    ok('no green "Created"', `!__toasts().some((t) => t.kind === 'success' && /Created/.test(t.text))`),
    run('the detail page also shows the failure (premise)', `await new Promise((r) => setTimeout(r, 800))`),
    ok('the page says it could not start', `/could not start/i.test(document.querySelector('main').innerText)`),
    run('edit another service onto that port', `window.__clash = (await __api('GET', '/services')).find((s) => s.name === 'qa-clash').id; location.hash = '#/services'; await new Promise((r) => setTimeout(r, 700)); document.querySelectorAll('.toast-x').forEach((b) => b.click())`), sleep(500),
    run('open the editor of orders', `__t.click('button', 'Edit service', __row(/orders/))`), sleep(500),
    run('move it to another port in use (the mock treats 8484 like 9999)', `__t.set('Listen address', ':8484', __t.modal())`),
    run('save', `__t.click('.modal-foot button', 'Save changes')`),
    waitFor('a warning toast for the edit', `__toasts().some((t) => t.kind === 'warn' && /orders/.test(t.text))`, 4000),
    ok('it is not a green "Saved"', `!__toasts().some((t) => t.kind === 'success' && /Saved orders/.test(t.text))`),
    // The Go server keeps a running service on its old listener when the new
    // one cannot be bound: running stays true and error says why.
    run('a running service whose change could not be applied', `
      document.querySelectorAll('.toast-x').forEach((b) => b.click()); await new Promise((r) => setTimeout(r, 400));
      const { toastService } = await import('/assets/js/ui.js');
      toastService({ status: { running: true, error: 'listen tcp 127.0.0.1:8484: bind: address already in use' } }, 'Saved orders')`),
    waitFor('it warns that the old settings keep running', `__toasts().some((t) => t.kind === 'warn' && /Saved orders/.test(t.text) && /previous settings/.test(t.text) && /already in use/.test(t.text))`, 3000),
    ok('and is not green', `!__toasts().some((t) => t.kind === 'success')`),
    run('put orders back', `
      const s = await __api('GET', '/services/svc_orders00000001'); const { status, created_at, updated_at, ...rest } = s;
      await __api('PUT', '/services/svc_orders00000001', { ...rest, listen_addr: '127.0.0.1:9003' })`),
    run('a good save is still green', `document.querySelectorAll('.toast-x').forEach((b) => b.click()); await new Promise((r) => setTimeout(r, 400)); location.hash = '#/services'; await new Promise((r) => setTimeout(r, 700)); __t.click('button', 'Edit service', __row(/catalog/)); await new Promise((r) => setTimeout(r, 500)); __t.click('.modal-foot button', 'Save changes')`),
    waitFor('a success toast', `__toasts().some((t) => t.kind === 'success' && /Saved catalog/.test(t.text))`, 4000),
    ok('and no warning', `!__toasts().some((t) => t.kind === 'warn')`),
    run('enabling a service that cannot start warns too', `
      document.querySelectorAll('.toast-x').forEach((b) => b.click());
      await __api('POST', '/services/' + window.__clash + '/disable'); await new Promise((r) => setTimeout(r, 1500));
      __t.click('[aria-label="Enable service"]', '', __row(/qa-clash/))`),
    waitFor('a warning toast for the enable', `__toasts().some((t) => t.kind === 'warn' && /qa-clash/.test(t.text) && /not running/.test(t.text))`, 4000),
    run('remove the clash service', `await __api('DELETE', '/services/' + window.__clash)`),
    okAsync('the restored catalog service is as it was', `const s = await __api('GET', '/services/svc_catalog00000001'); return s.error_floor === window.__catalog.error_floor && s.name === 'catalog'`),
  ];
}
