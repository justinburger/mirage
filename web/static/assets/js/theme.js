// Theme preference: 'system' (default; dark unless the OS asks for light),
// 'dark' or 'light'. Stored in localStorage('mirage_theme').
const KEY = 'mirage_theme';
const listeners = new Set();

export function getPref() {
  try {
    const v = localStorage.getItem(KEY);
    if (v === 'dark' || v === 'light') return v;
  } catch (_) { /* ignore */ }
  return 'system';
}

export function effective() {
  const p = getPref();
  if (p !== 'system') return p;
  return window.matchMedia && window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark';
}

export function applyTheme(pref) {
  const root = document.documentElement;
  if (pref === 'dark' || pref === 'light') root.setAttribute('data-theme', pref);
  else root.removeAttribute('data-theme');
  listeners.forEach((f) => f(effective()));
}

export function setPref(pref) {
  try {
    if (pref === 'system') localStorage.removeItem(KEY);
    else localStorage.setItem(KEY, pref);
  } catch (_) { /* ignore */ }
  applyTheme(pref);
}

export function toggleTheme() {
  setPref(effective() === 'dark' ? 'light' : 'dark');
}

export function onThemeChange(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

export function initTheme() {
  applyTheme(getPref());
  if (window.matchMedia) {
    const mq = window.matchMedia('(prefers-color-scheme: light)');
    const h = () => { if (getPref() === 'system') listeners.forEach((f) => f(effective())); };
    if (mq.addEventListener) mq.addEventListener('change', h);
  }
}
