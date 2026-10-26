// Shared application state plus a tiny pub/sub bus. High-frequency data
// (stats) flows through emit()/on(); pages subscribe and unsubscribe with
// their lifecycle (see router.js ctx.on).
import { api } from './api.js';

export const state = {
  conn: 'connecting', // connecting | connected | reconnecting | unauthorized
  info: null,
  services: [], // ServiceWithStatus[]
  servicesLoaded: false,
  replays: [], // ReplayJob[] (no series)
  replaysLoaded: false,
  snapshots: {}, // collector name -> StatsSnapshot
  lastStats: 0, // Date.now() of the last stats event
};

const handlers = new Map();

export function on(topic, fn) {
  let set = handlers.get(topic);
  if (!set) handlers.set(topic, (set = new Set()));
  set.add(fn);
  return () => set.delete(fn);
}

export function emit(topic, payload) {
  const set = handlers.get(topic);
  if (!set) return;
  for (const fn of Array.from(set)) {
    try { fn(payload); } catch (e) { console.error(`handler for "${topic}" failed`, e); }
  }
}

export function setConn(conn) {
  if (state.conn === conn) return;
  state.conn = conn;
  emit('conn', conn);
}

let servicesInflight = null;
export function refreshServices() {
  if (!servicesInflight) {
    servicesInflight = api.services().then((list) => {
      state.services = Array.isArray(list) ? list : [];
      state.servicesLoaded = true;
      emit('services', state.services);
      return state.services;
    }).finally(() => { servicesInflight = null; });
  }
  return servicesInflight;
}

let replaysInflight = null;
export function refreshReplays() {
  if (!replaysInflight) {
    replaysInflight = api.replays().then((list) => {
      state.replays = Array.isArray(list) ? list : [];
      state.replaysLoaded = true;
      emit('replays', state.replays);
      return state.replays;
    }).finally(() => { replaysInflight = null; });
  }
  return replaysInflight;
}

export async function refreshInfo() {
  state.info = await api.info();
  emit('info', state.info);
  return state.info;
}

/**
 * What ServiceStatus.error means depends on whether the listener is up. On a
 * service that is not running it is why it could not start (or why its
 * listener died). On a running one it is a problem found while serving. The
 * server reports "capture limit reached" in the separate `warning` field (the
 * service is fine, only capturing stopped), which is never a failure; older
 * servers put the same text in `error`, so the text is recognised in both.
 * Returns {kind: 'failed' | 'capture-limit' | 'runtime', text} or null.
 */
export function statusNotice(st) {
  if (!st) return null;
  const note = st.running ? (st.error || st.warning) : '';
  if (note) return { kind: /capture limit reached/i.test(note) ? 'capture-limit' : 'runtime', text: note };
  if (st.error) return { kind: 'failed', text: st.error };
  return null;
}

export function serviceById(id) {
  return state.services.find((s) => s.id === id) || null;
}

/** Applies an SSE `services` event: [ServiceStatus]. */
export function applyServiceStatuses(list) {
  if (!Array.isArray(list)) return;
  let unknown = false;
  const byId = new Map(state.services.map((s) => [s.id, s]));
  for (const st of list) {
    const s = byId.get(st.service_id);
    if (s) {
      s.status = st;
      s.mode = st.mode || s.mode;
    } else unknown = true;
  }
  if (list.length !== state.services.length) unknown = true;
  emit('services', state.services);
  if (unknown) refreshServices().catch(() => {});
}

/** Applies an SSE `replays` event: [{id, status, summary}]. */
export function applyReplayUpdates(list) {
  if (!Array.isArray(list)) return;
  let refetch = false;
  const byId = new Map(state.replays.map((r) => [r.id, r]));
  for (const u of list) {
    const r = byId.get(u.id);
    if (!r) { refetch = true; continue; }
    if (r.status !== u.status) refetch = true;
    r.status = u.status;
    if (u.summary) r.summary = u.summary;
  }
  emit('replays', state.replays);
  if (refetch) refreshReplays().catch(() => {});
}

/** Applies an SSE `stats` event. */
export function applyStats(evt) {
  if (!evt) return;
  evt.points = evt.points || {};
  evt.snapshots = evt.snapshots || [];
  const snaps = {};
  for (const s of evt.snapshots || []) snaps[s.name] = s;
  state.snapshots = snaps;
  state.lastStats = Date.now();
  emit('stats', evt);
}

/** Display name for a service id (falls back to the id). */
export function serviceName(id) {
  const s = serviceById(id);
  return s ? s.name : id;
}
