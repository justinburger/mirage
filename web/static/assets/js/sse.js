// Server-Sent Events client for /api/v1/stream with backoff reconnects.
import { apiUrl, api, getToken, onTokenChange } from './api.js';
import { applyStats, applyServiceStatuses, applyReplayUpdates, setConn, state } from './state.js';
import { ingestStats } from './series.js';

let es = null;
let timer = 0;
let backoff = 1000;
let started = false;
let everConnected = false;
let watchdog = 0;

function parse(e) {
  try { return JSON.parse(e.data); } catch (_) { return null; }
}

function connect(quiet) {
  clearTimeout(timer);
  if (es) { es.onerror = null; es.close(); es = null; }
  if (!quiet && state.conn !== 'unauthorized') setConn(everConnected ? 'reconnecting' : 'connecting');
  try {
    es = new EventSource(apiUrl('/stream'));
  } catch (_) {
    scheduleReconnect();
    return;
  }
  es.onopen = () => {
    everConnected = true;
    backoff = 1000;
    setConn('connected');
    armWatchdog();
  };
  es.addEventListener('stats', (e) => {
    const evt = parse(e);
    if (!evt) return;
    if (state.conn !== 'connected') setConn('connected');
    armWatchdog();
    ingestStats(evt);
    applyStats(evt);
  });
  es.addEventListener('services', (e) => {
    const d = parse(e);
    if (d) applyServiceStatuses(d);
  });
  es.addEventListener('replays', (e) => {
    const d = parse(e);
    if (d) applyReplayUpdates(d);
  });
  es.onerror = () => {
    if (es) { es.close(); es = null; }
    if (state.conn !== 'unauthorized') setConn('reconnecting');
    // A 401 closes an EventSource without telling us why. Probe a normal
    // endpoint: the fetch wrapper prompts for a token and we reconnect.
    api.info().then(() => {}, () => {});
    scheduleReconnect();
  };
}

function scheduleReconnect() {
  clearTimeout(timer);
  timer = setTimeout(connect, backoff);
  backoff = Math.min(backoff * 2, 15000);
}

// If the stream goes quiet for a long time (proxy buffering, sleeping laptop)
// reconnect rather than showing stale numbers as live.
function armWatchdog() {
  clearTimeout(watchdog);
  watchdog = setTimeout(() => {
    if (state.conn === 'connected') connect(true);
  }, 45000);
}

export function startStream() {
  if (started) return;
  started = true;
  onTokenChange(() => { backoff = 1000; connect(); });
  connect();
}

export function reconnectNow() {
  backoff = 1000;
  connect();
}

export { getToken };
