// Settings / About: server info, admin token, theme.
import { h } from '../dom.js';
import { api, getToken, setToken, clearToken, apiUrl, notifyTokenChange } from '../api.js';
import { state, refreshInfo, on } from '../state.js';
import { card, kvList, btn, badge, textCtl, field, segmentedCtl, toastOk, toast, copyButton, pageHeader } from '../ui.js';
import { fmtDateTime, fmtHuman, relTime } from '../format.js';
import { getPref, setPref } from '../theme.js';

export default async function settings(ctx) {
  ctx.setCrumbs([{ label: 'Settings' }]);
  const root = h('div', { class: 'stack' });
  root.appendChild(pageHeader({ subtitle: 'Server details, access control and appearance.' }));

  let info = state.info;
  let health = null;
  try { info = await refreshInfo(); } catch (_) { /* shown below */ }
  try { health = await api.health(); } catch (_) { /* ignore */ }

  // --- server
  const serverBody = h('div');
  function paintServer() {
    const auth = info ? info.auth : null;
    serverBody.replaceChildren(kvList([
      ['Version', info ? info.version : null, true],
      ['Go version', info ? info.go_version : null, true],
      ['Data path', info ? info.data_path : null, true],
      ['Started', info && info.started_at ? `${fmtDateTime(info.started_at)} (${relTime(info.started_at)})` : null],
      ['Uptime', health && health.uptime_s != null ? fmtHuman(health.uptime_s * 1000) : null],
      ['Authentication', auth == null ? null : h('span', null, auth ? badge('Admin token required', 'warn') : badge('Open (no token)', 'muted'))],
      ['Live stream', h('span', null, state.conn === 'connected' ? badge('Connected', 'ok', { dot: true }) : badge(state.conn, 'warn', { dot: true }))],
    ]));
  }
  paintServer();
  ctx.on('conn', paintServer);
  root.appendChild(card({ title: 'Server', body: serverBody }));

  // --- endpoints
  const metricsHref = apiUrl('', null, true).replace('/api/v1', '/metrics');
  const tok = getToken();
  root.appendChild(card({
    title: 'Endpoints',
    subtitle: 'Everything the UI does goes through the public JSON API.',
    body: h('div', { class: 'stack-sm' },
      kvList([
        ['Prometheus metrics', h('span', { class: 'row' }, h('a', { href: metricsHref, target: '_blank', rel: 'noopener' }, '/metrics'), h('span', { class: 'muted' }, 'scrape target, text format')), false],
        ['Health', h('a', { href: '/api/v1/health', target: '_blank', rel: 'noopener' }, '/api/v1/health')],
        ['Live stream', h('code', null, '/api/v1/stream'), false],
        ['API reference', h('span', null, h('code', null, 'docs/API.md'), ' in the Mirage repository')],
      ]),
      h('div', { class: 'hint-box' }, h('code', { class: 'grow' }, `curl ${location.origin}/api/v1/services${tok ? ' -H "Authorization: Bearer $MIRAGE_TOKEN"' : ''}`), copyButton(`curl ${location.origin}/api/v1/services${tok ? ' -H "Authorization: Bearer $MIRAGE_TOKEN"' : ''}`)))
  }));

  // --- token
  const tokInput = textCtl({ type: 'password', placeholder: 'Paste admin token', mono: true });
  const tokState = h('div');
  function paintTok() {
    const t = getToken();
    tokState.replaceChildren(t ? badge('A token is saved in this browser', 'ok', { dot: true }) : badge('No token saved', 'muted'));
  }
  paintTok();
  root.appendChild(card({
    title: 'Admin token',
    subtitle: 'Only needed when the server runs with --admin-token. The token stays in this browser (localStorage) and is sent as a Bearer header; live-stream and download links use an access_token query parameter.',
    body: h('div', { class: 'stack-sm' },
      tokState,
      h('div', { class: 'row' },
        h('div', { style: 'flex:1 1 280px;max-width:420px' }, field('Set token', tokInput)),
        h('div', { style: 'margin-top:20px' }, btn('Save token', { kind: 'primary', onClick: () => {
          const v = tokInput.get().trim();
          if (!v) { toast('Enter a token first', 'error'); return; }
          setToken(v); tokInput.set(''); paintTok(); notifyTokenChange(); toastOk('Token saved');
        } })),
        h('div', { style: 'margin-top:20px' }, btn('Clear token', { kind: 'danger', onClick: () => { clearToken(); paintTok(); notifyTokenChange(); toastOk('Token cleared'); } })))),
  }));

  // --- appearance
  const themeSeg = segmentedCtl([
    { value: 'system', label: 'System' },
    { value: 'dark', label: 'Dark' },
    { value: 'light', label: 'Light' },
  ], { value: getPref(), label: 'Theme' });
  themeSeg.onChange((v) => setPref(v));
  root.appendChild(card({
    title: 'Appearance',
    subtitle: 'System follows your OS setting and falls back to dark.',
    body: themeSeg.el,
  }));

  root.appendChild(card({
    title: 'About Mirage',
    body: h('p', { class: 'explain' },
      'Mirage sits between your clients and an HTTP API. Put a service in front of your API, ',
      h('b', null, 'record'), ' real traffic, ', h('b', null, 'anonymize'), ' it, then ', h('b', null, 'virtualize'),
      ' the API from the recording or ', h('b', null, 'replay'), ' it against a target at 10x for performance tests. Faults let you rehearse failure modes.'),
  }));
  return root;
}
