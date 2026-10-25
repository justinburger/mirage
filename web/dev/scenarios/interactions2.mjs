// More interaction checks: editing, recording choice, 409 handling, scope switching, exports.
import { HELPERS } from './helpers.mjs';
const BASE = process.env.BASE || 'http://127.0.0.1:8485';
const sleep = (ms) => ({ wait: ms });
const run = (name, code) => ({ name, eval: `(async()=>{ ${code} ; return true; })()` });
const ok = (name, expr) => ({ name, assert: expr, message: name });
const waitFor = (name, expr, timeout = 6000) => ({ name, waitFor: expr, timeout });
const go = (hash) => ({ name: `go ${hash}`, eval: `location.hash='${hash}'; true` });

export default function steps() {
  return [
    { media: { 'prefers-color-scheme': 'dark' } },
    { goto: `${BASE}/#/services/svc_payments0000001`, settle: 1500 },
    run('install helpers', HELPERS),

    // ---- edit an existing service (PUT with the full object)
    run('open edit', `__t.click('.page-actions button', '^\\\\s*Edit')`), sleep(500),
    ok('edit form is pre-filled', `__t.field('Name').value === 'payments' && __t.field('Listen address').value === ':9001'`),
    ok('timeout normalised', `__t.field('Upstream timeout').value === '30s'`),
    run('change timeout', `__t.set('Upstream timeout', '45s')`),
    run('save', `__t.click('.modal-foot button', 'Save changes')`), sleep(900),
    ok('modal closed after save', `!document.querySelector('.modal')`),
    run('reopen to verify persisted', `__t.click('.page-actions button', '^\\\\s*Edit')`), sleep(500),
    ok('timeout persisted', `__t.field('Upstream timeout').value === '45s'`),
    run('invalid duration', `__t.set('Upstream timeout', '45 parsecs')`),
    run('save invalid', `__t.click('.modal-foot button', 'Save changes')`), sleep(300),
    ok('duration validation shown', `document.querySelectorAll('.modal .has-error').length >= 1`),
    { key: 'Escape' }, sleep(300),

    // ---- record mode with a brand new recording created from the service form
    go('#/services'), sleep(700),
    run('new service', `__t.click('button', '^\\\\s*New service')`), sleep(300),
    run('fill', `__t.set('Name', 'cart'); __t.set('Listen address', ':9020'); __t.set('Upstream', 'http://cart.internal:8080'); __t.click('.seg-btn', '^\\\\s*Record', __t.modal())`), sleep(300),
    run('choose new recording', `__t.click('.seg-btn', 'Create new', __t.modal())`), sleep(200),
    run('name the recording', `__t.set('New recording name', 'cart capture A')`),
    run('create', `__t.click('.modal-foot button', 'Create service')`),
    waitFor('detail opened', `location.hash.startsWith('#/services/svc_') && /cart/.test(document.querySelector('.page-title').innerText)`),
    ok('recording link shows the new recording', `/cart capture A/.test(document.querySelector('main').innerText)`),

    // ---- virtualize needs an existing recording; deleting a recording that is used is refused
    go('#/recordings'), sleep(900),
    run('delete the in-use recording', `const row = [...document.querySelectorAll('tbody tr')].find((r) => /payments run 3/.test(r.innerText)); __t.click('button', 'Delete recording', row)`), sleep(300),
    run('confirm delete', `__t.click('.modal-foot button', 'Delete recording')`), sleep(900),
    ok('server 409 is surfaced as an error toast', `[...document.querySelectorAll('.toast-error')].some((t) => /used by running service/.test(t.innerText))`),
    ok('recording still exists', `/payments run 3/.test(document.querySelector('main').innerText)`),

    // ---- profile: regenerate-secret flow and delete refusal
    go('#/profiles/prof_catalogci00000002'), sleep(900),
    run('regenerate secret', `__t.click('button', 'Regenerate secret')`), sleep(300),
    run('confirm', `__t.click('.modal-foot button', 'Regenerate on save')`), sleep(300),
    ok('pending regeneration is announced', `/will be regenerated when you save/.test(document.querySelector('main').innerText)`),
    run('save', `__t.click('.page-actions button', 'Save profile')`), sleep(900),
    ok('regeneration flag cleared after save', `!/will be regenerated/.test(document.querySelector('main').innerText)`),
    go('#/profiles'), sleep(800),
    run('try deleting the profile used by orders', `const row = [...document.querySelectorAll('tbody tr')].find((r) => /Strict/.test(r.innerText)); __t.click('button', 'Delete profile', row)`), sleep(300),
    run('confirm', `__t.click('.modal-foot button', 'Delete profile')`), sleep(900),
    ok('409 for a profile in use', `[...document.querySelectorAll('.toast-error')].some((t) => /used by service/.test(t.innerText))`),

    // ---- faults: edit and delete
    go('#/faults'), sleep(900),
    run('edit the first rule', `document.querySelector('tbody tr .cell-actions button').click()`), sleep(500),
    ok('edit form prefilled', `__t.field('Name').value.length > 0`),
    run('rename', `__t.set('Name', 'renamed by ui test')`),
    run('save', `__t.click('.modal-foot button', 'Save rule')`), sleep(900),
    ok('rename visible', `/renamed by ui test/.test(document.querySelector('main').innerText)`),
    run('delete it', `const row = [...document.querySelectorAll('tbody tr')].find((r) => /renamed by ui test/.test(r.innerText)); __t.click('button', 'Delete rule', row)`), sleep(300),
    run('confirm', `__t.click('.modal-foot button', 'Delete rule')`), sleep(900),
    ok('rule removed', `!/renamed by ui test/.test(document.querySelector('main').innerText)`),

    // ---- dashboard scope and window
    go('#/'), sleep(2500),
    run('scope: payments', `const sel = document.querySelector('.toolbar select'); sel.value = 'service:svc_payments0000001'; __t.fire(sel, 'change')`), sleep(1500),
    ok('scope is in the URL', `/scope=service/.test(location.hash)`),
    ok('tile reflects one service', `/selected collector/.test(document.querySelector('.tile').innerText)`),
    run('scope: running replay', `const sel = document.querySelector('.toolbar select'); const o = [...sel.options].find((x) => x.value.startsWith('replay:')); if (o) { sel.value = o.value; __t.fire(sel, 'change'); }`), sleep(1500),
    run('window 1h', `document.querySelector('.window-seg [data-value="3600000"]').click()`), sleep(500),
    ok('window persisted', `localStorage.getItem('mirage_dash_window') === '3600000'`),

    // ---- export HAR returns a valid HAR and an attachment header
    run('export HAR is valid', `
      const rec = (await (await fetch('/api/v1/recordings')).json())[0];
      const res = await fetch('/api/v1/recordings/' + rec.id + '/export.har?profile_id=default');
      const har = await res.json();
      window.__har = { cd: res.headers.get('content-disposition'), n: har.log.entries.length, count: rec.exchange_count };`),
    ok('HAR export is an attachment with all entries', `/attachment/.test(window.__har.cd) && window.__har.n === window.__har.count`),

    // ---- settings page
    go('#/settings'), sleep(900),
    run('save a token', `__t.set('Set token', 'abc123'); __t.click('button', 'Save token')`), sleep(300),
    ok('token stored', `localStorage.getItem('mirage_token') === 'abc123'`),
    run('clear a token', `__t.click('button', 'Clear token')`), sleep(300),
    ok('token cleared', `!localStorage.getItem('mirage_token')`),
    run('choose light theme', `__t.click('.seg-btn', '^\\\\s*Light')`), sleep(300),
    ok('light theme applied and persisted', `document.documentElement.dataset.theme === 'light' && localStorage.getItem('mirage_theme') === 'light'`),
    run('back to system', `__t.click('.seg-btn', '^\\\\s*System')`), sleep(300),
    ok('system clears the attribute', `!document.documentElement.dataset.theme`),
  ];
}
