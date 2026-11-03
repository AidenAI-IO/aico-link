/**
 * Aico Link settings design tokens — the single source of truth.
 *
 * Both consumers are generated from this file by
 * `scripts/gen_config_web_tokens.mjs`:
 *
 *   - src/config_web/web/assets/css/tokens.css   (CSS custom properties)
 *   - src/config_web/design/tokens.native.js     (React Native StyleSheet values)
 *
 * Values are plain numbers/strings/arrays on purpose. The generator is the only
 * place that knows how to render them for a target, so a CSS string such as
 * `0.5px` or `cubic-bezier(...)` never leaks into the React Native side and a
 * React Native-only concept such as `StyleSheet.hairlineWidth` never leaks into
 * the web side.
 *
 * Colour provenance: canvas, card, ink, separator, success and the primary
 * button fill were sampled from the "设置交互 v2" design comps; `accent` matches
 * the Wi-Fi glyph SVG supplied with the design.
 *
 * Geometry is iOS-idiomatic rather than pixel-traced from the comps, so keep it
 * in this file and calibrate on device — every value here is a one-line change.
 */

/** Colour palette. */
export const color = {
  /** Page background behind the inset card groups. */
  canvas: '#F6F8FA',
  /** Inset card surface. */
  card: '#FFFFFF',
  /** Primary label and icon ink. */
  ink: '#111214',
  /** Secondary/value label, e.g. the "Realtime" accessory text. */
  inkSecondary: '#8A8A8E',
  /** Field captions and input placeholders. */
  inkPlaceholder: '#9A9AA0',
  /** Chevron and other decorative affordances, per the supplied chevron SVG. */
  inkTertiary: '#C7C7CC',
  /** Disclosure chevron stroke, per the supplied chevron SVG. */
  chevron: '#738294',
  /** Hairline between rows inside a card. */
  separator: '#F3F3F3',
  /** Brand blue: icon tiles, links, focused field border. */
  accent: '#4B8EFF',
  /** Connected / success text and glyphs. */
  success: '#34C759',
  /** Error text and error field border. */
  danger: '#FF3B30',
  /** Warning text, e.g. WARN lines in the Agent log: amber dark enough to read on the canvas. */
  warning: '#B86E00',
  /** Primary action pill fill. */
  primaryFill: '#000000',
  /** Primary action fill while its form is incomplete. */
  primaryFillDisabled: '#D2D5DD',
  /** Primary action label. */
  primaryInk: '#FFFFFF',
  /** Outline of a secondary button and the loading spinner's track. */
  fieldBorder: '#D6D6DA',
  /** Resting border of a text field and text area: #E6E6E6 at 45%, 1px, per the design. */
  inputBorder: 'rgba(230, 230, 230, 0.45)',
  /** Focused text field border. Only the colour changes; the width is fixed so
   *  focusing never reflows the form. */
  inputFocus: '#5478FF',
  /** Invalid text field border. */
  inputInvalid: '#FF5454',
  /** Switch track while off. */
  switchTrackOff: '#D9D9D9',
  /** Dimming layer under a sheet. */
  overlay: 'rgba(0, 0, 0, 0.4)',
  /** Sheet title, e.g. "Wi-Fi 密码": a quiet label above the form, not a heading. */
  sheetTitle: '#545B66',
  /** Outline of an option card in a choice sheet, and of its icon tile. */
  optionBorder: '#E5E7EB',
  /** Fill of the selected option card. */
  optionSelected: '#F7F8FA',
  /** Pill behind the selected option's check mark. */
  successTint: '#E9F9EE',
  /** Translucent card of an action sheet (iOS system grouped material). */
  actionSheetSurface: 'rgba(249, 249, 249, 0.94)',
  /** Hairline between an action sheet's message and its actions. */
  actionSheetSeparator: 'rgba(60, 60, 67, 0.29)',
  /** Action sheet button text: iOS system blue, per the confirmation comp. */
  actionSheetTint: '#007AFF',
  /** Wash over any tappable surface while it is pressed. */
  pressed: 'rgba(0, 0, 0, 0.05)',
  /** The navigation rail's current entry. */
  selected: 'rgba(0, 0, 0, 0.06)',
  /** Toast background: near-opaque ink so white text stays readable on any page. */
  toast: 'rgba(17, 18, 20, 0.92)',
  /** TOML editor: string values. */
  codeString: '#1E9E4A',
  /** TOML editor: numbers, booleans and dates. */
  codeLiteral: '#E0532D',
  /** TOML editor: selected text, the accent at a quarter strength. */
  codeSelection: 'rgba(75, 142, 255, 0.25)',
};

/** Spacing scale, in CSS pixels. */
export const space = {
  xs: 4,
  sm: 8,
  md: 12,
  lg: 16,
  xl: 20,
  xxl: 24,
};

/** Corner radii. */
export const radius = {
  card: 12,
  field: 10,
  /**
   * Top corners of a bottom sheet. Measured from the comps at 32px on a 598px
   * wide frame, which is ~21px at a 390px viewport.
   */
  sheet: 20,
  tile: 12,
  /** Action sheet cards. */
  actionSheet: 14,
  /** Option card in a choice sheet, per the device-type comp. */
  option: 16,
  /** Icon tile inside an option card. */
  optionIcon: 10,
  pill: 999,
};

/** Fixed control and glyph sizes. */
export const size = {
  /** Row height inside an inset card. */
  row: 56,
  /** Primary/secondary pill button height. */
  control: 50,
  /** Icon tile edge, per the Wi-Fi comp. */
  tile: 46,
  /** Default glyph box. */
  icon: 18,
  /** Chevron glyph box. */
  chevron: 13,
  /** Height of one action-sheet button. */
  actionSheetButton: 57,
  /** Inset of an action sheet from the screen edges, and the gap to Cancel. */
  actionSheetInset: 10,
  /** Provider marks and other row-leading images. */
  mark: 32,
  /** Compact row, e.g. an entry in the desktop navigation rail. */
  rowCompact: 48,
  /** Tap target of a trailing control inside a text field (the eye toggle). */
  fieldAction: 48,
  /** Option card height in a choice sheet. */
  option: 66,
  /** Icon tile edge inside an option card. */
  optionIcon: 40,
  /** Sheet grab handle width. */
  sheetHandle: 36,
  /** Sheet grab handle lane height, above the title. */
  sheetGrabber: 24,
  /** Space between a sheet's grab handle lane and its title. */
  sheetTop: 30,
  /**
   * Hairline thickness. The web side renders `0.5px`; React Native must use
   * `StyleSheet.hairlineWidth`, which is why this is expressed as a number.
   */
  hairline: 0.5,
};

/** Typography. */
export const font = {
  /** Body and row label size. */
  body: 17,
  /** Slightly smaller body, used for descriptions. */
  bodySm: 15,
  /** Section captions and helper text. */
  caption: 13,
  /**
   * Text-entry fields. iOS zooms the page when a field under 16px takes
   * focus, so nothing typed into is ever smaller than this.
   */
  input: 16,
  /** Screen-level title inside the content area. */
  title: 20,
  /**
   * Whole-pixel line heights. A fractional line box made row heights
   * fractional, which let 0.5px separators land between device pixels and
   * vanish; these keep every row a whole number of pixels tall.
   */
  lineBody: 22,
  lineCaption: 18,
  lineCode: 22,
  /** Sheet title. */
  sheetTitle: 14,
  /** Action sheet title and message. */
  actionSheetText: 13,
  /** Action sheet button label. */
  actionSheetButton: 20,
  weightRegular: 400,
  weightMedium: 500,
  weightSemibold: 600,
  weightBold: 700,
  /**
   * iOS resolves `-apple-system` to SF, which is exactly what React Native's
   * `System` font resolves to, so the two renderings share metrics.
   */
  family:
    "-apple-system, BlinkMacSystemFont, 'SF Pro Text', 'Helvetica Neue', 'PingFang SC', 'Hiragino Sans GB', 'Microsoft YaHei', sans-serif",
  /** The TOML editor's monospace stack. */
  familyMono: "ui-monospace, 'SF Mono', Menlo, Consolas, monospace",
};

/** Motion. Durations are milliseconds; the sheet curve is the iOS sheet curve. */
export const motion = {
  /** Sheet present/dismiss duration, ms. */
  sheet: 320,
  /** Small state changes such as a toggle or pressed colour, ms. */
  fast: 180,
  /** Sheet curve as cubic-bezier control points, shared by CSS and RN easing. */
  sheetBezier: [0.32, 0.72, 0, 1],
};

/** Responsive layout. */
export const layout = {
  /** Horizontal inset of a card group from the viewport edge. */
  pageMargin: 20,
  /** Fixed left navigation width once the desktop layout engages. */
  navWidth: 350,
  /** Viewport width at which the left-nav + panel layout replaces the stack. */
  breakpointDesktop: 900,
  /** Maximum content width in the desktop layout. */
  maxContent: 720,
};

/**
 * Elevation. Structured rather than a CSS string, because React Native takes the
 * offset, radius and colour separately; the generator renders the CSS form.
 */
export const shadow = {
  /**
   * Upward shadow a bottom sheet casts onto the page behind it. The comps dim
   * nothing else, so this shadow is what separates the sheet from the page.
   *
   * The offset/blur/alpha are fitted to the comps' measured falloff above the
   * panel edge (alpha 0.13 at 5px, 0.098 at 10px, 0.057 at 20px, 0.032 at 30px),
   * not chosen by eye.
   */
  sheet: {offsetX: 0, offsetY: -8, blur: 36, color: 'rgba(0, 0, 0, 0.22)'},
  /** The switch knob, lifted off its track. */
  knob: {offsetX: 0, offsetY: 3, blur: 8, color: 'rgba(0, 0, 0, 0.15)'},
  /** A floating list opened below its trigger, e.g. the time zone dropdown. */
  dropdown: {offsetX: 0, offsetY: 8, blur: 24, color: 'rgba(0, 0, 0, 0.12)'},
  /** A raised surface on a tinted track, e.g. the segmented control's thumb. */
  raised: {offsetX: 0, offsetY: 1, blur: 3, color: 'rgba(0, 0, 0, 0.08)'},
};

/** Everything, for generators and tests that want one object. */
export const tokens = {color, space, radius, size, font, motion, shadow, layout};

export default tokens;
