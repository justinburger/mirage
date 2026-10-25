// Mirage UI bootstrap: theme, shell layout, router, SSE and the auth prompt.
import { h, svg, $ } from './js/dom.js';
import { icon } from './js/icons.js';
import { initTheme, toggleTheme, effective, onThemeChange } from './js/theme.js';
import { createRouter, href } from './js/router.js';
import { state, on, refreshServices, refreshReplays, refreshInfo, setConn } from './js/state.js';
import { startStream } from './js/sse.js';
import { setAuthHandler, clearToken, askForToken } from './js/api.js';
import { openModal, btn, textCtl, field, emptyState, alertBox } from './js/ui.js';
import { fmtInt } from './js/format.js';

initTheme();

// ------------------------------------------------------------ navigation
const NAV = [
  { id: 'dashboard', label: 'Dashboard', icon: 'dashboard', href: '#/' },
  { id: 'services', label: 'Services', icon: 'services', href: '#/services' },
  { id: 'recordings', label: 'Recordings', icon: 'recordings', href: '#/recordings' },
  { id: 'replays', label: 'Replays', icon: 'replays', href: '#/replays' },
  { id: 'profiles', label: 'Anonymization', icon: 'shield', href: '#/profiles' },
  { id: 'faults', label: 'Faults', icon: 'faults', href: '#/faults' },
  '-',
  { id: 'settings', label: 'Settings', icon: 'settings', href: '#/settings' },
];

const page = (file, exp) => () => import(`./js/pages/${file}.js`);
const ROUTES = [
  ['/', page('dashboard'), 'default', 'dashboard'],
  ['/services', page('services'), 'list', 'services'],
  ['/services/new', page('services'), 'list', 'services'],
  ['/services/:id', page('services'), 'detail', 'services'],
  ['/recordings', page('recordings'), 'list', 'recordings'],
  ['/recordings/:id', page('recordings'), 'detail', 'recordings'],
  ['/replays', page('replays'), 'list', 'replays'],
  ['/replays/new', page('replays'), 'create', 'replays'],
  ['/replays/compare', page('replays'), 'compare', 'replays'],
  ['/replays/:id', page('replays'), 'detail', 'replays'],
  ['/profiles', page('profiles'), 'list', 'profiles'],
  ['/profiles/new', page('profiles'), 'editor', 'profiles'],
  ['/profiles/:id', page('profiles'), 'editor', 'profiles'],
  ['/faults', page('faults'), 'default', 'faults'],
  ['/settings', page('settings'), 'default', 'settings'],
];

// ------------------------------------------------------------------ shell
function brandMark() {
  const s = svg('svg', { width: 28, height: 28, viewBox: '0 0 32 32', 'aria-hidden': 'true' },
    svg('rect', { width: 32, height: 32, rx: 8, fill: 'var(--accent-solid)' }),
    svg('path', { d: 'M7 16.5 16 7l9 9.5', fill: 'none', stroke: '#fff', 'stroke-width': 2.6, 'stroke-linecap': 'round', 'stroke-linejoin': 'round' }),
    svg('path', { d: 'M7 16.5 16 26l9-9.5', fill: 'none', stroke: '#fff', 'stroke-opacity': 0.55, 'stroke-width': 2.6, 'stroke-linecap': 'round', 'stroke-linejoin': 'round', 'stroke-dasharray': '1 5.2' }));
  return s;
}

const navLinks = new Map();
const badges = {};

function buildShell() {
  const root = $('#app');
  const nav = h('nav', { class: 'nav', 'aria-label': 'Main' });
  for (const n of NAV) {
    if (n === '-') { nav.appendChild(h('div', { class: 'nav-sep' })); continue; }
    const b = h('span', { class: 'nav-badge', hidden: true });
    badges[n.id] = b;
    const a = h('a', { class: 'nav-link', href: n.href, title: n.label }, icon(n.icon, 18), h('span', null, n.label), b);
    a.addEventListener('click', () => document.body.classList.remove('nav-open'));
    navLinks.set(n.id, a);
    nav.appendChild(a);
  }
  const sidebar = h('aside', { class: 'sidebar', id: 'sidebar' },
    h('a', { class: 'brand', href: '#/', title: 'Mirage' }, brandMark(), h('span', { class: 'brand-name' }, 'Mirage')),
    nav,
    h('div', { class: 'sidebar-foot' },
      h('span', { class: 'foot-text' }, 'Service virtualization & traffic replay')));

  const crumbs = h('div', { class: 'crumbs', id: 'crumbs' });
  const conn = h('div', { class: 'conn is-connecting', id: 'conn', role: 'status', 'aria-live': 'polite' }, h('span', { class: 'conn-dot' }), h('span', { class: 'conn-text' }, 'Connecting…'));
  conn.addEventListener('click', () => {
    if (state.conn !== 'unauthorized') return;
    askForToken().then(() => { refreshInfo().catch(() => {}); refreshServices().catch(() => {}); refreshReplays().catch(() => {}); router.resolve(); });
  });
  const ver = h('span', { class: 'version-tag', id: 'version' }, '');
  const themeBtn = h('button', { type: 'button', class: 'icon-btn', id: 'theme-toggle', title: 'Toggle light/dark theme', 'aria-label': 'Toggle light/dark theme', onclick: () => toggleTheme() });
  const paintTheme = () => { themeBtn.replaceChildren(icon(effective() === 'dark' ? 'sun' : 'moon', 18)); };
  paintTheme();
  onThemeChange(paintTheme);

  const topbar = h('header', { class: 'topbar' },
    h('button', { type: 'button', class: 'icon-btn menu-toggle', 'aria-label': 'Open navigation', onclick: () => document.body.classList.toggle('nav-open') }, icon('menu', 20)),
    crumbs,
    h('div', { class: 'topbar-right' }, conn, ver, themeBtn));
  const content = h('main', { class: 'content', id: 'main', tabindex: -1 });
  const skip = h('button', { type: 'button', class: 'skip-link', onclick: () => content.focus() }, 'Skip to content');
  root.replaceChildren(skip, sidebar, h('div', { class: 'main' }, topbar, content));
  document.addEventListener('click', (e) => {
    if (document.body.classList.contains('nav-open') && !e.target.closest('#sidebar') && !e.target.closest('.menu-toggle')) document.body.classList.remove('nav-open');
  });
  return { content, crumbs, conn, ver };
}

const shell = buildShell();

const CONN_TEXT = { connected: 'Live', connecting: 'Connecting…', reconnecting: 'Reconnecting…', unauthorized: 'Auth required' };
function paintConn() {
  const c = state.conn;
  shell.conn.className = `conn is-${c}`;
  shell.conn.lastChild.textContent = CONN_TEXT[c] || c;
  shell.conn.title = c === 'connected' ? 'Live metrics stream connected' : 'Live metrics stream: ' + (CONN_TEXT[c] || c);
}
on('conn', paintConn);
paintConn();

on('info', (info) => {
  shell.ver.textContent = info && info.version ? info.version : '';
});

function paintBadges() {
  const running = state.replays.filter((r) => r.status === 'running').length;
  const set = (id, n) => { const b = badges[id]; if (!b) return; b.hidden = !n; b.textContent = fmtInt(n); };
  set('services', state.services.length);
  set('replays', running);
}
on('services', paintBadges);
on('replays', paintBadges);

function onRoute({ nav, crumbs, path }) {
  for (const [id, a] of navLinks) {
    a.classList.toggle('is-on', id === nav);
    if (id === nav) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
  }
  let list = crumbs;
  if (!list) {
    const item = NAV.find((n) => n !== '-' && n.id === nav);
    list = [{ label: item ? item.label : 'Not found' }];
  }
  shell.crumbs.replaceChildren();
  list.forEach((c, i) => {
    const last = i === list.length - 1;
    if (i) shell.crumbs.appendChild(h('span', { class: 'sep', 'aria-hidden': 'true' }, '/'));
    shell.crumbs.appendChild(c.href && !last ? h('a', { href: c.href }, c.label) : h(last ? 'h1' : 'span', { class: 'cur', 'aria-current': last ? 'page' : null }, c.label));
  });
  const title = list[list.length - 1].label;
  document.title = `${title} · Mirage`;
}

function notFound(path, err) {
  const section = ({ services: 'services', recordings: 'recordings', replays: 'replays', profiles: 'profiles' })[path.split('/')[1]] || 'the dashboard';
  const retry = btn('Retry', { icon: 'refresh', kind: 'primary', onClick: () => window.dispatchEvent(new HashChangeEvent('hashchange')) });
  if (err) {
    const missing = err.status === 404;
    return h('div', { class: 'card' }, emptyState({
      icon: missing ? 'search' : 'alert',
      title: missing ? 'Not found' : 'Something went wrong',
      text: err.message || String(err),
      actions: [btn(`Back to ${section}`, { href: `#/${path.split('/')[1] || ''}`, kind: 'secondary' }), missing ? null : retry],
    }));
  }
  return h('div', { class: 'card' }, emptyState({ icon: 'search', title: 'Page not found', text: `There is no page at ${path}.`, actions: [btn('Go to dashboard', { href: '#/', kind: 'primary' })] }));
}

const router = createRouter({ routes: ROUTES, outlet: shell.content, onRoute, notFound });

// ------------------------------------------------------------ auth prompt
function promptToken(rejected) {
  return new Promise((resolve) => {
    let value = null;
    const input = textCtl({ type: 'password', placeholder: 'Admin token', mono: true });
    const err = h('div', { class: 'field-error', hidden: true });
    const submit = () => {
      const v = input.get().trim();
      if (!v) { err.textContent = 'Enter the token the server was started with (--admin-token).'; err.hidden = false; return; }
      value = v;
      m.close(v);
    };
    input.input.addEventListener('keydown', (e) => { if (e.key === 'Enter') submit(); });
    const m = openModal({
      title: 'Admin token required',
      size: 'sm',
      closeOnBackdrop: false,
      content: h('div', { class: 'stack-sm' },
        rejected ? alertBox('error', 'That token was not accepted. Check it and try again.') : null,
        h('p', { class: 'muted' }, 'This Mirage server is protected. Enter the admin token to continue. It is stored in this browser only.'),
        field('Token', input), err),
      footer: [btn('Continue', { kind: 'primary', onClick: submit })],
      onClose: () => { if (value) setConn('connecting'); resolve(value); },
    });
    setConn('unauthorized');
  });
}
setAuthHandler(promptToken);

// ------------------------------------------------------------------ start
startStream();
refreshInfo().catch(() => {});
refreshServices().catch(() => {});
refreshReplays().catch(() => {});
router.resolve();

// exposed for the dev harness and quick debugging in the console
window.__mirage = { state, router, href, clearToken };
