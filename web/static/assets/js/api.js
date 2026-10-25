// Fetch wrapper for the Mirage management API (/api/v1).

const TOKEN_KEY = 'mirage_token';
export const API_BASE = '/api/v1';

export class ApiError extends Error {
  constructor(message, { status = 0, problems = [], body = null } = {}) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.problems = problems;
    this.body = body;
  }
}

export function getToken() {
  try { return localStorage.getItem(TOKEN_KEY) || ''; } catch (_) { return ''; }
}
export function setToken(t) {
  try { if (t) localStorage.setItem(TOKEN_KEY, t); else localStorage.removeItem(TOKEN_KEY); } catch (_) { /* ignore */ }
}
export function clearToken() { setToken(''); }

let authHandler = null;
let authPromise = null;
let lastCancel = 0;
const tokenListeners = new Set();

/** Registers fn() -> Promise<string|null> that asks the user for a token. */
export function setAuthHandler(fn) { authHandler = fn; }
export function onTokenChange(fn) { tokenListeners.add(fn); return () => tokenListeners.delete(fn); }
export function notifyTokenChange() { tokenListeners.forEach((f) => f(getToken())); }

function requestToken(rejected) {
  if (!authHandler) return Promise.resolve(null);
  // After the user dismisses the prompt, background requests (the live
  // stream probes) must not nag again straight away.
  if (!authPromise && Date.now() - lastCancel < 20000) return Promise.resolve(null);
  if (!authPromise) {
    authPromise = Promise.resolve(authHandler(rejected)).then((t) => {
      if (t) { setToken(t); notifyTokenChange(); } else lastCancel = Date.now();
      return t;
    }).finally(() => { authPromise = null; });
  }
  return authPromise;
}

/** Explicitly ask for a token (e.g. the user clicked the "Auth required" indicator). */
export function askForToken() {
  lastCancel = 0;
  return requestToken(false);
}

function qs(query) {
  if (!query) return '';
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(query)) {
    if (v === undefined || v === null || v === '' || v === false) continue;
    p.set(k, v === true ? '1' : String(v));
  }
  const s = p.toString();
  return s ? '?' + s : '';
}

/** Absolute-path URL for the API with optional query and access_token. */
export function apiUrl(path, query, withToken = true) {
  const q = { ...(query || {}) };
  const t = getToken();
  if (withToken && t) q.access_token = t;
  return API_BASE + path + qs(q);
}

async function parseBody(res) {
  if (res.status === 204) return null;
  const ct = res.headers.get('content-type') || '';
  if (ct.includes('json')) {
    try { return await res.json(); } catch (_) { return null; }
  }
  const t = await res.text();
  return t || null;
}

export async function request(method, path, { body, query, headers, signal, raw } = {}, attempt = 0) {
  const h = { Accept: 'application/json', ...(headers || {}) };
  const tok = getToken();
  if (tok) h.Authorization = 'Bearer ' + tok;
  let payload;
  if (body !== undefined && body !== null) {
    if (body instanceof FormData || body instanceof Blob || typeof body === 'string' || body instanceof ArrayBuffer) {
      payload = body;
    } else {
      payload = JSON.stringify(body);
      h['Content-Type'] = 'application/json';
    }
  }
  let res;
  try {
    res = await fetch(API_BASE + path + qs(query), { method, headers: h, body: payload, signal, credentials: 'same-origin' });
  } catch (e) {
    if (e && e.name === 'AbortError') throw e;
    throw new ApiError('Cannot reach the Mirage server. Is it running?', { status: 0 });
  }
  if (res.status === 401 && attempt < 3) {
    // The stored token (if any) was refused: forget it and ask again.
    const rejected = !!tok;
    if (rejected) clearToken();
    const t = await requestToken(rejected);
    if (t) return request(method, path, { body, query, headers, signal, raw }, attempt + 1);
  }
  const data = await parseBody(res);
  if (!res.ok) {
    const msg = (data && typeof data === 'object' && data.error) || (typeof data === 'string' && data) || res.statusText || 'Request failed';
    const problems = data && typeof data === 'object' && Array.isArray(data.problems) ? data.problems : [];
    throw new ApiError(msg, { status: res.status, problems, body: data });
  }
  return raw ? res : data;
}

export const get = (path, query, opts) => request('GET', path, { ...(opts || {}), query });
export const post = (path, body, opts) => request('POST', path, { ...(opts || {}), body });
export const put = (path, body, opts) => request('PUT', path, { ...(opts || {}), body });
export const patch = (path, body, opts) => request('PATCH', path, { ...(opts || {}), body });
export const del = (path, opts) => request('DELETE', path, opts);

/** Human-readable error text, including validation problems, for toasts. */
export function errorText(e) {
  if (!e) return 'Unknown error';
  if (e instanceof ApiError && e.problems.length) return `${e.message}: ${e.problems.join('; ')}`;
  return e.message || String(e);
}

// Convenience endpoint helpers -------------------------------------------------
// Go marshals a nil slice as null, so list endpoints are normalised to arrays.
const arr = (p) => p.then((v) => (Array.isArray(v) ? v : []));
const page = (p) => p.then((v) => ({ items: (v && Array.isArray(v.items) ? v.items : []), total: (v && v.total) || 0 }));

export const api = {
  health: () => get('/health'),
  info: () => get('/info'),
  services: () => arr(get('/services')),
  service: (id) => get(`/services/${enc(id)}`),
  createService: (s) => post('/services', s),
  updateService: (id, s) => put(`/services/${enc(id)}`, s),
  deleteService: (id) => del(`/services/${enc(id)}`),
  setMode: (id, body) => post(`/services/${enc(id)}/mode`, body),
  enableService: (id) => post(`/services/${enc(id)}/enable`),
  disableService: (id) => post(`/services/${enc(id)}/disable`),
  reloadService: (id) => post(`/services/${enc(id)}/reload`),
  serviceStats: (id) => get(`/services/${enc(id)}/stats`),
  serviceSeries: (id, since) => arr(get(`/services/${enc(id)}/series`, { since })),
  recordings: () => arr(get('/recordings')),
  recording: (id) => get(`/recordings/${enc(id)}`),
  createRecording: (r) => post('/recordings', r),
  patchRecording: (id, r) => patch(`/recordings/${enc(id)}`, r),
  deleteRecording: (id) => del(`/recordings/${enc(id)}`),
  endpoints: (id) => arr(get(`/recordings/${enc(id)}/endpoints`)),
  exchanges: (id, q) => page(get(`/recordings/${enc(id)}/exchanges`, q)),
  exchange: (id) => get(`/exchanges/${enc(id)}`),
  anonymizeRecording: (id, body) => post(`/recordings/${enc(id)}/anonymize`, body),
  profiles: () => arr(get('/profiles')),
  profile: (id) => get(`/profiles/${enc(id)}`),
  createProfile: (p) => post('/profiles', p),
  updateProfile: (id, p) => put(`/profiles/${enc(id)}`, p),
  deleteProfile: (id) => del(`/profiles/${enc(id)}`),
  detectors: () => arr(get('/anonymize/detectors')),
  preview: (body) => post('/anonymize/preview', body),
  faults: () => arr(get('/faults')),
  createFault: (f) => post('/faults', f),
  updateFault: (id, f) => put(`/faults/${enc(id)}`, f),
  deleteFault: (id) => del(`/faults/${enc(id)}`),
  replays: () => arr(get('/replays')),
  replay: (id) => get(`/replays/${enc(id)}`),
  createReplay: (r) => post('/replays', r),
  startReplay: (id) => post(`/replays/${enc(id)}/start`),
  stopReplay: (id) => post(`/replays/${enc(id)}/stop`),
  cloneReplay: (id, body) => post(`/replays/${enc(id)}/clone`, body || {}),
  deleteReplay: (id) => del(`/replays/${enc(id)}`),
  replaySeries: (id, since) => arr(get(`/replays/${enc(id)}/series`, { since })),
  stats: () => arr(get('/stats')),
};

function enc(s) { return encodeURIComponent(s); }
