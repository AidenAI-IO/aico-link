/**
 * Icon set.
 *
 * Every glyph is described once, as geometry. The companion app ports the same
 * path data into `react-native-svg`, which is what keeps the two renderings
 * identical rather than merely similar.
 *
 * Geometry provenance:
 *   - `tile` and the Wi-Fi glyph are the SVGs supplied with the design, used
 *     verbatim. The tile and the glyph live in one 46x46 `viewBox` so the glyph
 *     can be positioned exactly as drawn instead of being centred by eye.
 *   - `chevron` is the supplied disclosure SVG (6x11, `#738294`).
 *   - `checkCircle` is the connected indicator: a filled disc with a knocked-out
 *     check, which is a different mark from the bare `check` stroke.
 *   - `monitor` (macOS) and `terminal` (Linux) are the device-type SVGs supplied
 *     with the design (18x18, 2px round stroke).
 *   - `gearBadge` and `alertBadge` put a white glyph on the supplied 17px blue
 *     and 18px red discs. The gear and the search glass are Tabler Icons 3.48
 *     outlines; the exclamation mark is drawn on the disc's own centre line.
 *   - `apple`, `android` and `windows` are Tabler Icons 3.48 outline brand marks
 *     (MIT, https://tabler.io/icons), chosen because they share the supplied
 *     glyphs' 2px round-capped stroke; the design's own two were placeholders.
 */

import {svg} from './dom.js';

/** Wi-Fi radio arcs, from the supplied glyph (viewBox 0 0 18 14). */
const WIFI_ARC_OUTER = 'M1.09961 4.14089C5.48114 0.0863368 12.4131 0.0863368 16.7947 4.14089';
const WIFI_ARC_INNER = 'M4.36941 8.06465C6.98525 5.7104 10.909 5.7104 13.5249 8.06465';
const WIFI_ARC = `${WIFI_ARC_OUTER}${WIFI_ARC_INNER}`;
/** Wi-Fi centre dot, from the supplied glyph. */
const WIFI_DOT =
  'M8.94669 13.0999C9.56069 13.0999 10.0584 12.6022 10.0584 11.9882C10.0584 11.3742 9.56069 10.8765 8.94669 10.8765C8.3327 10.8765 7.83496 11.3742 7.83496 11.9882C7.83496 12.6022 8.3327 13.0999 8.94669 13.0999Z';
/** Rounded-square tile plate, from the supplied design SVG (viewBox 0 0 46 46). */
const TILE_PLATE =
  'M31.625 0H14.375C6.43591 0 0 6.43591 0 14.375V31.625C0 39.5641 6.43591 46 14.375 46H31.625C39.5641 46 46 39.5641 46 31.625V14.375C46 6.43591 39.5641 0 31.625 0Z';

/** The Wi-Fi page tile's glyph, supplied as a filled 27x18 mark (viewBox 0 0 27 18). */
const WIFI_TILE_GLYPH =
  'M13.5 5.61225C16.9845 5.61225 20.3265 6.91725 22.7903 9.24075L21.0457 10.8855C16.878 6.95625 10.122 6.95625 5.95425 10.8855L4.209 9.24C6.67275 6.9165 10.0148 5.6115 13.5 5.6115V5.61225ZM27 5.27175L25.2548 6.91725C22.1378 3.97725 17.9085 2.32575 13.5 2.32575C9.09 2.32575 4.86225 3.97725 1.7445 6.91725L0 5.27175C7.45575 -1.75725 19.5443 -1.75725 27 5.27175ZM18.4275 13.3545C15.7058 10.7888 11.2943 10.7888 8.5725 13.3545L13.5 18L18.4275 13.3545Z';
/** Centres the 27x18 glyph on the 46x46 plate. */
const TILE_GLYPH_OFFSET = 'translate(9.5 14)';

/** Globe mark used by Language & Time Zone. The plate follows the supplied 46px SVG. */
const GLOBE_SHAPES = [
  {tag: 'circle', attrs: {cx: 23, cy: 23, r: 14.5, class: 'ds-tile__globe-stroke'}},
  {tag: 'path', attrs: {d: 'M8.5 23H37.5', class: 'ds-tile__globe-stroke'}},
  {tag: 'path', attrs: {d: 'M23 8.5C18.4 12.4 16 17.2 16 23C16 28.8 18.4 33.6 23 37.5C27.6 33.6 30 28.8 30 23C30 17.2 27.6 12.4 23 8.5Z', class: 'ds-tile__globe-stroke'}},
  {tag: 'path', attrs: {d: 'M11.5 15.5C14.8 18 18.7 19.2 23 19.2C27.3 19.2 31.2 18 34.5 15.5', class: 'ds-tile__globe-stroke'}},
  {tag: 'path', attrs: {d: 'M11.5 30.5C14.8 28 18.7 26.8 23 26.8C27.3 26.8 31.2 28 34.5 30.5', class: 'ds-tile__globe-stroke'}},
];

/** A 2px round-capped outline path, the stroke style every device glyph shares. */
const outline = d => ({
  tag: 'path',
  attrs: {d, stroke: 'currentColor', 'stroke-width': 2, 'stroke-linecap': 'round', 'stroke-linejoin': 'round'},
});

/** Glyph geometry, keyed by name. `box` is the viewBox. */
export const glyphs = {
  wifi: {
    box: '0 0 18 14',
    shapes: [
      {
        class: 'ds-wifi__arc-outer',
        tag: 'path',
        attrs: {
          d: WIFI_ARC_OUTER,
          stroke: 'currentColor',
          'stroke-width': 2.2,
          'stroke-linecap': 'round',
        },
      },
      {
        class: 'ds-wifi__arc-inner',
        tag: 'path',
        attrs: {
          d: WIFI_ARC_INNER,
          stroke: 'currentColor',
          'stroke-width': 2.2,
          'stroke-linecap': 'round',
        },
      },
      {tag: 'path', attrs: {d: WIFI_DOT, fill: 'currentColor'}},
    ],
  },

  // Supplied disclosure chevron; the stroke colour comes from the token so the
  // rail and the list stay in step.
  chevron: {
    box: '0 0 6 11',
    shapes: [
      {
        tag: 'path',
        attrs: {
          d: 'M0.5 0.5L5.5 5.5L0.5 10.5',
          stroke: 'currentColor',
          'stroke-width': 1,
          'stroke-linecap': 'round',
          'stroke-linejoin': 'round',
        },
      },
    ],
  },

  // Bare check stroke, for inline confirmation.
  check: {
    box: '0 0 24 24',
    shapes: [
      {
        tag: 'path',
        attrs: {
          d: 'M20 6.5 9.5 17 4 11.5',
          stroke: 'currentColor',
          'stroke-width': 2.6,
          'stroke-linecap': 'round',
          'stroke-linejoin': 'round',
        },
      },
    ],
  },

  // Details button on a saved network row, as iOS draws it: an outlined disc
  // with an "i".
  info: {
    box: '0 0 24 24',
    shapes: [
      {tag: 'circle', attrs: {cx: 12, cy: 12, r: 10, fill: 'none', stroke: 'currentColor', 'stroke-width': 1.6}},
      {tag: 'circle', attrs: {cx: 12, cy: 7.6, r: 1.25, fill: 'currentColor'}},
      {tag: 'path', attrs: {d: 'M12 11v6.2', stroke: 'currentColor', 'stroke-width': 2, 'stroke-linecap': 'round'}},
    ],
  },

  // Connected indicator: solid disc with a white check, as the design draws it.
  checkCircle: {
    box: '0 0 24 24',
    shapes: [
      {tag: 'circle', attrs: {cx: 12, cy: 12, r: 11, fill: 'currentColor'}},
      {
        tag: 'path',
        class: 'ds-glyph-on-accent',
        attrs: {
          d: 'M7.2 12.4 10.6 15.8 16.8 9.2',
          'stroke-width': 2.4,
          'stroke-linecap': 'round',
          'stroke-linejoin': 'round',
        },
      },
    ],
  },

  // Device-type glyphs; the basic settings page maps each `device_type` to one.
  apple: {
    box: '0 0 24 24',
    shapes: [
      outline('M8.286 7.008c-3.216 0 -4.286 3.23 -4.286 5.92c0 3.229 2.143 8.072 4.286 8.072c1.165 -.05 1.799 -.538 3.214 -.538c1.406 0 1.607 .538 3.214 .538s4.286 -3.229 4.286 -5.381c-.03 -.011 -2.649 -.434 -2.679 -3.23c-.02 -2.335 2.589 -3.179 2.679 -3.228c-1.096 -1.606 -3.162 -2.113 -3.75 -2.153c-1.535 -.12 -3.032 1.077 -3.75 1.077c-.729 0 -2.036 -1.077 -3.214 -1.077'),
      outline('M12 4a2 2 0 0 0 2 -2a2 2 0 0 0 -2 2'),
    ],
  },
  android: {
    box: '0 0 24 24',
    shapes: [
      outline('M4 10l0 6'),
      outline('M20 10l0 6'),
      outline('M7 9h10v8a1 1 0 0 1 -1 1h-8a1 1 0 0 1 -1 -1v-8a5 5 0 0 1 10 0'),
      outline('M8 3l1 2'),
      outline('M16 3l-1 2'),
      outline('M9 18l0 3'),
      outline('M15 18l0 3'),
    ],
  },
  windows: {
    box: '0 0 24 24',
    shapes: [
      outline('M17.8 20l-12 -1.5c-1 -.1 -1.8 -.9 -1.8 -1.9v-9.2c0 -1 .8 -1.8 1.8 -1.9l12 -1.5c1.2 -.1 2.2 .8 2.2 1.9v12.1c0 1.2 -1.1 2.1 -2.2 1.9l0 .1'),
      outline('M12 5l0 14'),
      outline('M4 12l16 0'),
    ],
  },
  monitor: {
    box: '0 0 18 18',
    shapes: [
      outline('M6.00036 15.75H12.0008M9.0006 12.75V15.75M3.00012 2.25H15.0011C15.8296 2.25 16.5012 2.92157 16.5012 3.75V11.25C16.5012 12.0784 15.8296 12.75 15.0011 12.75H3.00012C2.17163 12.75 1.5 12.0784 1.5 11.25V3.75C1.5 2.92157 2.17163 2.25 3.00012 2.25Z'),
    ],
  },
  terminal: {
    box: '0 0 18 18',
    shapes: [outline('M8.9994 14.2506H14.9988M3 12.7504L7.49955 8.2499L3 3.74939')],
  },

  // "Tool settings" row: the supplied 17px blue disc with a white gear.
  gearBadge: {
    box: '0 0 17 17',
    shapes: [
      {tag: 'circle', class: 'ds-badge__disc--accent', attrs: {cx: 8.5, cy: 8.5, r: 8.5}},
      {
        tag: 'g',
        class: 'ds-badge__glyph',
        // Tabler's 24px gear, scaled to 11px and centred on the disc.
        attrs: {transform: 'translate(3 3) scale(0.4583)', 'stroke-width': 2.4, 'stroke-linecap': 'round', 'stroke-linejoin': 'round', fill: 'none'},
        children: [
          'M10.325 4.317c.426 -1.756 2.924 -1.756 3.35 0a1.724 1.724 0 0 0 2.573 1.066c1.543 -.94 3.31 .826 2.37 2.37a1.724 1.724 0 0 0 1.065 2.572c1.756 .426 1.756 2.924 0 3.35a1.724 1.724 0 0 0 -1.066 2.573c.94 1.543 -.826 3.31 -2.37 2.37a1.724 1.724 0 0 0 -2.572 1.065c-.426 1.756 -2.924 1.756 -3.35 0a1.724 1.724 0 0 0 -2.573 -1.066c-1.543 .94 -3.31 -.826 -2.37 -2.37a1.724 1.724 0 0 0 -1.065 -2.572c-1.756 -.426 -1.756 -2.924 0 -3.35a1.724 1.724 0 0 0 1.066 -2.573c-.94 -1.543 .826 -3.31 2.37 -2.37c1 .608 2.296 .07 2.572 -1.065',
          'M9 12a3 3 0 1 0 6 0a3 3 0 0 0 -6 0',
        ],
      },
    ],
  },

  // "Reset conversation" row: the supplied 18px red disc with a white "!".
  // The bar and the dot share x = 9, the disc's centre, so the mark cannot lean;
  // together they span y 4.5-13.5, centred on the disc as well.
  alertBadge: {
    box: '0 0 18 18',
    shapes: [
      {tag: 'path', class: 'ds-badge__disc--danger', attrs: {d: 'M9 18C13.9706 18 18 13.9706 18 9C18 4.02944 13.9706 0 9 0C4.02944 0 0 4.02944 0 9C0 13.9706 4.02944 18 9 18Z'}},
      {tag: 'path', class: 'ds-badge__glyph', attrs: {d: 'M9 5.2V10.2', 'stroke-width': 2, 'stroke-linecap': 'round'}},
      {tag: 'circle', class: 'ds-badge__glyph-fill', attrs: {cx: 9, cy: 12.8, r: 1.15}},
    ],
  },

  // Reveal a secret field's value. Tabler Icons 3.48 `eye`.
  eye: {
    box: '0 0 24 24',
    shapes: [
      {tag: 'path', attrs: {d: 'M10 12a2 2 0 1 0 4 0a2 2 0 0 0 -4 0', stroke: 'currentColor', 'stroke-width': 2, 'stroke-linecap': 'round', 'stroke-linejoin': 'round'}},
      {tag: 'path', attrs: {d: 'M21 12c-2.4 4 -5.4 6 -9 6c-3.6 0 -6.6 -2 -9 -6c2.4 -4 5.4 -6 9 -6c3.6 0 6.6 2 9 6', stroke: 'currentColor', 'stroke-width': 2, 'stroke-linecap': 'round', 'stroke-linejoin': 'round'}},
    ],
  },

  // Hide a revealed secret. Tabler Icons 3.48 `eye-off`.
  eyeOff: {
    box: '0 0 24 24',
    shapes: [
      {tag: 'path', attrs: {d: 'M10.585 10.587a2 2 0 0 0 2.829 2.828', stroke: 'currentColor', 'stroke-width': 2, 'stroke-linecap': 'round', 'stroke-linejoin': 'round'}},
      {tag: 'path', attrs: {d: 'M16.681 16.673a8.717 8.717 0 0 1 -4.681 1.327c-3.6 0 -6.6 -2 -9 -6c1.272 -2.12 2.712 -3.678 4.32 -4.674m2.86 -1.146a9.055 9.055 0 0 1 1.82 -.18c3.6 0 6.6 2 9 6c-.666 1.11 -1.379 2.067 -2.138 2.87', stroke: 'currentColor', 'stroke-width': 2, 'stroke-linecap': 'round', 'stroke-linejoin': 'round'}},
      {tag: 'path', attrs: {d: 'M3 3l18 18', stroke: 'currentColor', 'stroke-width': 2, 'stroke-linecap': 'round', 'stroke-linejoin': 'round'}},
    ],
  },

  // Firmware update tile. Tabler Icons 3.48 `cpu`.
  chip: {
    box: '0 0 24 24',
    shapes: [
      outline('M5 6a1 1 0 0 1 1 -1h12a1 1 0 0 1 1 1v12a1 1 0 0 1 -1 1h-12a1 1 0 0 1 -1 -1l0 -12'),
      outline('M9 9h6v6h-6l0 -6'),
      outline('M3 10h2'),
      outline('M3 14h2'),
      outline('M10 3v2'),
      outline('M14 3v2'),
      outline('M21 10h-2'),
      outline('M21 14h-2'),
      outline('M14 21v-2'),
      outline('M10 21v-2'),
    ],
  },

  // Copy-to-clipboard: two overlapping rounded sheets.
  copy: {
    box: '0 0 24 24',
    shapes: [
      {tag: 'rect', attrs: {x: 9, y: 9, width: 11, height: 11, rx: 2.5, fill: 'none', stroke: 'currentColor', 'stroke-width': 1.8}},
      {
        tag: 'path',
        attrs: {
          d: 'M15 5.5V5.5a1.5 1.5 0 0 0-1.5-1.5h-8A1.5 1.5 0 0 0 4 5.5v8A1.5 1.5 0 0 0 5.5 15',
          fill: 'none',
          stroke: 'currentColor',
          'stroke-width': 1.8,
          'stroke-linecap': 'round',
        },
      },
    ],
  },

  // Secured-network indicator.
  lock: {
    box: '0 0 24 24',
    shapes: [
      {
        tag: 'path',
        attrs: {
          d: 'M8 10V7.5a4 4 0 0 1 8 0V10',
          stroke: 'currentColor',
          'stroke-width': 2,
          'stroke-linecap': 'round',
        },
      },
      {tag: 'rect', attrs: {x: 5, y: 10, width: 14, height: 10, rx: 2.5, fill: 'currentColor'}},
    ],
  },
};

/**
 * Build an icon element.
 *
 * @param {keyof typeof glyphs} name
 * @param {object} [options]
 * @param {string} [options.class]
 * @param {number} [options.size] - rendered width; height follows the viewBox.
 * @param {number} [options.signalLevel] - Wi-Fi level from 0 (dot only) to 3.
 */
export function icon(name, options = {}) {
  const glyph = glyphs[name];
  if (!glyph) throw new Error(`unknown icon: ${name}`);
  const [, , boxWidth, boxHeight] = glyph.box.split(/\s+/).map(Number);
  const width = options.size ?? boxWidth;
  const height = Math.round((width * boxHeight) / boxWidth);

  let shapes = glyph.shapes;
  if (name === 'wifi' && Number.isInteger(options.signalLevel)) {
    const level = Math.max(0, Math.min(3, options.signalLevel));
    shapes = glyph.shapes.map(shape => {
      if (level === 3 || !shape.class) return shape;
      const inactiveOuter = level < 3 && shape.class.includes('outer');
      const inactiveInner = level < 1 && shape.class.includes('inner');
      return inactiveOuter || inactiveInner
        ? {...shape, attrs: {...shape.attrs, stroke: 'var(--color-ink-tertiary)'}}
        : shape;
    });
  }
  return svg({
    viewBox: glyph.box,
    shapes,
    class: options.class,
    width,
    height,
  });
}

/**
 * The 46x46 blue tile from the design, with the Wi-Fi glyph inside it.
 *
 * The plate and the filled 27x18 glyph are both supplied SVGs; CSS paints the
 * plate with the accent token and the glyph with the on-accent token, so the
 * mark follows the palette rather than hard-coded fills. The smaller stroked
 * Wi-Fi glyph in the network rows is a different mark and stays as it is.
 *
 * @param {{size?: number, class?: string}} [options]
 */
export function wifiTile(options = {}) {
  const size = options.size ?? 46;
  return svg({
    viewBox: '0 0 46 46',
    class: options.class || 'ds-tile',
    width: size,
    height: size,
    shapes: [
      {tag: 'path', attrs: {d: TILE_PLATE, fill: 'currentColor'}},
      {tag: 'path', class: 'ds-tile__glyph-fill', attrs: {d: WIFI_TILE_GLYPH, transform: TILE_GLYPH_OFFSET}},
    ],
  });
}

/** The 46x46 blue tile for Web search: the supplied plate with Tabler's search glass. */
export function searchTile(options = {}) {
  const size = options.size ?? 46;
  return svg({
    viewBox: '0 0 46 46',
    class: options.class || 'ds-tile',
    width: size,
    height: size,
    shapes: [
      {tag: 'path', attrs: {d: TILE_PLATE, fill: 'currentColor'}},
      {
        tag: 'g',
        class: 'ds-tile__glyph-stroke',
        // The 24px glyph drawn at 22px, centred on the plate.
        attrs: {transform: 'translate(12 12) scale(0.9167)', 'stroke-width': 2.2, 'stroke-linecap': 'round', 'stroke-linejoin': 'round', fill: 'none'},
        children: ['M3 10a7 7 0 1 0 14 0a7 7 0 1 0 -14 0', 'M21 21l-6 -6'],
      },
    ],
  });
}

/** The 46x46 blue tile supplied for Language & Time Zone. */
export function languageTile(options = {}) {
  const size = options.size ?? 46;
  return svg({
    viewBox: '0 0 46 46',
    class: options.class || 'ds-tile',
    width: size,
    height: size,
    shapes: [
      {tag: 'path', attrs: {d: TILE_PLATE, fill: 'currentColor'}},
      ...GLOBE_SHAPES.map(shape => ({
        ...shape,
        attrs: {...shape.attrs, stroke: 'currentColor', 'stroke-width': 1.5, 'stroke-linecap': 'round', 'stroke-linejoin': 'round'},
        class: 'ds-tile__globe-stroke',
      })),
    ],
  });
}
