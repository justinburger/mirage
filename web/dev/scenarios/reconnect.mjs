const BASE = 'http://127.0.0.1:8485';
const SHOTS = new URL('../screenshots', import.meta.url).pathname;
export default () => [
  { media: { 'prefers-color-scheme': 'dark' } },
  { goto: `${BASE}/#/`, settle: 2500 },
  { waitFor: `document.getElementById('conn').classList.contains('is-connected')`, timeout: 5000 },
  { eval: `fetch('/__mock/sse-drop?ms=6000', { method: 'POST' }).then(() => true)` },
  { waitFor: `document.getElementById('conn').classList.contains('is-reconnecting')`, timeout: 4000 },
  { waitFor: `document.getElementById('conn').classList.contains('is-connected')`, timeout: 20000 },
  { assert: `document.querySelectorAll('.chart-canvas').length === 4`, message: 'page still intact after reconnect' },
];
