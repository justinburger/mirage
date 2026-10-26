// Visits every route of the UI against a REAL Mirage started with real/up.sh and
// reports console errors, uncaught exceptions and failed (4xx/5xx) requests.
// Also checks that no page ended in the "Something went wrong" / "Not found"
// state and that the live stream is connected.
//
//   web/dev/real/up.sh && node web/dev/run.mjs web/dev/scenarios/real.mjs
import { readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';

const BASE = process.env.BASE || 'http://127.0.0.1:8484';
const dir = process.env.REAL_DIR || path.join(process.env.TMPDIR || tmpdir(), 'mirage-ui-real');
const ids = JSON.parse(readFileSync(path.join(dir, 'ids.json'), 'utf8'));
const J = ids.jobs;

const ROUTES = [
  '#/', '#/services', `#/services/${ids.service}`, `#/services/${ids.virtualService}`,
  '#/recordings', `#/recordings/${ids.recording}`,
  '#/replays', '#/replays/new', `#/replays/new?from=${J.ramp}`, `#/replays/new?recording=${ids.recording}`,
  `#/replays/${J.timeline}`, `#/replays/${J.ramp}`, `#/replays/${J.saturated}`, `#/replays/${J.errors}`, `#/replays/${J.max}`, `#/replays/${J.live}`,
  `#/replays/compare?a=${J.timeline}&b=${J.errors}`,
  '#/profiles', `#/profiles/${ids.profile}`, '#/profiles/default', '#/profiles/new',
  '#/faults', '#/settings',
];

const CHECK = `(() => {
  const main = document.querySelector('#main');
  const text = main ? main.innerText : '';
  const bad = /Something went wrong|Page not found|Cannot reach the Mirage server/.test(text) || /^Not found/m.test(text);
  const cards = document.querySelectorAll('#main .card, #main .stat-cell').length;
  return JSON.stringify({ bad, cards, conn: window.__mirage ? window.__mirage.state.conn : '?', title: document.title });
})()`;

export default () => {
  const steps = [{ media: { 'prefers-color-scheme': 'dark' } }, { viewport: { w: 1440, h: 1000 } }, { goto: `${BASE}/#/`, settle: 1500 }];
  for (const r of ROUTES) {
    steps.push({ eval: `location.hash='${r}'; true` }, { wait: 1800 });
    steps.push({ eval: CHECK, as: r });
  }
  return steps;
};
