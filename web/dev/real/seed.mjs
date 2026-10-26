// Seeds a running Mirage (started by up.sh) with a virtual service, replay jobs
// of every shape and an active fault rule, using only the public API.
// Env: BASE (admin URL), REAL_DIR (where ids.json is written).
import { writeFileSync } from 'node:fs';

const BASE = process.env.BASE || 'http://127.0.0.1:8484';
const OUT = process.env.REAL_DIR ? `${process.env.REAL_DIR}/ids.json` : null;

async function call(method, path, body) {
  const res = await fetch(`${BASE}/api/v1${path}`, { method, headers: body ? { 'content-type': 'application/json' } : {}, body: body ? JSON.stringify(body) : undefined });
  const text = await res.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch { data = text; }
  if (!res.ok) throw new Error(`${method} ${path} -> ${res.status} ${text}`);
  return data;
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function waitDone(id, timeoutMs = 120000) {
  const t0 = Date.now();
  for (;;) {
    const j = await call('GET', `/replays/${id}`);
    if (['completed', 'stopped', 'failed'].includes(j.status)) return j;
    if (Date.now() - t0 > timeoutMs) throw new Error(`replay ${id} still ${j.status}`);
    await sleep(500);
  }
}

const services = await call('GET', '/services');
const demo = services.find((s) => s.name === 'demo');
const rec = (await call('GET', `/recordings/${demo.recording_id}`));
console.log(`recording ${rec.name}: ${rec.exchange_count} exchanges, ${rec.error_count} errors`);

// A virtual twin of the demo service on :9002, with errors turned up.
const virt = await call('POST', '/services', {
  name: 'demo-virtual', listen_addr: '127.0.0.1:9002', upstream: demo.upstream, mode: 'virtualize', enabled: true,
  recording_id: rec.id, anonymize_capture: false,
  match: { query: 'exact', body: 'json', path_templates: true, selection: 'sequential', miss_status: 404, ignore_params: ['_ts'] },
  latency: { mode: 'recorded', multiplier: 1, fixed: '0s', jitter: '2ms' },
  error_amplification: 3, error_floor: 0.01, timeout: '30s', max_body_bytes: 1048576,
});

// The record service also samples and caps its capture.
await call('PUT', `/services/${demo.id}`, { ...demo, capture_sample: 1, max_capture_bytes: 268435456 });

// A latency fault on the search endpoint that actually fires.
const faults = await call('GET', '/faults');
const search = faults.find((f) => f.name === 'search +200ms');
if (search) await call('PUT', `/faults/${search.id}`, { ...search, enabled: true, probability: 0.3 });

const target = 'http://127.0.0.1:9002';
const jobs = {};
async function job(key, name, config) {
  const j = await call('POST', '/replays', { name, config: { recording_id: rec.id, target, compare_status: true, ...config }, start: true });
  jobs[key] = j.id;
  return waitDone(j.id);
}
await job('timeline', 'Peak day at 8x', { mode: 'timeline', speed: 8, max_gap: '2s', loop: true, duration: '14s' });
await job('ramp', 'Ramp to 600 req/s', { mode: 'rate', stages: [{ duration: '8s', rate: 150 }, { duration: '6s', rate: 600, hold: true }, { duration: '4s', rate: 600 }], duration: '18s' });
await job('saturated', 'Too few workers (3)', { mode: 'timeline', speed: 25, concurrency: 3, loop: true, duration: '10s' });
await job('errors', 'Errors x5', { mode: 'timeline', speed: 10, error_amplification: 5, loop: true, duration: '10s' });
await job('max', 'Closed loop, 32 workers', { mode: 'max', concurrency: 32, loop: true, duration: '8s' });
// one that is still running when the screenshots are taken
const live = await call('POST', '/replays', { name: 'Soak at 120 req/s', config: { recording_id: rec.id, target, mode: 'rate', rate: 120, duration: '10m', compare_status: true }, start: true });
jobs.live = live.id;

const ids = { service: demo.id, virtualService: virt.id, recording: rec.id, jobs, profile: (await call('GET', '/profiles')).find((p) => p.name === 'strict')?.id || 'default' };
if (OUT) writeFileSync(OUT, JSON.stringify(ids, null, 2));
console.log(JSON.stringify(ids));
