// End-to-end interaction checks against the mock server (see README).
// Run: node web/dev/run.mjs web/dev/scenarios/interactions.mjs
const BASE = process.env.BASE || 'http://127.0.0.1:8485';
const sleep = (ms) => ({ wait: ms });
const run = (name, code) => ({ name, eval: `(async()=>{ ${code} ; return true; })()` });
const ok = (name, expr) => ({ name, assert: expr, message: name });
const waitFor = (name, expr, timeout = 6000) => ({ name, waitFor: expr, timeout });
const go = (hash) => ({ name: `go ${hash}`, eval: `location.hash='${hash}'; true` });

import { HELPERS } from './helpers.mjs';

export default function steps() {
  return [
    { media: { 'prefers-color-scheme': 'dark' } },
    { goto: `${BASE}/#/services`, settle: 1500 },
    run('install helpers', HELPERS),

    // ---- modal basics: Escape and backdrop close
    run('open new service modal', `__t.click('button', '^\\\\s*New service')`),
    waitFor('modal is open', `!!document.querySelector('.modal')`),
    ok('focus moved into the modal', `document.querySelector('.modal').contains(document.activeElement)`),
    ok('app is inert while modal is open', `document.getElementById('app').inert === true`),
    { key: 'Escape' }, sleep(400),
    ok('Escape closes the modal', `!document.querySelector('.modal')`),
    ok('inert cleared', `document.getElementById('app').inert === false`),
    run('reopen', `__t.click('button', '^\\\\s*New service')`), sleep(300),
    { click: [8, 300] }, sleep(400),
    ok('backdrop click closes the modal', `!document.querySelector('.modal')`),

    // ---- client-side validation then server-side conflict
    run('reopen', `__t.click('button', '^\\\\s*New service')`), sleep(300),
    run('submit empty', `__t.click('.modal-foot button', 'Create service')`), sleep(300),
    ok('client validation highlights fields', `document.querySelectorAll('.modal .has-error').length >= 3`),
    run('fill conflicting service', `__t.set('Name', 'payments'); __t.set('Listen address', ':9555'); __t.set('Upstream', 'http://x.internal:8080')`),
    run('submit', `__t.click('.modal-foot button', 'Create service')`), sleep(700),
    ok('server 409 is shown inline', `!!document.querySelector('.modal .alert-error') && /already exists/.test(document.querySelector('.modal .alert-error').innerText)`),
    run('fix name', `__t.set('Name', 'inventory'); __t.set('Listen address', ':9010'); __t.set('Upstream', 'http://inv.internal:8080')`),
    run('submit', `__t.click('.modal-foot button', 'Create service')`),
    waitFor('redirected to the new service', `location.hash.startsWith('#/services/svc_')`),
    waitFor('detail page shows the name', `/inventory/.test(document.querySelector('.page-title').innerText)`),

    // ---- mode switching (record asks for a recording)
    run('switch to record', `__t.click('.seg-btn', '^\\\\s*Record')`), sleep(500),
    ok('record dialog asks for a recording', `/Switch inventory to Record/.test(__t.modal().innerText)`),
    run('confirm record', `__t.click('.modal-foot button', 'Switch to Record')`), sleep(900),
    waitFor('mode badge says Record', `/Record/.test(document.querySelector('.page-title').innerText)`),
    run('switch to passthrough', `__t.click('.seg-btn', '^\\\\s*Passthrough')`), sleep(900),
    waitFor('mode badge says Passthrough', `/Passthrough/.test(document.querySelector('.page-title').innerText)`),

    // ---- disable, then delete with confirmation
    run('disable', `__t.click('.page-actions button', '^\\\\s*Disable')`), sleep(900),
    waitFor('now disabled', `/Disabled/.test(document.querySelector('.page-title').innerText)`),
    run('delete', `__t.click('.page-actions button', '^\\\\s*Delete')`), sleep(400),
    ok('confirm dialog is shown', `/Delete inventory\\?/.test(__t.modal().innerText)`),
    { key: 'Escape' }, sleep(300),
    ok('Escape cancels the delete', `location.hash.startsWith('#/services/svc_')`),
    run('delete again', `__t.click('.page-actions button', '^\\\\s*Delete')`), sleep(300),
    run('confirm', `__t.click('.modal-foot button', 'Delete service')`),
    waitFor('back on the list', `location.hash === '#/services'`),
    ok('service is gone', `!/inventory/.test(document.querySelector('main').innerText)`),

    // ---- recording import + anonymized copy
    go('#/recordings'), sleep(800),
    run('open import', `__t.click('button', 'Import HAR')`), sleep(400),
    run('attach a HAR', `
      const har = { log: { version: '1.2', entries: [
        { startedDateTime: '2026-10-25T10:00:00Z', time: 42, request: { method: 'POST', url: 'http://shop.local/v1/customers?email=jane@acme.io', headers: [{name:'Authorization', value:'Bearer secret-token-1'}], postData: { mimeType: 'application/json', text: '{"email":"jane@acme.io","card":"4242424242424242"}' } }, response: { status: 201, headers: [{name:'Content-Type', value:'application/json'}], content: { mimeType: 'application/json', text: '{"id":7,"email":"jane@acme.io"}' } } },
        { startedDateTime: '2026-10-25T10:00:01Z', time: 12, request: { method: 'GET', url: 'http://shop.local/v1/customers/7', headers: [] }, response: { status: 503, headers: [], content: { mimeType: 'text/plain', text: 'busy' } } } ] } };
      const f = new File([JSON.stringify(har)], 'checkout-session.har', { type: 'application/json' });
      const dt = new DataTransfer(); dt.items.add(f);
      const input = document.querySelector('#har-file'); input.files = dt.files; __t.fire(input, 'change');`),
    sleep(200),
    run('import', `__t.click('.modal-foot button', '^\\\\s*Import\\\\b')`),
    waitFor('landed on the imported recording', `/#\\/recordings\\/rec_/.test(location.hash) && /checkout-session/.test(document.querySelector('.page-title').innerText)`, 8000),
    ok('imported recording has 2 exchanges', `/2\\s+exchanges|EXCHANGES\\s*\\n?2/i.test(document.querySelector('main').innerText) || document.querySelector('.stat-v').innerText.trim() === '2'`),
    run('open the first exchange', `document.querySelector('#exchanges-card tbody tr').click()`), sleep(700),
    ok('drawer is open', `!!document.querySelector('.modal.drawer')`),
    run('preview anonymization in the drawer', `__t.click('.modal button', 'Preview anonymization')`), sleep(900),
    ok('preview lists changes', `/values? changed|Changes \\(\\d+\\)/.test(document.querySelector('.modal').innerText) || /Changes \\(/.test(document.querySelector('.modal').innerText)`),
    { key: 'Escape' }, sleep(300),
    run('anonymized copy', `__t.click('.page-actions button', 'Anonymized copy')`), sleep(500),
    run('confirm copy', `__t.click('.modal-foot button', 'Create anonymized copy')`),
    waitFor('copy created', `/\\(anon\\)/.test(document.querySelector('.page-title').innerText)`, 8000),
    ok('copy is flagged anonymized', `/anonymized/.test(document.querySelector('.page-title').innerText)`),

    // ---- replay: new form, validation, start, stop
    go('#/replays/new'), sleep(900),
    run('submit without a target', `__t.click('button', 'Start replay')`), sleep(300),
    ok('target flagged', `!!__t.field('Target URL').closest('.field').classList.contains('has-error')`),
    run('rate mode with stages', `__t.click('.seg-btn', '^\\\\s*Rate')`), sleep(200),
    ok('stage editor visible after choosing stages', `(__t.click('.seg-btn', 'Stages'), true) && document.querySelectorAll('.stage-row').length >= 2`),
    run('fill and start', `__t.set('Target URL', 'http://staging.internal:8080'); __t.set('Name', 'ui test ramp'); __t.click('button', 'Start replay')`),
    waitFor('on the replay page', `/#\\/replays\\/rpl_/.test(location.hash) && /ui test ramp/.test(document.querySelector('.page-title').innerText)`, 8000),
    waitFor('job is running', `/running/i.test(document.querySelector('.page-title').innerText)`),
    sleep(2500),
    ok('live charts have data', `document.querySelectorAll('canvas').length === 4`),
    run('stop', `__t.click('.page-actions button', '^\\\\s*Stop')`),
    waitFor('job stopped', `/stopped/i.test(document.querySelector('.page-title').innerText)`),
    run('clone', `__t.click('.page-actions button', '^\\\\s*Clone')`),
    waitFor('clone is pending', `/pending/i.test(document.querySelector('.page-title').innerText) && /copy/.test(document.querySelector('.page-title').innerText)`, 6000),

    // ---- faults
    go('#/faults'), sleep(900),
    run('toggle the first rule', `const sw = document.querySelector('.table tbody tr .switch-input'); const before = sw.checked; sw.click(); window.__before = before`), sleep(700),
    ok('toggle persisted', `document.querySelector('.table tbody tr .switch-input').checked !== window.__before`),
    run('use a preset', `__t.click('.preset-card', '1% connection resets')`), sleep(400),
    run('save preset rule', `__t.click('.modal-foot button', 'Create rule')`), sleep(900),
    ok('rule appears', `/1% connection resets/.test(document.querySelector('main').innerText)`),

    // ---- profile editor and test panel
    go('#/profiles/new'), sleep(900),
    run('name it and add a rule', `__t.set('Name', 'ui test profile'); __t.click('button', 'Add rule'); __t.set('Selector', '$..customer_id', document.querySelector('.rule-card'))`),
    run('switch the test panel to a sample', `__t.click('.seg-btn', 'Paste a sample')`), sleep(200),
    run('run the test', `__t.click('button', 'Test profile')`), sleep(1000),
    ok('test result is shown', `/values? changed/.test(document.querySelector('.sticky').innerText)`),
    run('save profile', `__t.click('.page-actions button', 'Create profile')`),
    waitFor('profile saved', `/#\\/profiles\\/prof_/.test(location.hash)`),

    // ---- theme toggle persists
    run('toggle theme', `document.getElementById('theme-toggle').click()`), sleep(300),
    ok('theme attribute set', `['light','dark'].includes(document.documentElement.dataset.theme)`),
  ];
}
