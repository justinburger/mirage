// Tiny DOM helpers. Everything that touches API data goes through text nodes
// or setAttribute, never innerHTML: recorded bodies and headers are untrusted.

const SVG_NS = 'http://www.w3.org/2000/svg';
const BOOL_PROPS = new Set([
  'disabled', 'checked', 'selected', 'readOnly', 'required', 'hidden',
  'multiple', 'autofocus', 'open', 'indeterminate', 'inert',
]);

function applyProps(el, props, isSvg) {
  if (!props) return null;
  let deferredValue;
  let hasValue = false;
  for (const k of Object.keys(props)) {
    const v = props[k];
    if (k === 'value') {
      hasValue = true;
      deferredValue = v;
      continue;
    }
    if (v == null || v === false) {
      if (BOOL_PROPS.has(k) && !isSvg) el[k] = false;
      continue;
    }
    if (k === 'class' || k === 'className') {
      el.setAttribute('class', Array.isArray(v) ? v.filter(Boolean).join(' ') : String(v));
    } else if (k === 'style') {
      if (typeof v === 'string') el.style.cssText = v;
      else for (const sk of Object.keys(v)) {
        if (v[sk] == null) continue;
        if (sk.startsWith('--')) el.style.setProperty(sk, v[sk]);
        else el.style[sk] = v[sk];
      }
    } else if (k === 'dataset') {
      Object.assign(el.dataset, v);
    } else if (k.length > 2 && k.startsWith('on') && typeof v === 'function') {
      el.addEventListener(k.slice(2).toLowerCase(), v);
    } else if (BOOL_PROPS.has(k) && !isSvg) {
      el[k] = true;
    } else {
      el.setAttribute(k, v === true ? '' : String(v));
    }
  }
  return hasValue ? { v: deferredValue } : null;
}

export function append(parent, children) {
  for (const c of children) {
    if (c == null || c === false || c === true) continue;
    if (Array.isArray(c)) append(parent, c);
    else if (c instanceof Node) parent.appendChild(c);
    else parent.appendChild(document.createTextNode(String(c)));
  }
  return parent;
}

/** h('div', {class: 'x', onclick: fn}, child, 'text', [more]) */
export function h(tag, props, ...children) {
  // Allow h('div.card.wide', ...) shorthand: tag.class1.class2
  let cls = null;
  if (tag.includes('.') || tag.includes('#')) {
    const m = tag.match(/^([a-z0-9-]*)((?:[.#][\w-]+)*)$/i);
    if (m) {
      tag = m[1] || 'div';
      cls = [];
      for (const part of m[2].match(/[.#][\w-]+/g) || []) {
        if (part[0] === '.') cls.push(part.slice(1));
      }
    }
  }
  const el = document.createElement(tag);
  if (props && (props instanceof Node || Array.isArray(props) || typeof props !== 'object')) {
    // props omitted: first argument is a child
    children.unshift(props);
    props = null;
  }
  if (cls && cls.length) el.className = cls.join(' ');
  const dv = applyProps(el, props, false);
  if (cls && cls.length && props && (props.class || props.className)) {
    el.setAttribute('class', cls.join(' ') + ' ' + (props.class || props.className));
  }
  append(el, children);
  if (dv) el.value = dv.v == null ? '' : dv.v;
  return el;
}

export function svg(tag, props, ...children) {
  const el = document.createElementNS(SVG_NS, tag);
  if (props && (props instanceof Node || Array.isArray(props) || typeof props !== 'object')) {
    children.unshift(props);
    props = null;
  }
  applyProps(el, props, true);
  append(el, children);
  return el;
}

export function frag(...children) {
  const f = document.createDocumentFragment();
  append(f, children);
  return f;
}

export function text(s) {
  return document.createTextNode(s == null ? '' : String(s));
}

export function clear(el) {
  el.replaceChildren();
  return el;
}

export function mount(el, ...children) {
  el.replaceChildren();
  append(el, children);
  return el;
}

export const $ = (sel, root = document) => root.querySelector(sel);
export const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

/** Event delegation: delegate(root, 'click', '[data-act]', (e, el) => ...) */
export function delegate(root, type, selector, fn) {
  root.addEventListener(type, (e) => {
    const t = e.target instanceof Element ? e.target.closest(selector) : null;
    if (t && root.contains(t)) fn(e, t);
  });
}

export function debounce(fn, ms) {
  let t = 0;
  const d = (...a) => {
    clearTimeout(t);
    t = setTimeout(() => fn(...a), ms);
  };
  d.cancel = () => clearTimeout(t);
  return d;
}

export function throttleRaf(fn) {
  let queued = false;
  return (...a) => {
    if (queued) return;
    queued = true;
    requestAnimationFrame(() => {
      queued = false;
      fn(...a);
    });
  };
}

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

let uidN = 0;
export const uid = (p = 'id') => `${p}-${++uidN}`;

export function copyText(str) {
  if (navigator.clipboard && window.isSecureContext) {
    return navigator.clipboard.writeText(str).then(() => true, () => fallbackCopy(str));
  }
  return Promise.resolve(fallbackCopy(str));
}

function fallbackCopy(str) {
  const ta = h('textarea', { value: str, style: 'position:fixed;opacity:0;top:0;left:0', 'aria-hidden': 'true' });
  document.body.appendChild(ta);
  ta.select();
  let ok = false;
  try { ok = document.execCommand('copy'); } catch (_) { ok = false; }
  ta.remove();
  return ok;
}
