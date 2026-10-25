// First-run experience. Needs: PORT=8486 MOCK_EMPTY=1 node web/dev/mock-server.mjs
const BASE = process.env.BASE || 'http://127.0.0.1:8486';
const OUT = new URL('../screenshots/', import.meta.url).pathname;
const nav = (hash, wait = 1000) => [{ eval: `location.hash='${hash}'; true` }, { wait }];
export default () => [
  { media: { 'prefers-color-scheme': 'dark' } },
  { goto: `${BASE}/#/`, settle: 2500 },
  { assert: `/Getting started/.test(document.querySelector('main').innerText)`, message: 'getting started panel is shown without services' },
  { screenshot: `${OUT}empty-dashboard.png`, clip: { x: 0, y: 0, w: 1440, h: 380 } },
  ...nav('#/services'), { assert: `/No services yet/.test(document.querySelector('main').innerText)`, message: 'services empty state' },
  ...nav('#/recordings'), { assert: `/No recordings yet/.test(document.querySelector('main').innerText)`, message: 'recordings empty state' },
  ...nav('#/replays'), { assert: `/No replays yet/.test(document.querySelector('main').innerText)`, message: 'replays empty state' },
  ...nav('#/faults'), { assert: `/No fault rules/.test(document.querySelector('main').innerText)`, message: 'faults empty state' },
  ...nav('#/replays/new'), { assert: `/You need a recording first/.test(document.querySelector('main').innerText)`, message: 'replay form explains the missing recording' },
  ...nav('#/replays/compare'), { assert: `/two finished replays/.test(document.querySelector('main').innerText)`, message: 'compare empty state' },
];
