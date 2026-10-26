#!/usr/bin/env node
// Mirage mock server: serves web/static and implements the whole management
// API from docs/API.md with stateful, realistic in-memory data so the UI can
// be developed and screenshot-tested without the Go backend.
//
//   node web/dev/mock-server.mjs            # http://127.0.0.1:8485
//   PORT=9000 MOCK_TOKEN=secret node web/dev/mock-server.mjs
//
// Env: PORT (8485), HOST (127.0.0.1), MOCK_TOKEN (enables admin-token auth),
//      MOCK_LATENCY (extra ms per API call, default 12), MOCK_QUIET=1.
import http from 'node:http';
import { readFile, stat } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { Readable } from 'node:stream';
import {
  mulberry32, hash01, smooth01, strHash, newId, newSecret, fmtGoDuration, parseGoDuration, iso,
  latencySummary, HttpError, bad, round, pick, percentile,
} from './mock/util.mjs';
import { anonymizeExchange, DETECTOR_INFO, validateProfile } from './mock/anon.mjs';
import {
  makeCustomers, generateExchanges, finalizeExchange, publicExchange, exchangeSummary, isErrorEx, endpointNames, DOMAINS_DEF,
} from './mock/data.mjs';

const here = path.dirname(fileURLToPath(import.meta.url));
const STATIC_DIR = path.resolve(here, '../static');
const PORT = Number(process.env.PORT || 8485);
const HOST = process.env.HOST || '127.0.0.1';
const TOKEN = process.env.MOCK_TOKEN || '';
const EXTRA_LATENCY = Number(process.env.MOCK_LATENCY ?? 12);
const STARTED = Date.now();
const log = (...a) => { if (!process.env.MOCK_QUIET) console.log(...a); };

// ======================================================================
// State
// ======================================================================
const db = {
  services: new Map(),
  recordings: new Map(),
  exchanges: new Map(), // recordingId -> exchange[] (with _ms/_t)
  exById: new Map(),
  profiles: new Map(),
  faults: new Map(),
  replays: new Map(),
};
let nextExchangeId = 1000;

const rngSeed = mulberry32(20261025);
const customers = makeCustomers(rngSeed, 90);

// ---- profiles ---------------------------------------------------------
const DEFAULT_PROFILE = {
  id: 'default', name: 'Default',
  description: 'Hashes credentials and session cookies and replaces common PII (emails, cards, SSNs, phone numbers, IPs, JWTs, AWS keys, IBANs) with consistent fake values.',
  detectors: ['email', 'credit_card', 'ssn', 'phone', 'ipv4', 'ipv6', 'jwt', 'aws_key', 'iban'],
  detector_strategy: 'fake',
  redact_headers: ['Authorization', 'Proxy-Authorization', 'Cookie', 'Set-Cookie', 'X-Api-Key', 'X-Auth-Token', 'X-Access-Token', 'X-Csrf-Token', 'X-Forwarded-For', 'X-Real-Ip', 'Forwarded'],
  rules: [], secret: newSecret(),
  created_at: iso(STARTED - 9 * 86400e3), updated_at: iso(STARTED - 9 * 86400e3),
};
db.profiles.set('default', DEFAULT_PROFILE);
db.profiles.set('prof_strict0000000001', {
  id: 'prof_strict0000000001', name: 'Strict (payments)',
  description: 'Everything in Default plus UUIDs, keyed customer IDs and masked card numbers. Use for data leaving the building.',
  detectors: ['email', 'credit_card', 'ssn', 'phone', 'ipv4', 'ipv6', 'jwt', 'aws_key', 'iban', 'uuid'],
  detector_strategy: 'fake',
  redact_headers: ['Authorization', 'Cookie', 'Set-Cookie', 'X-Api-Key', 'X-Forwarded-For', 'User-Agent'],
  rules: [
    { id: 'r1', name: 'customer id', enabled: true, target: 'json', selector: '$..customer', scope: 'both', strategy: 'hash' },
    { id: 'r2', name: 'card number', enabled: true, target: 'json', selector: '$.card.number', scope: 'request', strategy: 'mask', keep_suffix: 4 },
    { id: 'r3', name: 'customer names', enabled: true, target: 'json', selector: '$..name', scope: 'response', strategy: 'fake', fake: 'name' },
    { id: 'r4', name: 'account email query', enabled: true, target: 'query', selector: 'email', scope: 'request', strategy: 'fake', fake: 'email' },
    { id: 'r5', name: 'billing ssn', enabled: false, target: 'json', selector: '$..ssn', scope: 'both', strategy: 'redact', replacement: '[SSN]' },
  ],
  secret: newSecret(), created_at: iso(STARTED - 4 * 86400e3), updated_at: iso(STARTED - 3600e3),
});
db.profiles.set('prof_catalogci00000002', {
  id: 'prof_catalogci00000002', name: 'Catalog CI',
  description: 'Light touch for the public catalog: only reviewer contact details are replaced.',
  detectors: ['email', 'phone'], detector_strategy: 'hash', redact_headers: ['Authorization', 'Cookie'],
  rules: [{ id: 'r1', name: 'review author', enabled: true, target: 'json', selector: '$.reviews[*].author', scope: 'response', strategy: 'fake', fake: 'name' }],
  secret: newSecret(), created_at: iso(STARTED - 2 * 86400e3), updated_at: iso(STARTED - 2 * 86400e3),
});

function profileOut(p) {
  const { secret, ...rest } = p;
  return { ...rest, has_secret: !!secret };
}

// ---- recordings --------------------------------------------------------
function addRecording({ id, name, description = '', service_id = '', source = 'proxy', anonymized, domain, n, seed, startMs, spanMs, host, scale = 1, labels = {} }) {
  const recId = id || newId('rec');
  const raw = generateExchanges({ domain, n, seed, startMs, spanMs, host, customers, scale });
  const list = [];
  raw.forEach((r, i) => {
    let ex = finalizeExchange(r, { id: nextExchangeId++, seq: i + 1, recordingId: recId, serviceId: service_id });
    if (anonymized) {
      const { exchange } = anonymizeExchange(publicExchange(ex), DEFAULT_PROFILE);
      ex = { ...exchange, _ms: ex._ms, _t: ex._t };
    }
    list.push(ex);
    db.exById.set(ex.id, ex);
  });
  db.exchanges.set(recId, list);
  const rec = { id: recId, name, description, service_id, source, anonymized: !!anonymized, labels, created_at: iso(startMs + spanMs + 5000), updated_at: iso(startMs + spanMs + 5000) };
  db.recordings.set(recId, rec);
  recompute(rec);
  return rec;
}

function recompute(rec) {
  const list = db.exchanges.get(rec.id) || [];
  rec.exchange_count = list.length;
  rec.error_count = list.filter(isErrorEx).length;
  rec.bytes = list.reduce((s, e) => s + (e.request.body.size || 0) + (e.response.body.size || 0), 0);
  rec.first_at = list.length ? list[0].started_at : undefined;
  rec.last_at = list.length ? list[list.length - 1].started_at : undefined;
}

const NOW = Date.now();
addRecording({ id: 'rec_paymentsrun3000', name: 'payments run 3', description: 'Captured during the Tuesday load rehearsal behind the checkout service.', service_id: 'svc_payments0000001', source: 'proxy', anonymized: true, domain: 'payments', n: 720, seed: 11, startMs: NOW - 3 * 3600e3, spanMs: 14 * 60e3, host: 'localhost:9001', labels: { env: 'staging', build: '2026.10.21' } });
addRecording({ id: 'rec_catalogbase00000', name: 'catalog baseline (HAR)', description: 'Exported from Chrome DevTools while browsing the storefront.', source: 'har', anonymized: true, domain: 'catalog', n: 520, seed: 22, startMs: NOW - 26 * 3600e3, spanMs: 9 * 60e3, host: 'localhost:9002', scale: 0.8 });
addRecording({ id: 'rec_checkoutraw00000', name: 'checkout peak (raw)', description: 'Raw capture of Friday peak. Contains customer PII: anonymize before sharing.', source: 'proxy', anonymized: false, domain: 'payments', n: 410, seed: 33, startMs: NOW - 5 * 86400e3, spanMs: 6 * 60e3, host: 'localhost:9001', scale: 1.3 });
addRecording({ id: 'rec_ordershybrid0000', name: 'orders hybrid cache', description: '', service_id: 'svc_orders00000001', source: 'proxy', anonymized: true, domain: 'orders', n: 160, seed: 44, startMs: NOW - 50 * 60e3, spanMs: 6 * 60e3, host: 'localhost:9003' });

// ---- services ----------------------------------------------------------
function svcDefaults(s) {
  const out = { ...s };
  out.mode ||= 'passthrough';
  out.enabled = s.enabled ?? false;
  out.anonymize_capture = !!s.anonymize_capture;
  out.preserve_host = !!s.preserve_host;
  out.insecure_skip_verify = !!s.insecure_skip_verify;
  out.timeout = fmtGoDuration(parseGoDuration(s.timeout) || 60000);
  out.max_body_bytes = s.max_body_bytes || 1048576;
  if (s.capture_sample > 0) out.capture_sample = s.capture_sample; else delete out.capture_sample;
  if (s.max_capture_bytes > 0) out.max_capture_bytes = s.max_capture_bytes; else delete out.max_capture_bytes;
  const m = s.match || {};
  out.match = { query: m.query || 'exact', ...(m.ignore_params?.length ? { ignore_params: m.ignore_params } : {}), body: m.body || 'json', ...(m.headers?.length ? { headers: m.headers } : {}), path_templates: !!m.path_templates, selection: m.selection || 'sequential', miss_status: m.miss_status || 404 };
  const l = s.latency || {};
  out.latency = { mode: l.mode || 'recorded', multiplier: l.multiplier || 1, fixed: fmtGoDuration(parseGoDuration(l.fixed) || 0), jitter: fmtGoDuration(parseGoDuration(l.jitter) || 0) };
  out.error_amplification = s.error_amplification || 1;
  out.error_floor = s.error_floor || 0;
  return out;
}

const SERVICE_MODELS = {
  svc_payments0000001: { base: 420, p50: 28, err: 0.006, domain: 'payments', phase: 0.4 },
  svc_catalog00000001: { base: 1650, p50: 8, err: 0.002, domain: 'catalog', phase: 2.1 },
  svc_orders00000001: { base: 210, p50: 21, err: 0.009, domain: 'orders', phase: 4.0 },
};

function seedService(s) {
  const svc = svcDefaults({ created_at: iso(STARTED - 6 * 86400e3), updated_at: iso(STARTED - 3600e3), ...s });
  db.services.set(svc.id, svc);
  return svc;
}
seedService({ id: 'svc_payments0000001', name: 'payments', listen_addr: ':9001', upstream: 'https://payments.internal:8443', mode: 'record', enabled: true, recording_id: 'rec_paymentsrun3000', anonymize_capture: true, profile_id: 'default', endpoint_patterns: ['/v1/customers/{id}', '/v1/charges/{id}'], tls: { self_signed: true }, timeout: '30s' });
seedService({ id: 'svc_catalog00000001', name: 'catalog', listen_addr: ':9002', upstream: 'https://catalog.internal', mode: 'virtualize', enabled: true, recording_id: 'rec_catalogbase00000', match: { query: 'subset', ignore_params: ['_ts', 'utm_source'], body: 'json', path_templates: true, selection: 'sequential', miss_status: 404 }, latency: { mode: 'recorded', multiplier: 1, fixed: '0s', jitter: '2ms' }, error_amplification: 2, error_floor: 0.002 });
seedService({ id: 'svc_orders00000001', name: 'orders', listen_addr: '127.0.0.1:9003', upstream: 'http://orders.internal:8080', mode: 'hybrid', enabled: true, recording_id: 'rec_ordershybrid0000', anonymize_capture: true, profile_id: 'prof_strict0000000001', match: { query: 'exact', body: 'json', path_templates: true, selection: 'random', miss_status: 404 }, latency: { mode: 'fixed', multiplier: 1, fixed: '40ms', jitter: '10ms' } });
seedService({ id: 'svc_legacysoap00001', name: 'legacy-soap', listen_addr: ':9004', upstream: 'http://soap.legacy.internal:7001', mode: 'passthrough', enabled: false, preserve_host: true });

// runtime state per service
const runtime = new Map(); // id -> {startedAt, captured, dropped, hits, misses, acc, ring}
function serviceModel(svc) {
  if (SERVICE_MODELS[svc.id]) return SERVICE_MODELS[svc.id];
  const h = strHash(svc.name);
  const m = { base: 60 + (h % 700), p50: 10 + (h % 40), err: 0.003 + (h % 7) / 1000, domain: 'orders', phase: (h % 628) / 100 };
  SERVICE_MODELS[svc.id] = m;
  return m;
}
const portOf = (addr) => Number((String(addr).match(/:(\d+)$/) || [])[1] || 0);

function resolveRunning(svc) {
  if (!svc.enabled) return { running: false, error: '' };
  const port = portOf(svc.listen_addr);
  if (port === 8484 || port === PORT || port === 9999) return { running: false, error: `listen tcp ${svc.listen_addr}: bind: address already in use` };
  for (const o of db.services.values()) {
    if (o.id !== svc.id && o.enabled && portOf(o.listen_addr) === port && resolveRunning2(o)) return { running: false, error: `listen tcp ${svc.listen_addr}: bind: address already in use` };
  }
  return { running: true, error: '' };
}
const resolveRunning2 = (o) => { const p = portOf(o.listen_addr); return o.enabled && p !== 8484 && p !== 9999; };

function rt(svc) {
  let r = runtime.get(svc.id);
  if (!r) {
    r = { startedAt: Date.now(), captured: 0, dropped: 0, hits: 0, misses: 0, since: Date.now(), acc: newAcc(), ring: [] };
    runtime.set(svc.id, r);
  }
  return r;
}
function newAcc() {
  return { requests: 0, errors: 0, bytes_in: 0, bytes_out: 0, cls: { '2xx': 0, '3xx': 0, '4xx': 0, '5xx': 0, err: 0 }, wsum: 0, p50: 0, p95: 0, p99: 0, max: 0, min: 1e9, recent: [] };
}

// Like the Go proxy: once the recording holds max_capture_bytes of bodies, a
// record or hybrid service keeps running and says "capture limit reached" in
// ServiceStatus.warning (a runtime note, not an error or a start failure).
function captureLimitNote(svc, rec, running) {
  const captures = svc.mode === 'record' || svc.mode === 'hybrid';
  return running && captures && svc.max_capture_bytes > 0 && rec && rec.bytes >= svc.max_capture_bytes ? 'capture limit reached' : '';
}

function statusOf(svc) {
  const { running, error: startError } = resolveRunning(svc);
  const r = rt(svc);
  const rec = db.recordings.get(svc.recording_id);
  const error = startError;
  const warning = captureLimitNote(svc, rec, running);
  const usesIndex = svc.mode === 'virtualize' || svc.mode === 'hybrid';
  return {
    service_id: svc.id, running, addr: running ? (svc.listen_addr.startsWith(':') ? `[::]${svc.listen_addr}` : svc.listen_addr) : '', mode: svc.mode, ...(error ? { error } : {}), ...(warning ? { warning } : {}),
    ...(running ? { started_at: iso(r.startedAt) } : {}),
    captured: Math.round(r.captured), capture_dropped: r.dropped, capture_skipped: Math.round(r.captured * 0.004), virtual_hits: Math.round(r.hits), virtual_misses: Math.round(r.misses),
    virtual_injected: usesIndex ? Math.round(r.hits * Math.min(1, 0.004 * (svc.error_amplification || 1))) : 0,
    indexed_exchanges: running && usesIndex && rec ? rec.exchange_count : 0,
  };
}
const svcOut = (svc) => ({ ...svc, status: statusOf(svc) });

for (const svc of db.services.values()) {
  const r = rt(svc);
  r.startedAt = STARTED - 2 * 3600e3;
  const rec = db.recordings.get(svc.recording_id);
  if (svc.mode === 'record' && rec) r.captured = rec.exchange_count * 4;
  if (svc.mode === 'virtualize') { r.hits = 802311; r.misses = 4320; }
  if (svc.mode === 'hybrid') { r.hits = 11930; r.misses = 2231; r.captured = 2231; }
}

// ---- validation ---------------------------------------------------------
const SERVICE_KEYS = ['id', 'name', 'listen_addr', 'upstream', 'mode', 'enabled', 'recording_id', 'anonymize_capture', 'profile_id', 'endpoint_patterns', 'preserve_host', 'insecure_skip_verify', 'tls', 'timeout', 'max_body_bytes', 'capture_sample', 'max_capture_bytes', 'match', 'latency', 'error_amplification', 'error_floor', 'created_at', 'updated_at'];

function checkKeys(obj, allowed, where = '') {
  if (obj === null || typeof obj !== 'object' || Array.isArray(obj)) throw bad(`json: cannot unmarshal ${Array.isArray(obj) ? 'array' : typeof obj} into Go value${where ? ' of ' + where : ''}`);
  for (const k of Object.keys(obj)) if (!allowed.includes(k)) throw bad(`json: unknown field "${k}"`);
}

function checkDurationField(v, name, problems) {
  if (v === undefined || v === null || v === '') return;
  if (parseGoDuration(v) === null) problems.push(`invalid duration "${v}" for ${name}`);
}

function validateService(s, { skipUniq = false, selfId = '' } = {}) {
  const p = [];
  if (!/^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$/.test(s.name || '')) p.push("name must be 1-64 chars of letters, digits, '.', '_' or '-'");
  if (!/^(\[[^\]]*\]|[^:[\]]*):[^:]*$/.test(s.listen_addr || '')) p.push(`listen_addr "${s.listen_addr ?? ''}" must be host:port or :port`);
  if (!['passthrough', 'record', 'virtualize', 'hybrid'].includes(s.mode)) p.push(`mode "${s.mode}" must be passthrough, record, virtualize or hybrid`);
  const usesUpstream = s.mode !== 'virtualize';
  if (usesUpstream || s.upstream) {
    let ok = false;
    try { const u = new URL(s.upstream); ok = (u.protocol === 'http:' || u.protocol === 'https:') && !!u.host; } catch (_) { ok = false; }
    if (!ok) p.push(`upstream "${s.upstream ?? ''}" must be an absolute http(s) URL`);
  }
  if (s.mode !== 'passthrough' && !s.recording_id) p.push(`mode ${s.mode} needs a recording_id`);
  if (s.recording_id && !db.recordings.has(s.recording_id)) p.push(`recording_id "${s.recording_id}" does not exist`);
  if (s.profile_id && !db.profiles.has(s.profile_id)) p.push(`profile_id "${s.profile_id}" does not exist`);
  if (s.tls && !s.tls.self_signed && !(s.tls.cert_file && s.tls.key_file)) p.push('tls needs self_signed or both cert_file and key_file');
  if (s.max_body_bytes < -1) p.push('max_body_bytes must be -1, 0 or positive');
  if (s.capture_sample < 0 || s.capture_sample > 1) p.push('capture_sample must be between 0 and 1');
  if (s.max_capture_bytes < 0) p.push('max_capture_bytes must not be negative');
  if ((parseGoDuration(s.timeout) ?? 0) < 0) p.push('timeout must not be negative');
  if (!['exact', 'ignore', 'subset'].includes(s.match.query)) p.push(`match.query "${s.match.query}" must be exact, ignore or subset`);
  if (!['ignore', 'exact', 'json'].includes(s.match.body)) p.push(`match.body "${s.match.body}" must be ignore, exact or json`);
  if (!['sequential', 'random', 'first'].includes(s.match.selection)) p.push(`match.selection "${s.match.selection}" must be sequential, random or first`);
  if (!(s.match.miss_status >= 100 && s.match.miss_status <= 599)) p.push('match.miss_status must be a valid HTTP status');
  if (!['none', 'recorded', 'fixed'].includes(s.latency.mode)) p.push(`latency.mode "${s.latency.mode}" must be none, recorded or fixed`);
  if (s.latency.multiplier < 0) p.push('latency values must not be negative');
  if (!(s.error_amplification >= 0 && s.error_amplification <= 1000)) p.push('error_amplification must be between 0 and 1000');
  if (!(s.error_floor >= 0 && s.error_floor <= 1)) p.push('error_floor must be between 0 and 1');
  for (const pat of s.endpoint_patterns || []) if (!String(pat).startsWith('/')) p.push(`endpoint pattern "${pat}" must start with /`);
  if (p.length) throw bad('validation failed', p);
  if (!skipUniq) {
    for (const o of db.services.values()) {
      if (o.id === selfId) continue;
      if (o.name === s.name) throw new HttpError(409, `service name "${s.name}" already exists`);
      if (o.listen_addr === s.listen_addr) throw new HttpError(409, `listen address "${s.listen_addr}" is already used by service "${o.name}"`);
    }
  }
}

function parseServiceBody(body, existing) {
  checkKeys(body, SERVICE_KEYS);
  if (body.tls) checkKeys(body.tls, ['self_signed', 'cert_file', 'key_file']);
  if (body.match) checkKeys(body.match, ['query', 'ignore_params', 'body', 'headers', 'path_templates', 'selection', 'miss_status']);
  if (body.latency) checkKeys(body.latency, ['mode', 'multiplier', 'fixed', 'jitter']);
  const probs = [];
  checkDurationField(body.timeout, 'timeout', probs);
  checkDurationField(body.latency?.fixed, 'latency.fixed', probs);
  checkDurationField(body.latency?.jitter, 'latency.jitter', probs);
  if (probs.length) throw bad('validation failed', probs);
  const merged = svcDefaults({ ...body, id: body.id || existing?.id || newId('svc'), enabled: body.enabled ?? existing?.enabled ?? true, created_at: existing?.created_at || iso(Date.now()), updated_at: iso(Date.now()) });
  if (body.tls === null || body.tls === undefined) delete merged.tls;
  return merged;
}

// ---- faults --------------------------------------------------------------
function seedFault(f) { db.faults.set(f.id, { enabled: true, order: 0, probability: 1, methods: [], headers: {}, service_id: '', path_pattern: '', fired: 0, created_at: iso(STARTED - 86400e3), updated_at: iso(STARTED - 3600e3), ...f }); }
seedFault({ id: 'flt_checkout503000001', name: '10% 503s on checkout', service_id: 'svc_payments0000001', methods: ['POST'], path_pattern: '/v1/charges/**', probability: 0.1, type: 'error', status: 503, body: '{"error":"overloaded"}', response_headers: { 'Retry-After': '1' }, fired: 18324 });
seedFault({ id: 'flt_slowall00000002', name: '+250ms latency on everything', enabled: false, order: 1, probability: 1, type: 'latency', delay: '250ms', jitter: '50ms', fired: 0 });
seedFault({ id: 'flt_hungcatalog00003', name: '1% hung catalog requests', order: 2, service_id: 'svc_catalog00000001', path_pattern: 're:^/products/\\d+$', probability: 0.01, type: 'timeout', delay: '30s', fired: 214, enabled: true });
seedFault({ id: 'flt_throttle000000004', name: 'Throttle receipts to 50 KB/s', enabled: false, order: 3, service_id: 'svc_payments0000001', methods: ['GET'], path_pattern: '/v1/receipts/*', probability: 1, type: 'throttle', bytes_per_second: 51200, fired: 0 });

const FAULT_KEYS = ['id', 'name', 'enabled', 'order', 'service_id', 'methods', 'path_pattern', 'headers', 'probability', 'type', 'delay', 'jitter', 'status', 'body', 'response_headers', 'bytes_per_second', 'created_at', 'updated_at', 'fired'];
function parseFault(body, existing) {
  checkKeys(body, FAULT_KEYS.filter((k) => k !== 'fired'));
  const probs = [];
  checkDurationField(body.delay, 'delay', probs);
  checkDurationField(body.jitter, 'jitter', probs);
  if (probs.length) throw bad('validation failed', probs);
  const f = { ...body };
  f.id = body.id || existing?.id || newId('flt');
  f.enabled = body.enabled ?? false;
  f.order = body.order ?? 0;
  f.probability = body.probability ?? 0;
  if (f.type === 'error' && !f.status) f.status = 503;
  if (f.type === 'timeout' && !parseGoDuration(f.delay)) f.delay = '1m0s';
  for (const k of ['delay', 'jitter']) if (f[k] !== undefined) f[k] = fmtGoDuration(parseGoDuration(f[k]) || 0);
  const p = [];
  if (!String(f.name || '').trim()) p.push('name is required');
  if (f.probability < 0 || f.probability > 1) p.push('probability must be between 0 and 1');
  const d = parseGoDuration(f.delay) || 0, j = parseGoDuration(f.jitter) || 0;
  if (f.type === 'latency' && d <= 0 && j <= 0) p.push('latency fault needs delay or jitter');
  else if (f.type === 'error' && !(f.status >= 100 && f.status <= 599)) p.push('status must be a valid HTTP status');
  else if (f.type === 'timeout' && d <= 0) p.push('timeout fault needs a positive delay');
  else if (f.type === 'throttle' && !(f.bytes_per_second > 0)) p.push('throttle fault needs bytes_per_second > 0');
  else if (!['latency', 'error', 'abort', 'timeout', 'throttle'].includes(f.type)) p.push(`type "${f.type}" must be latency, error, abort, timeout or throttle`);
  if (f.path_pattern?.startsWith('re:')) { try { new RegExp(f.path_pattern.slice(3)); } catch (e) { p.push(`path_pattern regex: ${e.message}`); } } else if (f.path_pattern && !f.path_pattern.startsWith('/')) p.push('path_pattern must start with / (or re:)');
  if (f.service_id && !db.services.has(f.service_id)) p.push(`service_id "${f.service_id}" does not exist`);
  if (p.length) throw bad('validation failed', p);
  f.created_at = existing?.created_at || iso(Date.now());
  f.updated_at = iso(Date.now());
  f.fired = existing?.fired || 0;
  return f;
}
const faultOut = (f) => ({ ...f });

// ======================================================================
// Metrics simulation
// ======================================================================
function faultEffects(svc) {
  let errP = 0, extraMs = 0, throttled = false;
  for (const f of db.faults.values()) {
    if (!f.enabled) continue;
    if (f.service_id && f.service_id !== svc.id) continue;
    const share = f.path_pattern || (f.methods && f.methods.length) ? 0.5 : 1;
    if (f.type === 'error' || f.type === 'abort' || f.type === 'timeout') errP += f.probability * share;
    if (f.type === 'latency') extraMs += (parseGoDuration(f.delay) || 0) * f.probability * share;
    if (f.type === 'throttle') throttled = true;
  }
  return { errP: Math.min(1, errP), extraMs, throttled };
}

function servicePoint(svc, tMs) {
  const m = serviceModel(svc);
  const ts = Math.floor(tMs / 1000);
  const wave = 1 + 0.3 * Math.sin(ts / 53 + m.phase) + 0.1 * Math.sin(ts / 11 + m.phase * 2);
  const noise = 0.95 + 0.1 * hash01(ts, Math.round(m.phase * 100));
  const rps = Math.max(0, Math.round(m.base * wave * noise));
  const fx = faultEffects(svc);
  let p50 = m.p50 * (1 + 0.16 * Math.sin(ts / 37 + m.phase)) * (0.9 + 0.2 * smooth01(ts, 7, 5) + 0.04 * hash01(ts, 70)) * (1 + Math.max(0, wave - 1.2) * 0.9);
  if (svc.mode === 'virtualize' || svc.mode === 'hybrid') {
    const lat = svc.latency;
    if (lat.mode === 'none') p50 = 0.25;
    else if (lat.mode === 'fixed') p50 = (parseGoDuration(lat.fixed) || 0) + 0.3;
    else p50 = p50 * (lat.multiplier || 1);
  }
  p50 += fx.extraMs;
  const p95 = p50 * (2.6 + 0.9 * smooth01(ts, 8, 7) + 0.1 * hash01(ts, 80)) + fx.extraMs * 0.2;
  const p99 = p95 * (1.7 + 0.6 * smooth01(ts, 9, 9) + 0.1 * hash01(ts, 90));
  const max = p99 * (1.3 + 1.6 * hash01(ts, 10));
  let errRate = m.err + (Math.sin(ts / 260 + m.phase) > 0.96 ? 0.025 : 0);
  if (svc.mode === 'virtualize' || svc.mode === 'hybrid') errRate = Math.min(1, Math.max(errRate * svc.error_amplification, svc.error_floor));
  errRate = Math.min(1, errRate + fx.errP);
  const errors = Math.min(rps, Math.round(rps * errRate * (0.6 + 0.8 * hash01(ts, 11))));
  return {
    t: ts * 1000, requests: rps, errors,
    p50: round(p50, 2), p95: round(p95, 2), p99: round(p99, 2), max: round(max, 2),
    in_flight: Math.round(rps * (p50 / 1000) * 1.4 + 1 + 3 * hash01(ts, 12)),
    bytes_in: rps * (300 + Math.round(200 * hash01(ts, 13))), bytes_out: rps * (fx.throttled ? 820 : 2300 + Math.round(900 * hash01(ts, 14))),
  };
}

function ingestServicePoint(svc, pt, live = false) {
  const r = rt(svc);
  const a = r.acc;
  a.requests += pt.requests; a.errors += pt.errors; a.bytes_in += pt.bytes_in; a.bytes_out += pt.bytes_out;
  const e5 = Math.round(pt.errors * 0.85), eN = pt.errors - e5;
  a.cls['5xx'] += e5; a.cls['4xx'] += Math.round((pt.requests - e5) * 0.012); a.cls.err += eN;
  a.cls['3xx'] += Math.round(pt.requests * 0.01);
  a.cls['2xx'] += Math.max(0, pt.requests - e5 - eN - Math.round((pt.requests - e5) * 0.012) - Math.round(pt.requests * 0.01));
  a.wsum += pt.requests; a.p50 += pt.p50 * pt.requests; a.p95 += pt.p95 * pt.requests; a.p99 += pt.p99 * pt.requests;
  a.max = Math.max(a.max, pt.max); a.min = Math.min(a.min, pt.p50 * 0.2);
  a.recent.push(pt); if (a.recent.length > 10) a.recent.shift();
  r.ring.push(pt); if (r.ring.length > 3700) r.ring.shift();
  if (svc.mode === 'record' || svc.mode === 'hybrid') {
    const share = svc.mode === 'hybrid' ? 0.15 : 1;
    r.captured += pt.requests * share;
    const rec = db.recordings.get(svc.recording_id);
    if (live && rec && hash01(Math.floor(pt.t / 1000), 5) < 0.3) growRecording(rec, svc);
  }
  if (svc.mode === 'virtualize') { r.hits += pt.requests * 0.995; r.misses += pt.requests * 0.005; }
  if (svc.mode === 'hybrid') { r.hits += pt.requests * 0.85; r.misses += pt.requests * 0.15; }
  for (const f of db.faults.values()) {
    if (f.enabled && (!f.service_id || f.service_id === svc.id)) f.fired += Math.round(pt.requests * f.probability * ((f.path_pattern || f.methods?.length) ? 0.5 : 1));
  }
}

function growRecording(rec, svc) {
  const list = db.exchanges.get(rec.id);
  if (!list || list.length > 4000) return;
  const m = serviceModel(svc);
  const raw = generateExchanges({ domain: m.domain, n: 1, seed: Math.floor(Math.random() * 1e9), startMs: Date.now(), spanMs: 1, host: `localhost:${portOf(svc.listen_addr)}`, customers, includeTimeout: false })[0];
  raw.started_at = Date.now();
  let ex = finalizeExchange(raw, { id: nextExchangeId++, seq: list.length + 1, recordingId: rec.id, serviceId: svc.id });
  if (svc.anonymize_capture) {
    const prof = db.profiles.get(svc.profile_id || 'default') || DEFAULT_PROFILE;
    ex = { ...anonymizeExchange(publicExchange(ex), prof).exchange, _ms: ex._ms, _t: ex._t };
  }
  list.push(ex);
  db.exById.set(ex.id, ex);
  recompute(rec);
}

// backfill an hour of history for the seeded services
for (const svc of db.services.values()) {
  if (!svc.enabled) continue;
  const r = rt(svc);
  const end = Math.floor(NOW / 1000) * 1000;
  for (let t = end - 3600e3; t < end; t += 1000) ingestServicePoint(svc, servicePoint(svc, t));
}

function endpointShares(svc) {
  const m = serviceModel(svc);
  const defs = DOMAINS_DEF[m.domain];
  const total = defs.reduce((s, d) => s + d.w, 0);
  return defs.map((d) => ({ endpoint: `${d.method} ${d.tmpl}`, w: d.w / total, ms: d.ms, err: /POST|PUT/.test(d.method) ? 1.8 : 0.6 }));
}

function aggSummary(sumP50, sumP95, sumP99, wsum, max, min, count) {
  if (!wsum) return { count, min: 0, mean: 0, p50: 0, p90: 0, p95: 0, p99: 0, p999: 0, max: 0 };
  const p50 = sumP50 / wsum, p95 = sumP95 / wsum, p99 = sumP99 / wsum;
  return { count, min: round(min, 2), mean: round(p50 * 1.35, 2), p50: round(p50, 2), p90: round(p50 + (p95 - p50) * 0.62, 2), p95: round(p95, 2), p99: round(p99, 2), p999: round(p99 * 1.7, 2), max: round(max, 2) };
}

function serviceSnapshot(svc) {
  const r = rt(svc);
  const a = r.acc;
  const last = a.recent[a.recent.length - 1];
  const rc = a.recent;
  const rcReq = rc.reduce((s, p) => s + p.requests, 0);
  const rcErr = rc.reduce((s, p) => s + p.errors, 0);
  const wr = (k) => rc.reduce((s, p) => s + p[k] * p.requests, 0);
  const shares = endpointShares(svc);
  return {
    name: `service:${svc.id}`, kind: 'service', id: svc.id, since: iso(r.since),
    requests: a.requests, errors: a.errors, bytes_in: a.bytes_in, bytes_out: a.bytes_out,
    in_flight: last ? last.in_flight : 0, status_classes: { ...a.cls },
    rps: last ? last.requests : 0, rps10: rc.length ? round(rcReq / rc.length, 1) : 0, error_rate10: rcReq ? round(rcErr / rcReq, 5) : 0,
    latency: aggSummary(a.p50, a.p95, a.p99, a.wsum, a.max, a.min === 1e9 ? 0 : a.min, a.requests),
    recent: aggSummary(wr('p50'), wr('p95'), wr('p99'), rcReq, Math.max(0, ...rc.map((p) => p.max)), rc.length ? Math.min(...rc.map((p) => p.p50 * 0.2)) : 0, rcReq),
    endpoints: shares.map((s) => {
      const count = Math.round(a.requests * s.w);
      const k = 0.85 + 0.3 * hash01(strHash(s.endpoint), 3);
      const errs = Math.round(a.errors * s.w * s.err * 0.7);
      const base = s.ms * (svc.mode === 'virtualize' ? 0.5 : 1);
      return { endpoint: s.endpoint, count, errors: errs, latency: { count, min: round(base * 0.3, 2), mean: round(base * 1.2 * k, 2), p50: round(base * k, 2), p90: round(base * 2.4 * k, 2), p95: round(base * 3.1 * k, 2), p99: round(base * 7 * k, 2), p999: round(base * 12 * k, 2), max: round(base * 24 * k, 2) } };
    }).sort((x, y) => y.count - x.count),
  };
}

// ======================================================================
// Replay jobs
// ======================================================================
function replayPlan(cfg, rec) {
  const n = rec ? rec.exchange_count : 1000;
  const spanSec = rec && rec.first_at && rec.last_at ? Math.max(5, (Date.parse(rec.last_at) - Date.parse(rec.first_at)) / 1000) : 300;
  const amp = cfg.amplify || 1;
  const cap = 420;
  let durationSec; let at;
  if (cfg.mode === 'rate') {
    const stages = (cfg.stages || []).map((s) => ({ d: (parseGoDuration(s.duration) || 0) / 1000, rate: s.rate, hold: !!s.hold }));
    if (stages.length) {
      durationSec = Math.min(cap, stages.reduce((s, x) => s + x.d, 0));
      at = (t) => { let acc = 0, prev = 0; for (const s of stages) { if (t <= acc + s.d) return s.hold ? s.rate : prev + (s.rate - prev) * ((t - acc) / s.d); acc += s.d; prev = s.rate; } return prev; };
    } else {
      durationSec = Math.min(cap, (parseGoDuration(cfg.duration) || 60000) / 1000);
      at = () => cfg.rate;
    }
  } else if (cfg.mode === 'max') {
    durationSec = Math.min(cap, (parseGoDuration(cfg.duration) || 45000) / 1000);
    const conc = cfg.concurrency || 64;
    at = (t) => Math.min(conc * 8, 2600) * (0.95 + 0.05 * Math.sin(t / 5));
  } else {
    const avg = (n / spanSec) * (cfg.speed || 1) * amp * 60; // mock recordings are small samples of real traffic
    durationSec = Math.min(cap, Math.max(20, spanSec / (cfg.speed || 1)));
    const d = (parseGoDuration(cfg.duration) || 0) / 1000;
    if (d) durationSec = Math.min(cap, d);
    at = (t) => avg * (1 + 0.35 * Math.sin(t / 9) + 0.12 * Math.sin(t / 3.1));
  }
  let planned = 0;
  for (let t = 0; t < durationSec; t++) planned += at(t + 0.5);
  planned = Math.round(planned);
  if (cfg.max_requests > 0) planned = Math.min(planned, cfg.max_requests);
  if (cfg.loop && !cfg.max_requests) planned = -1;
  return { durationSec, at, planned, avg: durationSec ? Math.round(Array.from({ length: Math.floor(durationSec) }, (_, i) => at(i + 0.5)).reduce((s, x) => s + x, 0) / durationSec) : 0 };
}

function newSim() {
  return { sent: 0, completed: 0, failed: 0, timeouts: 0, errors: 0, mismatches: 0, bytes_sent: 0, bytes_received: 0, wsum: 0, p50: 0, p95: 0, p99: 0, c50: 0, c95: 0, c99: 0, max: 0, cmax: 0, maxLag: 0, status: {}, targetSum: 0, secs: 0, min: 1e9 };
}

function replayPoint(job, plan, sec, tMs) {
  const cfg = job.config;
  const target = plan.at(sec + 0.5);
  const cap = (/peak/i.test(job.name) ? 1100 : 1850) + 200 * hash01(strHash(job.id), 1);
  const load = target / cap;
  const achieved = Math.max(0, Math.round(Math.min(target, cap * (0.98 + 0.03 * hash01(sec, 2))) * (0.98 + 0.03 * hash01(sec, 3))));
  const base = 24 * (cfg.amplify > 1 ? 1.1 : 1);
  const sat = Math.max(0, load - 0.8);
  const p50 = base * (1 + 0.15 * hash01(sec, 4)) * (1 + sat * 6);
  const p95 = p50 * (2.8 + sat * 12 + hash01(sec, 5));
  const p99 = p95 * (1.8 + sat * 6 + hash01(sec, 6) * 0.6);
  const errRate = 0.004 * (cfg.error_amplification || 1) + Math.max(0, load - 0.92) * 0.5;
  const errors = Math.min(achieved, Math.round(achieved * Math.min(0.9, errRate) * (0.7 + 0.6 * hash01(sec, 7))));
  const inflight = Math.min(cfg.concurrency || (cfg.mode === 'max' ? 64 : 2048), Math.round(achieved * (p50 / 1000) * 1.5 + 2));
  const lag = sat * 900 * (0.6 + hash01(sec, 8));
  return { t: tMs, requests: achieved, errors, p50: round(p50, 2), p95: round(p95, 2), p99: round(p99, 2), max: round(p99 * 2.1, 2), in_flight: inflight,
    corrected_p95: round(p95 + lag * 1.8, 2), corrected_p99: round(p99 + lag * 4.5, 2), bytes_in: achieved * 2400, bytes_out: achieved * 420, _target: target, _sat: sat };
}

function foldSim(job, pt) {
  const s = job._sim;
  s.secs += 1;
  s.maxInFlight = Math.max(s.maxInFlight || 0, pt.in_flight);
  s.targetSum += pt._target;
  s.sent += Math.round(pt.requests * 1.01);
  s.completed += pt.requests;
  const timeouts = Math.round(pt.errors * 0.08);
  s.failed += timeouts + Math.round(pt.errors * 0.04); s.timeouts += timeouts;
  s.errors += pt.errors;
  s.mismatches += Math.round(pt.requests * (job.config.compare_status ? 0.0015 : 0));
  s.bytes_sent += pt.bytes_out; s.bytes_received += pt.bytes_in;
  const w = pt.requests;
  s.wsum += w; s.p50 += pt.p50 * w; s.p95 += pt.p95 * w; s.p99 += pt.p99 * w;
  const lag = pt._sat * 900 * (0.6 + hash01(s.secs, 8));
  s.c50 += (pt.p50 + lag * 0.3) * w; s.c95 += (pt.p95 + lag * 1.8) * w; s.c99 += (pt.p99 + lag * 4.5) * w;
  s.max = Math.max(s.max, pt.max); s.cmax = Math.max(s.cmax, pt.max + lag * 8); s.maxLag = Math.max(s.maxLag, lag * 1.2 + 0.4);
  s.min = Math.min(s.min, pt.p50 * 0.25);
  const errN = pt.errors;
  const add = (k, v) => { s.status[k] = (s.status[k] || 0) + v; };
  add('503', Math.round(errN * 0.7)); add('500', Math.round(errN * 0.15)); add('429', errN - Math.round(errN * 0.7) - Math.round(errN * 0.15));
  add('404', Math.round(pt.requests * 0.012)); add('201', Math.round(pt.requests * 0.18));
  add('200', Math.max(0, pt.requests - errN - Math.round(pt.requests * 0.012) - Math.round(pt.requests * 0.18)));
}

function replaySummary(job) {
  const s = job._sim;
  const plan = job._plan;
  const rec = db.recordings.get(job.config.recording_id);
  const L = aggSummary(s.p50, s.p95, s.p99, s.wsum, s.max, s.min === 1e9 ? 0 : s.min, s.completed);
  const C = aggSummary(s.c50, s.c95, s.c99, s.wsum, s.cmax, s.min === 1e9 ? 0 : s.min, s.completed);
  const shares = serviceEndpointShares(rec);
  const elapsed = s.secs;
  const statusCounts = {};
  for (const [k, v] of Object.entries(s.status)) if (v > 0) statusCounts[k] = v;
  return {
    planned: plan.planned, sent: s.sent, completed: s.completed, failed: s.failed, timeouts: s.timeouts, errors: s.errors, mismatches: s.mismatches,
    status_counts: statusCounts, latency: L, corrected_latency: C,
    // Like the Go engine: unknown (0) until the first second has been scheduled.
    target_rps: job.config.mode === 'max' || !elapsed ? 0 : round(s.targetSum / elapsed, 1),
    achieved_rps: round(elapsed ? s.completed / elapsed : 0, 1),
    error_rate: s.completed ? round(s.errors / s.completed, 5) : 0,
    bytes_sent: s.bytes_sent, bytes_received: s.bytes_received,
    elapsed: fmtGoDuration(elapsed * 1000), max_lag: fmtGoDuration(s.maxLag),
    max_in_flight: s.maxInFlight || 0, saturated: (s.maxInFlight || 0) >= (job.config.concurrency || (job.config.mode === 'max' ? 64 : 2048)),
    endpoints: shares.map((e) => {
      const count = Math.round(s.completed * e.w);
      const k = 0.85 + 0.3 * hash01(strHash(e.endpoint), 4);
      return { endpoint: e.endpoint, count, errors: Math.round(s.errors * e.w * e.err), latency: { count, min: round(L.min, 2), mean: round(L.mean * e.ms / 24 * k, 2), p50: round(L.p50 * e.ms / 24 * k, 2), p90: round(L.p90 * e.ms / 24 * k, 2), p95: round(L.p95 * e.ms / 24 * k, 2), p99: round(L.p99 * e.ms / 24 * k, 2), p999: round(L.p999 * e.ms / 24 * k, 2), max: round(L.max * e.ms / 24 * k, 2) } };
    }).sort((a, b) => b.count - a.count),
  };
}

function serviceEndpointShares(rec) {
  const dom = rec && (rec.name.includes('catalog') ? 'catalog' : rec.name.includes('orders') ? 'orders' : 'payments') || 'payments';
  const defs = DOMAINS_DEF[dom];
  const total = defs.reduce((s, d) => s + d.w, 0);
  return defs.map((d) => ({ endpoint: `${d.method} ${d.tmpl}`, w: d.w / total, ms: d.ms, err: /POST|PUT/.test(d.method) ? 1.8 : 0.6 }));
}

function initJob(job) {
  const rec = db.recordings.get(job.config.recording_id);
  job._plan = replayPlan(job.config, rec);
  job._sim = newSim();
  job.series = [];
}

function finishJob(job, status = 'completed') {
  job.status = status;
  job.ended_at = iso(Date.now());
  job.summary = replaySummary(job);
}

function seedReplay({ id, name, config, status, startedAgoSec, durationSec, error }) {
  const job = { id, name, config, status: 'pending', error: error || '', created_at: iso(NOW - (startedAgoSec + 30) * 1000), started_at: null, ended_at: null };
  initJob(job);
  const startMs = NOW - startedAgoSec * 1000;
  job.started_at = iso(startMs);
  const run = Math.min(job._plan.durationSec, durationSec ?? (status === 'running' ? startedAgoSec : job._plan.durationSec));
  for (let sec = 0; sec < Math.floor(run); sec++) {
    const pt = replayPoint(job, job._plan, sec, Math.floor(startMs / 1000) * 1000 + sec * 1000);
    foldSim(job, pt);
    job.series.push(pt);
  }
  if (status === 'completed') { finishJob(job, 'completed'); job.ended_at = iso(startMs + run * 1000); }
  else if (status === 'running') { job.status = 'running'; job.summary = replaySummary(job); }
  else if (status === 'failed') { job.status = 'failed'; job.ended_at = iso(startMs + 2000); job.summary = replaySummary(job); }
  else if (status === 'stopped') { finishJob(job, 'stopped'); job.ended_at = iso(startMs + run * 1000); }
  db.replays.set(job.id, job);
  return job;
}

seedReplay({ id: 'rpl_baseline1x0000001', name: '1x checkout baseline', status: 'completed', startedAgoSec: 5400, config: { recording_id: 'rec_paymentsrun3000', target: 'http://staging.internal:8080', mode: 'timeline', speed: 1, amplify: 1, error_amplification: 1, concurrency: 256, loop: false, timeout: '30s', compare_status: true, set_headers: { Authorization: 'Bearer staging-token' } } });
seedReplay({ id: 'rpl_peak10x00000002', name: '10x checkout peak', status: 'completed', startedAgoSec: 3000, config: { recording_id: 'rec_paymentsrun3000', target: 'http://staging.internal:8080', mode: 'timeline', speed: 10, amplify: 2, error_amplification: 1, concurrency: 512, loop: false, timeout: '30s', compare_status: true, profile_id: 'default', vary_clones: true, set_headers: { Authorization: 'Bearer staging-token' } } });
seedReplay({ id: 'rpl_ramp2k000000003', name: 'Ramp to 2k rps (staging)', status: 'running', startedAgoSec: 38, config: { recording_id: 'rec_paymentsrun3000', target: 'http://staging.internal:8080', mode: 'rate', stages: [{ duration: '30s', rate: 500 }, { duration: '6m', rate: 2400 }], amplify: 1, error_amplification: 3, concurrency: 256, loop: false, timeout: '30s', compare_status: true } });
seedReplay({ id: 'rpl_soakfailed0000004', name: 'Nightly soak (unreachable target)', status: 'failed', startedAgoSec: 20000, durationSec: 3, error: 'dial tcp 10.4.0.12:8080: connect: connection refused', config: { recording_id: 'rec_catalogbase00000', target: 'http://10.4.0.12:8080', mode: 'rate', rate: 300, duration: '30m', loop: true, concurrency: 128, timeout: '10s' } });

if (process.env.MOCK_EMPTY === '1') {
  db.services.clear(); db.recordings.clear(); db.exchanges.clear(); db.exById.clear(); db.faults.clear(); db.replays.clear(); runtime.clear();
  for (const id of Array.from(db.profiles.keys())) if (id !== 'default') db.profiles.delete(id);
}

function startJob(job) {
  initJob(job);
  job.status = 'running';
  job.error = '';
  job.started_at = iso(Date.now());
  job.ended_at = null;
  job._startMs = Date.now();
  job.summary = replaySummary(job);
}
for (const j of db.replays.values()) if (j.status === 'running') j._startMs = NOW - 38 * 1000;

function tickReplays(tMs) {
  const changed = [];
  for (const job of db.replays.values()) {
    if (job.status !== 'running') continue;
    const sec = job._sim.secs;
    const elapsed = (tMs - job._startMs) / 1000;
    if (sec >= job._plan.durationSec || (job.config.max_requests && job._sim.sent >= job.config.max_requests)) {
      finishJob(job, 'completed');
      changed.push(job);
      continue;
    }
    const pt = replayPoint(job, job._plan, sec, tMs);
    foldSim(job, pt);
    job.series.push(pt);
    job.summary = replaySummary(job);
    void elapsed;
    changed.push(job);
  }
  return changed;
}

function replaySnapshot(job) {
  const s = job._sim;
  const last = job.series[job.series.length - 1];
  const rc = job.series.slice(-10);
  const rcReq = rc.reduce((a, p) => a + p.requests, 0);
  const wr = (k) => rc.reduce((a, p) => a + p[k] * p.requests, 0);
  return {
    name: `replay:${job.id}`, kind: 'replay', id: job.id, since: job.started_at,
    requests: s.completed, errors: s.errors, bytes_in: s.bytes_sent, bytes_out: s.bytes_received, in_flight: last ? last.in_flight : 0,
    status_classes: { '2xx': s.completed - s.errors, '4xx': 0, '5xx': s.errors, err: s.failed }, rps: last ? last.requests : 0, rps10: rc.length ? round(rcReq / rc.length, 1) : 0,
    error_rate10: rcReq ? round(rc.reduce((a, p) => a + p.errors, 0) / rcReq, 5) : 0,
    latency: job.summary.latency, recent: aggSummary(wr('p50'), wr('p95'), wr('p99'), rcReq, Math.max(0, ...rc.map((p) => p.max)), 0, rcReq), endpoints: [],
  };
}

const cleanPoint = (p) => { const { _target, _sat, ...rest } = p; return rest; };
// The Go API returns the values of credential-like set_headers as "***" on
// every read, including the create response, by a fixed rule (not the default
// profile's redact list): well-known names, names containing one of a few
// fragments, and names ending in -key or _key. It also refuses to store "***"
// for such a header (400), so a fetched job cannot be posted back with the mask.
const MASKED_VALUE = '***';
const SENSITIVE_NAMES = new Set(['authorization', 'proxy-authorization', 'cookie', 'set-cookie', 'x-api-key', 'x-auth-token', 'x-access-token', 'x-csrf-token', 'x-xsrf-token']);
const SENSITIVE_PARTS = ['auth', 'token', 'secret', 'password', 'passwd', 'credential', 'session', 'cookie', 'api-key', 'apikey', 'api_key', 'private-key', 'access-key', 'signature'];
function isSensitiveHeader(name) {
  const n = String(name).trim().toLowerCase();
  return SENSITIVE_NAMES.has(n) || n === 'key' || n.endsWith('-key') || n.endsWith('_key') || SENSITIVE_PARTS.some((part) => n.includes(part));
}
function maskSetHeaders(cfg) {
  if (!cfg || !cfg.set_headers || !Object.keys(cfg.set_headers).length) return cfg;
  return { ...cfg, set_headers: Object.fromEntries(Object.entries(cfg.set_headers).map(([k, v]) => [k, isSensitiveHeader(k) ? MASKED_VALUE : v])) };
}
function jobOut(job, withSeries) {
  const { _sim, _plan, _startMs, series, ...rest } = job;
  const out = { ...rest, config: maskSetHeaders(rest.config) };
  if (!out.started_at) delete out.started_at;
  if (out.ended_at === null) out.ended_at = null;
  if (withSeries) out.series = series.map(cleanPoint);
  return out;
}

const REPLAY_KEYS = ['recording_id', 'target', 'mode', 'speed', 'max_gap', 'seed', 'rate', 'stages', 'amplify', 'error_amplification', 'concurrency', 'duration', 'loop', 'max_requests', 'timeout', 'include', 'exclude', 'set_headers', 'remove_headers', 'host_header', 'insecure_skip_verify', 'profile_id', 'vary_clones', 'compare_status'];
function parseReplayConfig(cfg) {
  checkKeys(cfg, REPLAY_KEYS, 'ReplayConfig');
  for (const s of cfg.stages || []) checkKeys(s, ['duration', 'rate', 'hold'], 'ReplayStage');
  const probs = [];
  checkDurationField(cfg.duration, 'duration', probs);
  checkDurationField(cfg.timeout, 'timeout', probs);
  checkDurationField(cfg.max_gap, 'max_gap', probs);
  (cfg.stages || []).forEach((s, i) => checkDurationField(s.duration, `stages[${i}].duration`, probs));
  if (probs.length) throw bad('validation failed', probs);
  const c = { ...cfg };
  c.mode ||= 'timeline'; c.speed ||= 1; c.amplify ||= 1; c.error_amplification ||= 1; c.concurrency ||= (c.mode === 'max' ? 64 : 2048);
  c.seed ||= Math.floor(Math.random() * 2 ** 52) + 1;
  const norm = (k) => { if (c[k] !== undefined && c[k] !== '') c[k] = fmtGoDuration(parseGoDuration(c[k]) || 0); };
  norm('duration'); norm('timeout'); if (c.max_gap) norm('max_gap');
  if (c.stages) c.stages = c.stages.map((s) => ({ duration: fmtGoDuration(parseGoDuration(s.duration) || 0), rate: s.rate, ...(s.hold ? { hold: true } : {}) }));
  if (!c.timeout) c.timeout = '30s';
  const p = [];
  if (!c.recording_id) p.push('recording_id is required');
  else if (!db.recordings.has(c.recording_id)) p.push(`recording_id "${c.recording_id}" does not exist`);
  let okTarget = false;
  try { const u = new URL(c.target); okTarget = (u.protocol === 'http:' || u.protocol === 'https:') && !!u.host; } catch (_) { okTarget = false; }
  if (!okTarget) p.push(`target "${c.target ?? ''}" must be an absolute http(s) URL`);
  const dur = parseGoDuration(c.duration) || 0;
  if (c.mode === 'timeline') { if (!(c.speed > 0 && c.speed <= 100000)) p.push('speed must be > 0 and <= 100000'); }
  else if (c.mode === 'rate') {
    if (!(c.stages || []).length && !(c.rate > 0)) p.push('rate mode needs rate > 0 or stages');
    (c.stages || []).forEach((s, i) => { if (!(parseGoDuration(s.duration) > 0) || s.rate < 0) p.push(`stage ${i + 1} needs duration > 0 and rate >= 0`); });
    if (!dur && !(c.stages || []).length && !c.max_requests) p.push('rate mode needs duration, stages or max_requests');
  } else if (c.mode !== 'max') p.push(`mode "${c.mode}" must be timeline, rate or max`);
  if (!(c.amplify >= 1 && c.amplify <= 1000)) p.push('amplify must be between 1 and 1000');
  if (c.amplify > 1 && c.mode !== 'timeline') p.push('amplify applies to timeline mode only (in rate mode raise the rate instead)');
  if ((parseGoDuration(c.max_gap) || 0) < 0) p.push('max_gap must not be negative');
  if (!(c.error_amplification > 0 && c.error_amplification <= 1000)) p.push('error_amplification must be > 0 and <= 1000');
  if (!(c.concurrency >= 1 && c.concurrency <= 100000)) p.push('concurrency must be between 1 and 100000');
  if (dur < 0 || (c.max_requests || 0) < 0) p.push('duration, timeout and max_requests must not be negative');
  if (c.loop && !dur && !c.max_requests) p.push('loop needs duration or max_requests');
  if (c.vary_clones && !c.profile_id) p.push('vary_clones needs profile_id');
  if (c.profile_id && !db.profiles.has(c.profile_id)) p.push(`profile_id "${c.profile_id}" does not exist`);
  for (const re of [...(c.include || []), ...(c.exclude || [])]) { try { new RegExp(re); } catch (e) { p.push(`filter "${re}": ${e.message}`); } }
  for (const [k, v] of Object.entries(c.set_headers || {})) {
    if (!k.trim()) p.push('set_headers has an empty header name');
    else if (v === MASKED_VALUE && isSensitiveHeader(k)) p.push(`config.set_headers "${k}" is "${MASKED_VALUE}", the masked form the API returns; send the real value, or clone the replay to reuse the stored one`);
  }
  if (p.length) throw bad('validation failed', p);
  return c;
}

// ======================================================================
// HTTP plumbing
// ======================================================================
const sseClients = new Set();
let sseBlockedUntil = 0; // dev helper: POST /__mock/sse-drop?ms=4000 simulates an outage

function sse(res, event, data) {
  res.write(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`);
}
function broadcast(event, data) { for (const c of sseClients) sse(c, event, data); }
const allStatuses = () => Array.from(db.services.values()).map(statusOf);
let lastStatusSig = '';
function emitServices(force) {
  const list = allStatuses();
  const sig = JSON.stringify(list.map((s) => [s.service_id, s.running, s.mode, s.error, s.warning, s.addr]));
  if (force || sig !== lastStatusSig) { lastStatusSig = sig; broadcast('services', list); }
}

function buildStats(tMs) {
  const points = {};
  const snapshots = [];
  for (const svc of db.services.values()) {
    const st = resolveRunning(svc);
    if (!st.running) { snapshots.push({ ...serviceSnapshot(svc), rps: 0, rps10: 0, in_flight: 0 }); continue; }
    const pt = servicePoint(svc, tMs);
    ingestServicePoint(svc, pt, true);
    points[`service:${svc.id}`] = pt;
    snapshots.push(serviceSnapshot(svc));
  }
  const changed = tickReplays(tMs);
  for (const job of changed) {
    if (!process.env.MOCK_NO_REPLAY_POINTS && job.series.length) points[`replay:${job.id}`] = cleanPoint(job.series[job.series.length - 1]);
    if (job.status === 'running') snapshots.push(replaySnapshot(job));
  }
  return { time: iso(tMs), snapshots, points, replayChanges: changed };
}

setInterval(() => {
  const tMs = Math.floor(Date.now() / 1000) * 1000;
  const ev = buildStats(tMs);
  if (sseClients.size) {
    broadcast('stats', { time: ev.time, snapshots: ev.snapshots, points: ev.points });
    if (ev.replayChanges.length) {
      broadcast('replays', ev.replayChanges.map((j) => ({ id: j.id, status: j.status, summary: j.summary })));
    }
    emitServices(false);
  }
}, 1000);
setInterval(() => { for (const c of sseClients) c.write(': ping\n\n'); }, 15000);

const MIME = {
  '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.mjs': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8', '.svg': 'image/svg+xml', '.json': 'application/json; charset=utf-8', '.png': 'image/png',
  '.ico': 'image/x-icon', '.txt': 'text/plain; charset=utf-8', '.woff2': 'font/woff2', '.map': 'application/json', '.webmanifest': 'application/manifest+json',
};
const SECURITY_HEADERS = {
  'X-Content-Type-Options': 'nosniff',
  'Referrer-Policy': 'no-referrer',
  'X-Frame-Options': 'DENY',
  'Content-Security-Policy': "default-src 'self'; style-src 'self' 'unsafe-inline'",
};

function sendJSON(res, status, data, extra = {}) {
  const body = data === undefined || data === null ? '' : JSON.stringify(data);
  res.writeHead(status, { ...SECURITY_HEADERS, 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store', ...extra });
  res.end(status === 204 ? undefined : body);
}

async function readBody(req) {
  const chunks = [];
  for await (const c of req) chunks.push(c);
  return Buffer.concat(chunks);
}
async function readJSON(req) {
  const buf = await readBody(req);
  if (!buf.length) return {};
  try { return JSON.parse(buf.toString('utf8')); } catch (e) { throw bad(`invalid JSON body: ${e.message}`); }
}

function authorized(req, url) {
  if (!TOKEN) return true;
  const h = req.headers.authorization || '';
  if (h.startsWith('Bearer ') && h.slice(7) === TOKEN) return true;
  if ((req.headers.cookie || '').split(/;\s*/).includes(`mirage_token=${TOKEN}`)) return true;
  return url.searchParams.get('access_token') === TOKEN;
}

// ---- route table --------------------------------------------------------
const routes = [];
const route = (method, pattern, handler, raw = false) => {
  const keys = [];
  const re = new RegExp('^' + pattern.replace(/:([a-z_]+)/g, (_, k) => { keys.push(k); return '([^/]+)'; }) + '$');
  routes.push({ method, re, keys, handler, raw });
};

const getSvc = (id) => { const s = db.services.get(id); if (!s) throw new HttpError(404, `service "${id}" not found`); return s; };
const getRec = (id) => { const r = db.recordings.get(id); if (!r) throw new HttpError(404, `recording "${id}" not found`); return r; };
const getProf = (id) => { const p = db.profiles.get(id); if (!p) throw new HttpError(404, `profile "${id}" not found`); return p; };
const getFault = (id) => { const f = db.faults.get(id); if (!f) throw new HttpError(404, `fault "${id}" not found`); return f; };
const getJob = (id) => { const j = db.replays.get(id); if (!j) throw new HttpError(404, `replay "${id}" not found`); return j; };
const recOut = (r) => { const o = { ...r }; for (const k of ['first_at', 'last_at']) if (o[k] === undefined) delete o[k]; return o; };

// system
route('GET', '/api/v1/health', async () => [200, { status: 'ok', version: 'v0.1.0-mock', uptime_s: round((Date.now() - STARTED) / 1000, 1) }]);
route('GET', '/api/v1/info', async () => [200, { version: 'v0.1.0-mock', go_version: 'go1.26.0', data_path: '/var/lib/mirage/mirage.db', started_at: iso(STARTED), auth: !!TOKEN, detectors: DETECTOR_INFO }]);
route('GET', '/metrics', async (c) => {
  let out = '# HELP mirage_requests_total Requests handled per service.\n# TYPE mirage_requests_total counter\n';
  for (const s of db.services.values()) out += `mirage_requests_total{service="${s.name}"} ${rt(s).acc.requests}\n`;
  return [200, out, { 'Content-Type': 'text/plain; version=0.0.4; charset=utf-8' }];
});

// services
route('GET', '/api/v1/services', async () => [200, Array.from(db.services.values()).sort((a, b) => a.name.localeCompare(b.name)).map(svcOut)]);
route('POST', '/api/v1/services', async (c) => {
  const svc = parseServiceBody(await c.json());
  if (db.services.has(svc.id)) throw new HttpError(409, `service "${svc.id}" already exists`);
  validateService(svc);
  db.services.set(svc.id, svc);
  const r = rt(svc); r.startedAt = Date.now();
  emitServices(true);
  return [201, svcOut(svc)];
});
route('GET', '/api/v1/services/:id', async (c) => [200, svcOut(getSvc(c.p.id))]);
route('PUT', '/api/v1/services/:id', async (c) => {
  const old = getSvc(c.p.id);
  const body = await c.json();
  if (body.id && body.id !== old.id) throw bad('validation failed', ['id cannot be changed']);
  const svc = parseServiceBody({ ...body, id: old.id }, old);
  validateService(svc, { selfId: old.id });
  db.services.set(svc.id, svc);
  rt(svc).startedAt = Date.now();
  emitServices(true);
  return [200, svcOut(svc)];
});
route('DELETE', '/api/v1/services/:id', async (c) => { getSvc(c.p.id); db.services.delete(c.p.id); runtime.delete(c.p.id); emitServices(true); return [204]; });
route('POST', '/api/v1/services/:id/mode', async (c) => {
  const svc = getSvc(c.p.id);
  const b = await c.json();
  checkKeys(b, ['mode', 'recording_id', 'new_recording_name']);
  const p = [];
  if (!['passthrough', 'record', 'virtualize', 'hybrid'].includes(b.mode)) p.push(`mode "${b.mode}" must be passthrough, record, virtualize or hybrid`);
  if (b.recording_id && !db.recordings.has(b.recording_id)) p.push(`recording_id "${b.recording_id}" does not exist`);
  let recId = b.recording_id || svc.recording_id;
  if (b.new_recording_name) {
    if (!b.new_recording_name.trim()) p.push('new_recording_name must not be blank');
  } else if (b.mode && b.mode !== 'passthrough' && !recId) p.push(`mode ${b.mode} needs a recording_id`);
  if (p.length) throw bad('validation failed', p);
  if (b.new_recording_name) {
    const rec = { id: newId('rec'), name: b.new_recording_name.trim(), description: '', service_id: svc.id, source: 'proxy', anonymized: !!svc.anonymize_capture, labels: {}, created_at: iso(Date.now()), updated_at: iso(Date.now()) };
    db.recordings.set(rec.id, rec); db.exchanges.set(rec.id, []); recompute(rec);
    recId = rec.id;
  }
  svc.mode = b.mode;
  if (b.mode !== 'passthrough') svc.recording_id = recId;
  if ((b.mode === 'record' || b.mode === 'hybrid') && !svc.upstream) throw bad('validation failed', [`upstream "" must be an absolute http(s) URL`]);
  svc.updated_at = iso(Date.now());
  const r = rt(svc); r.captured = 0; r.hits = 0; r.misses = 0;
  emitServices(true);
  return [200, svcOut(svc)];
});
route('POST', '/api/v1/services/:id/enable', async (c) => { const s = getSvc(c.p.id); s.enabled = true; rt(s).startedAt = Date.now(); emitServices(true); return [200, svcOut(s)]; });
route('POST', '/api/v1/services/:id/disable', async (c) => { const s = getSvc(c.p.id); s.enabled = false; emitServices(true); return [200, svcOut(s)]; });
route('POST', '/api/v1/services/:id/reload', async (c) => { const s = getSvc(c.p.id); emitServices(true); return [200, svcOut(s)]; });
route('GET', '/api/v1/services/:id/stats', async (c) => [200, serviceSnapshot(getSvc(c.p.id))]);
route('GET', '/api/v1/services/:id/series', async (c) => {
  const s = getSvc(c.p.id);
  const since = Number(c.q.get('since')) || Date.now() - 15 * 60e3;
  return [200, rt(s).ring.filter((p) => p.t >= since)];
});

// recordings
route('GET', '/api/v1/recordings', async () => [200, Array.from(db.recordings.values()).sort((a, b) => b.created_at.localeCompare(a.created_at)).map(recOut)]);
route('POST', '/api/v1/recordings', async (c) => {
  const b = await c.json();
  checkKeys(b, ['name', 'description', 'labels']);
  if (!String(b.name || '').trim()) throw bad('validation failed', ['name is required']);
  const rec = { id: newId('rec'), name: b.name.trim(), description: b.description || '', service_id: '', source: 'proxy', anonymized: false, labels: b.labels || {}, created_at: iso(Date.now()), updated_at: iso(Date.now()) };
  db.recordings.set(rec.id, rec); db.exchanges.set(rec.id, []); recompute(rec);
  return [201, recOut(rec)];
});
route('POST', '/api/v1/recordings/import', async (c) => {
  const ct = c.req.headers['content-type'] || '';
  let text; let fileName = '';
  if (ct.startsWith('multipart/form-data')) {
    const r = new Request('http://x/', { method: 'POST', headers: { 'content-type': ct }, body: Readable.toWeb(Readable.from([c.rawBody])), duplex: 'half' });
    const fd = await r.formData();
    const f = fd.get('file');
    if (!f || typeof f === 'string') throw bad('multipart field "file" is required');
    fileName = f.name || ''; text = await f.text();
  } else text = c.rawBody.toString('utf8');
  let har;
  try { har = JSON.parse(text); } catch (e) { throw bad(`invalid HAR: ${e.message}`); }
  const entries = har?.log?.entries;
  if (!Array.isArray(entries)) throw bad('invalid HAR: log.entries is missing');
  const name = c.q.get('name') || fileName.replace(/\.har$/i, '') || 'imported HAR';
  const profId = c.q.get('profile_id');
  const prof = profId ? getProf(profId) : null;
  const recId = newId('rec');
  const list = [];
  entries.forEach((e, i) => {
    let u; try { u = new URL(e.request.url); } catch (_) { u = new URL('http://unknown/'); }
    const hdr = (arr) => { const o = {}; for (const h of arr || []) (o[h.name] ||= []).push(h.value); return o; };
    const resText = e.response?.content?.text || '';
    const view = {
      id: nextExchangeId++, recording_id: recId, seq: i + 1, started_at: e.startedDateTime || iso(Date.now() + i), duration: fmtGoDuration(e.time || 1), ttfb: fmtGoDuration((e.time || 1) * 0.7),
      endpoint: `${e.request.method} ${u.pathname}`,
      request: { method: e.request.method, scheme: u.protocol.replace(':', ''), host: u.host, path: u.pathname, ...(u.search ? { raw_query: u.search.slice(1) } : {}), proto: 'HTTP/1.1', header: hdr(e.request.headers), body: e.request.postData?.text ? { encoding: 'text', data: e.request.postData.text, size: e.request.postData.text.length, content_type: e.request.postData.mimeType } : { encoding: 'none', size: 0 } },
      response: { status: e.response?.status || 0, proto: 'HTTP/1.1', header: hdr(e.response?.headers), body: resText ? { encoding: e.response.content.encoding === 'base64' ? 'base64' : 'text', data: resText, size: resText.length, content_type: e.response.content.mimeType } : { encoding: 'none', size: 0 } },
      error: '', anonymized: false,
    };
    let ex = view;
    if (prof) ex = anonymizeExchange(view, prof).exchange;
    ex._ms = e.time || 1; ex._t = Date.parse(view.started_at) || Date.now();
    list.push(ex); db.exById.set(ex.id, ex);
  });
  db.exchanges.set(recId, list);
  const rec = { id: recId, name, description: `Imported from ${fileName || 'HAR'}`, service_id: '', source: 'har', anonymized: !!prof, labels: {}, created_at: iso(Date.now()), updated_at: iso(Date.now()) };
  db.recordings.set(recId, rec); recompute(rec);
  return [201, recOut(rec)];
}, true);
route('GET', '/api/v1/recordings/:id', async (c) => [200, recOut(getRec(c.p.id))]);
route('PATCH', '/api/v1/recordings/:id', async (c) => {
  const r = getRec(c.p.id);
  const b = await c.json();
  checkKeys(b, ['name', 'description', 'labels']);
  if (b.name !== undefined) { if (!String(b.name).trim()) throw bad('validation failed', ['name is required']); r.name = b.name.trim(); }
  if (b.description !== undefined) r.description = b.description;
  if (b.labels !== undefined) r.labels = b.labels;
  r.updated_at = iso(Date.now());
  return [200, recOut(r)];
});
route('DELETE', '/api/v1/recordings/:id', async (c) => {
  const r = getRec(c.p.id);
  for (const s of db.services.values()) if (s.recording_id === r.id && resolveRunning(s).running) throw new HttpError(409, `recording is used by running service "${s.name}"`);
  for (const e of db.exchanges.get(r.id) || []) db.exById.delete(e.id);
  db.exchanges.delete(r.id); db.recordings.delete(r.id);
  for (const s of db.services.values()) if (s.recording_id === r.id) { s.recording_id = ''; if (s.mode !== 'passthrough') { s.mode = 'passthrough'; s.enabled = false; } }
  return [204];
});
route('GET', '/api/v1/recordings/:id/endpoints', async (c) => {
  const list = db.exchanges.get(getRec(c.p.id).id) || [];
  const by = new Map();
  for (const e of list) { let a = by.get(e.endpoint); if (!a) by.set(e.endpoint, (a = [])); a.push(e); }
  const out = [];
  for (const [endpoint, arr] of by) {
    const ms = arr.map((e) => e._ms).sort((x, y) => x - y);
    const counts = {};
    for (const e of arr) counts[e.response.status] = (counts[e.response.status] || 0) + 1;
    const errors = arr.filter(isErrorEx).length;
    out.push({ endpoint, count: arr.length, errors, error_rate: round(errors / arr.length, 5), avg: fmtGoDuration(ms.reduce((s, x) => s + x, 0) / ms.length), p50: fmtGoDuration(percentile(ms, 50)), p95: fmtGoDuration(percentile(ms, 95)), p99: fmtGoDuration(percentile(ms, 99)), max: fmtGoDuration(ms[ms.length - 1]), status_counts: counts });
  }
  return [200, out.sort((a, b) => b.count - a.count)];
});
route('GET', '/api/v1/recordings/:id/exchanges', async (c) => {
  const rec = getRec(c.p.id);
  let list = db.exchanges.get(rec.id) || [];
  const q = c.q;
  if (q.get('method')) list = list.filter((e) => e.request.method === q.get('method').toUpperCase());
  if (q.get('status')) list = list.filter((e) => e.response.status === Number(q.get('status')));
  if (q.get('status_class')) list = list.filter((e) => Math.floor(e.response.status / 100) === Number(q.get('status_class')));
  if (q.get('path')) { const s = q.get('path').toLowerCase(); list = list.filter((e) => (e.request.path + (e.request.raw_query ? '?' + e.request.raw_query : '')).toLowerCase().includes(s)); }
  if (q.get('endpoint')) list = list.filter((e) => e.endpoint === q.get('endpoint'));
  if (q.get('errors') === '1' || q.get('errors') === 'true') list = list.filter(isErrorEx);
  const total = list.length;
  if (q.get('desc') === '1' || q.get('desc') === 'true') list = list.slice().reverse();
  const offset = Math.max(0, Number(q.get('offset')) || 0);
  const limit = Math.min(1000, Number(q.get('limit')) || 100);
  return [200, { items: list.slice(offset, offset + limit).map(exchangeSummary), total }];
});
route('GET', '/api/v1/exchanges/:id', async (c) => {
  const ex = db.exById.get(Number(c.p.id));
  if (!ex) throw new HttpError(404, `exchange ${c.p.id} not found`);
  return [200, publicExchange(ex)];
});
route('POST', '/api/v1/recordings/:id/anonymize', async (c) => {
  const rec = getRec(c.p.id);
  const b = await c.json();
  checkKeys(b, ['profile_id', 'name']);
  const prof = getProf(b.profile_id || 'default');
  const recId = newId('rec');
  const list = [];
  (db.exchanges.get(rec.id) || []).forEach((e, i) => {
    const out = anonymizeExchange(publicExchange(e), prof).exchange;
    const ex = { ...out, id: nextExchangeId++, recording_id: recId, seq: i + 1, _ms: e._ms, _t: e._t };
    list.push(ex); db.exById.set(ex.id, ex);
  });
  db.exchanges.set(recId, list);
  const copy = { id: recId, name: b.name || `${rec.name} (anon)`, description: `Anonymized copy of ${rec.name} using profile ${prof.name}`, service_id: '', source: 'copy', anonymized: true, labels: {}, created_at: iso(Date.now()), updated_at: iso(Date.now()) };
  db.recordings.set(recId, copy); recompute(copy);
  return [201, recOut(copy)];
});
route('GET', '/api/v1/recordings/:id/export.har', async (c) => {
  const rec = getRec(c.p.id);
  const profId = c.q.get('profile_id');
  const prof = profId ? getProf(profId) : null;
  const entries = (db.exchanges.get(rec.id) || []).map((e0) => {
    const e = prof ? anonymizeExchange(publicExchange(e0), prof).exchange : e0;
    const harH = (o) => Object.entries(o || {}).flatMap(([name, vs]) => vs.map((value) => ({ name, value })));
    return {
      startedDateTime: e.started_at, time: e0._ms,
      request: { method: e.request.method, url: `${e.request.scheme || 'http'}://${e.request.host}${e.request.path}${e.request.raw_query ? '?' + e.request.raw_query : ''}`, httpVersion: 'HTTP/1.1', headers: harH(e.request.header), queryString: [], cookies: [], headersSize: -1, bodySize: e.request.body.size || 0, ...(e.request.body.data ? { postData: { mimeType: e.request.body.content_type || '', text: e.request.body.data } } : {}) },
      response: { status: e.response.status, statusText: '', httpVersion: 'HTTP/1.1', headers: harH(e.response.header), cookies: [], content: { size: e.response.body.size || 0, mimeType: e.response.body.content_type || '', text: e.response.body.data || '', ...(e.response.body.encoding === 'base64' ? { encoding: 'base64' } : {}) }, redirectURL: '', headersSize: -1, bodySize: e.response.body.size || 0 },
      cache: {}, timings: { send: 0, wait: e0._ms, receive: 0 },
    };
  });
  const har = { log: { version: '1.2', creator: { name: 'Mirage', version: 'v0.1.0-mock' }, entries } };
  const fname = rec.name.replace(/[^a-z0-9._-]+/gi, '_') + (prof ? '.anon' : '') + '.har';
  return [200, JSON.stringify(har), { 'Content-Type': 'application/json', 'Content-Disposition': `attachment; filename="${fname}"` }];
});

// anonymization
route('GET', '/api/v1/profiles', async () => [200, Array.from(db.profiles.values()).sort((a, b) => (a.id === 'default' ? -1 : b.id === 'default' ? 1 : a.name.localeCompare(b.name))).map(profileOut)]);
route('GET', '/api/v1/anonymize/detectors', async () => [200, DETECTOR_INFO]);
const PROFILE_KEYS = ['id', 'name', 'description', 'detectors', 'detector_strategy', 'redact_headers', 'opaque_bodies', 'rules', 'secret', 'has_secret', 'created_at', 'updated_at'];
const RULE_KEYS = ['id', 'name', 'enabled', 'target', 'selector', 'scope', 'strategy', 'fake', 'keep_prefix', 'keep_suffix', 'replacement'];
function parseProfile(b, existing) {
  checkKeys(b, [...PROFILE_KEYS, 'regenerate_secret']);
  (b.rules || []).forEach((r) => checkKeys(r, RULE_KEYS, 'AnonymizationRule'));
  const p = { ...b };
  delete p.regenerate_secret; delete p.has_secret;
  p.id = b.id || existing?.id || newId('prof');
  p.detector_strategy ||= 'fake'; p.detectors ||= []; p.redact_headers ||= []; p.rules ||= [];
  p.opaque_bodies ||= 'keep';
  if (!['keep', 'fill'].includes(p.opaque_bodies)) throw bad('validation failed', [`opaque_bodies "${p.opaque_bodies}" must be keep or fill`]);
  p.rules = p.rules.map((r, i) => ({ ...r, id: r.id || `r${i + 1}`, scope: r.scope || 'both', strategy: r.strategy || 'hash', ...(r.strategy === 'fake' && !r.fake ? { fake: 'auto' } : {}) }));
  p.secret = b.regenerate_secret ? newSecret() : (b.secret || existing?.secret || newSecret());
  p.created_at = existing?.created_at || iso(Date.now());
  p.updated_at = iso(Date.now());
  const probs = validateProfile(p);
  if (probs.length) throw bad('validation failed', probs);
  return p;
}
route('POST', '/api/v1/profiles', async (c) => {
  const p = parseProfile(await c.json());
  if (db.profiles.has(p.id)) throw new HttpError(409, `profile "${p.id}" already exists`);
  db.profiles.set(p.id, p);
  return [201, profileOut(p)];
});
route('GET', '/api/v1/profiles/:id', async (c) => [200, profileOut(getProf(c.p.id))]);
route('PUT', '/api/v1/profiles/:id', async (c) => {
  const old = getProf(c.p.id);
  const p = parseProfile({ ...(await c.json()), id: old.id }, old);
  db.profiles.set(p.id, p);
  return [200, profileOut(p)];
});
route('DELETE', '/api/v1/profiles/:id', async (c) => {
  const p = getProf(c.p.id);
  if (p.id === 'default') throw new HttpError(409, 'the default profile cannot be deleted');
  for (const s of db.services.values()) if (s.profile_id === p.id) throw new HttpError(409, `profile is used by service "${s.name}"`);
  db.profiles.delete(p.id);
  return [204];
});
route('POST', '/api/v1/anonymize/preview', async (c) => {
  const b = await c.json();
  checkKeys(b, ['profile', 'profile_id', 'exchange_id', 'exchange']);
  let prof;
  if (b.profile) {
    checkKeys(b.profile, [...PROFILE_KEYS, 'regenerate_secret']);
    const stored = db.profiles.get(b.profile.id);
    const merged = { ...b.profile, secret: b.profile.secret || stored?.secret || 'preview-secret', rules: (b.profile.rules || []).map((r) => ({ scope: 'both', ...r })) };
    prof = { detector_strategy: 'fake', detectors: [], redact_headers: [], ...merged };
    const probs = validateProfile({ name: 'x', ...prof });
    if (probs.length) throw bad('validation failed', probs);
  } else prof = getProf(b.profile_id || 'default');
  let ex;
  if (b.exchange_id !== undefined) {
    const found = db.exById.get(Number(b.exchange_id));
    if (!found) throw new HttpError(404, `exchange ${b.exchange_id} not found`);
    ex = publicExchange(found);
  } else if (b.exchange) ex = b.exchange;
  else throw bad('validation failed', ['exchange_id or exchange is required']);
  ex.request = { header: {}, body: { encoding: 'none', size: 0 }, ...ex.request };
  ex.response = { header: {}, body: { encoding: 'none', size: 0 }, ...ex.response };
  const out = anonymizeExchange(ex, prof);
  return [200, { exchange: out.exchange, changes: out.changes }];
});

// faults
route('GET', '/api/v1/faults', async () => [200, Array.from(db.faults.values()).sort((a, b) => a.order - b.order || a.name.localeCompare(b.name)).map(faultOut)]);
route('POST', '/api/v1/faults', async (c) => { const f = parseFault(await c.json()); db.faults.set(f.id, f); return [201, faultOut(f)]; });
route('GET', '/api/v1/faults/:id', async (c) => [200, faultOut(getFault(c.p.id))]);
route('PUT', '/api/v1/faults/:id', async (c) => { const old = getFault(c.p.id); const f = parseFault({ ...(await c.json()), id: old.id }, old); db.faults.set(f.id, f); return [200, faultOut(f)]; });
route('DELETE', '/api/v1/faults/:id', async (c) => { getFault(c.p.id); db.faults.delete(c.p.id); return [204]; });

// replays
route('GET', '/api/v1/replays', async () => [200, Array.from(db.replays.values()).sort((a, b) => (b.created_at || '').localeCompare(a.created_at || '')).map((j) => jobOut(j, false))]);
route('POST', '/api/v1/replays', async (c) => {
  const b = await c.json();
  checkKeys(b, ['name', 'config', 'start']);
  const cfg = parseReplayConfig(b.config || {});
  const job = { id: newId('rpl'), name: b.name?.trim() || `replay ${new Date().toLocaleTimeString()}`, config: cfg, status: 'pending', error: '', created_at: iso(Date.now()), started_at: null, ended_at: null };
  initJob(job);
  db.replays.set(job.id, job);
  if (b.start) startJob(job);
  return [201, jobOut(job, true)];
});
route('GET', '/api/v1/replays/:id', async (c) => [200, jobOut(getJob(c.p.id), true)]);
route('POST', '/api/v1/replays/:id/start', async (c) => {
  const j = getJob(c.p.id);
  if (j.status === 'running') throw new HttpError(409, 'replay is already running');
  startJob(j);
  broadcast('replays', [{ id: j.id, status: j.status, summary: j.summary }]);
  return [200, jobOut(j, true)];
});
route('POST', '/api/v1/replays/:id/stop', async (c) => {
  const j = getJob(c.p.id);
  if (j.status !== 'running') throw new HttpError(409, 'replay is not running');
  finishJob(j, 'stopped');
  broadcast('replays', [{ id: j.id, status: j.status, summary: j.summary }]);
  return [200, jobOut(j, true)];
});
route('POST', '/api/v1/replays/:id/clone', async (c) => {
  const j = getJob(c.p.id);
  const b = await c.json();
  checkKeys(b, ['name']);
  const copy = { id: newId('rpl'), name: b.name || `${j.name} (copy)`, config: structuredClone(j.config), status: 'pending', error: '', created_at: iso(Date.now()), started_at: null, ended_at: null };
  initJob(copy);
  db.replays.set(copy.id, copy);
  return [201, jobOut(copy, true)];
});
route('DELETE', '/api/v1/replays/:id', async (c) => {
  const j = getJob(c.p.id);
  if (j.status === 'running') throw new HttpError(409, 'stop the replay before deleting it');
  db.replays.delete(j.id);
  return [204];
});
route('GET', '/api/v1/replays/:id/series', async (c) => {
  const j = getJob(c.p.id);
  const since = Number(c.q.get('since')) || 0;
  return [200, j.series.filter((p) => p.t >= since).map(cleanPoint)];
});

// stats + stream
route('GET', '/api/v1/stats', async () => {
  const snaps = Array.from(db.services.values()).map(serviceSnapshot);
  for (const j of db.replays.values()) if (j.status === 'running') snaps.push(replaySnapshot(j));
  return [200, snaps];
});

// ---- server ----------------------------------------------------------------
async function handleApi(req, res, url) {
  if (url.pathname === '/__mock/sse-drop' && req.method === 'POST') {
    sseBlockedUntil = Date.now() + (Number(url.searchParams.get('ms')) || 4000);
    for (const c of sseClients) c.end();
    sseClients.clear();
    return sendJSON(res, 200, { dropped: true });
  }
  if (url.pathname === '/api/v1/stream') {
    if (!authorized(req, url)) return sendJSON(res, 401, { error: 'unauthorized' });
    if (Date.now() < sseBlockedUntil) return sendJSON(res, 503, { error: 'stream temporarily unavailable (mock outage)' });
    res.writeHead(200, { ...SECURITY_HEADERS, 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache', Connection: 'keep-alive', 'X-Accel-Buffering': 'no' });
    res.write(': connected\n\n');
    sseClients.add(res);
    sse(res, 'services', allStatuses());
    req.on('close', () => sseClients.delete(res));
    return undefined;
  }
  const open = url.pathname === '/api/v1/health';
  if (!open && !authorized(req, url)) return sendJSON(res, 401, { error: 'unauthorized: send "Authorization: Bearer <token>"' });
  for (const r of routes) {
    if (r.method !== req.method) continue;
    const m = r.re.exec(url.pathname);
    if (!m) continue;
    const p = {};
    r.keys.forEach((k, i) => { p[k] = decodeURIComponent(m[i + 1]); });
    const ctx = { req, res, url, p, q: url.searchParams, rawBody: null };
    ctx.json = async () => { ctx.rawBody ??= await readBody(req); if (!ctx.rawBody.length) return {}; try { return JSON.parse(ctx.rawBody.toString('utf8')); } catch (e) { throw bad(`invalid JSON body: ${e.message}`); } };
    if (r.raw) ctx.rawBody = await readBody(req);
    if (EXTRA_LATENCY > 0) await new Promise((ok) => setTimeout(ok, EXTRA_LATENCY * (0.4 + Math.random())));
    const out = await r.handler(ctx);
    const [status, data, headers] = out;
    if (typeof data === 'string') {
      res.writeHead(status, { ...SECURITY_HEADERS, 'Cache-Control': 'no-store', ...headers });
      return res.end(data);
    }
    return sendJSON(res, status, data, headers);
  }
  // method known on another route => 405, else 404
  const known = routes.some((r) => r.re.test(url.pathname));
  return sendJSON(res, known ? 405 : 404, { error: known ? 'method not allowed' : `no such endpoint: ${req.method} ${url.pathname}` });
}

async function serveStatic(req, res, url) {
  let rel = decodeURIComponent(url.pathname);
  if (rel.endsWith('/')) rel += 'index.html';
  let file = path.normalize(path.join(STATIC_DIR, rel));
  if (!file.startsWith(STATIC_DIR)) { res.writeHead(403); return res.end('forbidden'); }
  let st = null;
  try { st = await stat(file); if (st.isDirectory()) { file = path.join(file, 'index.html'); st = await stat(file); } } catch (_) { st = null; }
  if (!st) {
    if (path.extname(rel) && rel.startsWith('/assets/')) { res.writeHead(404, { ...SECURITY_HEADERS, 'Content-Type': 'text/plain' }); return res.end('not found'); }
    file = path.join(STATIC_DIR, 'index.html'); // SPA fallback
  }
  const data = await readFile(file);
  res.writeHead(200, { ...SECURITY_HEADERS, 'Content-Type': MIME[path.extname(file)] || 'application/octet-stream', 'Cache-Control': 'no-cache' });
  res.end(req.method === 'HEAD' ? undefined : data);
}

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, `http://${req.headers.host || 'localhost'}`);
  const t0 = Date.now();
  try {
    if (url.pathname.startsWith('/api/') || url.pathname === '/metrics' || url.pathname.startsWith('/__mock/')) {
      if (url.pathname === '/metrics' && !authorized(req, url)) sendJSON(res, 401, { error: 'unauthorized' });
      else await handleApi(req, res, url);
    } else await serveStatic(req, res, url);
  } catch (e) {
    if (e instanceof HttpError) sendJSON(res, e.status, { error: e.message, ...(e.problems ? { problems: e.problems } : {}) });
    else { console.error(e); if (!res.headersSent) sendJSON(res, 500, { error: String(e && e.message || e) }); else res.end(); }
  }
  if (!url.pathname.startsWith('/assets/') && url.pathname !== '/api/v1/stream') log(`${req.method} ${url.pathname}${url.search ? '?' + url.search.slice(1, 60) : ''} ${res.statusCode} ${Date.now() - t0}ms`);
});

server.listen(PORT, HOST, () => {
  console.log(`Mirage mock server on http://${HOST}:${PORT}${TOKEN ? '  (auth on: token ' + TOKEN + ')' : ''}`);
});
process.on('SIGINT', () => process.exit(0));
process.on('SIGTERM', () => process.exit(0));

void pick; void endpointNames; void latencySummary; void mulberry32;
