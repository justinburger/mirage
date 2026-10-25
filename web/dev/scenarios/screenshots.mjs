// Regenerates the README screenshots in web/dev/screenshots (run right after ./restart-mock.sh
// so the "running" replay is still in progress).
const BASE = process.env.BASE || 'http://127.0.0.1:8485';
const OUT = new URL('../screenshots/', import.meta.url).pathname;
const shot = (name, extra = {}) => ({ screenshot: `${OUT}${name}.png`, ...extra });
const nav = (hash, wait = 1800) => [{ eval: `location.hash='${hash}'; true` }, { wait }];
const HELPERS = `window.__t = { click(sel, re, root) { const el = [...(root || document).querySelectorAll(sel)].find((b) => !re || new RegExp(re, 'i').test(b.textContent + ' ' + (b.getAttribute('aria-label') || '') + ' ' + (b.title || ''))); if (!el) throw new Error('nothing to click: ' + sel + ' /' + re + '/'); el.click(); return el; } }; true`;

export default () => [
  { media: { 'prefers-color-scheme': 'dark' } },
  { viewport: { w: 1440, h: 1180 } },
  { goto: `${BASE}/#/`, settle: 2500 },
  { eval: HELPERS },
  { wait: 9000 },
  shot('dashboard'),
  ...nav('#/services', 1500), shot('services', { clip: { x: 0, y: 0, w: 1440, h: 560 } }),
  ...nav('#/services/svc_payments0000001', 3500), shot('service-detail'),
  ...nav('#/recordings', 1500), shot('recordings', { clip: { x: 0, y: 0, w: 1440, h: 560 } }),
  ...nav('#/recordings/rec_checkoutraw00000', 1800),
  { eval: `document.querySelector('#exchanges-card tbody tr:nth-child(3)').click(); true` }, { wait: 900 },
  shot('recording-drawer'),
  { eval: `__t.click('.modal button', 'Preview anonymization'); true` }, { wait: 900 },
  shot('recording-drawer-preview'),
  { key: 'Escape' }, { wait: 300 },
  ...nav('#/replays', 1500), shot('replays', { clip: { x: 0, y: 0, w: 1440, h: 560 } }),
  ...nav('#/replays/new?recording=rec_paymentsrun3000', 1500),
  { eval: `document.querySelector('.seg-btn[data-value="rate"]').click(); true` }, { wait: 200 },
  { eval: `__t.click('.seg-btn', 'Stages'); true` }, { wait: 400 },
  shot('replay-new', { clip: { x: 0, y: 0, w: 1440, h: 1180 } }),
  ...nav('#/replays/rpl_ramp2k000000003', 3000), shot('replay-running', { clip: { x: 0, y: 0, w: 1440, h: 1180 } }),
  ...nav('#/replays/rpl_peak10x00000002', 2500), shot('replay-completed', { clip: { x: 0, y: 0, w: 1440, h: 1180 } }),
  ...nav('#/replays/compare?a=rpl_baseline1x0000001&b=rpl_peak10x00000002', 2500), shot('replay-compare'),
  ...nav('#/profiles/prof_strict0000000001', 1800),
  { eval: `__t.click('button', 'Test profile'); true` }, { wait: 1200 },
  shot('profile-editor'),
  ...nav('#/faults', 1800), shot('faults', { clip: { x: 0, y: 0, w: 1440, h: 700 } }),
  ...nav('#/services', 1200),
  { eval: `__t.click('button', '^\\\\s*New service'); true` }, { wait: 600 },
  { eval: `document.querySelector('.modal details').open = true; document.querySelector('.modal-body').scrollTop = 300; true` }, { wait: 300 },
  shot('service-form'),
  { key: 'Escape' }, { wait: 300 },
  { media: { 'prefers-color-scheme': 'light' } },
  { eval: `localStorage.removeItem('mirage_theme'); true` },
  { eval: `location.hash='#/'; location.reload(); true` }, { wait: 5500 }, shot('dashboard-light'),
];
