// A small but real anonymization engine for the mock server, so the UI's
// preview/test panels show believable before/after results. Mirrors the
// documented behaviour in internal/model/anonymize.go (not the Go code).
import { hmacHex } from './util.mjs';

export const DETECTOR_INFO = [
  { name: 'email', description: 'Email addresses (RFC 5322 subset)', example: 'jane@acme.io' },
  { name: 'credit_card', description: 'Luhn-checked 13-19 digit card numbers, with or without separators', example: '4242 4242 4242 4242' },
  { name: 'ssn', description: 'US social security numbers', example: '123-45-6789' },
  { name: 'phone', description: 'E.164 and North American phone numbers', example: '+1 (503) 555-0142' },
  { name: 'ipv4', description: 'IPv4 addresses', example: '203.0.113.45' },
  { name: 'ipv6', description: 'IPv6 addresses (full and compressed forms)', example: '2001:db8::8a2e:370:7334' },
  { name: 'jwt', description: 'JSON Web Tokens (three base64url segments)', example: 'eyJhbGciOi...' },
  { name: 'aws_key', description: 'AWS access key IDs (AKIA / ASIA prefixes)', example: 'AKIAIOSFODNN7EXAMPLE' },
  { name: 'iban', description: 'International bank account numbers', example: 'DE89 3704 0044 0532 0130 00' },
  { name: 'uuid', description: 'UUIDs. Off by default: they are often harmless identifiers', example: '3f2504e0-4f89-41d3-9a0c-0305e82c3301' },
];

const FIRST = ['Mason', 'Avery', 'Riley', 'Jordan', 'Taylor', 'Quinn', 'Harper', 'Rowan', 'Skyler', 'Emerson', 'Logan', 'Parker', 'Sage', 'Reese', 'Dakota', 'Finley'];
const LAST = ['Reed', 'Hayes', 'Brooks', 'Carter', 'Ellis', 'Foster', 'Grant', 'Holt', 'Irwin', 'Keller', 'Lowe', 'Marsh', 'Nolan', 'Owens', 'Price', 'Stone'];

const REMOVED = Symbol('removed');

const DETECTORS = {
  email: { re: /[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/g, fake: 'email' },
  credit_card: { re: /\b(?:\d[ -]?){12,18}\d\b/g, fake: 'credit_card', check: luhnOk },
  ssn: { re: /\b\d{3}-\d{2}-\d{4}\b/g, fake: 'ssn' },
  phone: { re: /(?:\+?1[ .-]?)?\(?\b\d{3}\)?[ .-]\d{3}[ .-]\d{4}\b|\+\d{10,15}\b/g, fake: 'phone' },
  ipv4: { re: /\b(?:(?:25[0-5]|2[0-4]\d|1?\d?\d)\.){3}(?:25[0-5]|2[0-4]\d|1?\d?\d)\b/g, fake: 'ipv4' },
  ipv6: { re: /\b(?:[A-Fa-f0-9]{1,4}:){7}[A-Fa-f0-9]{1,4}\b/g, fake: 'string' },
  jwt: { re: /\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\b/g, fake: 'string' },
  aws_key: { re: /\b(?:AKIA|ASIA)[A-Z0-9]{16}\b/g, fake: 'string' },
  iban: { re: /\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]{4}){3,7}(?: ?[A-Z0-9]{1,3})?\b/g, fake: 'string' },
  uuid: { re: /\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b/gi, fake: 'uuid' },
};

function luhnOk(s) {
  const d = s.replace(/\D/g, '');
  if (d.length < 13 || d.length > 19) return false;
  let sum = 0, alt = false;
  for (let i = d.length - 1; i >= 0; i--) {
    let n = d.charCodeAt(i) - 48;
    if (alt) { n *= 2; if (n > 9) n -= 9; }
    sum += n; alt = !alt;
  }
  return sum % 10 === 0;
}

function digestBytes(secret, kind, value) {
  return Buffer.from(hmacHex(secret, kind + ':' + value), 'hex');
}

function digitsLike(value, d, keepFirst = false) {
  let k = 0;
  return String(value).replace(/\d/g, (c, idx) => (keepFirst && idx === 0 ? c : String(d[k++ % d.length] % 10)));
}

function luhnFix(digits) {
  // digits: 16-char string; recompute last digit so the number is Luhn valid
  const body = digits.slice(0, -1);
  let sum = 0, alt = true;
  for (let i = body.length - 1; i >= 0; i--) {
    let n = body.charCodeAt(i) - 48;
    if (alt) { n *= 2; if (n > 9) n -= 9; }
    sum += n; alt = !alt;
  }
  return body + String((10 - (sum % 10)) % 10);
}

function guessKind(v) {
  const s = String(v);
  if (/^[^@\s]+@[^@\s]+\.[A-Za-z]{2,}$/.test(s)) return 'email';
  if (DETECTORS.uuid.re.test(s)) { DETECTORS.uuid.re.lastIndex = 0; return 'uuid'; }
  DETECTORS.uuid.re.lastIndex = 0;
  if (/^\d{3}-\d{2}-\d{4}$/.test(s)) return 'ssn';
  if (/^[\d -]{13,23}$/.test(s) && luhnOk(s)) return 'credit_card';
  if (/^(\d{1,3}\.){3}\d{1,3}$/.test(s)) return 'ipv4';
  if (/^\+?[\d ().-]{10,16}$/.test(s)) return 'phone';
  if (/^\d+$/.test(s)) return 'digits';
  return 'string';
}

export function fakeValue(secret, kind, value) {
  const s = String(value);
  if (kind === 'auto') kind = guessKind(s);
  const d = digestBytes(secret, kind, s);
  switch (kind) {
    case 'email': return `${FIRST[d[0] % FIRST.length].toLowerCase()}.${LAST[d[1] % LAST.length].toLowerCase()}${10 + (d[2] % 89)}@example.com`;
    case 'name': return `${FIRST[d[0] % FIRST.length]} ${LAST[d[1] % LAST.length]}`;
    case 'phone': { const cc = /^\+\d/.exec(s); const rest = cc ? s.slice(2) : s; const body = digitsLike(rest, d.subarray(3), false).replace(/(\d{3})([ .)-]+)(\d{3})/, '555$2$3'); return (cc ? s.slice(0, 2) : '') + body; }
    case 'uuid': {
      const hex = d.toString('hex');
      return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-4${hex.slice(13, 16)}-8${hex.slice(17, 20)}-${hex.slice(20, 32)}`;
    }
    case 'ssn': return `9${String(d[0] % 10)}${String(d[1] % 10)}-${String(d[2] % 90 + 10)}-${String(1000 + (d[3] * 37 + d[4]) % 9000)}`;
    case 'digits': return digitsLike(s, d, false);
    case 'ipv4': return `10.${d[0]}.${d[1]}.${(d[2] % 253) + 1}`;
    case 'credit_card': {
      const digits = s.replace(/\D/g, '');
      let gen = digits[0] || '4';
      for (let i = 1; i < digits.length; i++) gen += String(d[i % d.length] % 10);
      gen = luhnFix(gen);
      let k = 0;
      return s.replace(/\d/g, () => gen[k++]);
    }
    default: {
      let k = 0;
      return s.replace(/[A-Za-z0-9]/g, (c) => {
        const b = d[k++ % d.length];
        if (c >= '0' && c <= '9') return String(b % 10);
        const ch = String.fromCharCode(97 + (b % 26));
        return c === c.toUpperCase() ? ch.toUpperCase() : ch;
      });
    }
  }
}

function strategyValue(ctx, value, spec, detectorFake) {
  const s = String(value);
  switch (spec.strategy) {
    case 'remove': return REMOVED;
    case 'redact': return spec.replacement || '[REDACTED]';
    case 'hash': return 'h_' + hmacHex(ctx.secret, s).slice(0, 16);
    case 'mask': {
      const p = Math.min(spec.keep_prefix || 0, s.length);
      const sf = Math.min(spec.keep_suffix || 0, s.length - p);
      return s.slice(0, p) + '*'.repeat(Math.max(0, s.length - p - sf)) + (sf ? s.slice(s.length - sf) : '');
    }
    case 'fake': {
      const kind = spec.fake && spec.fake !== 'auto' ? spec.fake : (detectorFake || 'auto');
      return fakeValue(ctx.secret, kind, s);
    }
    default: return s;
  }
}

// ---- JSONPath subset -----------------------------------------------------
function parsePath(sel) {
  if (!sel.startsWith('$')) throw new Error('selector must start with $');
  const toks = [];
  let i = 1;
  while (i < sel.length) {
    if (sel.startsWith('..', i)) {
      i += 2;
      let j = i;
      while (j < sel.length && sel[j] !== '.' && sel[j] !== '[') j++;
      toks.push({ t: 'desc', k: sel.slice(i, j) });
      i = j;
    } else if (sel[i] === '.') {
      i++;
      if (sel[i] === '*') { toks.push({ t: 'wild' }); i++; continue; }
      let j = i;
      while (j < sel.length && sel[j] !== '.' && sel[j] !== '[') j++;
      toks.push({ t: 'key', k: sel.slice(i, j) });
      i = j;
    } else if (sel[i] === '[') {
      const j = sel.indexOf(']', i);
      if (j < 0) throw new Error('unterminated [');
      const inner = sel.slice(i + 1, j).trim();
      if (inner === '*') toks.push({ t: 'wild' });
      else if (/^\d+$/.test(inner)) toks.push({ t: 'idx', i: Number(inner) });
      else toks.push({ t: 'key', k: inner.replace(/^['"]|['"]$/g, '') });
      i = j + 1;
    } else throw new Error('bad selector');
  }
  return toks;
}

function isContainer(v) { return v !== null && typeof v === 'object'; }

function evalPath(root, toks) {
  let nodes = [{ parent: null, key: null, value: root, path: '$' }];
  for (const tk of toks) {
    const next = [];
    for (const n of nodes) {
      const v = n.value;
      if (tk.t === 'key') {
        if (isContainer(v) && !Array.isArray(v) && tk.k in v) next.push({ parent: v, key: tk.k, value: v[tk.k], path: `${n.path}.${tk.k}` });
      } else if (tk.t === 'idx') {
        if (Array.isArray(v) && tk.i < v.length) next.push({ parent: v, key: tk.i, value: v[tk.i], path: `${n.path}[${tk.i}]` });
      } else if (tk.t === 'wild') {
        if (Array.isArray(v)) v.forEach((x, i) => next.push({ parent: v, key: i, value: x, path: `${n.path}[${i}]` }));
        else if (isContainer(v)) for (const k of Object.keys(v)) next.push({ parent: v, key: k, value: v[k], path: `${n.path}.${k}` });
      } else if (tk.t === 'desc') {
        const walk = (x, p) => {
          if (Array.isArray(x)) x.forEach((e, i) => walk(e, `${p}[${i}]`));
          else if (isContainer(x)) {
            for (const k of Object.keys(x)) {
              if (k === tk.k) next.push({ parent: x, key: k, value: x[k], path: `${p}.${k}` });
              walk(x[k], `${p}.${k}`);
            }
          }
        };
        walk(v, n.path);
      }
    }
    nodes = next;
  }
  return nodes;
}

// ---- engine --------------------------------------------------------------
const ctOf = (headers) => {
  for (const k of Object.keys(headers || {})) if (k.toLowerCase() === 'content-type') return String((headers[k] || [])[0] || '');
  return '';
};

function scopeOk(rule, side) {
  return rule.enabled !== false && (rule.scope === 'both' || !rule.scope || rule.scope === side);
}

function runDetectors(ctx, text, where) {
  let out = text;
  for (const name of ctx.profile.detectors || []) {
    const d = DETECTORS[name];
    if (!d) continue;
    out = out.replace(d.re, (m) => {
      if (d.check && !d.check(m)) return m;
      if (m.startsWith('h_') || m.endsWith('@example.com')) return m;
      const r = strategyValue(ctx, m, { strategy: ctx.profile.detector_strategy || 'fake', fake: d.fake }, d.fake);
      const after = r === REMOVED ? '' : r;
      if (after !== m) ctx.changes.push({ location: where, source: `detector:${name}`, before: m, after });
      return after;
    });
  }
  return out;
}

function hashToken(ctx, value) {
  const m = /^(Bearer|Basic|Digest|Token)\s+/i.exec(value);
  const prefix = m ? m[0] : '';
  return prefix + 'h_' + hmacHex(ctx.secret, value.slice(prefix.length)).slice(0, 16);
}

function processHeaders(ctx, side, headers) {
  const redact = new Set((ctx.profile.redact_headers || []).map((h) => h.toLowerCase()));
  for (const name of Object.keys(headers)) {
    const lower = name.toLowerCase();
    headers[name] = headers[name].map((val) => {
      let cur = val;
      let handled = false;
      if (redact.has(lower)) {
        handled = true;
        let next;
        if (lower === 'cookie') {
          next = cur.split(/;\s*/).map((kv) => { const i = kv.indexOf('='); return i < 0 ? kv : kv.slice(0, i + 1) + 'h_' + hmacHex(ctx.secret, kv.slice(i + 1)).slice(0, 12); }).join('; ');
        } else if (lower === 'set-cookie') {
          const parts = cur.split(/;\s*/);
          const i = parts[0].indexOf('=');
          if (i >= 0) parts[0] = parts[0].slice(0, i + 1) + 'h_' + hmacHex(ctx.secret, parts[0].slice(i + 1)).slice(0, 12);
          next = parts.join('; ');
        } else next = hashToken(ctx, cur);
        if (next !== cur) ctx.changes.push({ location: `${side}.header.${name}`, source: 'redact_headers', before: cur, after: next });
        cur = next;
      }
      for (const r of ctx.profile.rules || []) {
        if (!scopeOk(r, side)) continue;
        if (r.target === 'header' && r.selector.toLowerCase() === lower) {
          const res = strategyValue(ctx, cur, r);
          const after = res === REMOVED ? null : res;
          if (after === null) { ctx.changes.push({ location: `${side}.header.${name}`, source: r.name || r.selector, before: cur, after: '(removed)' }); cur = null; break; }
          if (after !== cur) ctx.changes.push({ location: `${side}.header.${name}`, source: r.name || r.selector, before: cur, after });
          cur = after;
          handled = true;
        } else if (r.target === 'cookie' && (lower === 'cookie' || lower === 'set-cookie')) {
          const re = new RegExp(`(^|;\\s*)(${r.selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')})=([^;]*)`);
          const m = re.exec(cur);
          if (m) {
            const res = strategyValue(ctx, m[3], r);
            const after = res === REMOVED ? '' : res;
            const next = cur.replace(re, (_, a, b) => `${a}${b}=${after}`);
            ctx.changes.push({ location: `${side}.cookie.${r.selector}`, source: r.name || r.selector, before: m[3], after });
            cur = next;
          }
        } else if (r.target === 'regex') {
          cur = applyRegexRule(ctx, r, cur, `${side}.header.${name}`);
        }
      }
      if (cur === null) return null;
      if (!handled) cur = runDetectors(ctx, cur, `${side}.header.${name}`);
      return cur;
    }).filter((v) => v !== null);
    if (!headers[name].length) delete headers[name];
  }
}

function applyRegexRule(ctx, rule, text, where) {
  let re;
  try { re = new RegExp(rule.selector, 'g'); } catch (_) { return text; }
  return text.replace(re, (...args) => {
    const m = args[0];
    const groups = args.slice(1, args.findIndex((x) => typeof x === 'number'));
    const target = groups.length && groups[0] !== undefined ? groups[0] : m;
    const res = strategyValue(ctx, target, rule);
    const after = res === REMOVED ? '' : res;
    if (after !== target) ctx.changes.push({ location: where, source: rule.name || rule.selector, before: target, after });
    return groups.length && groups[0] !== undefined ? m.replace(target, after) : after;
  });
}

function processQuery(ctx, rawQuery) {
  if (!rawQuery) return rawQuery;
  const pairs = rawQuery.split('&').map((p) => {
    const i = p.indexOf('=');
    const dec = (s) => { try { return decodeURIComponent(s.replace(/\+/g, ' ')); } catch (_) { return s; } };
    return i < 0 ? [dec(p), null] : [dec(p.slice(0, i)), dec(p.slice(i + 1))];
  });
  const out = [];
  for (const [k, v] of pairs) {
    if (v === null) { out.push(encodeURIComponent(k)); continue; }
    let cur = v;
    let drop = false;
    for (const r of ctx.profile.rules || []) {
      if (!scopeOk(r, 'request')) continue;
      if (r.target === 'query' && r.selector === k) {
        const res = strategyValue(ctx, cur, r);
        if (res === REMOVED) { ctx.changes.push({ location: `request.query.${k}`, source: r.name || r.selector, before: cur, after: '(removed)' }); drop = true; break; }
        if (res !== cur) ctx.changes.push({ location: `request.query.${k}`, source: r.name || r.selector, before: cur, after: res });
        cur = res;
      } else if (r.target === 'regex') cur = applyRegexRule(ctx, r, cur, `request.query.${k}`);
    }
    if (drop) continue;
    cur = runDetectors(ctx, cur, `request.query.${k}`);
    out.push(`${encodeURIComponent(k)}=${encodeURIComponent(cur)}`);
  }
  return out.join('&');
}

function processPath(ctx, path) {
  let cur = path;
  for (const r of ctx.profile.rules || []) {
    if (!scopeOk(r, 'request') || r.target !== 'path') continue;
    const tmpl = r.selector.split('/');
    const segs = cur.split('/');
    if (tmpl.length !== segs.length) continue;
    let ok = true;
    for (let i = 0; i < tmpl.length; i++) if (!/^\{[^}]+\}$/.test(tmpl[i]) && tmpl[i] !== segs[i]) ok = false;
    if (!ok) continue;
    cur = segs.map((s, i) => {
      if (!/^\{[^}]+\}$/.test(tmpl[i])) return s;
      const res = strategyValue(ctx, s, r);
      const after = res === REMOVED ? '' : res;
      if (after !== s) ctx.changes.push({ location: 'request.path', source: r.name || r.selector, before: s, after });
      return after;
    }).join('/');
  }
  return cur;
}

function walkStrings(ctx, node, path, where, parent, key) {
  if (typeof node === 'string') {
    const next = runDetectors(ctx, node, `${where} ${path}`);
    if (next !== node && parent) parent[key] = next;
    return;
  }
  if (Array.isArray(node)) node.forEach((x, i) => walkStrings(ctx, x, `${path}[${i}]`, where, node, i));
  else if (isContainer(node)) for (const k of Object.keys(node)) walkStrings(ctx, node[k], `${path}.${k}`, where, node, k);
}

function transformLeaf(ctx, rule, node, where) {
  const apply = (parent, key, value, path) => {
    if (isContainer(value)) {
      for (const k of Object.keys(value)) apply(value, k, value[k], `${path}.${k}`);
      return;
    }
    if (value === null || value === undefined) return;
    const before = String(value);
    const res = strategyValue(ctx, before, rule);
    if (res === REMOVED) {
      if (Array.isArray(parent)) parent.splice(key, 1); else delete parent[key];
      ctx.changes.push({ location: `${where} ${path}`, source: rule.name || rule.selector, before, after: '(removed)' });
      return;
    }
    const after = typeof value === 'number' && /^-?\d+(\.\d+)?$/.test(res) ? Number(res) : res;
    if (String(after) !== before) ctx.changes.push({ location: `${where} ${path}`, source: rule.name || rule.selector, before, after: String(after) });
    parent[key] = after;
  };
  apply(node.parent, node.key, node.value, node.path);
}

function processBody(ctx, side, bv, headers) {
  if (!bv || bv.encoding !== 'text' || !bv.data) return;
  const ct = ctOf(headers).toLowerCase();
  const where = `${side}.body`;
  let text = bv.data;
  const t = text.trimStart();
  const jsonish = ct.includes('json') || t.startsWith('{') || t.startsWith('[');
  let tree = null;
  if (jsonish) { try { tree = JSON.parse(text); } catch (_) { tree = null; } }
  if (tree !== null) {
    const wrapper = { root: tree };
    for (const r of ctx.profile.rules || []) {
      if (!scopeOk(r, side) || r.target !== 'json') continue;
      let toks;
      try { toks = parsePath(r.selector); } catch (_) { continue; }
      const matches = evalPath(tree, toks).filter((n) => n.parent);
      for (const n of matches) transformLeaf(ctx, r, n, where);
    }
    walkStrings(ctx, tree, '$', where, wrapper, 'root');
    text = JSON.stringify(wrapper.root);
    for (const r of ctx.profile.rules || []) if (scopeOk(r, side) && r.target === 'regex') text = applyRegexRule(ctx, r, text, where);
  } else if (ct.includes('x-www-form-urlencoded')) {
    const parts = text.split('&').map((p) => {
      const i = p.indexOf('=');
      if (i < 0) return p;
      const k = decodeURIComponent(p.slice(0, i));
      let v = decodeURIComponent(p.slice(i + 1).replace(/\+/g, ' '));
      for (const r of ctx.profile.rules || []) {
        if (scopeOk(r, side) && r.target === 'form' && r.selector === k) {
          const res = strategyValue(ctx, v, r);
          const after = res === REMOVED ? '' : res;
          if (after !== v) ctx.changes.push({ location: `${where} ${k}`, source: r.name || r.selector, before: v, after });
          v = after;
        }
      }
      v = runDetectors(ctx, v, `${where} ${k}`);
      return `${encodeURIComponent(k)}=${encodeURIComponent(v)}`;
    });
    text = parts.join('&');
  } else {
    for (const r of ctx.profile.rules || []) if (scopeOk(r, side) && r.target === 'regex') text = applyRegexRule(ctx, r, text, where);
    text = runDetectors(ctx, text, where);
  }
  bv.data = text;
  bv.size = Math.max(Buffer.byteLength(text), bv.truncated ? bv.size : 0);
}

/** Anonymize an ExchangeView with a profile (including its secret). */
export function anonymizeExchange(view, profile) {
  const ex = structuredClone(view);
  const ctx = { profile, secret: profile.secret || 'mock-secret', changes: [] };
  ex.request.path = processPath(ctx, ex.request.path);
  ex.request.raw_query = processQuery(ctx, ex.request.raw_query || '');
  if (!ex.request.raw_query) delete ex.request.raw_query;
  processHeaders(ctx, 'request', ex.request.header || (ex.request.header = {}));
  processHeaders(ctx, 'response', ex.response.header || (ex.response.header = {}));
  processBody(ctx, 'request', ex.request.body, ex.request.header);
  processBody(ctx, 'response', ex.response.body, ex.response.header);
  ex.anonymized = true;
  return { exchange: ex, changes: ctx.changes };
}

export function validateProfile(p) {
  const problems = [];
  if (!p.name || !String(p.name).trim()) problems.push('name is required');
  const strategies = ['redact', 'remove', 'hash', 'fake', 'mask'];
  if (!strategies.includes(p.detector_strategy)) problems.push(`detector_strategy "${p.detector_strategy}" is not a strategy`);
  const known = new Set(DETECTOR_INFO.map((d) => d.name));
  for (const d of p.detectors || []) if (!known.has(d)) problems.push(`unknown detector "${d}"`);
  (p.rules || []).forEach((r, i) => {
    const label = r.name || r.selector;
    const n = i + 1;
    if (!['header', 'query', 'cookie', 'json', 'form', 'path', 'regex'].includes(r.target)) problems.push(`rule ${n} (${label}): unknown target "${r.target}"`);
    if (r.target === 'regex') { try { new RegExp(r.selector); } catch (e) { problems.push(`rule ${n} (${label}): invalid regex: ${e.message}`); } }
    if (!r.selector || !String(r.selector).trim()) problems.push(`rule ${n}: selector is required`);
    if (r.target === 'json' && r.selector && !r.selector.startsWith('$')) problems.push(`rule ${n} (${label}): JSON selector must start with $`);
    if (r.target === 'path' && r.selector && !r.selector.startsWith('/')) problems.push(`rule ${n} (${label}): path selector must start with /`);
    if (!['both', 'request', 'response'].includes(r.scope)) problems.push(`rule ${n} (${label}): unknown scope "${r.scope}"`);
    if (!strategies.includes(r.strategy)) problems.push(`rule ${n} (${label}): unknown strategy "${r.strategy}"`);
    if (r.strategy === 'fake' && !['auto', 'email', 'name', 'phone', 'uuid', 'digits', 'ipv4', 'credit_card', 'string'].includes(r.fake)) problems.push(`rule ${n} (${label}): unknown fake kind "${r.fake}"`);
    if ((r.keep_prefix || 0) < 0 || (r.keep_suffix || 0) < 0) problems.push(`rule ${n} (${label}): keep_prefix/keep_suffix must not be negative`);
  });
  return problems;
}
