// Dependency-free canvas time-series chart, sparkline and status bar.
//
// TimeChart expects rows: [{t: unixMs, <key>: number, ...}] sorted by t, one
// row per second. Series pick values by key. Colors are CSS custom property
// names ("c1", "err", ...) resolved per draw so theme changes just work.
import { h, svg, throttleRaf } from './dom.js';
import { fmtTime, fmtClock, fmtCompact, fmtInt } from './format.js';

const MAX_ROWS = 4200;
const TICK_STEPS_S = [1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 14400, 43200, 86400];

export function niceStep(range, ticks) {
  const raw = range / Math.max(1, ticks);
  if (!(raw > 0)) return 1;
  const mag = Math.pow(10, Math.floor(Math.log10(raw)));
  const n = raw / mag;
  const f = n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10;
  return f * mag;
}

function lowerBound(rows, t) {
  let lo = 0, hi = rows.length;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (rows[mid].t < t) lo = mid + 1; else hi = mid;
  }
  return lo;
}

export class TimeChart {
  /**
   * @param {object} o
   * @param {string} [o.title]
   * @param {Array<{key:string,label:string,color:string,area?:boolean,dash?:number[],width?:number,format?:Function,hidden?:boolean}>} o.series
   * @param {number} [o.height]
   * @param {number|null} [o.windowMs] visible window; null = fit all rows
   * @param {Function} [o.yFormat] axis labels
   * @param {Function} [o.tipFormat] tooltip values (defaults to yFormat)
   * @param {number|null} [o.yMax] fixed y maximum
   * @param {number|null} [o.yCap] upper limit of the automatic y maximum
   * @param {'clock'|'elapsed'} [o.xMode]
   * @param {number} [o.xBase] unix ms that counts as 0 in elapsed mode
   * @param {string} [o.hint] small text next to the title
   */
  constructor(o) {
    this.o = Object.assign({ height: 180, windowMs: 15 * 60000, yFormat: fmtCompact, yMin: 0, yMax: null, xMode: 'clock', xBase: 0, gapMs: 6000, legend: true }, o);
    this.series = this.o.series.map((s) => Object.assign({ width: 2, hidden: false }, s));
    this.rows = [];
    this.hover = -1; // index into this.rows
    this.vis = { lo: 0, hi: 0 };
    this.dirty = true;
    this.destroyed = false;
    this.geom = null;

    this.canvas = h('canvas', {
      class: 'chart-canvas', role: 'img', tabindex: 0,
      'aria-label': (this.o.title || 'Time series') + ' chart',
    });
    this.tip = h('div', { class: 'chart-tip', role: 'status', hidden: true });
    this.empty = h('div', { class: 'chart-empty' }, this.o.emptyText || 'Waiting for data…');
    this.plot = h('div', { class: 'chart-plot', style: { height: this.o.height + 'px' } }, this.canvas, this.empty, this.tip);
    this.legendEl = h('div', { class: 'chart-legend' });
    this.legendFixed = null; // see setLegendValues
    this.legendNote = h('span', { class: 'chart-legend-note', hidden: true });
    this.titleEl = h('div', { class: 'chart-title' }, this.o.title || '', this.o.hint ? h('span', { class: 'chart-hint' }, this.o.hint) : null);
    this.head = h('div', { class: 'chart-head' }, this.titleEl, this.o.legend ? this.legendEl : null);
    this.el = h('div', { class: 'chart' }, this.head, this.plot);
    this.buildLegend();

    this.draw = this.draw.bind(this);
    this.requestDraw = throttleRaf(this.draw);

    this.ro = new ResizeObserver(() => this.requestDraw());
    this.ro.observe(this.plot);
    this.canvas.addEventListener('pointermove', (e) => this.onMove(e));
    this.canvas.addEventListener('pointerdown', (e) => this.onMove(e));
    this.canvas.addEventListener('pointerleave', () => this.clearHover());
    this.canvas.addEventListener('blur', () => this.clearHover());
    this.canvas.addEventListener('keydown', (e) => this.onKey(e));
  }

  // ------------------------------------------------------------ data
  setRows(rows) {
    this.rows = rows.length > MAX_ROWS ? rows.slice(rows.length - MAX_ROWS) : rows.slice();
    this.hover = -1;
    this.requestDraw();
    this.updateLegendValues();
  }

  push(row) {
    const r = this.rows;
    const n = r.length;
    if (n && row.t === r[n - 1].t) r[n - 1] = row;
    else if (n && row.t < r[n - 1].t) return;
    else r.push(row);
    if (r.length > MAX_ROWS) r.splice(0, r.length - MAX_ROWS);
    if (this.hover >= 0) this.hover = Math.max(-1, this.hover - (r.length > MAX_ROWS ? 1 : 0));
    this.requestDraw();
    if (this.hover < 0) this.updateLegendValues();
  }

  setWindow(ms) {
    this.o.windowMs = ms;
    this.requestDraw();
  }

  setTitle(t) { this.titleEl.firstChild.nodeValue = t; }

  /** Small explanatory text next to the title. */
  setHint(text) {
    let hint = this.titleEl.querySelector('.chart-hint');
    if (!hint) { hint = h('span', { class: 'chart-hint' }); this.titleEl.appendChild(hint); }
    hint.textContent = text || '';
  }

  destroy() {
    this.destroyed = true;
    this.ro.disconnect();
  }

  // ---------------------------------------------------------- legend
  buildLegend() {
    this.legendEl.replaceChildren();
    this.legendBtns = this.series.map((s, i) => {
      const val = h('span', { class: 'legend-val' });
      const btn = h('button', {
        type: 'button', class: 'legend-item', 'aria-pressed': String(!s.hidden),
        title: `Toggle ${s.label}`,
        onclick: () => {
          s.hidden = !s.hidden;
          btn.setAttribute('aria-pressed', String(!s.hidden));
          btn.classList.toggle('is-off', s.hidden);
          this.requestDraw();
        },
      }, h('span', { class: 'legend-key', style: { background: `var(--${s.color})` } }), h('span', { class: 'legend-label' }, s.label), val);
      btn.classList.toggle('is-off', s.hidden);
      return { btn, val, s, i };
    });
    for (const b of this.legendBtns) this.legendEl.appendChild(b.btn);
    this.legendEl.appendChild(this.legendNote);
  }

  /**
   * Pins what the legend reads while nothing is hovered to fixed text per
   * series key, e.g. the figures of a whole finished run instead of its last
   * (partial) second. `note` says what the figures are. null goes back to the
   * latest row.
   */
  setLegendValues(texts, note) {
    this.legendFixed = texts || null;
    this.legendNote.textContent = texts && note ? note : '';
    this.legendNote.hidden = !(texts && note);
    this.updateLegendValues();
  }

  updateLegendValues(row) {
    if (!this.o.legend) return;
    if (!row && this.legendFixed) {
      for (const b of this.legendBtns) b.val.textContent = this.legendFixed[b.s.key] || '';
      return;
    }
    for (const b of this.legendBtns) {
      let r = row || this.rows[this.rows.length - 1];
      if (!row) {
        // latest row that actually has a value for this series
        for (let i = this.rows.length - 1, n = 0; i >= 0 && n < 400; i--, n++) { if (this.rows[i][b.s.key] != null) { r = this.rows[i]; break; } }
      }
      const v = r ? r[b.s.key] : null;
      b.val.textContent = v == null ? '' : (b.s.format || this.o.tipFormat || this.o.yFormat)(v);
    }
  }

  // ----------------------------------------------------------- hover
  onMove(e) {
    if (!this.geom || !this.rows.length) return;
    const rect = this.canvas.getBoundingClientRect();
    const x = e.clientX - rect.left;
    const g = this.geom;
    const t = g.t0 + ((x - g.l) / (g.r - g.l)) * (g.t1 - g.t0);
    const { lo, hi } = this.vis;
    if (hi <= lo) return;
    // nearest row in [lo, hi)
    let i = lowerBound(this.rows, t);
    if (i >= hi) i = hi - 1;
    if (i > lo && Math.abs(this.rows[i - 1].t - t) < Math.abs(this.rows[i].t - t)) i -= 1;
    if (i < lo) i = lo;
    this.hover = i;
    this.hoverY = e.clientY - rect.top;
    this.requestDraw();
  }

  onKey(e) {
    if (!this.rows.length) return;
    if (e.key === 'ArrowLeft' || e.key === 'ArrowRight') {
      e.preventDefault();
      const { lo, hi } = this.vis;
      let i = this.hover < 0 ? hi - 1 : this.hover + (e.key === 'ArrowLeft' ? -1 : 1);
      i = Math.max(lo, Math.min(hi - 1, i));
      this.hover = i;
      this.hoverY = 20;
      this.requestDraw();
    } else if (e.key === 'Escape') this.clearHover();
  }

  clearHover() {
    if (this.hover < 0) return;
    this.hover = -1;
    this.tip.hidden = true;
    this.updateLegendValues();
    this.requestDraw();
  }

  // ------------------------------------------------------------ draw
  draw() {
    if (this.destroyed) return;
    const w = this.plot.clientWidth;
    const hgt = this.plot.clientHeight;
    if (w < 10 || hgt < 10) return;
    const dpr = window.devicePixelRatio || 1;
    const cv = this.canvas;
    if (cv.width !== Math.round(w * dpr) || cv.height !== Math.round(hgt * dpr)) {
      cv.width = Math.round(w * dpr);
      cv.height = Math.round(hgt * dpr);
      cv.style.width = w + 'px';
      cv.style.height = hgt + 'px';
    }
    const ctx = cv.getContext('2d');
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, w, hgt);

    const cs = getComputedStyle(this.el);
    const css = (n) => cs.getPropertyValue(n).trim() || '#888';
    const grid = css('--chart-grid');
    const axis = css('--chart-axis');
    const muted = css('--text-3');
    const surface = css('--panel');
    const font = '11px ' + (cs.fontFamily || 'system-ui, sans-serif');

    const rows = this.rows;
    this.empty.hidden = rows.length > 0;
    if (!rows.length) { this.geom = null; return; }

    // x domain
    const last = rows[rows.length - 1].t;
    let t1 = last;
    let t0;
    if (this.o.windowMs == null) {
      t0 = rows[0].t;
      if (t1 - t0 < 4000) t1 = t0 + 4000;
    } else {
      t0 = t1 - this.o.windowMs;
    }
    const lo = lowerBound(rows, t0);
    const hi = rows.length;
    this.vis = { lo, hi };

    // y domain
    const live = this.series.filter((s) => !s.hidden);
    let max = 0;
    for (let i = lo; i < hi; i++) {
      for (const s of live) {
        const v = rows[i][s.key];
        if (v > max) max = v;
      }
    }
    let yMax;
    let step;
    if (this.o.yMax != null) {
      yMax = this.o.yMax;
      step = niceStep(yMax - this.o.yMin, 4);
    } else {
      let want = max > 0 ? max * 1.08 : 1;
      if (this.o.yCap != null) want = Math.min(want, this.o.yCap); // e.g. a percentage never needs more than 100
      step = niceStep(want - this.o.yMin, 4);
      yMax = Math.ceil(want / step) * step;
      if (yMax <= this.o.yMin) yMax = this.o.yMin + step;
    }
    const yMin = this.o.yMin;

    ctx.font = font;
    let maxLabel = 0;
    const yTicks = [];
    for (let v = yMin; v <= yMax + step * 1e-6; v += step) {
      const label = this.o.yFormat(v);
      yTicks.push([v, label]);
      maxLabel = Math.max(maxLabel, ctx.measureText(label).width);
    }
    const L = Math.ceil(maxLabel) + 12;
    const R = 10, T = 8, B = 20;
    const pw = w - L - R;
    const ph = hgt - T - B;
    if (pw < 20 || ph < 20) return;
    const X = (t) => L + ((t - t0) / (t1 - t0)) * pw;
    const Y = (v) => T + ph - ((v - yMin) / (yMax - yMin)) * ph;
    this.geom = { l: L, r: L + pw, t0, t1, top: T, bottom: T + ph };

    // grid + y labels
    ctx.lineWidth = 1;
    ctx.textBaseline = 'middle';
    ctx.textAlign = 'right';
    for (const [v, label] of yTicks) {
      const y = Math.round(Y(v)) + 0.5;
      ctx.strokeStyle = v === yMin ? axis : grid;
      ctx.beginPath(); ctx.moveTo(L, y); ctx.lineTo(L + pw, y); ctx.stroke();
      ctx.fillStyle = muted;
      ctx.fillText(label, L - 8, y);
    }

    // x ticks
    ctx.textBaseline = 'top';
    ctx.textAlign = 'center';
    const spanS = (t1 - t0) / 1000;
    const maxTicks = Math.max(2, Math.floor(pw / 78));
    let iv = TICK_STEPS_S[TICK_STEPS_S.length - 1];
    for (const c of TICK_STEPS_S) { if (spanS / c <= maxTicks) { iv = c; break; } }
    const elapsed = this.o.xMode === 'elapsed';
    const off = elapsed ? 0 : -new Date(t1).getTimezoneOffset() * 60000;
    const base = elapsed ? (this.o.xBase || 0) : 0;
    const ivMs = iv * 1000;
    let tick = Math.ceil((t0 - base + off) / ivMs) * ivMs - off + base;
    for (; tick <= t1 + 1; tick += ivMs) {
      const x = Math.round(X(tick)) + 0.5;
      if (x < L - 1 || x > L + pw + 1) continue;
      ctx.strokeStyle = grid;
      ctx.beginPath(); ctx.moveTo(x, T + ph); ctx.lineTo(x, T + ph + 4); ctx.stroke();
      ctx.fillStyle = muted;
      const lbl = elapsed ? fmtClock((tick - base) / 1000) : fmtTime(tick, iv < 60);
      const tw = ctx.measureText(lbl).width;
      const cx = Math.min(Math.max(x, L + tw / 2), L + pw - tw / 2 + 4);
      ctx.fillText(lbl, cx, T + ph + 6);
    }

    // series
    ctx.save();
    ctx.beginPath(); ctx.rect(L, T - 2, pw, ph + 4); ctx.clip();
    const gap = this.o.gapMs;
    for (const s of live) {
      const color = css('--' + s.color);
      ctx.lineJoin = 'round';
      ctx.lineCap = 'round';
      ctx.lineWidth = s.width;
      ctx.strokeStyle = color;
      ctx.setLineDash(s.dash || []);
      // draw in segments split on gaps
      let segStart = -1;
      const flush = (a, b) => { // inclusive indices
        if (b < a) return;
        ctx.beginPath();
        for (let i = a; i <= b; i++) {
          const v = rows[i][s.key];
          const x = X(rows[i].t);
          const y = Y(v == null ? 0 : v);
          if (i === a) ctx.moveTo(x, y); else ctx.lineTo(x, y);
        }
        if (a === b) { ctx.lineTo(X(rows[a].t) + 0.01, Y(rows[a][s.key] || 0)); }
        ctx.stroke();
        if (s.area && !s.dash) {
          ctx.lineTo(X(rows[b].t), Y(yMin));
          ctx.lineTo(X(rows[a].t), Y(yMin));
          ctx.closePath();
          const grad = ctx.createLinearGradient(0, T, 0, T + ph);
          grad.addColorStop(0, withAlpha(color, 0.28));
          grad.addColorStop(1, withAlpha(color, 0.02));
          ctx.fillStyle = grad;
          ctx.fill();
        }
      };
      for (let i = Math.max(0, lo - 1); i < hi; i++) {
        // A row without a value for this series (a target that is not known
        // yet, a run shorter than the other one) is a gap, not a zero.
        if (rows[i][s.key] == null) { if (segStart >= 0) { flush(segStart, i - 1); segStart = -1; } continue; }
        if (segStart < 0) { segStart = i; continue; }
        if (rows[i].t - rows[i - 1].t > gap) { flush(segStart, i - 1); segStart = i; }
      }
      if (segStart >= 0) flush(segStart, hi - 1);
      ctx.setLineDash([]);
    }
    ctx.restore();

    // crosshair
    if (this.hover >= 0 && this.hover < hi && this.hover >= lo) {
      const row = rows[this.hover];
      const x = Math.round(X(row.t)) + 0.5;
      ctx.strokeStyle = muted;
      ctx.globalAlpha = 0.7;
      ctx.setLineDash([3, 3]);
      ctx.beginPath(); ctx.moveTo(x, T); ctx.lineTo(x, T + ph); ctx.stroke();
      ctx.setLineDash([]);
      ctx.globalAlpha = 1;
      for (const s of live) {
        const v = row[s.key];
        if (v == null) continue;
        ctx.beginPath();
        ctx.arc(X(row.t), Y(v), 4, 0, Math.PI * 2);
        ctx.fillStyle = css('--' + s.color);
        ctx.fill();
        ctx.lineWidth = 2;
        ctx.strokeStyle = surface;
        ctx.stroke();
      }
      this.showTip(row, X(row.t), live);
    } else {
      this.tip.hidden = true;
    }
  }

  showTip(row, x, live) {
    const tip = this.tip;
    const label = this.o.xMode === 'elapsed' ? fmtClock((row.t - (this.o.xBase || 0)) / 1000) : fmtTime(row.t, true);
    const items = [h('div', { class: 'tip-time' }, label)];
    for (const s of live) {
      const v = row[s.key];
      items.push(h('div', { class: 'tip-row' },
        h('span', { class: 'tip-key', style: { background: `var(--${s.color})` } }),
        h('span', { class: 'tip-val' }, v == null ? '–' : (s.format || this.o.tipFormat || this.o.yFormat)(v)),
        h('span', { class: 'tip-label' }, s.label)));
    }
    tip.replaceChildren(...items);
    tip.hidden = false;
    this.updateLegendValues(row);
    const pw = this.plot.clientWidth;
    const tw = tip.offsetWidth;
    let left = x + 14;
    if (left + tw > pw - 4) left = x - tw - 14;
    if (left < 4) left = 4;
    tip.style.left = left + 'px';
    const th = tip.offsetHeight;
    let top = (this.hoverY || 20) - th / 2;
    top = Math.max(4, Math.min(this.plot.clientHeight - th - 4, top));
    tip.style.top = top + 'px';
  }
}

function withAlpha(color, a) {
  // color is "#rrggbb" or "rgb(...)"; produce rgba
  let m = /^#([0-9a-f]{3}|[0-9a-f]{6})$/i.exec(color);
  if (m) {
    let hex = m[1];
    if (hex.length === 3) hex = hex.split('').map((c) => c + c).join('');
    const n = parseInt(hex, 16);
    return `rgba(${(n >> 16) & 255},${(n >> 8) & 255},${n & 255},${a})`;
  }
  m = /^rgba?\(([^)]+)\)$/i.exec(color);
  if (m) {
    const p = m[1].split(',').map((x) => x.trim());
    return `rgba(${p[0]},${p[1]},${p[2]},${a})`;
  }
  return color;
}

// ------------------------------------------------------------ sparkline

/** Small inline SVG sparkline. update(values) re-renders in place. */
export function sparkline(values, { width = 96, height = 26, color = 'c1', area = true, fixedMax = null } = {}) {
  const root = svg('svg', { class: 'spark', width, height, viewBox: `0 0 ${width} ${height}`, preserveAspectRatio: 'none', 'aria-hidden': 'true' });
  const areaPath = svg('path', { class: 'spark-area', fill: `var(--${color})`, opacity: 0.16, stroke: 'none' });
  const line = svg('path', { class: 'spark-line', fill: 'none', stroke: `var(--${color})`, 'stroke-width': 1.5, 'stroke-linejoin': 'round', 'stroke-linecap': 'round', 'vector-effect': 'non-scaling-stroke' });
  if (area) root.appendChild(areaPath);
  root.appendChild(line);
  function update(vals) {
    if (!vals || vals.length < 2) { line.setAttribute('d', ''); areaPath.setAttribute('d', ''); return; }
    let hi = fixedMax != null ? fixedMax : Math.max(...vals);
    let lo = fixedMax != null ? 0 : Math.min(...vals);
    if (hi - lo < 1e-9) { lo = Math.max(0, lo - 1); hi = hi + 1; }
    // keep some headroom so a flat-ish series does not look like a wall
    const span = hi - lo;
    lo = Math.max(0, lo - span * 0.15);
    const n = vals.length;
    const pad = 2;
    const pts = vals.map((v, i) => [(i / (n - 1)) * width, height - pad - ((Math.max(lo, v) - lo) / (hi - lo)) * (height - pad * 2)]);
    const d = pts.map((p, i) => (i ? 'L' : 'M') + p[0].toFixed(1) + ' ' + p[1].toFixed(1)).join('');
    line.setAttribute('d', d);
    areaPath.setAttribute('d', d + `L${width} ${height}L0 ${height}Z`);
  }
  update(values);
  return { el: root, update };
}

// ------------------------------------------------------------ status bar

const CLASS_ORDER = ['1xx', '2xx', '3xx', '4xx', '5xx', 'err'];
const CLASS_LABEL = { '1xx': '1xx info', '2xx': '2xx ok', '3xx': '3xx redirect', '4xx': '4xx client', '5xx': '5xx server', err: 'no response' };

/** Group {"200": 10, "503": 2} into {"2xx": 10, "5xx": 2}. "0"/"err" -> err. */
export function groupStatus(counts) {
  const out = {};
  for (const [k, v] of Object.entries(counts || {})) {
    let c;
    if (/^\d+$/.test(k)) c = k === '0' ? 'err' : k[0] + 'xx';
    else c = k;
    out[c] = (out[c] || 0) + v;
  }
  return out;
}

/** Horizontal stacked bar of status classes with a legend row. */
export function statusBar(classCounts, { legend = true, height = 10 } = {}) {
  const total = CLASS_ORDER.reduce((s, c) => s + (classCounts[c] || 0), 0);
  const bar = h('div', { class: 'statusbar', style: { height: height + 'px' }, role: 'img', 'aria-label': 'Status class breakdown' });
  const leg = h('div', { class: 'statusbar-legend' });
  if (!total) {
    bar.appendChild(h('div', { class: 'statusbar-seg c-none', style: { flex: '1' } }));
  }
  for (const c of CLASS_ORDER) {
    const n = classCounts[c] || 0;
    if (!n) continue;
    const pct = (n / total) * 100;
    bar.appendChild(h('div', { class: `statusbar-seg sc-${c}`, style: { flex: `${n} 1 0`, minWidth: '3px' }, title: `${CLASS_LABEL[c]}: ${fmtInt(n)} (${pct.toFixed(pct < 1 ? 2 : 1)}%)` }));
    leg.appendChild(h('span', { class: 'sb-item' }, h('span', { class: `sb-dot sc-${c}` }), h('span', { class: 'sb-name' }, CLASS_LABEL[c]), h('span', { class: 'sb-n' }, fmtInt(n)), h('span', { class: 'sb-pct' }, pct.toFixed(pct < 1 ? 2 : 1) + '%')));
  }
  return h('div', { class: 'statusbar-wrap' }, bar, legend ? leg : null);
}

// -------------------------------------------------------- ramp preview

/** SVG preview of a rate ramp. stages: [{durationMs, rate}] (linear from previous). */
export function rampChart(stages, { width = 600, height = 120 } = {}) {
  const root = svg('svg', { class: 'ramp', viewBox: `0 0 ${width} ${height}`, preserveAspectRatio: 'xMidYMid meet', role: 'img', 'aria-label': 'Rate ramp preview', style: 'width:100%;height:auto' });
  const total = stages.reduce((s, x) => s + x.durationMs, 0);
  const peak = Math.max(...stages.map((s) => s.rate), 1);
  const padL = 40, padR = 8, padT = 8, padB = 18;
  const pw = width - padL - padR;
  const ph = height - padT - padB;
  const grid = svg('g', { class: 'ramp-grid' });
  for (let i = 0; i <= 2; i++) {
    const y = padT + (ph * i) / 2;
    grid.appendChild(svg('line', { x1: padL, x2: width - padR, y1: y, y2: y }));
    const t = svg('text', { x: padL - 6, y: y + 3, 'text-anchor': 'end' });
    t.textContent = fmtCompact(peak * (1 - i / 2));
    grid.appendChild(t);
  }
  root.appendChild(grid);
  if (!total || !stages.length) return root;
  let t = 0;
  const pts = [[padL, padT + ph]];
  const xAt = (ms) => padL + (ms / total) * pw;
  const yAt = (rate) => padT + ph - (rate / peak) * ph;
  for (const s of stages) {
    if (s.hold) pts.push([xAt(t), yAt(s.rate)]); // steps straight to the rate
    t += s.durationMs;
    pts.push([xAt(t), yAt(s.rate)]);
  }
  const d = pts.map((p, i) => (i ? 'L' : 'M') + p[0].toFixed(1) + ' ' + p[1].toFixed(1)).join('');
  root.appendChild(svg('path', { class: 'ramp-area', d: d + `L${pts[pts.length - 1][0]} ${padT + ph}L${padL} ${padT + ph}Z` }));
  root.appendChild(svg('path', { class: 'ramp-line', d }));
  // x labels
  const lbl = (x, txt, anchor) => { const e = svg('text', { class: 'ramp-x', x, y: height - 4, 'text-anchor': anchor }); e.textContent = txt; return e; };
  root.appendChild(lbl(padL, '0', 'start'));
  root.appendChild(lbl(width - padR, fmtClock(total / 1000), 'end'));
  return root;
}
