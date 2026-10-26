// Audits every route at several widths for page-level horizontal overflow.
// Against the mock by default; REAL=1 audits a real server started with
// real/up.sh (BASE and REAL_DIR as in real.mjs).
import { readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';

const REAL = process.env.REAL === '1';
const BASE = process.env.BASE || (REAL ? 'http://127.0.0.1:8484' : 'http://127.0.0.1:8485');
const realIds = () => JSON.parse(readFileSync(path.join(process.env.REAL_DIR || path.join(process.env.TMPDIR || tmpdir(), 'mirage-ui-real'), 'ids.json'), 'utf8'));
const MOCK_ROUTES = ['#/', '#/services', '#/services/svc_payments0000001', '#/services/svc_catalog00000001', '#/recordings', '#/recordings/rec_paymentsrun3000', '#/replays', '#/replays/new', '#/replays/rpl_peak10x00000002', '#/replays/rpl_ramp2k000000003', '#/replays/compare?a=rpl_baseline1x0000001&b=rpl_peak10x00000002', '#/profiles', '#/profiles/prof_strict0000000001', '#/faults', '#/settings'];
function realRoutes() {
  const ids = realIds();
  const J = ids.jobs;
  return ['#/', '#/services', `#/services/${ids.service}`, `#/services/${ids.virtualService}`, '#/recordings', `#/recordings/${ids.recording}`, '#/replays', `#/replays/new?recording=${ids.recording}`,
    `#/replays/${J.saturated}`, `#/replays/${J.ramp}`, `#/replays/${J.live}`, `#/replays/compare?a=${J.timeline}&b=${J.errors}`, '#/profiles', `#/profiles/${ids.profile}`, '#/faults', '#/settings'];
}
const WIDTHS = [1440, 1280, 1024, 768, 390];
const AUDIT = `(() => {
  const vw = document.documentElement.clientWidth;
  const bad = [];
  for (const el of document.querySelectorAll('#main *')) {
    const r = el.getBoundingClientRect();
    if (r.width === 0 || r.height === 0) continue;
    if (r.right > vw + 1) {
      if (el.closest('.table-wrap') || el.closest('pre') || el.closest('.chips') || el.closest('canvas')) continue;
      bad.push(el.tagName + '.' + String(el.className).slice(0, 40) + ' right=' + Math.round(r.right));
      if (bad.length > 4) break;
    }
  }
  return JSON.stringify({ sw: document.documentElement.scrollWidth, vw, bad });
})()`;
export default () => {
  const ROUTES = REAL ? realRoutes() : MOCK_ROUTES;
  const steps = [{ media: { 'prefers-color-scheme': 'dark' } }, { goto: `${BASE}/#/`, settle: 800 }];
  for (const w of WIDTHS) {
    steps.push({ viewport: { w, h: 900, mobile: w < 700 } });
    for (const r of ROUTES) {
      steps.push({ eval: `location.hash='${r}'; true` }, { wait: 900 });
      steps.push({ eval: AUDIT, as: `${w} ${r}` });
    }
  }
  return steps;
};
