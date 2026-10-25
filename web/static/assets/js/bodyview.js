// Rendering of captured HTTP exchanges: headers, bodies (pretty JSON with
// syntax highlighting, hex dumps for binary) and request/response sections.
// All recorded data is untrusted: only text nodes and attributes are used.
import { h, append } from './dom.js';
import { icon } from './icons.js';
import { btn, copyButton, badge, statusPill, methodTag, toast } from './ui.js';
import { fmtBytes, fmtDur, DASH } from './format.js';

const MAX_RENDER = 150000; // characters rendered before "show all"

/** Re-indent JSON text without parsing numbers (keeps exact digits). */
export function reindentJSON(text) {
  let out = '';
  let indent = 0;
  let inStr = false;
  let esc = false;
  const pad = () => '\n' + '  '.repeat(indent);
  const n = text.length;
  for (let i = 0; i < n; i++) {
    const ch = text[i];
    if (inStr) {
      out += ch;
      if (esc) esc = false;
      else if (ch === '\\') esc = true;
      else if (ch === '"') inStr = false;
      continue;
    }
    if (ch === '"') { inStr = true; out += ch; }
    else if (ch === '{' || ch === '[') {
      let j = i + 1;
      while (j < n && (text[j] === ' ' || text[j] === '\n' || text[j] === '\t' || text[j] === '\r')) j++;
      if ((ch === '{' && text[j] === '}') || (ch === '[' && text[j] === ']')) { out += ch + text[j]; i = j; }
      else { indent++; out += ch + pad(); }
    } else if (ch === '}' || ch === ']') { indent = Math.max(0, indent - 1); out += pad() + ch; }
    else if (ch === ',') out += ',' + pad();
    else if (ch === ':') out += ': ';
    else if (ch === ' ' || ch === '\n' || ch === '\t' || ch === '\r') { /* drop */ }
    else out += ch;
  }
  return out;
}

export function looksJSON(text, contentType) {
  const ct = (contentType || '').toLowerCase();
  const t = text.trimStart();
  if (ct.includes('json')) return true;
  return (t.startsWith('{') || t.startsWith('[')) && !ct.includes('html') && !ct.includes('xml');
}

const TOKEN = /("(?:\\.|[^"\\\n])*"?)(\s*:)?|(-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?)\b|\b(true|false|null)\b|([{}[\],:])/g;

function span(cls, txt) { return h('span', { class: cls }, txt); }

/** Highlighted JSON as a fragment. marks: strings whose containing values get emphasised. */
export function highlightJSON(text, marks) {
  const frag = document.createDocumentFragment();
  const ms = (marks || []).filter((m) => m && m.length >= 3);
  let last = 0;
  TOKEN.lastIndex = 0;
  let m;
  while ((m = TOKEN.exec(text))) {
    if (m.index > last) frag.appendChild(document.createTextNode(text.slice(last, m.index)));
    last = TOKEN.lastIndex;
    if (m[1] !== undefined) {
      const isKey = m[2] !== undefined;
      let cls = isKey ? 'j-key' : 'j-str';
      if (!isKey && ms.length) {
        const body = m[1];
        if (ms.some((x) => body.includes(x))) cls += ' j-mark';
      }
      frag.appendChild(span(cls, m[1]));
      if (isKey) frag.appendChild(document.createTextNode(m[2]));
    } else if (m[3] !== undefined) frag.appendChild(span('j-num', m[3]));
    else if (m[4] !== undefined) frag.appendChild(span('j-lit', m[4]));
    else frag.appendChild(span('j-punc', m[5]));
  }
  if (last < text.length) frag.appendChild(document.createTextNode(text.slice(last)));
  return frag;
}

/** Plain text with marked substrings wrapped. */
function highlightPlain(text, marks) {
  const ms = (marks || []).filter((m) => m && m.length >= 3);
  if (!ms.length) return document.createTextNode(text);
  const frag = document.createDocumentFragment();
  const re = new RegExp(ms.map((s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|'), 'g');
  let last = 0;
  let m;
  while ((m = re.exec(text))) {
    if (m.index > last) frag.appendChild(document.createTextNode(text.slice(last, m.index)));
    frag.appendChild(span('j-mark', m[0]));
    last = re.lastIndex;
    if (m[0].length === 0) re.lastIndex++;
  }
  if (last < text.length) frag.appendChild(document.createTextNode(text.slice(last)));
  return frag;
}

export function decodeBase64(b64) {
  try {
    const bin = atob(b64);
    const bytes = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
    return bytes;
  } catch (_) { return new Uint8Array(0); }
}

export function hexDump(bytes, max = 2048) {
  const lines = [];
  const n = Math.min(bytes.length, max);
  for (let o = 0; o < n; o += 16) {
    const chunk = bytes.subarray(o, Math.min(n, o + 16));
    const hex = Array.from(chunk, (b) => b.toString(16).padStart(2, '0')).join(' ');
    const asc = Array.from(chunk, (b) => (b >= 32 && b < 127 ? String.fromCharCode(b) : '.')).join('');
    lines.push(o.toString(16).padStart(8, '0') + '  ' + hex.padEnd(47, ' ') + '  ' + asc);
  }
  if (bytes.length > n) lines.push(`… ${bytes.length - n} more bytes`);
  return lines.join('\n');
}

function download(name, bytes, type) {
  const blob = new Blob([bytes], { type: type || 'application/octet-stream' });
  const url = URL.createObjectURL(blob);
  const a = h('a', { href: url, download: name });
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 2000);
}

/**
 * Renders a BodyView. opts: {marks, maxChars, name}
 */
export function bodyViewer(bv, opts = {}) {
  const marks = opts.marks || [];
  if (!bv || bv.encoding === 'none' || (!bv.data && bv.encoding !== 'base64')) {
    return h('div', { class: 'body-empty' }, 'No body', bv && bv.truncated ? badge('truncated', 'warn') : null);
  }
  const ct = bv.content_type || '';
  let rawText = null;
  let bytes = null;
  let pretty = null;
  let kind = 'text';
  if (bv.encoding === 'base64') {
    bytes = decodeBase64(bv.data);
    kind = 'binary';
  } else {
    rawText = bv.data;
    if (looksJSON(rawText, ct)) {
      let ok = false;
      try { JSON.parse(rawText); ok = true; } catch (_) { ok = !!bv.truncated; }
      if (ok) { pretty = reindentJSON(rawText); kind = 'json'; }
    }
  }
  const shown = kind === 'binary' ? hexDump(bytes) : (pretty != null ? pretty : rawText);
  const limit = opts.maxChars || MAX_RENDER;
  const code = h('pre', { class: ['code', kind === 'binary' && 'code-hex'], tabindex: 0 });
  const wrapState = { on: false };
  function paint(full) {
    const text = !full && shown.length > limit ? shown.slice(0, limit) : shown;
    code.replaceChildren(kind === 'json' ? highlightJSON(text, marks) : highlightPlain(text, marks));
    if (text.length < shown.length) {
      code.appendChild(h('div', { class: 'code-more' }, `Showing the first ${limit.toLocaleString()} of ${shown.length.toLocaleString()} characters. `,
        h('button', { type: 'button', class: 'link-btn', onclick: () => paint(true) }, 'Show all')));
    }
  }
  paint(false);

  const meta = h('div', { class: 'body-meta' },
    ct ? h('span', { class: 'body-ct mono' }, ct) : null,
    h('span', { class: 'body-size' }, fmtBytes(bv.size)),
    kind === 'binary' ? badge('binary', 'info') : null,
    bv.truncated ? badge('truncated', 'warn', { title: 'The capture size limit cut this body; the original was ' + fmtBytes(bv.size) }) : null);
  const tools = h('div', { class: 'body-tools' },
    kind !== 'binary' ? copyButton(() => shown, { title: 'Copy body' }) : null,
    h('button', {
      type: 'button', class: 'btn btn-ghost btn-sm btn-icon', title: 'Toggle line wrapping', 'aria-label': 'Toggle line wrapping',
      onclick: (e) => { wrapState.on = !wrapState.on; code.classList.toggle('wrap', wrapState.on); e.currentTarget.classList.toggle('is-on', wrapState.on); },
    }, h('span', { class: 'wrap-glyph', 'aria-hidden': 'true' }, '\u21b5')),
    btn('', {
      icon: 'download', kind: 'ghost', size: 'sm', title: 'Download body',
      onClick: () => {
        const data = kind === 'binary' ? bytes : (new TextEncoder()).encode(rawText);
        download(opts.name || 'body', data, ct);
        toast('Body downloaded', 'success', { timeout: 1500 });
      },
    }));
  return h('div', { class: 'body' }, h('div', { class: 'body-bar' }, meta, tools),
    kind === 'binary' ? h('div', { class: 'body-note' }, `Binary content. Showing a hex dump of the first ${Math.min(bytes.length, 2048)} bytes.`) : null,
    code);
}

export function headersView(header, { marks, empty = 'No headers' } = {}) {
  const names = Object.keys(header || {}).sort((a, b) => a.localeCompare(b));
  if (!names.length) return h('div', { class: 'body-empty' }, empty);
  const ms = (marks || []).filter((m) => m && m.length >= 3);
  const dl = h('dl', { class: 'hdrs' });
  for (const n of names) {
    const vals = Array.isArray(header[n]) ? header[n] : [header[n]];
    for (const v of vals) {
      dl.appendChild(h('dt', null, n));
      const hit = ms.some((x) => String(v).includes(x));
      dl.appendChild(h('dd', { class: hit ? 'j-mark' : null }, String(v)));
    }
  }
  return dl;
}

/** Strings from AnonChange[] used to highlight the before/after side. */
export function marksFromChanges(changes, side) {
  const out = new Set();
  for (const c of changes || []) {
    const v = c[side];
    if (typeof v === 'string' && v.length >= 3) out.add(v.length > 200 ? v.slice(0, 200) : v);
  }
  return Array.from(out);
}

function section(title, ...children) {
  return h('section', { class: 'xsec' }, h('h4', { class: 'xsec-title' }, title), ...children);
}

/** Request + response sections of an ExchangeView. */
export function exchangeSections(ex, { marks } = {}) {
  const req = ex.request || {};
  const res = ex.response || {};
  const uri = (req.path || '/') + (req.raw_query ? '?' + req.raw_query : '');
  const reqLine = h('div', { class: 'xline' }, methodTag(req.method || '?'), h('code', { class: 'xline-path' }, uri), req.host ? h('span', { class: 'xline-host' }, req.host) : null);
  const resLine = h('div', { class: 'xline' }, statusPill(res.status), res.proto ? h('span', { class: 'xline-host' }, res.proto) : null,
    res.content_encoding ? badge('was ' + res.content_encoding, 'muted', { title: 'The captured body was decoded from this Content-Encoding' }) : null);
  return h('div', { class: 'xsections' },
    section('Request', reqLine, h('h5', { class: 'xsub' }, 'Headers'), headersView(req.header, { marks }), h('h5', { class: 'xsub' }, 'Body'), bodyViewer(req.body, { marks, name: 'request-body' })),
    section('Response', resLine, h('h5', { class: 'xsub' }, 'Headers'), headersView(res.header, { marks }), h('h5', { class: 'xsub' }, 'Body'), bodyViewer(res.body, { marks, name: 'response-body' })));
}

export function timingStrip(ex) {
  const item = (k, v) => h('div', { class: 'tstrip-item' }, h('div', { class: 'tstrip-k' }, k), h('div', { class: 'tstrip-v' }, v));
  return h('div', { class: 'tstrip' },
    item('Duration', fmtDur(ex.duration)),
    item('Time to first byte', fmtDur(ex.ttfb)),
    item('Endpoint', h('code', null, ex.endpoint || DASH)),
    item('Started', ex.started_at ? new Date(ex.started_at).toLocaleString() : DASH));
}

/** Table of AnonChange[]. */
export function changesTable(changes) {
  if (!changes || !changes.length) return h('div', { class: 'body-empty' }, 'No values were changed.');
  const t = h('table', { class: 'table table-dense changes' },
    h('thead', null, h('tr', null, h('th', null, 'Location'), h('th', null, 'Source'), h('th', null, 'Before'), h('th', null, 'After'))));
  const tb = h('tbody');
  for (const c of changes) {
    tb.appendChild(h('tr', null,
      h('td', { class: 'mono wrap-any' }, c.location),
      h('td', null, badge(c.source, String(c.source).startsWith('detector') ? 'info' : 'muted')),
      h('td', { class: 'mono wrap-any chg-before' }, c.before),
      h('td', { class: 'mono wrap-any chg-after' }, c.after)));
  }
  t.appendChild(tb);
  return h('div', { class: 'table-wrap' }, t);
}

export { append, icon };
