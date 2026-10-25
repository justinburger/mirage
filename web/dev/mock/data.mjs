// Seed data generators for the Mirage mock server: realistic recorded
// exchanges (with PII in them), endpoint definitions and helpers.
import { mulberry32, pick, randInt, weighted, lognorm, iso, fmtGoDuration, newId } from './util.mjs';

const FIRST = ['Jane', 'Marcus', 'Priya', 'Tomas', 'Aisha', 'Liam', 'Sofia', 'Kenji', 'Olivia', 'Diego', 'Hannah', 'Noah', 'Chloe', 'Ethan', 'Mei', 'Omar', 'Grace', 'Lucas', 'Zara', 'Felix'];
const LAST = ['Doe', 'Whitfield', 'Raman', 'Novak', 'Khan', 'Murphy', 'Alvarez', 'Tanaka', 'Bennett', 'Ortega', 'Fischer', 'Walker', 'Dubois', 'Park', 'Chen', 'Haddad', 'Sullivan', 'Rossi', 'Ivanova', 'Berg'];
const DOMAINS = ['acme.io', 'gmail.com', 'outlook.com', 'fastmail.com', 'proton.me', 'initech.dev'];
const CITIES = [['Portland', 'OR', '97201'], ['Seattle', 'WA', '98101'], ['Austin', 'TX', '78701'], ['Denver', 'CO', '80202'], ['Boston', 'MA', '02108'], ['Chicago', 'IL', '60601']];
const STREETS = ['Maple St', 'Oak Ave', 'Pine Rd', 'Cedar Ln', 'Elm St', 'Lake Dr', 'Hill Ct', 'Park Blvd'];
const AGENTS = [
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Safari/605.1.15',
  'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36',
  'okhttp/4.12.0', 'checkout-web/3.18.2 (+https://shop.acme.io)', 'python-requests/2.31.0',
];
const PRODUCTS = ['Trail Runner 2', 'Merino Crew', 'Alpine Shell', 'Ridge Pack 28L', 'Summit Beanie', 'Canyon Bottle 1L', 'Ember Stove', 'Voyager Tent 3P', 'Delta Headlamp', 'Basecamp Chair'];
const CATEGORIES = ['footwear', 'apparel', 'packs', 'camp', 'accessories'];

function luhnComplete(prefix, len, rng) {
  let s = prefix;
  while (s.length < len - 1) s += String(randInt(rng, 0, 9));
  let sum = 0, alt = true;
  for (let i = s.length - 1; i >= 0; i--) { let n = s.charCodeAt(i) - 48; if (alt) { n *= 2; if (n > 9) n -= 9; } sum += n; alt = !alt; }
  return s + String((10 - (sum % 10)) % 10);
}
const rid = (rng, n = 14) => { let s = ''; for (let i = 0; i < n; i++) s += 'abcdefghijklmnopqrstuvwxyz0123456789'[Math.floor(rng() * 36)]; return s; };
const uuid = (rng) => { const h = () => Math.floor(rng() * 16).toString(16); const blk = (n) => Array.from({ length: n }, h).join(''); return `${blk(8)}-${blk(4)}-4${blk(3)}-a${blk(3)}-${blk(12)}`; };
const b64url = (s) => Buffer.from(s).toString('base64url');

export function makeCustomers(rng, n = 90) {
  const out = [];
  for (let i = 0; i < n; i++) {
    const first = pick(rng, FIRST), last = pick(rng, LAST);
    const city = pick(rng, CITIES);
    out.push({
      id: 'cus_' + rid(rng, 12),
      first, last,
      name: `${first} ${last}`,
      email: `${first}.${last}${randInt(rng, 1, 99)}@${pick(rng, DOMAINS)}`.toLowerCase(),
      phone: `+1 (${pick(rng, ['503', '206', '512', '303', '617', '312'])}) ${randInt(rng, 200, 899)}-${String(randInt(rng, 0, 9999)).padStart(4, '0')}`,
      ssn: `${randInt(rng, 100, 899)}-${randInt(rng, 10, 99)}-${randInt(rng, 1000, 9999)}`,
      card: luhnComplete(pick(rng, ['4', '4', '5', '37']) + String(randInt(rng, 1, 9)), 16, rng),
      brand: pick(rng, ['visa', 'visa', 'mastercard', 'amex']),
      ip: `${pick(rng, [203, 198, 192, 73, 24, 98])}.${randInt(rng, 0, 255)}.${randInt(rng, 0, 255)}.${randInt(rng, 1, 254)}`,
      address: { line1: `${randInt(rng, 10, 9999)} ${pick(rng, STREETS)}`, city: city[0], state: city[1], postal_code: city[2], country: 'US' },
      iban: `DE${randInt(rng, 10, 99)} ${randInt(rng, 1000, 9999)} ${randInt(rng, 1000, 9999)} ${randInt(rng, 1000, 9999)} ${randInt(rng, 1000, 9999)} ${randInt(rng, 10, 99)}`,
    });
  }
  return out;
}

const jsonBody = (obj, ct = 'application/json') => {
  const data = typeof obj === 'string' ? obj : JSON.stringify(obj);
  return { encoding: 'text', data, size: Buffer.byteLength(data), content_type: ct };
};
const noBody = () => ({ encoding: 'none', size: 0 });

function jwt(rng, c, exp = 3600) {
  const now = 1761400000 + randInt(rng, 0, 80000);
  return `${b64url(JSON.stringify({ alg: 'HS256', typ: 'JWT' }))}.${b64url(JSON.stringify({ sub: c.id, email: c.email, iat: now, exp: now + exp }))}.${b64url(rid(rng, 32))}`;
}

function reqHeaders(rng, c, { hasBody, host, cookie = false, auth = 'jwt' }) {
  const h = {
    Accept: ['application/json'],
    'Accept-Encoding': ['gzip'],
    'User-Agent': [pick(rng, AGENTS)],
    'X-Request-Id': [uuid(rng)],
    'X-Forwarded-For': [c.ip],
    Host: [host],
  };
  if (auth === 'jwt') h.Authorization = [`Bearer ${jwt(rng, c)}`];
  else if (auth === 'key') h.Authorization = [`Bearer sk_live_${rid(rng, 24)}`];
  if (hasBody) h['Content-Type'] = ['application/json'];
  if (cookie) h.Cookie = [`session=${rid(rng, 32)}; theme=dark; cart=${rid(rng, 10)}`];
  return h;
}

function resHeaders(rng, ct, size, extra = {}) {
  return {
    'Content-Type': [ct],
    'Content-Length': [String(size)],
    'X-Request-Id': [uuid(rng)],
    'Cache-Control': ['no-store'],
    Server: ['nginx/1.25.4'],
    Date: ['Sat, 25 Oct 2026 14:12:09 GMT'],
    ...extra,
  };
}

const PNG = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==';

// ---- endpoint definitions -------------------------------------------------
// gen(rng, ctx) -> { path, query?, req?, res:{status, body, ct?}, setCookie? }
const ERR = (type, code, message) => ({ error: { type, code, message } });

function pickStatus(rng, mix) {
  return weighted(rng, mix.map(([status, w]) => ({ status, w }))).status;
}

const PAYMENTS = [
  { method: 'POST', tmpl: '/v1/charges', w: 22, ms: 52, auth: 'key', gen(rng, { c }) {
    const amount = randInt(rng, 500, 120000);
    const status = pickStatus(rng, [[200, 94.5], [402, 3], [503, 1.8], [500, 0.7]]);
    const req = { customer: c.id, amount, currency: 'usd', description: `Order #${randInt(rng, 10000, 99999)}`, receipt_email: c.email, card: { number: c.card, exp_month: randInt(rng, 1, 12), exp_year: randInt(rng, 2027, 2031), cvc: String(randInt(rng, 100, 999)) }, metadata: { ip: c.ip, phone: c.phone } };
    let body;
    if (status === 200) body = { id: 'ch_' + rid(rng, 18), object: 'charge', status: 'succeeded', amount, currency: 'usd', customer: { id: c.id, name: c.name, email: c.email }, payment_method: { brand: c.brand, last4: c.card.slice(-4) }, billing: { phone: c.phone, address: c.address }, created: 1761400000 + randInt(rng, 0, 80000) };
    else if (status === 402) body = ERR('card_error', 'card_declined', 'Your card was declined.');
    else if (status === 503) body = { error: 'upstream timeout', retry_after: 1 };
    else body = { error: 'internal error', request_id: uuid(rng) };
    return { path: '/v1/charges', req, res: { status, body }, slow: status >= 500 ? 6 : 1 };
  } },
  { method: 'GET', tmpl: '/v1/charges/{id}', w: 30, ms: 14, auth: 'jwt', gen(rng, { c }) {
    const id = 'ch_' + rid(rng, 18);
    const status = pickStatus(rng, [[200, 97.5], [404, 2], [500, 0.5]]);
    const body = status === 200 ? { id, object: 'charge', status: 'succeeded', amount: randInt(rng, 500, 90000), currency: 'usd', customer: c.id, receipt_email: c.email, card_last4: c.card.slice(-4) } : status === 404 ? ERR('invalid_request', 'resource_missing', `No such charge: ${id}`) : { error: 'internal error' };
    return { path: `/v1/charges/${id}`, res: { status, body } };
  } },
  { method: 'GET', tmpl: '/v1/customers/{id}', w: 20, ms: 11, auth: 'jwt', cookie: true, gen(rng, { c }) {
    const status = pickStatus(rng, [[200, 99], [404, 1]]);
    const body = status === 200 ? { id: c.id, object: 'customer', name: c.name, email: c.email, phone: c.phone, ssn: c.ssn, address: c.address, sources: [{ brand: c.brand, last4: c.card.slice(-4), number: c.card }], created: 1700000000 + randInt(rng, 0, 9e7) } : ERR('invalid_request', 'resource_missing', 'No such customer');
    return { path: `/v1/customers/${c.id}`, res: { status, body }, setCookie: rng() < 0.15 };
  } },
  { method: 'GET', tmpl: '/v1/customers', w: 6, ms: 24, auth: 'key', gen(rng, { c, customers }) {
    const list = Array.from({ length: 3 }, () => pick(rng, customers)).map((x) => ({ id: x.id, name: x.name, email: x.email, phone: x.phone }));
    return { path: '/v1/customers', query: `email=${encodeURIComponent(c.email)}&limit=20`, res: { status: 200, body: { object: 'list', data: list, has_more: false } } };
  } },
  { method: 'POST', tmpl: '/v1/refunds', w: 6, ms: 66, auth: 'key', gen(rng, { c }) {
    const status = pickStatus(rng, [[200, 95], [409, 3], [503, 2]]);
    const req = { charge: 'ch_' + rid(rng, 18), amount: randInt(rng, 200, 20000), reason: pick(rng, ['requested_by_customer', 'duplicate', 'fraudulent']), notify: c.email };
    const body = status === 200 ? { id: 're_' + rid(rng, 18), status: 'succeeded', amount: req.amount, charge: req.charge } : status === 409 ? ERR('invalid_request', 'charge_already_refunded', 'Charge has already been refunded.') : { error: 'upstream timeout' };
    return { path: '/v1/refunds', req, res: { status, body }, slow: status >= 500 ? 6 : 1 };
  } },
  { method: 'GET', tmpl: '/v1/balance', w: 8, ms: 7, auth: 'key', gen(rng) {
    return { path: '/v1/balance', res: { status: 200, body: { object: 'balance', available: [{ amount: randInt(rng, 100000, 9000000), currency: 'usd' }], pending: [{ amount: randInt(rng, 1000, 400000), currency: 'usd' }] } } };
  } },
  { method: 'POST', tmpl: '/v1/payment_methods', w: 5, ms: 38, auth: 'jwt', gen(rng, { c }) {
    const status = pickStatus(rng, [[200, 96], [422, 4]]);
    const req = { type: 'sepa_debit', sepa_debit: { iban: c.iban }, billing_details: { name: c.name, email: c.email, phone: c.phone, address: c.address } };
    const body = status === 200 ? { id: 'pm_' + rid(rng, 16), type: 'sepa_debit', customer: c.id, sepa_debit: { last4: c.iban.slice(-4), country: 'DE' } } : ERR('invalid_request', 'iban_invalid', 'The IBAN you provided is invalid.');
    return { path: '/v1/payment_methods', req, res: { status, body } };
  } },
  { method: 'GET', tmpl: '/v1/receipts/{id}.png', w: 2, ms: 31, auth: 'jwt', gen(rng) {
    return { path: `/v1/receipts/rcpt_${rid(rng, 10)}.png`, res: { status: 200, body: { __binary: PNG, ct: 'image/png' } } };
  } },
  { method: 'POST', tmpl: '/v1/webhooks', w: 1, ms: 5, auth: 'key', gen(rng, { customers }) {
    const events = Array.from({ length: 400 }, (_, i) => ({ id: 'evt_' + rid(rng, 14), type: pick(rng, ['charge.succeeded', 'charge.failed', 'customer.updated']), data: { object: { customer: pick(rng, customers).email, seq: i } } }));
    const full = JSON.stringify({ object: 'event_batch', data: events });
    return { path: '/v1/webhooks', req: { __truncated: full.slice(0, 4096), size: full.length * 6 }, res: { status: 202, body: { received: true } } };
  } },
];

const CATALOG = [
  { method: 'GET', tmpl: '/products', w: 30, ms: 18, auth: 'none', gen(rng) {
    const cat = pick(rng, CATEGORIES), page = randInt(rng, 1, 6);
    const items = Array.from({ length: 6 }, (_, i) => ({ id: `p_${randInt(rng, 1000, 9999)}`, name: pick(rng, PRODUCTS), price: randInt(rng, 1900, 29900), category: cat, in_stock: rng() > 0.1 }));
    return { path: '/products', query: `category=${cat}&page=${page}`, res: { status: 200, body: { page, per_page: 6, total: 148, items } } };
  } },
  { method: 'GET', tmpl: '/products/{id}', w: 40, ms: 9, auth: 'none', gen(rng) {
    const id = `p_${randInt(rng, 1000, 9999)}`;
    const status = pickStatus(rng, [[200, 98.5], [404, 1.2], [503, 0.3]]);
    const body = status === 200 ? { id, name: pick(rng, PRODUCTS), price: randInt(rng, 1900, 29900), description: 'Lightweight, packable and built for long days on the trail.', stock: randInt(rng, 0, 400), rating: Math.round(rng() * 40 + 10) / 10 } : status === 404 ? { error: 'not_found', id } : { error: 'unavailable' };
    return { path: `/products/${id}`, res: { status, body } };
  } },
  { method: 'GET', tmpl: '/categories', w: 8, ms: 5, auth: 'none', gen() {
    return { path: '/categories', res: { status: 200, body: CATEGORIES.map((n, i) => ({ id: i + 1, slug: n, count: 20 + i * 11 })) } };
  } },
  { method: 'GET', tmpl: '/search', w: 14, ms: 46, auth: 'none', gen(rng) {
    const q = pick(rng, ['tent', 'jacket', 'boots', 'stove', 'headlamp', 'merino']);
    const status = pickStatus(rng, [[200, 99], [500, 1]]);
    return { path: '/search', query: `q=${q}&limit=10`, res: { status, body: status === 200 ? { q, hits: Array.from({ length: 4 }, () => ({ id: `p_${randInt(rng, 1000, 9999)}`, name: pick(rng, PRODUCTS), score: Math.round(rng() * 100) / 100 })) } : { error: 'search backend error' } } };
  } },
  { method: 'GET', tmpl: '/products/{id}/reviews', w: 8, ms: 22, auth: 'none', gen(rng, { customers }) {
    const id = `p_${randInt(rng, 1000, 9999)}`;
    return { path: `/products/${id}/reviews`, res: { status: 200, body: { product: id, reviews: Array.from({ length: 3 }, () => { const x = pick(rng, customers); return { author: x.name, email: x.email, stars: randInt(rng, 3, 5), text: 'Great gear, arrived quickly. Contact me at ' + x.phone }; }) } } };
  } },
];

const ORDERS = [
  { method: 'GET', tmpl: '/orders/{id}', w: 40, ms: 12, auth: 'jwt', gen(rng, { c }) {
    const id = `ord_${randInt(rng, 100000, 999999)}`;
    const status = pickStatus(rng, [[200, 98], [404, 2]]);
    return { path: `/orders/${id}`, res: { status, body: status === 200 ? { id, status: pick(rng, ['open', 'shipped', 'delivered']), customer: { id: c.id, email: c.email, phone: c.phone }, total: randInt(rng, 1500, 90000), items: [{ sku: pick(rng, PRODUCTS), qty: randInt(rng, 1, 3) }] } : { error: 'order not found' } } };
  } },
  { method: 'POST', tmpl: '/orders', w: 25, ms: 44, auth: 'jwt', gen(rng, { c }) {
    const status = pickStatus(rng, [[201, 96], [422, 2.5], [503, 1.5]]);
    const req = { customer_id: c.id, email: c.email, ship_to: c.address, items: [{ sku: pick(rng, PRODUCTS), qty: randInt(rng, 1, 3) }], payment_token: 'tok_' + rid(rng, 20) };
    return { path: '/orders', req, res: { status, body: status === 201 ? { id: `ord_${randInt(rng, 100000, 999999)}`, status: 'open' } : status === 422 ? { error: 'invalid address' } : { error: 'overloaded' } }, slow: status >= 500 ? 5 : 1 };
  } },
  { method: 'GET', tmpl: '/orders', w: 20, ms: 28, auth: 'jwt', gen(rng, { c }) {
    return { path: '/orders', query: 'status=open&limit=25', res: { status: 200, body: { items: Array.from({ length: 3 }, () => ({ id: `ord_${randInt(rng, 100000, 999999)}`, status: 'open', email: c.email })), total: 3 } } };
  } },
  { method: 'PUT', tmpl: '/orders/{id}/status', w: 12, ms: 31, auth: 'jwt', gen(rng) {
    const id = `ord_${randInt(rng, 100000, 999999)}`;
    const status = pickStatus(rng, [[200, 93], [409, 5], [500, 2]]);
    return { path: `/orders/${id}/status`, req: { status: pick(rng, ['shipped', 'cancelled']) }, res: { status, body: status === 200 ? { id, status: 'shipped' } : status === 409 ? { error: 'invalid transition' } : { error: 'internal error' } } };
  } },
  { method: 'DELETE', tmpl: '/orders/{id}', w: 3, ms: 19, auth: 'jwt', gen(rng) {
    return { path: `/orders/ord_${randInt(rng, 100000, 999999)}`, res: { status: 204, body: null } };
  } },
];

export const DOMAINS_DEF = { payments: PAYMENTS, catalog: CATALOG, orders: ORDERS };
export const endpointNames = (domain) => DOMAINS_DEF[domain].map((e) => `${e.method} ${e.tmpl}`);

/** Build one ExchangeView-like object (without ids). */
function buildExchange(rng, def, ctx, host, startMs, scale) {
  const g = def.gen(rng, ctx);
  const c = ctx.c;
  const status = g.res.status;
  const ms = Math.max(0.6, lognorm(rng, def.ms * (g.slow || 1) * scale, 0.45));
  const hasBody = g.req !== undefined;
  const reqH = reqHeaders(rng, c, { hasBody, host, cookie: def.cookie || rng() < 0.12, auth: def.auth });
  let reqBody = noBody();
  if (g.req && g.req.__truncated) {
    reqBody = { encoding: 'text', data: g.req.__truncated, size: g.req.size, truncated: true, content_type: 'application/json' };
  } else if (g.req) reqBody = jsonBody(g.req);
  let resBody;
  let resCT = 'application/json';
  if (g.res.body === null) resBody = noBody();
  else if (g.res.body && g.res.body.__binary) {
    resCT = g.res.body.ct;
    resBody = { encoding: 'base64', data: g.res.body.__binary, size: Buffer.from(g.res.body.__binary, 'base64').length, content_type: resCT };
  } else resBody = jsonBody(g.res.body);
  const extra = {};
  if (g.setCookie) extra['Set-Cookie'] = [`session=${rid(rng, 32)}; Path=/; HttpOnly; Secure; SameSite=Lax`];
  if (status === 503) extra['Retry-After'] = ['1'];
  const request = {
    method: def.method, scheme: 'http', host, path: g.path, ...(g.query ? { raw_query: g.query } : {}), proto: 'HTTP/1.1',
    header: reqH, body: reqBody,
  };
  const response = { status, proto: 'HTTP/1.1', header: resHeaders(rng, resCT, resBody.size || 0, extra), body: resBody, ...(rng() < 0.3 && status === 200 && resBody.encoding === 'text' ? { content_encoding: 'gzip' } : {}) };
  return {
    ms, started_at: startMs, endpoint: `${def.method} ${def.tmpl}`, request, response,
    error: '', anonymized: false,
  };
}

/** Generate n exchanges for a domain. Returns [{view, ms, t}] ordered by time. */
export function generateExchanges({ domain, n, seed, startMs, spanMs, host, customers, scale = 1, includeTimeout = true }) {
  const rng = mulberry32(seed);
  const defs = DOMAINS_DEF[domain];
  const out = [];
  let t = startMs;
  const mean = spanMs / n;
  for (let i = 0; i < n; i++) {
    const burst = Math.sin(i / 27) > 0.6 ? 0.25 : 1;
    t += -Math.log(1 - rng()) * mean * burst;
    const def = weighted(rng, defs.map((d) => ({ w: d.w, d }))).d;
    const c = pick(rng, customers);
    const ex = buildExchange(rng, def, { c, customers }, host, t, scale);
    out.push(ex);
  }
  // sprinkle a few transport-level failures (upstream unreachable / timeout)
  if (includeTimeout) {
    for (let k = 0; k < Math.max(2, Math.floor(n / 150)); k++) {
      const ex = out[Math.floor(rng() * out.length)];
      ex.response = { status: 504, proto: 'HTTP/1.1', header: { 'Content-Type': ['application/json'] }, body: jsonBody({ error: 'upstream timeout' }) };
      ex.error = 'upstream request timed out after 30s';
      ex.ms = 30000 + rng() * 40;
    }
  }
  return out;
}

export function finalizeExchange(raw, { id, seq, recordingId, serviceId }) {
  const ttfb = raw.ms * 0.72;
  return {
    id, recording_id: recordingId, seq, ...(serviceId ? { service_id: serviceId } : {}),
    started_at: iso(raw.started_at), duration: fmtGoDuration(raw.ms), ttfb: fmtGoDuration(ttfb),
    endpoint: raw.endpoint, request: raw.request, response: raw.response,
    error: raw.error, anonymized: raw.anonymized,
    _ms: raw.ms, _t: raw.started_at,
  };
}

export function publicExchange(ex) {
  const { _ms, _t, ...rest } = ex;
  return rest;
}

export function exchangeSummary(ex) {
  const ct = (ex.response.header['Content-Type'] || [''])[0];
  return {
    id: ex.id, seq: ex.seq, started_at: ex.started_at, duration: ex.duration,
    method: ex.request.method, path: ex.request.path, ...(ex.request.raw_query ? { raw_query: ex.request.raw_query } : {}),
    status: ex.response.status, endpoint: ex.endpoint,
    req_bytes: ex.request.body.size || 0, resp_bytes: ex.response.body.size || 0,
    content_type: ct, error: ex.error || '',
  };
}

export const isErrorEx = (ex) => !!ex.error || ex.response.status === 0 || ex.response.status >= 500 || ex.response.status === 429;

export { newId };
