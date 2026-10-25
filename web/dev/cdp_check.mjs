#!/usr/bin/env node
// cdp_check.mjs - dependency-free headless-Chrome scenario runner over the DevTools protocol.
//
// Launches Google Chrome (--headless=new) with a remote-debugging port, connects to the page
// target with Node's built-in WebSocket, runs a JSON scenario and prints ONE JSON object to
// stdout: { results, console, errors, requests, hosts, steps }. Exit status is non-zero when
// any step fails. Chrome is always killed on exit (normal, error, SIGINT, SIGTERM).
//
// Usage: see README.md next to this file (run.mjs wraps it).  Requires Node >= 22 (global fetch + WebSocket).
// Dependency-free Chrome DevTools Protocol harness; step reference: README.md.

import { spawn } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync, mkdirSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, resolve as resolvePath } from 'node:path';
import { createServer } from 'node:net';

const DEFAULT_CHROME = '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';

/* ------------------------------------------------------------------ CLI */
function usage(msg) {
  if (msg) process.stderr.write('cdp_check: ' + msg + '\n');
  process.stderr.write(
    'usage: node cdp_check.mjs [--chrome PATH] [--width W] [--height H] [--dpr N] [--timeout MS]\n' +
    '                          [--settle MS] [--verbose] (--scenario FILE | --scenario - | \'<json array>\')\n');
  process.exit(2);
}

const opts = { chrome: process.env.CDP_CHROME || DEFAULT_CHROME, width: 1440, height: 900, dpr: 1, timeout: 15000, settle: 250, verbose: false, scenario: null };
{
  const a = process.argv.slice(2);
  for (let i = 0; i < a.length; i++) {
    const k = a[i];
    const next = () => { if (i + 1 >= a.length) usage('missing value for ' + k); return a[++i]; };
    if (k === '--chrome') opts.chrome = next();
    else if (k === '--width') opts.width = +next();
    else if (k === '--height') opts.height = +next();
    else if (k === '--dpr') opts.dpr = +next();
    else if (k === '--timeout') opts.timeout = +next();
    else if (k === '--settle') opts.settle = +next();
    else if (k === '--verbose' || k === '-v') opts.verbose = true;
    else if (k === '--scenario') opts.scenario = next();
    else if (k === '--help' || k === '-h') usage();
    else if (k.startsWith('--')) usage('unknown option ' + k);
    else if (opts.scenario === null) opts.scenario = k;
    else usage('unexpected argument ' + k);
  }
}
if (opts.scenario === null) usage('no scenario given');

let scenarioText;
if (opts.scenario === '-') scenarioText = readFileSync(0, 'utf8');
else if (/^\s*\[/.test(opts.scenario)) scenarioText = opts.scenario;
else scenarioText = readFileSync(opts.scenario, 'utf8');
let scenario;
try { scenario = JSON.parse(scenarioText); } catch (e) { usage('scenario is not valid JSON: ' + e.message); }
if (!Array.isArray(scenario)) usage('scenario must be a JSON array of step objects');

const log = (...m) => { if (opts.verbose) process.stderr.write('[cdp] ' + m.join(' ') + '\n'); };

/* ------------------------------------------------------------ output state */
const out = { results: {}, console: [], errors: [], requests: [], hosts: [], steps: [] };
let exitCode = 0;

/* ---------------------------------------------------------------- chrome */
function freePort() {
  return new Promise((res, rej) => {
    const s = createServer();
    s.unref();
    s.on('error', rej);
    s.listen(0, '127.0.0.1', () => { const p = s.address().port; s.close(() => res(p)); });
  });
}

let chrome = null;
let userDataDir = null;
let ws = null;
let cleaned = false;
function cleanup() {
  if (cleaned) return;
  cleaned = true;
  try { if (ws && ws.readyState <= 1) ws.close(); } catch (_) { /* ignore */ }
  if (chrome && chrome.exitCode === null) {
    try { chrome.kill('SIGKILL'); } catch (_) { /* ignore */ }
  }
  if (userDataDir) { try { rmSync(userDataDir, { recursive: true, force: true }); } catch (_) { /* ignore */ } }
}
process.on('exit', cleanup);
for (const sig of ['SIGINT', 'SIGTERM', 'SIGHUP']) {
  process.on(sig, () => { out.errors.push({ kind: 'harness', text: 'interrupted by ' + sig }); cleanup(); emit(); process.exit(130); });
}
process.on('uncaughtException', (e) => { out.errors.push({ kind: 'harness', text: 'uncaught: ' + (e && e.stack || e) }); cleanup(); emit(); process.exit(1); });

function hostOf(url) {
  let u = String(url || '');
  if (u.startsWith('blob:')) u = u.slice(5);
  if (u.startsWith('data:')) return 'data:';
  try { return new URL(u).host || u.split(':')[0] + ':'; } catch (_) { return '?'; }
}
let emitted = false;
function emit() {
  if (emitted) return;
  emitted = true;
  const hosts = new Set();
  for (const r of out.requests) hosts.add(hostOf(r.url));
  out.hosts = [...hosts].sort();
  process.stdout.write(JSON.stringify(out, null, 2) + '\n');
}

async function launchChrome() {
  const port = await freePort();
  userDataDir = mkdtempSync(resolvePath(tmpdir(), 'cdp-check-'));
  const args = [
    '--headless=new',
    '--remote-debugging-port=' + port,
    '--user-data-dir=' + userDataDir,
    '--window-size=' + opts.width + ',' + opts.height,
    '--disable-gpu',
    '--no-first-run',
    '--no-default-browser-check',
    '--hide-scrollbars',
    'about:blank'
  ];
  log('launch', opts.chrome, args.join(' '));
  chrome = spawn(opts.chrome, args, { stdio: ['ignore', 'ignore', opts.verbose ? 'inherit' : 'ignore'] });
  chrome.on('exit', (code, sig) => { log('chrome exited', code, sig); });
  // wait for the devtools endpoint
  const base = 'http://127.0.0.1:' + port;
  const deadline = Date.now() + 20000;
  let list = null;
  while (Date.now() < deadline) {
    if (chrome.exitCode !== null) throw new Error('chrome exited early with code ' + chrome.exitCode);
    try {
      const r = await fetch(base + '/json/list');
      if (r.ok) { list = await r.json(); if (list.some((t) => t.type === 'page')) break; }
    } catch (_) { /* not up yet */ }
    await sleep(100);
  }
  if (!list) throw new Error('chrome devtools endpoint did not come up on port ' + port);
  let page = list.find((t) => t.type === 'page');
  if (!page) {
    const r = await fetch(base + '/json/new?about:blank', { method: 'PUT' });
    page = await r.json();
  }
  if (!page.webSocketDebuggerUrl) throw new Error('page target has no webSocketDebuggerUrl');
  return page.webSocketDebuggerUrl;
}

/* ------------------------------------------------------------- protocol */
const pending = new Map();
const eventWaiters = [];
const eventHandlers = new Map();
let nextId = 0;

function connect(url) {
  return new Promise((res, rej) => {
    ws = new WebSocket(url);
    ws.addEventListener('open', () => res());
    ws.addEventListener('error', (e) => rej(new Error('websocket error: ' + (e && e.message || 'unknown'))));
    ws.addEventListener('close', () => {
      for (const [, p] of pending) p.reject(new Error('websocket closed'));
      pending.clear();
    });
    ws.addEventListener('message', (ev) => {
      let msg;
      try { msg = JSON.parse(typeof ev.data === 'string' ? ev.data : Buffer.from(ev.data).toString('utf8')); } catch (_) { return; }
      if (msg.id !== undefined) {
        const p = pending.get(msg.id);
        if (!p) return;
        pending.delete(msg.id);
        if (msg.error) p.reject(new Error(msg.error.message + (msg.error.data ? ' (' + msg.error.data + ')' : '')));
        else p.resolve(msg.result || {});
        return;
      }
      if (msg.method) {
        const h = eventHandlers.get(msg.method);
        if (h) { try { h(msg.params || {}); } catch (e) { log('handler error', msg.method, e.message); } }
        for (let i = eventWaiters.length - 1; i >= 0; i--) {
          const w = eventWaiters[i];
          if (w.method === msg.method && (!w.filter || w.filter(msg.params || {}))) { eventWaiters.splice(i, 1); w.resolve(msg.params || {}); }
        }
      }
    });
  });
}

function send(method, params = {}) {
  const id = ++nextId;
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
    try { ws.send(JSON.stringify({ id, method, params })); } catch (e) { pending.delete(id); reject(e); }
  });
}

function waitForEvent(method, timeout, filter) {
  return new Promise((resolve, reject) => {
    const w = { method, filter, resolve: (p) => { clearTimeout(t); resolve(p); } };
    const t = setTimeout(() => {
      const i = eventWaiters.indexOf(w);
      if (i >= 0) eventWaiters.splice(i, 1);
      reject(new Error('timed out after ' + timeout + ' ms waiting for ' + method));
    }, timeout);
    eventWaiters.push(w);
  });
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/* --------------------------------------------------------- event capture */
function describeRemote(o) {
  if (!o) return '';
  if (o.value !== undefined) return typeof o.value === 'string' ? o.value : JSON.stringify(o.value);
  if (o.unserializableValue !== undefined) return String(o.unserializableValue);
  if (o.description !== undefined) return o.description;
  return o.type || '';
}
function frameOf(st) {
  const f = st && st.callFrames && st.callFrames[0];
  return f ? { url: f.url, line: f.lineNumber + 1, column: f.columnNumber + 1 } : {};
}
const requestUrls = new Map();

function installCapture() {
  eventHandlers.set('Runtime.consoleAPICalled', (p) => {
    const entry = { kind: 'console', type: p.type, text: (p.args || []).map(describeRemote).join(' '), ...frameOf(p.stackTrace) };
    out.console.push(entry);
    if (p.type === 'error' || p.type === 'assert') out.errors.push(entry);
  });
  eventHandlers.set('Runtime.exceptionThrown', (p) => {
    const d = p.exceptionDetails || {};
    const ex = d.exception;
    const entry = { kind: 'exception', text: (ex && ex.description) || d.text || 'exception', url: d.url, line: d.lineNumber !== undefined ? d.lineNumber + 1 : undefined, column: d.columnNumber !== undefined ? d.columnNumber + 1 : undefined };
    if (!entry.url && d.stackTrace) Object.assign(entry, frameOf(d.stackTrace));
    out.console.push(entry);
    out.errors.push(entry);
  });
  eventHandlers.set('Log.entryAdded', (p) => {
    const e = p.entry || {};
    const entry = { kind: 'log', source: e.source, type: e.level, text: e.text, url: e.url, line: e.lineNumber !== undefined ? e.lineNumber + 1 : undefined };
    out.console.push(entry);
    if (e.level === 'error') out.errors.push(entry);
  });
  eventHandlers.set('Network.requestWillBeSent', (p) => {
    const r = p.request || {};
    requestUrls.set(p.requestId, r.url);
    out.requests.push({ url: r.url, method: r.method, type: p.type, step: currentStepLabel });
  });
  eventHandlers.set('Network.loadingFailed', (p) => {
    if (p.canceled) return;
    const entry = { kind: 'network', text: 'request failed: ' + (p.errorText || '?'), url: requestUrls.get(p.requestId) };
    out.errors.push(entry);
  });
  eventHandlers.set('Network.responseReceived', (p) => {
    const s = p.response && p.response.status;
    if (s >= 400) out.errors.push({ kind: 'network', text: 'HTTP ' + s, url: p.response.url });
  });
  eventHandlers.set('Page.javascriptDialogOpening', (p) => {
    out.console.push({ kind: 'dialog', type: p.type, text: p.message });
    send('Page.handleJavaScriptDialog', { accept: false }).catch(() => {});
  });
}

/* ------------------------------------------------------------ helpers */
async function evaluate(expression, { awaitPromise = true } = {}) {
  const r = await send('Runtime.evaluate', { expression, awaitPromise, returnByValue: true, userGesture: true });
  if (r.exceptionDetails) {
    const d = r.exceptionDetails;
    throw new Error('evaluate threw: ' + ((d.exception && d.exception.description) || d.text));
  }
  return r.result ? r.result.value : undefined;
}

// Scroll a selector into view and return the centre of its bounding rect (viewport coordinates).
async function elementCentre(selector) {
  const js = `(function(){
    var el = document.querySelector(${JSON.stringify(selector)});
    if (!el) return null;
    try { el.scrollIntoView({block:'center', inline:'center'}); } catch (e) { try { el.scrollIntoView(); } catch (_) {} }
    var r = el.getBoundingClientRect();
    return { x: r.left + r.width / 2, y: r.top + r.height / 2, w: r.width, h: r.height, hidden: (r.width === 0 && r.height === 0) };
  })()`;
  const rect = await evaluate(js, { awaitPromise: false });
  if (!rect) throw new Error('no element matches ' + selector);
  if (rect.hidden) throw new Error('element ' + selector + ' has no box (hidden?)');
  return rect;
}

const NAMED_KEYS = {
  Escape: { code: 'Escape', vk: 27 }, Enter: { code: 'Enter', vk: 13, text: '\r' }, Tab: { code: 'Tab', vk: 9 },
  Backspace: { code: 'Backspace', vk: 8 }, Delete: { code: 'Delete', vk: 46 }, ' ': { code: 'Space', vk: 32, text: ' ' },
  Space: { key: ' ', code: 'Space', vk: 32, text: ' ' },
  ArrowLeft: { code: 'ArrowLeft', vk: 37 }, ArrowUp: { code: 'ArrowUp', vk: 38 }, ArrowRight: { code: 'ArrowRight', vk: 39 }, ArrowDown: { code: 'ArrowDown', vk: 40 },
  Home: { code: 'Home', vk: 36 }, End: { code: 'End', vk: 35 }, PageUp: { code: 'PageUp', vk: 33 }, PageDown: { code: 'PageDown', vk: 34 },
  F1: { code: 'F1', vk: 112 }, F2: { code: 'F2', vk: 113 }, F5: { code: 'F5', vk: 116 }
};
const PUNCT_CODES = { '+': 'Equal', '=': 'Equal', '-': 'Minus', '_': 'Minus', '.': 'Period', ',': 'Comma', '/': 'Slash', '?': 'Slash', ';': 'Semicolon', ':': 'Semicolon', "'": 'Quote', '"': 'Quote', '[': 'BracketLeft', ']': 'BracketRight', '\\': 'Backslash', '`': 'Backquote' };
const PUNCT_VK = { '+': 187, '=': 187, '-': 189, '_': 189, '.': 190, ',': 188, '/': 191, '?': 191, ';': 186, ':': 186, "'": 222, '"': 222, '[': 219, ']': 221, '\\': 220, '`': 192 };

function keyDescriptor(name, modifiers) {
  let key = name, code, vk, text;
  if (NAMED_KEYS[name]) {
    const d = NAMED_KEYS[name];
    key = d.key || name; code = d.code; vk = d.vk; text = d.text;
  } else if (name.length === 1) {
    text = name;
    if (/[a-z]/i.test(name)) { code = 'Key' + name.toUpperCase(); vk = name.toUpperCase().charCodeAt(0); }
    else if (/[0-9]/.test(name)) { code = 'Digit' + name; vk = name.charCodeAt(0); }
    else { code = PUNCT_CODES[name] || ''; vk = PUNCT_VK[name] || name.charCodeAt(0); }
  } else {
    throw new Error('unknown key name ' + name);
  }
  let mod = 0;
  for (const m of (modifiers || [])) { if (m === 'alt') mod |= 1; else if (m === 'ctrl') mod |= 2; else if (m === 'meta') mod |= 4; else if (m === 'shift') mod |= 8; }
  return { key, code, vk, text, modifiers: mod };
}

async function pressKey(name, modifiers) {
  const d = keyDescriptor(name, modifiers);
  const common = { key: d.key, code: d.code, windowsVirtualKeyCode: d.vk, nativeVirtualKeyCode: d.vk, modifiers: d.modifiers };
  const down = { type: d.text ? 'keyDown' : 'rawKeyDown', ...common };
  if (d.text) { down.text = d.text; down.unmodifiedText = d.text; }
  await send('Input.dispatchKeyEvent', down);
  await send('Input.dispatchKeyEvent', { type: 'keyUp', ...common });
}

async function clickAt(x, y, { button = 'left', clickCount = 1 } = {}) {
  await send('Input.dispatchMouseEvent', { type: 'mouseMoved', x, y, button: 'none' });
  await send('Input.dispatchMouseEvent', { type: 'mousePressed', x, y, button, clickCount });
  await send('Input.dispatchMouseEvent', { type: 'mouseReleased', x, y, button, clickCount });
}

function ensureDir(p) { try { mkdirSync(dirname(resolvePath(p)), { recursive: true }); } catch (_) { /* ignore */ } }

/* ---------------------------------------------------------------- steps */
let currentStepLabel = 'setup';
let viewport = { w: opts.width, h: opts.height, dpr: opts.dpr };

async function setViewport(v) {
  viewport = { w: v.w || v.width || viewport.w, h: v.h || v.height || viewport.h, dpr: v.dpr || v.deviceScaleFactor || viewport.dpr || 1 };
  await send('Emulation.setDeviceMetricsOverride', { width: viewport.w, height: viewport.h, deviceScaleFactor: viewport.dpr, mobile: !!v.mobile, screenWidth: viewport.w, screenHeight: viewport.h });
  if (v.touch !== undefined) await send('Emulation.setTouchEmulationEnabled', { enabled: !!v.touch, maxTouchPoints: 5 });
}

async function runStep(step, index) {
  const timeout = step.timeout || opts.timeout;
  const keys = Object.keys(step);
  if (step.goto !== undefined) {
    const loaded = waitForEvent('Page.loadEventFired', timeout);
    const r = await send('Page.navigate', { url: step.goto });
    if (r.errorText) throw new Error('navigation failed: ' + r.errorText);
    await loaded;
    await sleep(step.settle !== undefined ? step.settle : opts.settle);
    return { url: await evaluate('location.href', { awaitPromise: false }) };
  }
  if (step.wait !== undefined) { await sleep(+step.wait); return null; }
  if (step.waitFor !== undefined) {
    const start = Date.now();
    let last;
    while (Date.now() - start < timeout) {
      try { last = await evaluate('!!(' + step.waitFor + ')', { awaitPromise: true }); } catch (e) { last = false; }
      if (last) return { ms: Date.now() - start };
      await sleep(step.poll || 100);
    }
    throw new Error('waitFor timed out after ' + timeout + ' ms: ' + step.waitFor);
  }
  if (step.viewport !== undefined) { await setViewport(step.viewport); await sleep(step.settle !== undefined ? step.settle : 150); return { ...viewport }; }
  if (step.reducedMotion !== undefined) {
    await send('Emulation.setEmulatedMedia', { features: [{ name: 'prefers-reduced-motion', value: step.reducedMotion ? 'reduce' : 'no-preference' }] });
    return { reducedMotion: !!step.reducedMotion };
  }
  if (step.media !== undefined) { // e.g. {media:{"prefers-color-scheme":"dark"}}
    const features = Object.entries(step.media).map(([name, value]) => ({ name, value: String(value) }));
    await send('Emulation.setEmulatedMedia', { features });
    return { features };
  }
  if (step.screenshot !== undefined) {
    const params = { format: step.format || 'png', captureBeyondViewport: false };
    if (step.format === 'jpeg') params.quality = step.quality || 80;
    if (step.clip) params.clip = { x: step.clip.x, y: step.clip.y, width: step.clip.w || step.clip.width, height: step.clip.h || step.clip.height, scale: 1 };
    const r = await send('Page.captureScreenshot', params);
    ensureDir(step.screenshot);
    const buf = Buffer.from(r.data, 'base64');
    writeFileSync(step.screenshot, buf);
    return { path: resolvePath(step.screenshot), bytes: buf.length };
  }
  if (step.click !== undefined) {
    let x, y, info = null;
    if (Array.isArray(step.click)) { [x, y] = step.click; }
    else { const rect = await elementCentre(step.click); x = rect.x; y = rect.y; info = rect; }
    await clickAt(x, y, { button: step.button || 'left', clickCount: step.clickCount || 1 });
    await sleep(step.settle !== undefined ? step.settle : 120);
    return { x: +x.toFixed(1), y: +y.toFixed(1), rect: info };
  }
  if (step.dblclick !== undefined) {
    let x, y;
    if (Array.isArray(step.dblclick)) { [x, y] = step.dblclick; }
    else { const rect = await elementCentre(step.dblclick); x = rect.x; y = rect.y; }
    await clickAt(x, y, { clickCount: 1 });
    await clickAt(x, y, { clickCount: 2 });
    await sleep(step.settle !== undefined ? step.settle : 120);
    return { x, y };
  }
  if (step.hover !== undefined) {
    let x, y;
    if (Array.isArray(step.hover)) { [x, y] = step.hover; }
    else { const rect = await elementCentre(step.hover); x = rect.x; y = rect.y; }
    await send('Input.dispatchMouseEvent', { type: 'mouseMoved', x, y, button: 'none' });
    await sleep(step.settle !== undefined ? step.settle : 80);
    return { x, y };
  }
  if (step.drag !== undefined) { // {drag:{from:[x,y], to:[x,y], steps:8}}
    const [x0, y0] = step.drag.from, [x1, y1] = step.drag.to, n = step.drag.steps || 8;
    await send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: x0, y: y0, button: 'none' });
    await send('Input.dispatchMouseEvent', { type: 'mousePressed', x: x0, y: y0, button: 'left', clickCount: 1, buttons: 1 });
    for (let i = 1; i <= n; i++) {
      await send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: x0 + (x1 - x0) * i / n, y: y0 + (y1 - y0) * i / n, button: 'left', buttons: 1 });
      await sleep(16);
    }
    await send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: x1, y: y1, button: 'left', clickCount: 1 });
    await sleep(step.settle !== undefined ? step.settle : 120);
    return { from: [x0, y0], to: [x1, y1] };
  }
  if (step.key !== undefined) {
    const names = Array.isArray(step.key) ? step.key : [step.key];
    for (const k of names) await pressKey(k, step.modifiers);
    await sleep(step.settle !== undefined ? step.settle : 80);
    return { keys: names };
  }
  if (step.focus !== undefined) {
    const ok = await evaluate(`(function(){var el=document.querySelector(${JSON.stringify(step.focus)}); if(!el) return false; el.focus(); return document.activeElement===el;})()`, { awaitPromise: false });
    if (!ok) throw new Error('could not focus ' + step.focus);
    return { focused: step.focus };
  }
  if (step.insertText !== undefined) {
    if (step.into) await runStep({ focus: step.into }, index);
    await send('Input.insertText', { text: String(step.insertText) });
    await sleep(step.settle !== undefined ? step.settle : 80);
    return { inserted: String(step.insertText) };
  }
  if (step.wheel !== undefined) {
    const w = step.wheel;
    await send('Input.dispatchMouseEvent', { type: 'mouseWheel', x: w.x, y: w.y, deltaX: w.deltaX || 0, deltaY: w.deltaY || 0, button: 'none' });
    await sleep(step.settle !== undefined ? step.settle : 120);
    return { x: w.x, y: w.y, deltaY: w.deltaY || 0 };
  }
  if (step.eval !== undefined) {
    const value = await evaluate(step.eval, { awaitPromise: true });
    if (step.as) out.results[step.as] = value;
    return step.as ? { stored: step.as } : { value };
  }
  if (step.text !== undefined) {
    const value = await evaluate(`(function(){var el=document.querySelector(${JSON.stringify(step.text)}); return el ? el.innerText : null;})()`, { awaitPromise: false });
    if (value === null) throw new Error('no element matches ' + step.text);
    out.results[step.as || step.text] = value;
    return { stored: step.as || step.text, length: value.length };
  }
  if (step.assert !== undefined) { // {assert:"js expression", message:"..."}
    const v = await evaluate('!!(' + step.assert + ')', { awaitPromise: true });
    if (!v) throw new Error('assertion failed: ' + (step.message || step.assert));
    return { ok: true };
  }
  throw new Error('unknown step: ' + JSON.stringify(keys));
}

/* ----------------------------------------------------------------- main */
async function main() {
  const wsUrl = await launchChrome();
  log('connect', wsUrl);
  await connect(wsUrl);
  installCapture();
  await send('Runtime.enable');
  await send('Page.enable');
  await send('Network.enable');
  await send('Log.enable');
  await send('Emulation.setDeviceMetricsOverride', { width: viewport.w, height: viewport.h, deviceScaleFactor: viewport.dpr, mobile: false });
  for (let i = 0; i < scenario.length; i++) {
    const step = scenario[i];
    const label = step.name || (i + ':' + Object.keys(step).filter((k) => !['timeout', 'settle', 'as', 'name'].includes(k)).join(','));
    currentStepLabel = label;
    const t0 = Date.now();
    try {
      const r = await runStep(step, i);
      out.steps.push({ step: label, ok: true, ms: Date.now() - t0, ...(r && typeof r === 'object' ? { info: r } : {}) });
      log('ok', label, (Date.now() - t0) + 'ms');
    } catch (e) {
      const text = 'step ' + label + ' failed: ' + (e && e.message || e);
      out.steps.push({ step: label, ok: false, ms: Date.now() - t0, error: e && e.message || String(e) });
      out.errors.push({ kind: 'step', step: label, text });
      log('FAIL', text);
      exitCode = 1;
      if (step.continueOnError) continue;
      break;
    }
  }
}

// Orderly shutdown: close the socket, ask Chrome to quit, wait for it, then remove the profile.
async function shutdown() {
  try { if (ws && ws.readyState <= 1) ws.close(); } catch (_) { /* ignore */ }
  if (chrome && chrome.exitCode === null) {
    const exited = new Promise((r) => chrome.once('exit', r));
    try { chrome.kill('SIGTERM'); } catch (_) { /* ignore */ }
    await Promise.race([exited, sleep(3000)]);
    if (chrome.exitCode === null) { try { chrome.kill('SIGKILL'); } catch (_) { /* ignore */ } await Promise.race([exited, sleep(1000)]); }
  }
  cleanup();
}

main().catch((e) => {
  out.errors.push({ kind: 'harness', text: e && e.stack || String(e) });
  exitCode = 1;
}).then(shutdown, shutdown).finally(() => {
  emit();
  process.exit(exitCode);
});
