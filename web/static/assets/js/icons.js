// Inline SVG icons (24x24, stroke based, currentColor). Built with the svg()
// helper so no markup strings are parsed.
import { svg } from './dom.js';

const P = (d) => ['path', { d }];
const C = (cx, cy, r) => ['circle', { cx, cy, r }];
const R = (x, y, w, h, rx = 0) => ['rect', { x, y, width: w, height: h, rx }];
const POLY = (points) => ['polygon', { points }];
const LINE = (x1, y1, x2, y2) => ['line', { x1, y1, x2, y2 }];

const ICONS = {
  dashboard: [R(3, 3, 7, 9, 1.5), R(14, 3, 7, 5, 1.5), R(14, 12, 7, 9, 1.5), R(3, 16, 7, 5, 1.5)],
  services: [R(2, 3, 20, 8, 2), R(2, 13, 20, 8, 2), P('M6 7h.01M6 17h.01')],
  recordings: [['ellipse', { cx: 12, cy: 5, rx: 9, ry: 3 }], P('M21 12c0 1.66-4 3-9 3s-9-1.34-9-3'), P('M3 5v14c0 1.66 4 3 9 3s9-1.34 9-3V5')],
  replays: [C(12, 12, 10), POLY('10 8 16 12 10 16 10 8')],
  shield: [P('M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z'), P('M9 12l2 2 4-4')],
  faults: [POLY('13 2 3 14 12 14 11 22 21 10 12 10 13 2')],
  settings: [P('M4 21v-7M4 10V3M12 21v-9M12 8V3M20 21v-5M20 12V3M1 14h6M9 8h6M17 16h6')],
  plus: [P('M12 5v14M5 12h14')],
  minus: [P('M5 12h14')],
  x: [P('M18 6L6 18M6 6l12 12')],
  check: [P('M20 6L9 17l-5-5')],
  'chevron-down': [P('M6 9l6 6 6-6')],
  'chevron-up': [P('M18 15l-6-6-6 6')],
  'chevron-right': [P('M9 18l6-6-6-6')],
  'chevron-left': [P('M15 18l-6-6 6-6')],
  trash: [P('M3 6h18M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6M10 11v6M14 11v6')],
  edit: [P('M12 20h9M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4L16.5 3.5z')],
  copy: [R(9, 9, 13, 13, 2), P('M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1')],
  download: [P('M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4M7 10l5 5 5-5M12 15V3')],
  upload: [P('M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4M17 8l-5-5-5 5M12 3v12')],
  search: [C(11, 11, 8), P('M21 21l-4.3-4.3')],
  refresh: [P('M23 4v6h-6M1 20v-6h6'), P('M3.5 9a9 9 0 0 1 14.9-3.4L23 10M1 14l4.6 4.4A9 9 0 0 0 20.5 15')],
  play: [POLY('6 3 20 12 6 21 6 3')],
  stop: [R(5, 5, 14, 14, 2)],
  pause: [R(6, 4, 4, 16, 1), R(14, 4, 4, 16, 1)],
  external: [P('M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6M15 3h6v6M10 14L21 3')],
  alert: [P('M10.29 3.86L1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0zM12 9v4M12 17h.01')],
  info: [C(12, 12, 10), P('M12 16v-4M12 8h.01')],
  sun: [C(12, 12, 4), P('M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M4.93 19.07l1.41-1.41M17.66 6.34l1.41-1.41')],
  moon: [P('M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z')],
  menu: [P('M3 12h18M3 6h18M3 18h18')],
  key: [C(7.5, 15.5, 5.5), P('M11.4 11.6L21 2M16 7l3 3M18 5l2 2')],
  'arrow-up': [P('M12 19V5M5 12l7-7 7 7')],
  'arrow-down': [P('M12 5v14M19 12l-7 7-7-7')],
  'arrow-right': [P('M5 12h14M12 5l7 7-7 7')],
  sort: [P('M7 15l5 5 5-5M7 9l5-5 5 5')],
  filter: [POLY('22 3 2 3 10 12.46 10 19 14 21 14 12.46 22 3')],
  clock: [C(12, 12, 10), P('M12 6v6l4 2')],
  activity: [P('M22 12h-4l-3 9L9 3l-3 9H2')],
  copyplus: [R(8, 8, 13, 13, 2), P('M4 16V5a2 2 0 0 1 2-2h11M14.5 12.5v6M11.5 15.5h6')],
  layers: [POLY('12 2 2 7 12 12 22 7 12 2'), P('M2 17l10 5 10-5M2 12l10 5 10-5')],
  gauge: [P('M12 14l4-4'), P('M3.34 19a10 10 0 1 1 17.32 0')],
  globe: [C(12, 12, 10), P('M2 12h20M12 2a15.3 15.3 0 0 1 4 10 15.3 15.3 0 0 1-4 10 15.3 15.3 0 0 1-4-10 15.3 15.3 0 0 1 4-10z')],
  file: [P('M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z'), P('M14 2v6h6')],
  eye: [P('M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z'), C(12, 12, 3)],
  grip: [P('M9 6h.01M9 12h.01M9 18h.01M15 6h.01M15 12h.01M15 18h.01')],
  compare: [P('M16 3h5v5M8 3H3v5M21 3l-7 7M3 3l7 7M16 21h5v-5M8 21H3v-5M21 21l-7-7M3 21l7-7')],
  git: [C(6, 6, 3), C(6, 18, 3), C(18, 9, 3), P('M6 9v6M18 12a9 9 0 0 1-9 9')],
  more: [C(12, 12, 1), C(5, 12, 1), C(19, 12, 1)],
};

export function icon(name, size = 16, extra = '') {
  const parts = ICONS[name];
  const el = svg('svg', {
    viewBox: '0 0 24 24', width: size, height: size, fill: 'none', stroke: 'currentColor',
    'stroke-width': 2, 'stroke-linecap': 'round', 'stroke-linejoin': 'round',
    class: ('icon ' + extra).trim(), 'aria-hidden': 'true', focusable: 'false',
  });
  if (!parts) return el;
  for (const [tag, attrs] of parts) el.appendChild(svg(tag, attrs));
  return el;
}

export const hasIcon = (name) => !!ICONS[name];
