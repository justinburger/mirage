// Interaction checks of the UI against a REAL Mirage (web/dev/real/up.sh):
// live service page without polling, new API fields in the forms, replay
// form modes, saturation warning, honest progress. It changes the demo
// data (service settings, a profile, two replay jobs), so run up.sh again
// to start from a clean state.
//
//   web/dev/real/up.sh && IGNORE_HTTP=1 node web/dev/run.mjs web/dev/scenarios/real-interactions.mjs
import { readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { HELPERS } from './helpers.mjs';

const BASE = process.env.BASE || 'http://127.0.0.1:8484';
const dir = process.env.REAL_DIR || path.join(process.env.TMPDIR || tmpdir(), 'mirage-ui-real');
const ids = JSON.parse(readFileSync(path.join(dir, 'ids.json'), 'utf8'));
const J = ids.jobs;
const sleep = (ms) => ({ wait: ms });
const run = (name, code) => ({ name, eval: `(async()=>{ ${code} ; return true; })()` });
const ok = (name, expr) => ({ name, assert: expr, message: name });
const waitFor = (name, expr, timeout = 8000) => ({ name, waitFor: expr, timeout });
const go = (hash) => ({ name: `go ${hash}`, eval: `location.hash='${hash}'; true` });
const api = (p) => `(await (await fetch('/api/v1${p}')).json())`;
const cellText = (label) => `([...document.querySelectorAll('.stat-cell')].find((c) => /${label}/i.test(c.innerText)) || { innerText: '' }).innerText`;
const title = `document.querySelector('.page-title').innerText`;

export default function steps() {
  const vid = ids.virtualService;
  return [
    { media: { 'prefers-color-scheme': 'dark' } },
    { viewport: { w: 1440, h: 1000 } },
    { goto: `${BASE}/#/services/${vid}`, settle: 2500 },
    run('install helpers', HELPERS),

    // ---- the service page is driven by the stream, not by polling
    run('count requests for the service so far', `window.__n0 = performance.getEntriesByType('resource').filter((r) => r.name.endsWith('/api/v1/services/${vid}')).length`),
    run('read the live counters', `window.__hits0 = ${cellText('Virtual hits')}`),
    sleep(7000),
    ok('no GET /services/{id} polling in 7 s', `performance.getEntriesByType('resource').filter((r) => r.name.endsWith('/api/v1/services/${vid}')).length === window.__n0`),
    ok('the live counters moved without a reload', `${cellText('Virtual hits')} !== window.__hits0`),
    ok('errors injected is shown for a virtual service', `/Errors injected/i.test(document.body.innerText)`),
    ok('capture cells are not shown for a virtual service', `!/Capture skipped/i.test(document.body.innerText)`),

    // ---- a mode change made elsewhere shows up within a couple of seconds
    run('switch to hybrid through the API', `await fetch('/api/v1/services/${vid}/mode', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ mode: 'hybrid', recording_id: '${ids.recording}' }) })`),
    waitFor('the page follows the mode change', `/Hybrid/.test(${title})`, 5000),
    ok('hybrid shows capture and virtual cells', `/Capture skipped/i.test(document.body.innerText) && /Virtual hits/i.test(document.body.innerText)`),
    run('switch back', `await fetch('/api/v1/services/${vid}/mode', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ mode: 'virtualize', recording_id: '${ids.recording}' }) })`),
    waitFor('the page follows again', `/Virtualize/.test(${title})`, 5000),

    // ---- editing a service keeps what the form does not show and saves the new fields
    go(`#/services/${ids.service}`), sleep(1500),
    run('open the edit form', `__t.click('.page-actions button', '^\\\\s*Edit')`), sleep(600),
    ok('the capture limit of the stored service is shown', `__t.field('Capture limit', __t.modal()).value === '268435456'`),
    run('sample a quarter of the traffic', `__t.set('Capture sampling', '25', __t.modal())`),
    run('save', `__t.click('.modal-foot button', 'Save changes')`), sleep(1200),
    run('read the service back', `window.__svc = ${api(`/services/${ids.service}`)}`),
    ok('capture_sample saved', `window.__svc.capture_sample === 0.25`),
    ok('max_capture_bytes survived the save', `window.__svc.max_capture_bytes === 268435456`),
    ok('the service still runs', `window.__svc.status.running === true`),
    ok('the detail page mentions sampling', `/25% sampled|25% of requests/.test(document.body.innerText)`),

    // ---- honest progress and the saturation warning
    go(`#/replays/${J.saturated}`), sleep(2500),
    ok('saturation warning', `/The sender saturated/.test(document.querySelector('#main').innerText)`),
    ok('progress is not rounded up to 100%', `!/\\(100%\\)/.test(document.querySelector('#main').innerText) && /of [\\d,]+ planned \\(\\d+%\\)/.test(document.querySelector('#main').innerText)`),
    ok('peak in flight is shown', `/Peak in flight/i.test(document.querySelector('#main').innerText)`),
    ok('the corrected p99 is plotted', `/p99 corrected/.test(document.querySelector('#main').innerText)`),
    go(`#/replays/${J.ramp}`), sleep(2000),
    ok('a run that sent its plan says 100%', `/\\(100%\\)/.test(document.querySelector('#main').innerText)`),
    ok('hold stages are described', `/step to/.test(document.querySelector('#main').innerText)`),

    // ---- replay form: rate mode with a stepped stage; amplify only exists in timeline mode
    go(`#/replays/new?recording=${ids.recording}`), sleep(1500),
    ok('amplify is offered in timeline mode', `!__t.field('Amplify (copies)').closest('.field').hidden`),
    run('rate mode', `__t.click('.seg-btn', '^\\\\s*Rate')`), sleep(300),
    ok('amplify is hidden in rate mode', `__t.field('Amplify (copies)').closest('.field').hidden`),
    run('use stages', `__t.click('.seg-btn', 'Stages')`), sleep(300),
    run('shape the stages', `
      const rows = document.querySelectorAll('.stage-row');
      const set = (el, v) => { el.value = v; el.dispatchEvent(new Event('input', { bubbles: true })); el.dispatchEvent(new Event('change', { bubbles: true })); };
      set(rows[0].querySelectorAll('input')[0], '2s'); set(rows[0].querySelectorAll('input')[1], '50');
      set(rows[1].querySelectorAll('input')[0], '3s'); set(rows[1].querySelectorAll('input')[1], '200');
      rows[1].querySelector('.switch-input').click();
      __t.set('Target URL', 'http://127.0.0.1:9002'); __t.set('Name', 'ui stepped ramp')`),
    run('start', `__t.click('button', 'Start replay')`),
    waitFor('on the job page', `/#\\/replays\\/rpl_/.test(location.hash) && /ui stepped ramp/.test(${title})`, 8000),
    waitFor('the job completes', `/Completed/.test(${title})`, 25000),
    run('read the job back', `window.__job = ${api(`/replays/' + location.hash.split('/').pop() + '`)}`),
    ok('the stage kept its hold flag', `window.__job.config.stages.length === 2 && window.__job.config.stages[1].hold === true && !window.__job.config.stages[0].hold`),
    ok('no amplify in a rate-mode config', `window.__job.config.amplify === undefined || window.__job.config.amplify === 1`),
    ok('the server default concurrency applies', `window.__job.config.concurrency === 2048`),
    ok('requests were sent', `window.__job.summary.sent > 100`),
    ok('a stage ends the run, and the page says so', `/ended with its last stage|sent everything|\\(100%\\)/.test(document.querySelector('#main').innerText)`),

    // ---- replay form: timeline with amplify, max gap and seed
    go(`#/replays/new?recording=${ids.recording}`), sleep(1500),
    run('timeline options', `
      __t.set('Target URL', 'http://127.0.0.1:9002'); __t.set('Name', 'ui amplified');
      __t.set('Amplify (copies)', '2'); __t.set('Cap idle gaps at', '1s'); __t.set('Stop after', '3s');
      [...document.querySelectorAll('.switch')].find((x) => /Loop the recording/.test(x.innerText)).querySelector('input').click();`),
    ok('loop is on', `[...document.querySelectorAll('.switch')].some((s) => /Loop/.test(s.innerText) && s.querySelector('input').checked)`),
    run('start', `__t.click('button', 'Start replay')`),
    waitFor('the amplified job completes', `/Completed/.test(${title})`, 25000),
    run('read it back', `window.__job2 = ${api(`/replays/' + location.hash.split('/').pop() + '`)}`),
    ok('amplify and max_gap were sent', `window.__job2.config.amplify === 2 && window.__job2.config.max_gap === '1s' && window.__job2.config.loop === true`),
    ok('the seed was stored', `window.__job2.config.seed > 0`),

    // ---- profile editor: opaque bodies
    go(`#/profiles/${ids.profile}`), sleep(1500),
    ok('opaque bodies default is shown', `__t.field('Opaque bodies').value === 'keep'`),
    run('choose fill and save', `__t.set('Opaque bodies', 'fill'); __t.click('.page-actions button', 'Save profile')`), sleep(1000),
    run('read the profile back', `window.__prof = ${api(`/profiles/${ids.profile}`)}`),
    ok('opaque_bodies saved', `window.__prof.opaque_bodies === 'fill'`),
    run('restore', `__t.set('Opaque bodies', 'keep'); __t.click('.page-actions button', 'Save profile')`), sleep(800),
  ];
}
