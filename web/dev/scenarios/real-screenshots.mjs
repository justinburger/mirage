// README screenshots from a REAL Mirage (web/dev/real/up.sh): dashboard,
// service, recording, replay, replay-new, profiles, faults (plus a few extras).
//
//   OUT=docs/screenshots node web/dev/run.mjs web/dev/scenarios/real-screenshots.mjs
import { readFileSync, mkdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';

const BASE = process.env.BASE || 'http://127.0.0.1:8484';
const dir = process.env.REAL_DIR || path.join(process.env.TMPDIR || tmpdir(), 'mirage-ui-real');
const ids = JSON.parse(readFileSync(path.join(dir, 'ids.json'), 'utf8'));
const OUT = path.resolve(process.env.OUT || path.join(dir, 'shots'));
mkdirSync(OUT, { recursive: true });
const shot = (name, extra = {}) => ({ screenshot: path.join(OUT, `${name}.png`), ...extra });
const nav = (hash, wait = 2200) => [{ eval: `location.hash='${hash}'; true` }, { wait }];
const HELPERS = `window.__t = { click(sel, re, root) { const el = [...(root || document).querySelectorAll(sel)].find((b) => !re || new RegExp(re, 'i').test(b.textContent + ' ' + (b.getAttribute('aria-label') || '') + ' ' + (b.title || ''))); if (!el) throw new Error('nothing to click: ' + sel + ' /' + re + '/'); el.click(); return el; } }; true`;
const J = ids.jobs;
const H = Number(process.env.HEIGHT || 1180);

export default () => [
  { media: { 'prefers-color-scheme': 'dark' } },
  { viewport: { w: 1440, h: H } },
  { goto: `${BASE}/#/`, settle: 2500 },
  { eval: HELPERS },
  { wait: 14000 }, // let the dashboard charts fill with live data
  shot('dashboard'),
  ...nav(`#/services/${ids.service}`, 9000), shot('service'),
  ...nav(`#/recordings/${ids.recording}`, 2500),
  { eval: `document.querySelector('#exchanges-card tbody tr:nth-child(3)').click(); true` }, { wait: 1000 },
  shot('recording-drawer'),
  { key: 'Escape' }, { wait: 300 },
  shot('recording'),
  ...nav(`#/replays/${J.ramp}`, 3000), shot('replay'),
  ...nav(`#/replays/${J.saturated}`, 3000), shot('replay-saturated'),
  ...nav(`#/replays/${J.live}`, 4000), shot('replay-running'),
  ...nav(`#/replays/new?recording=${ids.recording}`, 2000),
  { eval: `document.querySelector('.seg-btn[data-value="rate"]').click(); true` }, { wait: 200 },
  { eval: `__t.click('.seg-btn', 'Stages'); true` }, { wait: 500 },
  shot('replay-new'),
  ...nav(`#/profiles/${ids.profile}`, 2000),
  { eval: `__t.click('button', 'Test profile'); true` }, { wait: 1500 },
  shot('profiles'),
  ...nav('#/faults', 2500), shot('faults'),
  ...nav('#/replays', 2000), shot('replays'),
  ...nav('#/services', 2000), shot('services'),
];
