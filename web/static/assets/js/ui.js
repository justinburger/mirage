// Reusable UI components: buttons, toasts, modals/drawers, form controls,
// tables, badges, tiles. Everything is built with h() so API data is escaped.
import { h, append, uid, copyText, clear } from './dom.js';
import { icon } from './icons.js';
import { errorText, ApiError } from './api.js';
import { parseDuration, fmtInt, DASH } from './format.js';

// ------------------------------------------------------------- toasts

let toastHost = null;
export function toast(message, kind = 'info', { timeout } = {}) {
  if (!toastHost) {
    toastHost = h('div', { class: 'toasts', 'aria-live': 'polite', 'aria-atomic': 'false' });
    document.body.appendChild(toastHost);
  }
  const ic = kind === 'error' ? 'alert' : kind === 'success' ? 'check' : 'info';
  const el = h('div', { class: `toast toast-${kind}`, role: kind === 'error' ? 'alert' : 'status' },
    h('span', { class: 'toast-icon' }, icon(ic, 16)),
    h('div', { class: 'toast-msg' }, message),
    h('button', { type: 'button', class: 'icon-btn toast-x', 'aria-label': 'Dismiss', onclick: () => dismiss() }, icon('x', 14)));
  let timer = 0;
  function dismiss() {
    clearTimeout(timer);
    el.classList.add('is-leaving');
    setTimeout(() => el.remove(), 180);
  }
  toastHost.appendChild(el);
  while (toastHost.children.length > 4) toastHost.firstChild.remove();
  timer = setTimeout(dismiss, timeout || (kind === 'error' ? 8000 : 3800));
  return dismiss;
}
export const toastError = (e) => toast(errorText(e), 'error');
export const toastOk = (m) => toast(m, 'success');

// ------------------------------------------------------------ buttons

export function btn(label, o = {}) {
  const { kind = 'secondary', icon: ic, size, onClick, title, disabled, href, type = 'button', cls, ariaLabel, target, download, iconRight } = o;
  const content = [ic ? icon(ic, size === 'sm' ? 14 : 16) : null, label ? h('span', { class: 'btn-label' }, label) : null, iconRight ? icon(iconRight, 14) : null];
  const classes = ['btn', `btn-${kind}`, size && `btn-${size}`, !label && ic && 'btn-icon', cls];
  let el;
  if (href) {
    el = h('a', { class: classes, href, title, target, download, rel: target === '_blank' ? 'noopener' : null, 'aria-label': ariaLabel || (!label ? title : null) }, content);
  } else {
    el = h('button', { class: classes, type, title, disabled: !!disabled, 'aria-label': ariaLabel || (!label ? title : null) }, content);
  }
  if (onClick) {
    el.addEventListener('click', async (e) => {
      if (el.disabled || el.classList.contains('is-busy')) return;
      let r;
      try { r = onClick(e, el); } catch (err) { toastError(err); return; }
      if (r && typeof r.then === 'function') {
        el.classList.add('is-busy');
        el.setAttribute('aria-busy', 'true');
        const wasDisabled = el.disabled;
        el.disabled = true;
        try { await r; } catch (err) { if (!(err && err.name === 'AbortError')) toastError(err); } finally {
          el.classList.remove('is-busy');
          el.removeAttribute('aria-busy');
          el.disabled = wasDisabled;
        }
      }
    });
  }
  return el;
}

export function iconBtn(name, title, onClick, o = {}) {
  return btn('', { icon: name, title, onClick, kind: 'ghost', size: 'sm', ...o });
}

// ----------------------------------------------------- overlays (modal)

const stack = [];
let escBound = false;

function appRoot() { return document.getElementById('app'); }

function updateInert() {
  const app = appRoot();
  const top = stack[stack.length - 1];
  if (app) app.inert = stack.length > 0;
  for (const s of stack) s.overlay.inert = s !== top;
}

function bindEsc() {
  if (escBound) return;
  escBound = true;
  document.addEventListener('keydown', (e) => {
    if (e.key !== 'Escape' || !stack.length) return;
    const top = stack[stack.length - 1];
    if (top.closeOnEsc === false) return;
    e.preventDefault();
    e.stopPropagation();
    top.close();
  }, true);
}

/**
 * Opens a modal dialog (or a right-hand drawer with drawer:true).
 * Returns {el, body, close(result), setTitle, setFooter}.
 */
export function openModal({ title, content, footer, size = 'md', drawer = false, onClose, closeOnBackdrop = true, closeOnEsc = true, initialFocus, className, subtitle } = {}) {
  bindEsc();
  const prevFocus = document.activeElement;
  const titleId = uid('modal-title');
  const titleEl = h('h2', { class: 'modal-title', id: titleId }, title || '');
  const closeBtn = h('button', { type: 'button', class: 'icon-btn', 'aria-label': 'Close dialog', onclick: () => api.close() }, icon('x', 18));
  const head = h('div', { class: 'modal-head' }, h('div', { class: 'modal-titles' }, titleEl, subtitle ? h('div', { class: 'modal-sub' }, subtitle) : null), closeBtn);
  const body = h('div', { class: 'modal-body' }, content);
  let foot = footer ? h('div', { class: 'modal-foot' }, footer) : null;
  const dialog = h('div', {
    class: ['modal', `modal-${size}`, drawer && 'drawer', className],
    role: 'dialog', 'aria-modal': 'true', 'aria-labelledby': titleId, tabindex: -1,
  }, head, body, foot);
  const overlay = h('div', { class: ['overlay', drawer && 'overlay-drawer'] }, dialog);
  // Enter in a single-line field submits the primary action
  body.addEventListener('keydown', (e) => {
    if (e.key !== 'Enter' || e.defaultPrevented || e.isComposing || e.shiftKey) return;
    if (!(e.target instanceof HTMLInputElement) || ['checkbox', 'radio', 'file', 'range', 'button'].includes(e.target.type)) return;
    const primary = dialog.querySelector('.modal-foot .btn-primary, .modal-foot .btn-danger');
    if (primary && !primary.disabled) { e.preventDefault(); primary.click(); }
  });
  let downOnBackdrop = false;
  overlay.addEventListener('mousedown', (e) => { downOnBackdrop = e.target === overlay; });
  overlay.addEventListener('click', (e) => {
    if (closeOnBackdrop && downOnBackdrop && e.target === overlay) api.close();
    downOnBackdrop = false;
  });
  document.body.appendChild(overlay);
  let closed = false;
  const api = {
    el: dialog, body, overlay, closeOnEsc,
    setTitle: (t) => { titleEl.textContent = t; },
    setFooter(nodes) {
      const nf = h('div', { class: 'modal-foot' }, nodes);
      if (foot) foot.replaceWith(nf); else dialog.appendChild(nf);
      foot = nf;
    },
    close(result) {
      if (closed) return;
      closed = true;
      const i = stack.indexOf(api);
      if (i >= 0) stack.splice(i, 1);
      overlay.classList.add('is-closing');
      updateInert();
      const done = () => {
        overlay.remove();
        if (prevFocus && prevFocus.isConnected && typeof prevFocus.focus === 'function') {
          try { prevFocus.focus({ preventScroll: true }); } catch (_) { /* ignore */ }
        }
        if (onClose) onClose(result);
      };
      if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) done();
      else setTimeout(done, 120);
    },
  };
  stack.push(api);
  updateInert();
  requestAnimationFrame(() => {
    let f = null;
    if (initialFocus) f = typeof initialFocus === 'string' ? body.querySelector(initialFocus) : initialFocus;
    if (!f) f = body.querySelector('input:not([type=hidden]):not([disabled]), select, textarea');
    if (!f) f = dialog.querySelector('.modal-foot .btn-primary, .modal-foot .btn-danger');
    (f || dialog).focus({ preventScroll: true });
  });
  return api;
}

export function confirmDialog({ title, message, confirmText = 'Confirm', cancelText = 'Cancel', danger = false } = {}) {
  return new Promise((resolve) => {
    let result = false;
    const m = openModal({
      title, size: 'sm',
      content: h('div', { class: 'confirm-msg' }, message),
      footer: [
        btn(cancelText, { kind: 'secondary', onClick: () => m.close(false) }),
        btn(confirmText, { kind: danger ? 'danger' : 'primary', onClick: () => { result = true; m.close(true); } }),
      ],
      onClose: () => resolve(result),
    });
    // focus the safe choice for destructive actions
    requestAnimationFrame(() => {
      const b = m.el.querySelector(danger ? '.btn-secondary' : '.btn-primary');
      if (b) b.focus();
    });
  });
}

// ----------------------------------------------------------- popup menu

let openMenu = null;
export function popMenu(anchor, items) {
  closeMenu();
  const menu = h('div', { class: 'menu', role: 'menu' });
  for (const it of items) {
    if (it === '-') { menu.appendChild(h('div', { class: 'menu-sep', role: 'separator' })); continue; }
    const content = [it.icon ? icon(it.icon, 15) : null, h('span', null, it.label)];
    const el = it.href
      ? h('a', { class: ['menu-item', it.danger && 'danger'], role: 'menuitem', href: it.href, download: it.download, target: it.target, onclick: () => closeMenu() }, content)
      : h('button', {
        type: 'button', class: ['menu-item', it.danger && 'danger'], role: 'menuitem', disabled: !!it.disabled,
        onclick: async () => { closeMenu(); try { await it.onClick(); } catch (e) { toastError(e); } },
      }, content);
    menu.appendChild(el);
  }
  document.body.appendChild(menu);
  const r = anchor.getBoundingClientRect();
  const mw = menu.offsetWidth, mh = menu.offsetHeight;
  let left = Math.min(window.innerWidth - mw - 8, Math.max(8, r.right - mw));
  let top = r.bottom + 6;
  if (top + mh > window.innerHeight - 8) top = Math.max(8, r.top - mh - 6);
  menu.style.left = left + 'px';
  menu.style.top = top + 'px';
  const first = menu.querySelector('.menu-item');
  if (first) first.focus();
  const onDown = (e) => { if (!menu.contains(e.target) && e.target !== anchor && !anchor.contains(e.target)) closeMenu(); };
  const onKey = (e) => {
    if (e.key === 'Escape') { e.stopPropagation(); closeMenu(); anchor.focus(); }
    else if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      const its = Array.from(menu.querySelectorAll('.menu-item:not([disabled])'));
      const i = its.indexOf(document.activeElement);
      its[(i + (e.key === 'ArrowDown' ? 1 : -1) + its.length) % its.length].focus();
    }
  };
  setTimeout(() => document.addEventListener('mousedown', onDown), 0);
  document.addEventListener('keydown', onKey, true);
  window.addEventListener('hashchange', closeMenu, { once: true });
  openMenu = { menu, cleanup() { document.removeEventListener('mousedown', onDown); document.removeEventListener('keydown', onKey, true); } };
  return menu;
}
export function closeMenu() {
  if (!openMenu) return;
  openMenu.cleanup();
  openMenu.menu.remove();
  openMenu = null;
}

// -------------------------------------------------------------- badges

const MODE_LABEL = { passthrough: 'Passthrough', record: 'Record', virtualize: 'Virtualize', hybrid: 'Hybrid' };
export const modeLabel = (m) => MODE_LABEL[m] || m || DASH;
export function modeBadge(mode, { compact } = {}) {
  return h('span', { class: `badge mode-${mode}`, title: `Mode: ${modeLabel(mode)}` }, h('span', { class: 'badge-dot' }), compact ? null : modeLabel(mode));
}

export function badge(label, kind = 'muted', { dot = false, title } = {}) {
  return h('span', { class: `badge badge-${kind}`, title }, dot ? h('span', { class: 'badge-dot' }) : null, label);
}

const REPLAY_BADGE = { pending: 'muted', running: 'info', completed: 'ok', stopped: 'warn', failed: 'err' };
export function replayBadge(status) {
  return h('span', { class: `badge cap badge-${REPLAY_BADGE[status] || 'muted'} ${status === 'running' ? 'is-live' : ''}` }, h('span', { class: 'badge-dot' }), status || DASH);
}

export function statusPill(status) {
  const c = !status ? 'err' : status >= 500 ? 'err' : status === 429 ? 'err' : status >= 400 ? 'warn' : status >= 300 ? 'info' : 'ok';
  return h('span', { class: `pill pill-${c}` }, String(status || 'ERR'));
}

export function methodTag(method) {
  return h('span', { class: `method method-${String(method).toLowerCase()}` }, method);
}

export function dot(kind, title) {
  return h('span', { class: `dot dot-${kind}`, title, role: title ? 'img' : null, 'aria-label': title });
}

// ----------------------------------------------------- layout widgets

export function card({ title, subtitle, actions, body, className, flush } = {}) {
  const head = (title || actions)
    ? h('div', { class: 'card-head' },
      h('div', { class: 'card-titles' }, title ? h('h3', { class: 'card-title' }, title) : null, subtitle ? h('div', { class: 'card-sub' }, subtitle) : null),
      actions ? h('div', { class: 'card-actions' }, actions) : null)
    : null;
  return h('section', { class: ['card', flush && 'card-flush', className] }, head, body);
}

export function emptyState({ icon: ic = 'info', title, text, actions } = {}) {
  return h('div', { class: 'empty' },
    h('div', { class: 'empty-icon' }, icon(ic, 28)),
    h('div', { class: 'empty-title' }, title),
    text ? h('div', { class: 'empty-text' }, text) : null,
    actions ? h('div', { class: 'empty-actions' }, actions) : null);
}

export function alertBox(kind, content, { title } = {}) {
  return h('div', { class: `alert alert-${kind}`, role: kind === 'error' ? 'alert' : 'status' },
    h('span', { class: 'alert-icon' }, icon(kind === 'error' ? 'alert' : kind === 'warn' ? 'alert' : kind === 'success' ? 'check' : 'info', 16)),
    h('div', { class: 'alert-body' }, title ? h('div', { class: 'alert-title' }, title) : null, content));
}

/** Server validation error box (error + problems[]). Returns null element to clear. */
export function errorBox(err) {
  if (!err) return null;
  const problems = err instanceof ApiError ? err.problems : [];
  return alertBox('error', problems.length
    ? h('ul', { class: 'problems' }, problems.map((p) => h('li', null, p)))
    : null, { title: err.message || 'Request failed' });
}

export function statTile({ label, value, sub, tone, spark, hint } = {}) {
  const v = h('div', { class: 'tile-value' }, value == null ? DASH : value);
  const s = h('div', { class: 'tile-sub' }, sub || '');
  const el = h('div', { class: ['tile', tone && `tone-${tone}`], title: hint }, h('div', { class: 'tile-label' }, label), v, s, spark ? h('div', { class: 'tile-spark' }, spark) : null);
  return {
    el,
    set(value2, sub2, tone2) {
      v.textContent = value2 == null ? DASH : value2;
      if (sub2 !== undefined) s.textContent = sub2;
      if (tone2 !== undefined) {
        el.className = ['tile', tone2 && `tone-${tone2}`].filter(Boolean).join(' ');
      }
    },
  };
}

export function progressBar(frac, { tone, indeterminate, label } = {}) {
  const fill = h('div', { class: 'progress-fill' });
  const el = h('div', { class: ['progress', tone && `progress-${tone}`, indeterminate && 'is-indeterminate'], role: 'progressbar', 'aria-valuemin': 0, 'aria-valuemax': 100, 'aria-label': label || 'Progress' }, fill);
  function set(f, o = {}) {
    const ind = o.indeterminate != null ? o.indeterminate : indeterminate;
    el.classList.toggle('is-indeterminate', !!ind);
    if (!ind && Number.isFinite(f)) {
      const pct = Math.max(0, Math.min(1, f)) * 100;
      fill.style.width = pct + '%';
      el.setAttribute('aria-valuenow', String(Math.round(pct)));
    }
    if (o.tone !== undefined) el.className = el.className.replace(/progress-\w+/g, '').trim() + (o.tone ? ` progress-${o.tone}` : '');
  }
  set(frac);
  return { el, set };
}

export function collapsible(title, content, { open = false, hint } = {}) {
  const d = h('details', { class: 'collapsible', open: !!open },
    h('summary', null, icon('chevron-right', 14, 'caret'), h('span', { class: 'collapsible-title' }, title), hint ? h('span', { class: 'collapsible-hint' }, hint) : null),
    h('div', { class: 'collapsible-body' }, content));
  return d;
}

/** Definition list from [[label, value], ...]. */
export function kvList(items, { cls } = {}) {
  const dl = h('dl', { class: ['kvl', cls] });
  for (const [k, v, mono] of items) {
    if (v === undefined) continue;
    dl.appendChild(h('dt', null, k));
    dl.appendChild(h('dd', { class: mono ? 'mono' : null }, v == null || v === '' ? DASH : v));
  }
  return dl;
}

export function copyButton(getText, { label, title = 'Copy to clipboard', size = 'sm' } = {}) {
  return btn(label || '', {
    icon: 'copy', size, kind: 'ghost', title,
    onClick: async (e, el) => {
      const ok = await copyText(typeof getText === 'function' ? getText() : getText);
      if (ok) {
        toast('Copied to clipboard', 'success', { timeout: 1600 });
        el.classList.add('is-copied');
        setTimeout(() => el.classList.remove('is-copied'), 1200);
      } else toast('Copy failed. Select the text and copy manually.', 'error');
    },
  });
}

export function spinner() {
  return h('span', { class: 'spinner', role: 'status', 'aria-label': 'Loading' });
}

export function loading(textStr = 'Loading…') {
  return h('div', { class: 'loading' }, spinner(), h('span', null, textStr));
}

export function pageHeader({ title, subtitle, actions } = {}) {
  return h('div', { class: 'page-head' },
    h('div', { class: 'page-titles' }, title ? h('h2', { class: 'page-title' }, title) : null, subtitle ? h('p', { class: 'page-sub' }, subtitle) : null),
    actions ? h('div', { class: 'page-actions' }, actions) : null);
}

// -------------------------------------------------------------- tables

/**
 * Sortable keyed table.
 * columns: [{key,label,align,sortable,sortValue(row),render(row)->Node|string,sig(row),cls,width,title}]
 */
export function dataTable({ columns, rowKey = (r) => r.id, sortKey = null, sortDir = 'asc', onRowClick, rowClass, empty, dense = false, className, caption } = {}) {
  let rows = [];
  let sk = sortKey, sd = sortDir;
  const trs = new Map();
  const thead = h('thead');
  const tbody = h('tbody');
  const table = h('table', { class: ['table', dense && 'table-dense', className] }, caption ? h('caption', { class: 'sr-only' }, caption) : null, thead, tbody);
  const emptyHost = h('div', { class: 'table-empty', hidden: true });
  const wrap = h('div', { class: 'table-wrap' }, table, emptyHost);

  const ths = columns.map((c) => {
    const th = h('th', { scope: 'col', class: [c.align === 'right' && 'num', c.sortable !== false && c.label && 'sortable', c.cls], title: c.title, style: c.width ? { width: c.width } : null });
    if (c.sortable !== false && c.label) {
      const b = h('button', { type: 'button', class: 'th-btn', onclick: () => setSort(c.key) }, h('span', null, c.label), h('span', { class: 'sort-ind', 'aria-hidden': 'true' }));
      th.appendChild(b);
    } else th.textContent = c.label || '';
    return th;
  });
  thead.appendChild(h('tr', null, ths));

  function sortVal(c, r) {
    if (c.sortValue) return c.sortValue(r);
    const v = r[c.key];
    return v;
  }
  function paintHeader() {
    columns.forEach((c, i) => {
      const th = ths[i];
      const active = sk === c.key;
      th.setAttribute('aria-sort', active ? (sd === 'asc' ? 'ascending' : 'descending') : 'none');
      th.classList.toggle('is-sorted', active);
      const ind = th.querySelector('.sort-ind');
      if (ind) ind.textContent = active ? (sd === 'asc' ? '▲' : '▼') : '';
    });
  }
  function setSort(key) {
    if (sk === key) sd = sd === 'asc' ? 'desc' : 'asc';
    else { sk = key; const c = columns.find((x) => x.key === key); sd = c && c.align === 'right' ? 'desc' : 'asc'; }
    paintHeader();
    render();
  }
  function sorted() {
    if (!sk) return rows;
    const c = columns.find((x) => x.key === sk);
    if (!c) return rows;
    const dir = sd === 'asc' ? 1 : -1;
    return rows.slice().sort((a, b) => {
      const x = sortVal(c, a), y = sortVal(c, b);
      if (x == null && y == null) return 0;
      if (x == null) return 1;
      if (y == null) return -1;
      if (typeof x === 'number' && typeof y === 'number') return (x - y) * dir;
      return String(x).localeCompare(String(y), undefined, { numeric: true, sensitivity: 'base' }) * dir;
    });
  }
  function makeRow(r) {
    const tr = h('tr', { class: onRowClick ? 'clickable' : null });
    tr._cells = columns.map((c) => {
      const td = h('td', { class: [c.align === 'right' && 'num', c.cls], 'data-label': c.label || '' });
      tr.appendChild(td);
      return td;
    });
    if (onRowClick) {
      tr.addEventListener('click', (e) => {
        if (e.target.closest('a, button, input, select, textarea, label, summary')) return;
        const sel = window.getSelection && window.getSelection();
        if (sel && String(sel).length) return;
        onRowClick(tr._row, e);
      });
    }
    return tr;
  }
  function render() {
    const list = sorted();
    emptyHost.hidden = list.length > 0;
    table.hidden = list.length === 0;
    if (!list.length && empty) {
      clear(emptyHost);
      append(emptyHost, [typeof empty === 'function' ? empty() : empty]);
    }
    const seen = new Set();
    list.forEach((r, i) => {
      const key = rowKey(r);
      seen.add(key);
      let tr = trs.get(key);
      if (!tr) { tr = makeRow(r); trs.set(key, tr); }
      tr._row = r;
      const extra = rowClass ? rowClass(r) : '';
      tr.className = [onRowClick ? 'clickable' : '', extra].filter(Boolean).join(' ');
      columns.forEach((c, ci) => {
        const td = tr._cells[ci];
        const v = c.render ? c.render(r) : r[c.key];
        if (v instanceof Node) {
          const sig = c.sig ? c.sig(r) : null;
          if (sig != null && td._sig === sig) return;
          td._sig = sig;
          td._txt = undefined;
          td.replaceChildren(v);
        } else {
          const t = v == null || v === '' ? DASH : String(v);
          if (td._txt !== t) { td.textContent = t; td._txt = t; td._sig = undefined; }
        }
      });
      if (tbody.children[i] !== tr) tbody.insertBefore(tr, tbody.children[i] || null);
    });
    for (const [key, tr] of trs) if (!seen.has(key)) { tr.remove(); trs.delete(key); }
  }
  paintHeader();
  return {
    el: wrap,
    setRows(r) { rows = r || []; render(); },
    getRows: () => rows,
    setSort(key, dir) { sk = key; sd = dir || sd; paintHeader(); render(); },
  };
}

// ---------------------------------------------------------- form controls

function ctl(el, input, get, set) {
  const listeners = [];
  const c = {
    el, input, get, set,
    onChange(fn) { listeners.push(fn); return c; },
    fire() { for (const f of listeners) f(c.get()); },
    setDisabled(b) {
      el.querySelectorAll('input, select, textarea, button').forEach((x) => { x.disabled = !!b; });
      if (input) input.disabled = !!b;
      el.classList.toggle('is-disabled', !!b);
    },
  };
  return c;
}

export function textCtl({ value = '', placeholder = '', mono = false, type = 'text', maxLength, list, required, autocomplete = 'off', name } = {}) {
  const input = h('input', { class: ['input', mono && 'mono'], type, placeholder, value, maxlength: maxLength, list, required, autocomplete, spellcheck: 'false', name });
  const c = ctl(input, input, () => input.value, (v) => { input.value = v == null ? '' : v; });
  input.addEventListener('input', () => c.fire());
  return c;
}

export function textareaCtl({ value = '', placeholder = '', rows = 4, mono = true } = {}) {
  const input = h('textarea', { class: ['textarea', mono && 'mono'], placeholder, rows, spellcheck: 'false', value });
  const c = ctl(input, input, () => input.value, (v) => { input.value = v == null ? '' : v; });
  input.addEventListener('input', () => c.fire());
  return c;
}

export function numCtl({ value, min, max, step = 'any', placeholder = '', unit, mono = true } = {}) {
  const input = h('input', { class: ['input', mono && 'mono'], type: 'number', min, max, step, placeholder, inputmode: 'decimal', value: value == null ? '' : value });
  const el = unit ? h('div', { class: 'input-group' }, input, h('span', { class: 'input-unit' }, unit)) : input;
  const c = ctl(el, input, () => {
    if (input.value === '') return null;
    const n = Number(input.value);
    return Number.isFinite(n) ? n : null;
  }, (v) => { input.value = v == null || v === '' ? '' : String(v); });
  input.addEventListener('input', () => c.fire());
  return c;
}

/** Text input for Go durations ("250ms", "1m30s"); get() returns the string. */
export function durCtl({ value = '', placeholder = 'e.g. 250ms', allowEmpty = true } = {}) {
  const input = h('input', { class: 'input mono', type: 'text', placeholder, value, autocomplete: 'off', spellcheck: 'false', inputmode: 'text' });
  const c = ctl(input, input, () => input.value.trim(), (v) => { input.value = v == null ? '' : v; check(); });
  function check() {
    const s = input.value.trim();
    const bad = s !== '' && parseDuration(s) == null;
    input.classList.toggle('is-invalid', bad);
    input.setAttribute('aria-invalid', bad ? 'true' : 'false');
    return !bad;
  }
  c.valid = () => check() && (allowEmpty || input.value.trim() !== '');
  c.ms = () => { const s = input.value.trim(); return s === '' ? null : parseDuration(s); };
  input.addEventListener('input', () => { check(); c.fire(); });
  return c;
}

export function selectCtl(options, { value } = {}) {
  const input = h('select', { class: 'select' });
  const c = ctl(input, input, () => input.value, (v) => { input.value = v == null ? '' : v; });
  c.setOptions = (opts, keep = true) => {
    const cur = input.value;
    input.replaceChildren();
    for (const o of opts) {
      const op = typeof o === 'string' ? { value: o, label: o } : o;
      input.appendChild(h('option', { value: op.value, disabled: !!op.disabled }, op.label));
    }
    if (keep && opts.some((o) => (typeof o === 'string' ? o : o.value) === cur)) input.value = cur;
  };
  c.setOptions(options, false);
  if (value !== undefined) input.value = value;
  input.addEventListener('change', () => c.fire());
  return c;
}

export function switchCtl({ label, checked = false, help } = {}) {
  const input = h('input', { type: 'checkbox', role: 'switch', checked: !!checked, class: 'switch-input' });
  const el = h('label', { class: 'switch' }, input, h('span', { class: 'switch-track', 'aria-hidden': 'true' }, h('span', { class: 'switch-thumb' })),
    label ? h('span', { class: 'switch-text' }, label, help ? h('span', { class: 'switch-help' }, help) : null) : null);
  const c = ctl(el, input, () => input.checked, (v) => { input.checked = !!v; });
  input.addEventListener('change', () => c.fire());
  return c;
}

export function checkCtl({ label, checked = false, desc, example } = {}) {
  const input = h('input', { type: 'checkbox', checked: !!checked, class: 'check-input' });
  const el = h('label', { class: 'check' }, input, h('span', { class: 'check-box', 'aria-hidden': 'true' }, icon('check', 12)),
    h('span', { class: 'check-text' }, h('span', { class: 'check-label' }, label), desc ? h('span', { class: 'check-desc' }, desc) : null, example ? h('code', { class: 'check-ex' }, example) : null));
  const c = ctl(el, input, () => input.checked, (v) => { input.checked = !!v; });
  input.addEventListener('change', () => c.fire());
  return c;
}

/** Segmented radio group. options: [{value,label,title,cls}] */
export function segmentedCtl(options, { value, label, size } = {}) {
  let cur = value !== undefined ? value : options[0].value;
  const el = h('div', { class: ['seg', size && `seg-${size}`], role: 'radiogroup', 'aria-label': label });
  const btns = options.map((o) => {
    const b = h('button', {
      type: 'button', class: ['seg-btn', o.cls], role: 'radio', title: o.title, 'data-value': o.value,
      onclick: () => select(o.value, true),
    }, o.icon ? icon(o.icon, 14) : null, o.label);
    el.appendChild(b);
    return b;
  });
  function paint() {
    btns.forEach((b, i) => {
      const on = options[i].value === cur;
      b.setAttribute('aria-checked', String(on));
      b.tabIndex = on ? 0 : -1;
      b.classList.toggle('is-on', on);
    });
  }
  function select(v, fire) {
    if (v === cur && fire) return;
    cur = v;
    paint();
    if (fire) c.fire();
  }
  el.addEventListener('keydown', (e) => {
    const dir = e.key === 'ArrowRight' || e.key === 'ArrowDown' ? 1 : e.key === 'ArrowLeft' || e.key === 'ArrowUp' ? -1 : 0;
    if (!dir) return;
    e.preventDefault();
    const i = options.findIndex((o) => o.value === cur);
    const n = options[(i + dir + options.length) % options.length];
    select(n.value, true);
    btns[options.indexOf(n)].focus();
  });
  const c = ctl(el, btns[0], () => cur, (v) => select(v, false));
  paint();
  return c;
}

/** Tag/chip editor. get() -> string[] */
export function chipsCtl({ value = [], placeholder = 'Type and press Enter', validate, lower = false } = {}) {
  let items = [];
  const input = h('input', { class: 'chips-input', type: 'text', placeholder, autocomplete: 'off', spellcheck: 'false' });
  const err = h('div', { class: 'chips-error', hidden: true });
  const wrap = h('div', { class: 'chips' }, input);
  const el = h('div', { class: 'chips-wrap' }, wrap, err);
  const c = ctl(el, input, () => { commit(); return items.slice(); }, (v) => { items = (v || []).slice(); paint(); });
  function paint() {
    wrap.querySelectorAll('.chip').forEach((x) => x.remove());
    items.forEach((it, i) => {
      wrap.insertBefore(h('span', { class: 'chip' }, h('span', { class: 'chip-text' }, it), h('button', { type: 'button', class: 'chip-x', 'aria-label': `Remove ${it}`, onclick: () => { items.splice(i, 1); paint(); c.fire(); input.focus(); } }, icon('x', 12))), input);
    });
  }
  function add(raw) {
    let v = raw.trim();
    if (!v) return true;
    if (lower) v = v.toLowerCase();
    if (validate) {
      const m = validate(v);
      if (m) { err.textContent = m; err.hidden = false; return false; }
    }
    err.hidden = true;
    if (!items.includes(v)) items.push(v);
    return true;
  }
  function commit() {
    if (input.value.trim()) { if (add(input.value)) { input.value = ''; paint(); } }
  }
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === ',') {
      e.preventDefault();
      if (add(input.value)) { input.value = ''; paint(); c.fire(); }
    } else if (e.key === 'Backspace' && !input.value && items.length) {
      items.pop(); paint(); c.fire();
    }
  });
  input.addEventListener('paste', (e) => {
    const t = (e.clipboardData || window.clipboardData).getData('text');
    if (/[\n,]/.test(t)) {
      e.preventDefault();
      t.split(/[\n,]+/).forEach(add);
      paint(); c.fire();
    }
  });
  input.addEventListener('blur', () => { if (input.value.trim() && add(input.value)) { input.value = ''; paint(); c.fire(); } });
  wrap.addEventListener('click', (e) => { if (e.target === wrap) input.focus(); });
  c.set(value);
  return c;
}

/** Key/value rows editor. get() -> object */
export function kvCtl({ value = {}, keyPlaceholder = 'Header', valuePlaceholder = 'Value', addLabel = 'Add' } = {}) {
  const list = h('div', { class: 'kv-rows' });
  const el = h('div', { class: 'kv-editor' }, list);
  const c = ctl(el, null, () => {
    const o = {};
    list.querySelectorAll('.kv-row').forEach((r) => {
      const k = r.children[0].value.trim();
      if (k) o[k] = r.children[1].value;
    });
    return o;
  }, (v) => {
    list.replaceChildren();
    Object.entries(v || {}).forEach(([k, val]) => addRow(k, val));
  });
  function addRow(k = '', v = '') {
    const kin = h('input', { class: 'input mono', type: 'text', placeholder: keyPlaceholder, value: k, spellcheck: 'false', autocomplete: 'off', 'aria-label': keyPlaceholder });
    const vin = h('input', { class: 'input mono', type: 'text', placeholder: valuePlaceholder, value: v, spellcheck: 'false', autocomplete: 'off', 'aria-label': valuePlaceholder });
    const row = h('div', { class: 'kv-row' }, kin, vin, h('button', { type: 'button', class: 'icon-btn', 'aria-label': 'Remove row', onclick: () => { row.remove(); c.fire(); } }, icon('x', 14)));
    kin.addEventListener('input', () => c.fire());
    vin.addEventListener('input', () => c.fire());
    list.appendChild(row);
    return row;
  }
  el.appendChild(btn(addLabel, { icon: 'plus', size: 'sm', kind: 'ghost', onClick: () => { addRow().children[0].focus(); } }));
  c.input = null;
  c.set(value);
  return c;
}

/** Textarea of one item per line. get() -> string[] */
export function linesCtl({ value = [], placeholder = '', rows = 3, validate } = {}) {
  const input = h('textarea', { class: 'textarea mono', rows, placeholder, spellcheck: 'false' });
  const err = h('div', { class: 'chips-error', hidden: true });
  const el = h('div', null, input, err);
  const get = () => input.value.split('\n').map((s) => s.trim()).filter(Boolean);
  const c = ctl(el, input, get, (v) => { input.value = (v || []).join('\n'); });
  c.validate = () => {
    if (!validate) return null;
    for (const line of get()) {
      const m = validate(line);
      if (m) { err.textContent = `${line}: ${m}`; err.hidden = false; return m; }
    }
    err.hidden = true;
    return null;
  };
  input.addEventListener('input', () => { c.validate(); c.fire(); });
  c.set(value);
  return c;
}

/** Slider + number + preset buttons. get() -> number|null. opts.log = log scale. */
export function sliderCtl({ min = 0, max = 100, step = 1, value = 1, presets = [], log = false, format = (v) => String(v), unit, numMin, numMax } = {}) {
  const input = h('input', { class: 'slider-input', type: 'number', min: numMin != null ? numMin : min, max: numMax != null ? numMax : max, step: 'any', value, inputmode: 'decimal' });
  const range = h('input', { class: 'range', type: 'range', min: 0, max: 1000, step: 1, 'aria-label': 'Slider' });
  const presetBtns = presets.map((p) => h('button', { type: 'button', class: 'preset', onclick: () => { setV(p); c.fire(); } }, typeof p === 'number' ? format(p) : p));
  const el = h('div', { class: 'slider' }, range, h('div', { class: 'slider-num' }, input, unit ? h('span', { class: 'input-unit' }, unit) : null), presets.length ? h('div', { class: 'presets' }, presetBtns) : null);
  const toPos = (v) => {
    v = Math.min(max, Math.max(min, v));
    if (log) return Math.round((Math.log(v / min) / Math.log(max / min)) * 1000);
    return Math.round(((v - min) / (max - min)) * 1000);
  };
  const fromPos = (p) => {
    const f = p / 1000;
    let v = log ? min * Math.pow(max / min, f) : min + f * (max - min);
    const st = step;
    v = Math.round(v / st) * st;
    return Number(v.toFixed(6));
  };
  function paint() {
    const n = Number(input.value);
    if (Number.isFinite(n) && input.value !== '') range.value = toPos(n);
    presetBtns.forEach((b, i) => b.classList.toggle('is-on', Number(input.value) === presets[i]));
    range.style.setProperty('--fill', (range.value / 10) + '%');
  }
  function setV(v) { input.value = v == null ? '' : String(v); paint(); }
  const c = ctl(el, input, () => { if (input.value === '') return null; const n = Number(input.value); return Number.isFinite(n) ? n : null; }, setV);
  range.addEventListener('input', () => { input.value = String(fromPos(Number(range.value))); paint(); c.fire(); });
  input.addEventListener('input', () => { paint(); c.fire(); });
  paint();
  return c;
}

/**
 * Wraps a control with label/help/error.
 * Returns an element with setError(msg) and .ctl.
 */
export function field(label, control, { help, required, className, labelExtra } = {}) {
  const id = uid('f');
  const inputEl = control.input;
  if (inputEl) inputEl.id = id;
  const errEl = h('div', { class: 'field-error', role: 'alert', hidden: true });
  const lab = h(inputEl ? 'label' : 'div', { class: 'field-label', for: inputEl ? id : null, id: inputEl ? null : id + '-l' }, label, required ? h('span', { class: 'req', 'aria-hidden': 'true' }, ' *') : null, labelExtra || null);
  if (!inputEl && control.el) control.el.setAttribute('aria-labelledby', id + '-l');
  const el = h('div', { class: ['field', className] }, lab, control.el, help != null && help !== false ? h('div', { class: 'field-help' }, help) : null, errEl);
  el.setError = (msg) => {
    errEl.textContent = msg || '';
    errEl.hidden = !msg;
    el.classList.toggle('has-error', !!msg);
    if (inputEl) inputEl.setAttribute('aria-invalid', msg ? 'true' : 'false');
  };
  el.ctl = control;
  return el;
}

/** Group fields in a form section. */
export function formSection(title, desc, ...children) {
  return h('fieldset', { class: 'fsec' },
    h('legend', { class: 'fsec-title' }, title),
    desc ? h('p', { class: 'fsec-desc' }, desc) : null,
    h('div', { class: 'fsec-body' }, children));
}

export function formGrid(...children) {
  return h('div', { class: 'fgrid' }, children);
}

/**
 * Highlight fields whose name appears in server problems. fieldMap:
 * { 'listen_addr': fieldEl, ... }.
 */
export function applyProblems(fieldMap, err) {
  for (const f of Object.values(fieldMap)) if (f && f.setError) f.setError('');
  if (!(err instanceof ApiError)) return;
  const msgs = err.problems.length ? err.problems : [err.message];
  const keys = Object.keys(fieldMap).sort((a, b) => b.length - a.length);
  for (const m of msgs) {
    for (const k of keys) {
      if (m.includes(k)) { fieldMap[k].setError(m); break; }
    }
  }
}

export function scrollToFirstError(root) {
  const el = root.querySelector('.has-error, .is-invalid, .alert-error');
  if (el) el.scrollIntoView({ block: 'center', behavior: 'smooth' });
}

export function tabLinks(items, current) {
  return h('nav', { class: 'tabs', role: 'tablist' }, items.map((it) => h('a', { class: ['tab', it.id === current && 'is-on'], href: it.href, role: 'tab', 'aria-selected': String(it.id === current) }, it.label)));
}

export { fmtInt };
