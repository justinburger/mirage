const BASE = 'http://127.0.0.1:8487';
const SHOTS = new URL('../screenshots', import.meta.url).pathname;
export default () => [
  { media: { 'prefers-color-scheme': 'dark' } },
  { goto: `${BASE}/#/services`, settle: 1500 },
  { waitFor: `!!document.querySelector('.modal')`, timeout: 4000 },
  { assert: `/Admin token required/.test(document.querySelector('.modal').innerText)`, message: 'token prompt appears on 401' },
  { assert: `document.getElementById('conn').classList.contains('is-unauthorized')`, message: 'indicator shows auth required' },
  { screenshot: `${SHOTS}/auth-prompt.png` },
  { insertText: 'wrong-token', into: '.modal input[type=password]' },
  { key: 'Enter' }, { wait: 1200 },
  { assert: `/not accepted/.test(document.querySelector('.modal') ? document.querySelector('.modal').innerText : '')`, message: 'wrong token is rejected and re-prompts' },
  { insertText: 's3cret', into: '.modal input[type=password]' },
  { key: 'Enter' },
  { waitFor: `!document.querySelector('.modal') && document.querySelectorAll('.table tbody tr').length >= 4`, timeout: 8000 },
  { assert: `localStorage.getItem('mirage_token') === 's3cret'`, message: 'token stored' },
  { waitFor: `document.getElementById('conn').classList.contains('is-connected')`, timeout: 8000 },
  { eval: `(async () => { const r = await fetch('/api/v1/services'); return r.status; })()`, as: 'noAuthStatus' },
  { eval: `document.querySelector('a[href*="export.har"]') ? 1 : 0`, as: 'x' },
  { eval: `location.hash='#/recordings'; true` }, { wait: 1200 },
  { eval: `(() => { const b = document.querySelector('.cell-actions .btn'); b.click(); return true; })()` }, { wait: 400 },
  { eval: `document.querySelector('.menu a') ? document.querySelector('.menu a').getAttribute('href') : 'none'`, as: 'exportHref' },
  { assert: `/access_token=s3cret/.test(document.querySelector('.menu a').getAttribute('href'))`, message: 'HAR export links carry access_token when auth is on' },
];
