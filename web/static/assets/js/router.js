// Hash router with lazy-loaded page modules and per-page lifecycles.
import { on as busOn } from './state.js';

export function parseHash(hash = location.hash) {
  let h = hash.replace(/^#/, '');
  if (!h.startsWith('/')) h = '/' + h;
  const qi = h.indexOf('?');
  const path = (qi >= 0 ? h.slice(0, qi) : h).replace(/\/+$/, '') || '/';
  const query = {};
  if (qi >= 0) {
    for (const [k, v] of new URLSearchParams(h.slice(qi + 1))) query[k] = v;
  }
  return { path, query };
}

export function href(path, query) {
  let s = '#' + path;
  if (query) {
    const p = new URLSearchParams();
    for (const [k, v] of Object.entries(query)) if (v !== undefined && v !== null && v !== '') p.set(k, v);
    const q = p.toString();
    if (q) s += '?' + q;
  }
  return s;
}

export function navigate(path, query) {
  const target = href(path, query);
  if (location.hash === target) window.dispatchEvent(new HashChangeEvent('hashchange'));
  else location.hash = target;
}

/** Update the query string of the current route without re-mounting. */
export function replaceQuery(query) {
  const { path } = parseHash();
  history.replaceState(null, '', href(path, query));
}

function compile(pattern) {
  const keys = [];
  const re = new RegExp('^' + pattern.replace(/\/:([a-z_]+)/gi, (_, k) => { keys.push(k); return '/([^/]+)'; }) + '$');
  return { re, keys };
}

export function createRouter({ routes, outlet, onRoute, notFound }) {
  const table = routes.map(([pattern, load, exportName, nav]) => ({ pattern, ...compile(pattern), load, exportName: exportName || 'default', nav }));
  let current = null; // {cleanups, abort}
  let navSeq = 0;

  function teardown() {
    if (!current) return;
    const c = current;
    current = null;
    c.abort.abort();
    for (const fn of c.cleanups.splice(0)) {
      try { fn(); } catch (e) { console.error('cleanup failed', e); }
    }
  }

  async function resolve() {
    const seq = ++navSeq;
    const { path, query } = parseHash();
    let match = null;
    let params = {};
    for (const r of table) {
      const m = r.re.exec(path);
      if (m) {
        match = r;
        r.keys.forEach((k, i) => { params[k] = decodeURIComponent(m[i + 1]); });
        break;
      }
    }
    teardown();
    const cleanups = [];
    const abort = new AbortController();
    current = { cleanups, abort };
    const ctx = {
      path, query, params,
      signal: abort.signal,
      nav: match ? match.nav : null,
      isActive: () => seq === navSeq,
      onCleanup: (fn) => { cleanups.push(fn); return fn; },
      on: (topic, fn) => { const off = busOn(topic, fn); cleanups.push(off); return off; },
      every: (fn, ms) => { const id = setInterval(fn, ms); cleanups.push(() => clearInterval(id)); return id; },
      setCrumbs: (crumbs) => onRoute && onRoute({ path, nav: ctx.nav, crumbs, seq }),
    };
    if (onRoute) onRoute({ path, nav: ctx.nav, crumbs: null, seq });
    if (!match) {
      outlet.replaceChildren(notFound(path));
      return;
    }
    try {
      const mod = await match.load();
      if (seq !== navSeq) return;
      const render = mod[match.exportName];
      const node = await render(ctx);
      if (seq !== navSeq) { cleanups.splice(0).forEach((f) => { try { f(); } catch (_) { /* ignore */ } }); return; }
      outlet.replaceChildren(node);
      window.scrollTo(0, 0);
    } catch (e) {
      if (seq !== navSeq) return;
      if (!(e && e.status === 404)) console.error(e);
      outlet.replaceChildren(notFound(path, e));
    }
  }

  window.addEventListener('hashchange', resolve);
  return { resolve, teardown };
}
