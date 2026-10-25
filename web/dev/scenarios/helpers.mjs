// Shared in-page helpers for the interaction scenarios.
export const HELPERS = `
window.__t = {
  fire(el, ev) { el.dispatchEvent(new Event(ev, { bubbles: true })); },
  field(label, root) {
    const f = [...(root || document).querySelectorAll('.field')].find((x) => { const l = x.querySelector('.field-label'); return l && l.textContent.trim().replace(/\\s*\\*$/, '').toLowerCase() === label.toLowerCase(); });
    if (!f) throw new Error('no field ' + label);
    return f.querySelector('input:not([type=range]), select, textarea');
  },
  set(label, v, root) { const el = this.field(label, root); el.value = v; this.fire(el, 'input'); this.fire(el, 'change'); return el; },
  click(sel, re, root) {
    const el = [...(root || document).querySelectorAll(sel)].find((b) => !re || new RegExp(re, 'i').test(b.textContent + ' ' + (b.getAttribute('aria-label') || '') + ' ' + (b.title || '')));
    if (!el) throw new Error('nothing to click: ' + sel + ' /' + re + '/');
    el.click(); return el;
  },
  modal() { return [...document.querySelectorAll('.modal')].pop(); },
};
`;
