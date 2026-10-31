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
 * Fetches everything the stream does not replay again: server info, the
 * services and the replay jobs. The stream only carries changes, so whatever
 * happened while it was down (a restarted server marking a running replay
 * failed, a service edited elsewhere) is only visible after this. Emits
 * 'resync' when the fetches have settled, for pages that keep their own copy
 * of server data.
 */
export function resyncAll() {
  const quiet = (p) => p.then(() => {}, () => {});
  return Promise.all([quiet(refreshInfo()), quiet(refreshServices()), quiet(refreshReplays())]).then(() => emit('resync'));
}

/**
 * What ServiceStatus.error means depends on whether the listener is up. On a
 * service that is not running it is why it could not start (or why its
 * listener died). On a running one it is a problem found while serving. The
 * server reports "capture limit reached" in the separate `warning` field (the
 * service is fine, only capturing stopped), which is never a failure; older
 * servers put the same text in `error`, so the text is recognised in both.
 * Returns {kind: 'failed' | 'capture-limit' | 'truncated' | 'runtime', text} or null.
 */
export function statusNotice(st) {
  if (!st) return null;
  const note = st.running ? (st.error || st.warning) : '';
  if (note) {
    // Truncation notes come first when joined with "capture limit reached":
    // they describe requests that are failing right now.
    const kind = !st.error && /max_body_bytes/.test(note) ? 'truncated'
      : /capture limit reached/i.test(note) ? 'capture-limit' : 'runtime';
    return { kind, text: note };
  }
  if (st.error) return { kind: 'failed', text: st.error };
  return null;
}

export function serviceById(id) {
  return state.services.find((s) => s.id === id) || null;
}

/**
 * Applies an SSE `services` event: [ServiceStatus]. A status only says whether
 * a service runs, where, in which mode, since when and why it does not, so when
 * one of those differs from what the UI holds the service was changed
 * (disabled, enabled, moved, switched, restarted) and the rows may be stale in
 * ways a status cannot tell: the whole list is fetched again.
 */
export function applyServiceStatuses(list) {
  if (!Array.isArray(list)) return;
  let refetch = false;
  const byId = new Map(state.services.map((s) => [s.id, s]));
  for (const st of list) {
    const s = byId.get(st.service_id);
    if (!s) { refetch = true; continue; }
    const held = s.status;
    if (held && statusChanged(held, st)) refetch = true;
    s.status = st;
    s.mode = st.mode || s.mode;
  }
  if (list.length !== state.services.length) refetch = true;
  emit('services', state.services);
  if (refetch) refreshServices().catch(() => {});
}

function statusChanged(held, st) {
  // config_updated_at moves with every saved edit, a rename included.
  if ((held.config_updated_at || '') !== (st.config_updated_at || '')) return true;
  if (!!held.running !== !!st.running) return true;
  if ((held.error || '') !== (st.error || '')) return true;
  if (st.mode && st.mode !== held.mode) return true;
  if (st.addr && held.addr && st.addr !== held.addr) return true;
  return !!(st.started_at && held.started_at && st.started_at !== held.started_at);
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
