// The symbols a FlowLab node wears at the middle of its body, so a diagram can be
// read by shape as well as by name. Every one is drawn here, as plain strokes on
// a 24 by 24 grid: nothing is an icon font, a picture file or somebody else's
// artwork. A node type's `icon` names its symbol, and a Go test fails when a
// diagram node type names one that is not drawn.
export const SYMBOLS: Record<string, string> = {
  const: 'M6 6h12v12H6z M9 12h6',
  math: 'M12 6v12 M6 12h12',
  compare: 'M6 9h12 M6 15h12 M10 5l4 14',
  logic: 'M6 6h6a6 6 0 010 12H6z',
  control: 'M5 12h14 M9.5 9.5a2.5 2.5 0 100 5 2.5 2.5 0 000-5z',
  indicator: 'M12 5a7 7 0 100 14 7 7 0 000-14z M12 10a2 2 0 100 4 2 2 0 000-4z',
  chart: 'M5 5v14h14 M8 15l3-4 3 2 4-6',
  loop: 'M18 12a6 6 0 11-2-4.5 M16 4v4h-4',
  in: 'M5 12h10 M12 8l4 4-4 4 M19 6v12',
  out: 'M19 12H9 M12 8l-4 4 4 4 M5 6v12',
  wave: 'M4 12c2-6 4-6 6 0s4 6 6 0 2-4 4-2',
  serial: 'M8 8h8v6H8z M10 14v4 M14 14v4 M10 6v2 M14 6v2',
  mqtt: 'M5 12a7 7 0 017-7 M5 16a3 3 0 013-3 M19 12a7 7 0 01-7 7 M19 8a3 3 0 01-3 3',
  db: 'M6 7c0-1.7 2.7-3 6-3s6 1.3 6 3-2.7 3-6 3-6-1.3-6-3z M6 7v10c0 1.7 2.7 3 6 3s6-1.3 6-3V7 M6 12c0 1.7 2.7 3 6 3s6-1.3 6-3',
  log: 'M7 5h8l3 3v11H7z M10 12h5 M10 15h5',
  stats: 'M16 6H8l5 6-5 6h8',
  stable: 'M5 12h14 M16 9l3 3-3 3',
  parse: 'M9 6l-4 6 4 6 M15 6l4 6-4 6',
  field: 'M5 6h14v12H5z M5 12h14 M11 6v12',
  bundle: 'M8 5H6v14h2 M16 5h2v14h-2 M10 9h4 M10 15h4',
  array: 'M4 9h4v6H4z M10 9h4v6h-4z M16 9h4v6h-4z',
  wait: 'M8 5h8 M8 19h8 M8 5c0 6 8 6 8 14 M16 5c0 6-8 6-8 14',
  filter: 'M5 6h14l-5 7v5l-4-2v-3z',
  integral: 'M15 5c-3-1-4 1-4 4v6c0 3-1 5-4 4',
  derivative: 'M5 18L19 6 M15 6h4v4',
  daq: 'M5 7h14v10H5z M8 17v2 M12 17v2 M16 17v2 M8 10h8',
  hotloop: 'M12 4l7 3v5c0 4-3 7-7 8-4-1-7-4-7-8V7z M9 12l2 2 4-4',
};
