const BASE = 'http://127.0.0.1:8485';
const SHOTS = new URL('../screenshots', import.meta.url).pathname;
export default () => [
  { media: { 'prefers-color-scheme': 'dark' } },
  { goto: `${BASE}/#/`, settle: 3000 },
  { eval: `(() => { const c = document.querySelectorAll('.chart-canvas')[1]; const r = c.getBoundingClientRect(); const ev = (t, x, y) => c.dispatchEvent(new PointerEvent(t, { clientX: x, clientY: y, bubbles: true })); ev('pointermove', r.left + r.width * 0.55, r.top + 60); return true; })()` },
  { wait: 400 },
  { assert: `!document.querySelectorAll('.chart-tip')[1].hidden`, message: 'tooltip visible on hover' },
  { eval: `document.querySelectorAll('.chart-tip')[1].innerText`, as: 'tip' },
  { screenshot: `${SHOTS}/chart-hover.png` },
  { eval: `(() => { const b = document.querySelectorAll('.chart')[1].querySelector('.legend-item'); b.click(); return b.getAttribute('aria-pressed'); })()`, as: 'legendToggle' },
  { assert: `document.querySelectorAll('.chart')[1].querySelector('.legend-item').getAttribute('aria-pressed') === 'false'`, message: 'legend toggles series off' },
  { eval: `document.querySelector('.window-seg [data-value="300000"]').click(); true` }, { wait: 500 },
];
