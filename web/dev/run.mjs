#!/usr/bin/env node
// Compact wrapper around cdp_check.mjs: runs a scenario (JSON file or inline
// JSON array) and prints only failed steps, console errors/warnings, and
// exceptions. Usage: node web/dev/run.mjs <scenario.json|'[...]'> [cdp_check options]
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const here = path.dirname(fileURLToPath(import.meta.url));
const args = process.argv.slice(2);
if (!args.length) { console.error('usage: run.mjs <scenario.json|json> [--width W --height H ...]'); process.exit(2); }
let scenario = args.shift();
if (scenario.endsWith('.mjs')) {
  const mod = await import(path.resolve(scenario));
  scenario = JSON.stringify(typeof mod.default === 'function' ? mod.default() : mod.default);
}
const inline = scenario.trim().startsWith('[');
const r = spawnSync('node', [path.join(here, 'cdp_check.mjs'), ...args, ...(inline ? [] : ['--scenario']), scenario], { cwd: process.cwd(), encoding: 'utf8', maxBuffer: 64 * 1024 * 1024 });
let out;
try { out = JSON.parse(r.stdout); } catch (e) { console.error('could not parse harness output:\n', r.stdout.slice(0, 2000), r.stderr.slice(0, 2000)); process.exit(1); }
const IGNORE = [/Password field is not contained in a form/, /Autofocus processing was blocked/];
let bad = 0;
for (const s of out.steps) if (!s.ok) { bad++; console.log(`STEP FAIL  ${s.step}: ${s.error}`); }
for (const e of out.errors) {
  if (e.kind === 'step') continue;
  if (IGNORE.some((re) => re.test(e.text || ''))) continue;
  if (process.env.IGNORE_HTTP && ((e.kind === 'network' && /^HTTP 4/.test(e.text)) || (e.kind === 'log' && /Failed to load resource: the server responded with a status of 4/.test(e.text)))) continue;
  // the refused stream of a simulated outage (/__mock/sse-drop, /__mock/restart)
  if (process.env.IGNORE_HTTP && ((e.kind === 'network' && /^HTTP 503 .*\/api\/v1\/stream/.test(`${e.text} ${e.url || ''}`)) || (e.kind === 'log' && /status of 503 .*\/api\/v1\/stream/.test(`${e.text} ${e.url || ''}`)))) continue;
  bad++; console.log(`ERROR      [${e.kind}] ${e.text} ${e.url || ''}${e.line ? ':' + e.line : ''}`);
}
for (const c of out.console) {
  if (c.type === 'warning' || (c.kind === 'log' && c.type === 'warning')) console.log(`WARN       ${c.text} ${c.url || ''}`);
}
for (const [k, v] of Object.entries(out.results || {})) console.log(`RESULT     ${k}: ${typeof v === 'string' ? v.slice(0, 600) : JSON.stringify(v).slice(0, 600)}`);
console.log(bad ? `\n${bad} problem(s)` : `OK (${out.steps.length} steps, no console errors)`);
process.exit(bad ? 1 : 0);
