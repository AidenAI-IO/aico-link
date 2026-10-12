/**
 * Unit tests for the shared UI components.
 *
 * These components are the reuse surface of the settings UI: the same row, the
 * same switch and the same sheet appear on several routes, so a defect in one of
 * them is a defect everywhere. A missing repaint in `switchControl` shipped once
 * exactly because nothing exercised the component on its own, so each component
 * is now driven directly here with a minimal DOM stub.
 *
 * The stub implements only what `assets/js/ui/*` touches, which keeps the test
 * dependency-free and also documents that surface: if a component starts using
 * another browser API, this file is where the gap shows up.
 */

import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import path from 'node:path';
import {fileURLToPath, pathToFileURL} from 'node:url';

const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const uiRoot = path.join(repositoryRoot, 'src/config_web/web/assets/js/ui');

/* ------------------------------------------------------------- DOM stub --- */

class ClassList {
  constructor(node) {
    this.node = node;
  }

  add(...names) {
    names.forEach(name => name && this.node._classes.add(name));
  }

  remove(...names) {
    names.forEach(name => this.node._classes.delete(name));
  }

  contains(name) {
    return this.node._classes.has(name);
  }

  toggle(name, force) {
    const on = force === undefined ? !this.contains(name) : Boolean(force);
    if (on) this.add(name);
    else this.remove(name);
    return on;
  }
}

class Element {
  constructor(tag, namespace) {
    this.tagName = tag;
    this.namespaceURI = namespace || null;
    this.childNodes = [];
    this.parentNode = null;
    this._classes = new Set();
    this._attributes = new Map();
    this._listeners = new Map();
    this._text = '';
    this.classList = new ClassList(this);
    this.style = {};
    this.hidden = false;
    this.disabled = false;
    this.offsetHeight = 0;
  }

  get firstChild() {
    return this.childNodes[0] || null;
  }

  get children() {
    return this.childNodes.filter(child => child instanceof Element);
  }

  get className() {
    return [...this._classes].join(' ');
  }

  set className(value) {
    this._classes = new Set(String(value || '').split(/\s+/).filter(Boolean));
  }

  get textContent() {
    if (this.childNodes.length) return this.childNodes.map(child => child.textContent).join('');
    return this._text;
  }

  set textContent(value) {
    this.childNodes = [];
    this._text = String(value);
  }

  appendChild(child) {
    child.parentNode = this;
    this.childNodes.push(child);
    return child;
  }

  removeChild(child) {
    const index = this.childNodes.indexOf(child);
    if (index >= 0) this.childNodes.splice(index, 1);
    child.parentNode = null;
    return child;
  }

  setAttribute(name, value) {
    this._attributes.set(name, String(value));
    // A real element reflects a class attribute into classList; SVG builders set
    // their class this way, so the stub has to do the same.
    if (name === 'class') this._classes = new Set(String(value).split(/\s+/).filter(Boolean));
  }

  getAttribute(name) {
    return this._attributes.has(name) ? this._attributes.get(name) : null;
  }

  removeAttribute(name) {
    this._attributes.delete(name);
  }

  addEventListener(type, handler) {
    const handlers = this._listeners.get(type) || [];
    handlers.push(handler);
    this._listeners.set(type, handlers);
  }

  /** Test helper: run the listeners registered for a type. */
  fire(type, event = {}) {
    const payload = {type, target: this, preventDefault() {}, stopPropagation() {}, ...event};
    for (const handler of this._listeners.get(type) || []) handler(payload);
  }

  querySelector(selector) {
    const walk = node => {
      for (const child of node.children) {
        if (matches(child, selector)) return child;
        const found = walk(child);
        if (found) return found;
      }
      return null;
    };
    return walk(this);
  }

  closest(selector) {
    let node = this;
    while (node) {
      if (matches(node, selector)) return node;
      node = node.parentNode;
    }
    return null;
  }
}

/** Match the simple selectors the components actually use: `.class`, `tag`, and comma lists. */
function matches(node, selector) {
  return String(selector)
    .split(',')
    .map(part => part.trim())
    .filter(Boolean)
    .some(part =>
      part.startsWith('.') ? node._classes.has(part.slice(1)) : node.tagName === part.toLowerCase(),
    );
}

const body = new Element('body');
globalThis.document = {
  body,
  documentElement: new Element('html'),
  createElement: tag => new Element(tag),
  createElementNS: (namespace, tag) => new Element(tag, namespace),
  createTextNode: value => {
    const node = new Element('#text');
    node.textContent = value;
    return node;
  },
  // Counted, so a component that leaves a document listener behind shows up.
  listeners: new Map(),
  addEventListener(type, handler) {
    if (!this.listeners.has(type)) this.listeners.set(type, new Set());
    this.listeners.get(type).add(handler);
  },
  removeEventListener(type, handler) {
    if (this.listeners.has(type)) this.listeners.get(type).delete(handler);
  },
};
const documentListeners = type => (document.listeners.get(type) || new Set()).size;

const load = name => import(pathToFileURL(path.join(uiRoot, `${name}.js`)).href);
const {switchControl, setChecked} = await load('switch');
const {sheet} = await load('sheet');
const {group, row, separator} = await load('list');
const {text} = await load('text');

const countByClass = (node, className) => {
  let total = node._classes.has(className) ? 1 : 0;
  for (const child of node.children) total += countByClass(child, className);
  return total;
};

/* ------------------------------------------------------------ switch --- */

{
  const changes = [];
  const control = switchControl({
    checked: false,
    label: 'proxy',
    onChange: next => changes.push(next),
  });

  assert.equal(control.getAttribute('role'), 'switch');
  assert.equal(control.getAttribute('aria-checked'), 'false');
  assert.equal(control.classList.contains('ds-switch--on'), false, 'starts off');

  control.fire('click');
  assert.deepEqual(changes, [true], 'the press is reported once');
  assert.equal(control.classList.contains('ds-switch--on'), true, 'the track paints itself on');
  assert.equal(
    control.querySelector('.ds-switch__knob').classList.contains('ds-switch__knob--on'),
    true,
    'the knob paints itself on',
  );
  assert.equal(control.getAttribute('aria-checked'), 'true');

  control.fire('click');
  assert.deepEqual(changes, [true, false]);
  assert.equal(control.classList.contains('ds-switch--on'), false, 'the track paints itself off');

  // A caller that rejects the change must be able to revert, and the reverted
  // state has to be the internal one: the next press must go on, not off.
  control.fire('click');
  setChecked(control, false);
  assert.equal(control.classList.contains('ds-switch--on'), false, 'reverted');
  assert.equal(
    control.querySelector('.ds-switch__knob').classList.contains('ds-switch__knob--on'),
    false,
    'the knob reverts with the track',
  );
  control.fire('click');
  assert.deepEqual(changes, [true, false, true, true], 'a reverted switch still toggles correctly');
}

{
  const control = switchControl({checked: true});
  assert.equal(control.classList.contains('ds-switch--on'), true);
  setChecked(control, true);
  assert.equal(control.getAttribute('aria-checked'), 'true');
}

/* -------------------------------------------------------------- sheet --- */

{
  const keydownBefore = documentListeners('keydown');
  const view = sheet({title: 'Wi-Fi 密码', body: [new Element('div')]});
  assert.equal(view.isOpen(), false);
  assert.equal(documentListeners('keydown'), keydownBefore, 'a closed sheet listens for nothing');

  view.open();
  assert.equal(view.isOpen(), true);
  assert.equal(view.el.classList.contains('ds-sheet'), true, 'the sheet markup is intact');
  assert.equal(countByClass(view.el, 'ds-sheet__panel--open'), 1, 'the panel opens');
  assert.equal(countByClass(view.el, 'ds-sheet__scrim--open'), 1, 'the scrim fades in');
  assert.equal(document.body.children.length, 1, 'the sheet is attached while open');

  view.close();
  assert.equal(view.isOpen(), false);
  assert.equal(countByClass(view.el, 'ds-sheet__panel--open'), 0, 'the panel closes');
  assert.equal(countByClass(view.el, 'ds-sheet__scrim--open'), 0, 'the scrim fades out');
  assert.equal(documentListeners('keydown'), keydownBefore, 'closing releases the Escape listener');
}

{
  // Touch drags must reach the sheet: once the finger clearly moves, the panel
  // captures the pointer, follows the finger, and a long pull dismisses it.
  const view = sheet({title: 'Wi-Fi 密码', body: [new Element('div')]});
  view.open();
  const panel = view.el.children.find(child => child.classList.contains('ds-sheet__panel'));
  const captured = [];
  panel.setPointerCapture = id => captured.push(id);
  const grab = {closest: () => null};
  panel.fire('pointerdown', {pointerId: 7, clientY: 100, target: grab});
  assert.deepEqual(captured, [], 'a press alone captures nothing, so a tap still clicks');
  panel.fire('pointermove', {pointerId: 7, clientY: 300, target: grab});
  assert.deepEqual(captured, [7], 'the sheet keeps the drag when the finger leaves the panel');
  assert.equal(panel.style.transform, 'translateY(200px)', 'the panel follows the finger');
  panel.fire('pointerup', {pointerId: 7, clientY: 300, target: grab});
  assert.equal(view.isOpen(), false, 'a long pull dismisses the sheet');
}

{
  // A choice row is a div[role=button]: pressing it must not start a drag or
  // capture the pointer, or its click would land on the panel instead.
  const view = sheet({title: 'Choose', body: [new Element('div')]});
  view.open();
  const panel = view.el.children.find(child => child.classList.contains('ds-sheet__panel'));
  const captured = [];
  panel.setPointerCapture = id => captured.push(id);
  let asked = '';
  const choiceRow = {closest: selector => ((asked = selector), selector.includes('[role="button"]') ? {} : null)};
  panel.fire('pointerdown', {pointerId: 8, clientY: 100, target: choiceRow});
  panel.fire('pointermove', {pointerId: 8, clientY: 300, target: choiceRow});
  assert.match(asked, /\[role="button"\]/, 'tappable rows are excluded from dragging');
  assert.deepEqual(captured, [], 'a row press never captures the pointer');
  assert.equal(panel.style.transform || '', '', 'a row press does not move the panel');
  assert.equal(view.isOpen(), true);
  view.close();
}

{
  // Without this, WebKit claims a finger dragging the sheet's handle as a page
  // pan and cancels the pointer stream, so the sheet cannot be pulled down.
  const css = readFileSync(new URL('../src/config_web/web/assets/css/ds.css', import.meta.url), 'utf8');
  assert.match(
    css,
    /\.ds-sheet__grabber,\s*\.ds-sheet__title\s*\{\s*touch-action:\s*none;/,
    'the sheet handle and title take touch drags themselves',
  );
}

/* --------------------------------------------------------------- list --- */

{
  const card = group({rows: [row({label: 'a'}), row({label: 'b'}), row({label: 'c'})]});
  assert.equal(countByClass(card, 'ds-separator'), 2, 'separators go between rows only');

  const single = group({rows: [row({label: 'only'})]});
  assert.equal(countByClass(single, 'ds-separator'), 0, 'a lone row has no separator');

  const withCaption = group({caption: 'Other networks', rows: [row({label: 'a'})]});
  assert.equal(countByClass(withCaption, 'ds-group__caption'), 1, 'a caption wraps the card');
  assert.equal(countByClass(withCaption, 'ds-group'), 1);
}

{
  const presses = [];
  const tappable = row({label: 'Wi-Fi', chevron: true, onPress: () => presses.push('press')});
  assert.equal(tappable.getAttribute('role'), 'button', 'a tappable row is reachable');
  assert.equal(tappable.classList.contains('ds-row--tappable'), true);
  tappable.fire('click');
  tappable.fire('keydown', {key: 'Enter'});
  assert.deepEqual(presses, ['press', 'press'], 'pointer and keyboard both activate');
  // Enter on a control inside the row (the Wi-Fi ⓘ) bubbles to the row; it
  // must reach that control only, not run the row's action.
  let prevented = false;
  tappable.fire('keydown', {key: 'Enter', target: new Element('button'), preventDefault() { prevented = true; }});
  assert.deepEqual(presses, ['press', 'press'], 'a nested control\'s Enter does not activate the row');
  assert.equal(prevented, false, 'the nested control keeps its own Enter activation');

  const plain = row({label: 'Memory'});
  assert.equal(plain.getAttribute('role'), null, 'a static row is not announced as a button');
  assert.equal(countByClass(plain, 'ds-row__chevron'), 0);
}

{
  const chevronRow = row({label: 'Wi-Fi', chevron: true});
  assert.equal(countByClass(chevronRow, 'ds-row__chevron'), 1);
  assert.equal(separator().classList.contains('ds-separator'), true);
}

/* --------------------------------------------------------------- text --- */

{
  const translated = text({key: 'wifi.connected', fallback: '已连接'}, 'ds-row__value');
  assert.equal(translated.getAttribute('data-i18n'), 'wifi.connected', 're-translated on locale change');
  assert.equal(translated.getAttribute('data-i18n-default'), '已连接');
  assert.equal(translated.classList.contains('ds-row__value'), true);

  const literal = text('Wi-Fi');
  assert.equal(literal.textContent, 'Wi-Fi');
  assert.equal(literal.getAttribute('data-i18n'), null, 'a literal needs no catalogue key');
}

/* --------------------------------------------------------- environment --- */

{
  const {isEmbedded} = await load('environment');

  // The same document is served to a browser and to the app's WebView; the rail
  // shows the wordmark only in the first case, so the signals must be reliable.
  assert.equal(
    isEmbedded({ReactNativeWebView: {}, location: {search: ''}, navigator: {userAgent: 'Mozilla/5.0'}}),
    true,
    'the injected bridge object marks the document as embedded',
  );

  // react-native-webview only injects that bridge object when the host passes an
  // `onMessage` handler, so the iOS shim handler — registered unconditionally —
  // is what identifies the settings WebView in the companion app.
  assert.equal(
    isEmbedded({
      location: {search: ''},
      navigator: {userAgent: 'Mozilla/5.0 (iPhone; CPU iPhone OS 26_0 like Mac OS X)'},
      webkit: {messageHandlers: {ReactNativeHistoryShim: {}}},
    }),
    true,
    'the always-registered iOS history shim marks the document as embedded',
  );
  assert.equal(
    isEmbedded({
      location: {search: ''},
      navigator: {userAgent: 'Mozilla/5.0'},
      webkit: {messageHandlers: {ReactNativeWebView: {}}},
    }),
    true,
    'the iOS bridge message handler marks the document as embedded',
  );

  assert.equal(
    isEmbedded({location: {search: '?webview=true'}, navigator: {userAgent: 'Mozilla/5.0'}}),
    true,
    'an explicit opt-in parameter marks the document as embedded',
  );
  assert.equal(
    isEmbedded({location: {search: ''}, navigator: {userAgent: 'Mozilla/5.0 (iPhone) AidenApp/1.0'}}),
    true,
    'the app user-agent token marks the document as embedded',
  );

  // Android marks its WebView in the user agent; a desktop browser does not.
  assert.equal(
    isEmbedded({
      location: {search: ''},
      navigator: {userAgent: 'Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Version/4.0 Chrome/120 Mobile Safari/537.36; wv)'},
    }),
    true,
    'the Android WebView user-agent token marks the document as embedded',
  );

  // A plain desktop browser must stay standalone, or the wordmark disappears.
  assert.equal(
    isEmbedded({
      location: {search: '?webview=false'},
      navigator: {userAgent: 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0 Safari/537.36'},
    }),
    false,
    'a desktop browser is standalone',
  );
  assert.equal(
    isEmbedded({
      location: {search: ''},
      navigator: {userAgent: 'Mozilla/5.0 (iPhone; CPU iPhone OS 26_0 like Mac OS X) AppleWebKit/605.1.15 Safari/604.1'},
      webkit: {messageHandlers: {}},
    }),
    false,
    'mobile Safari is a standalone browser',
  );
  assert.equal(isEmbedded(null), false, 'a missing window is not embedded');
}

/* --------------------------------------------------------------- icons --- */

{
  const {icon, glyphs, wifiTile} = await load('icon');

  // The supplied chevron SVG is 6x11 including the half-pixel the 1px stroke
  // needs, so the drawn mark spans exactly the 5x10 the design specifies. The
  // renderer must therefore use the glyph's own box: `size` is a width, and
  // passing one blindly scaled the chevron up to 13x24.
  assert.equal(glyphs.chevron.box, '0 0 6 11');
  const chevron = icon('chevron');
  assert.equal(chevron.getAttribute('width'), '6');
  assert.equal(chevron.getAttribute('height'), '11');
  assert.equal(chevron.getAttribute('viewBox'), '0 0 6 11');

  const chevronPath = glyphs.chevron.shapes[0].attrs.d;
  const xs = [...chevronPath.matchAll(/[ML]\s*([\d.]+)\s+([\d.]+)/g)].map(match => Number(match[1]));
  const ys = [...chevronPath.matchAll(/[ML]\s*([\d.]+)\s+([\d.]+)/g)].map(match => Number(match[2]));
  assert.equal(Math.max(...xs) - Math.min(...xs), 5, 'the chevron ink is 5 wide');
  assert.equal(Math.max(...ys) - Math.min(...ys), 10, 'the chevron ink is 10 tall');

  // A row must not override that box, which is how the oversized chevron shipped.
  const withChevron = row({label: 'Wi-Fi', chevron: true});
  const rendered = withChevron.querySelector('.ds-row__chevron');
  assert.equal(rendered.getAttribute('width'), '6', 'a row uses the chevron at its own size');
  assert.equal(rendered.getAttribute('height'), '11');

  // The other glyphs keep their declared aspect when given an explicit width.
  const lock = icon('lock', {size: 18});
  assert.equal(lock.getAttribute('width'), '18');
  assert.equal(lock.getAttribute('height'), '18');
  assert.equal(icon('wifi').getAttribute('width'), '18');
  assert.equal(icon('wifi').getAttribute('height'), '14', 'the Wi-Fi glyph stays 18x14');
  const weakWifi = icon('wifi', {signalLevel: 1});
  assert.equal(weakWifi.querySelector('.ds-wifi__arc-outer').getAttribute('stroke'), 'var(--color-ink-tertiary)');
  assert.equal(weakWifi.querySelector('.ds-wifi__arc-inner').getAttribute('stroke'), 'currentColor');
  const unavailableWifi = icon('wifi', {signalLevel: 0});
  assert.equal(unavailableWifi.querySelector('.ds-wifi__arc-outer').getAttribute('stroke'), 'var(--color-ink-tertiary)');
  assert.equal(unavailableWifi.querySelector('.ds-wifi__arc-inner').getAttribute('stroke'), 'var(--color-ink-tertiary)');

  const tile = wifiTile();
  assert.equal(tile.getAttribute('width'), '46');
  assert.equal(tile.getAttribute('height'), '46');
  assert.equal(tile.getAttribute('viewBox'), '0 0 46 46');
  // The tile's glyph is the supplied filled 27x18 mark, centred on the plate.
  assert.equal(countByClass(tile, 'ds-tile__glyph-stroke'), 0);
  assert.equal(countByClass(tile, 'ds-tile__glyph-fill'), 1, 'the plate carries the supplied Wi-Fi glyph');
}

/* --------------------------------------------------------- agent log --- */

{
  globalThis.window = globalThis.window || {addEventListener() {}, location: {href: 'http://board.local/'}};
  const {agentLogLines} = await import(pathToFileURL(path.join(uiRoot, '../app/pages/advanced.js')).href);
  const lines = agentLogLines([
    '2026-09-30T08:34:41Z [INFO][agent][server] log_message message="ok"',
    '2026-09-30T08:34:42Z [WARN][agent][phone_bridge] fallback to HTTP',
    '2026-09-30T08:34:43Z [ERROR][agent][voice] stt failed',
    '  wrapped detail of the error',
    '2026-09-30T08:34:44Z [DEBUG][agent][hid] report sent',
    'panic: runtime error',
  ].join('\n'));
  const severities = lines.map(line => [...line._classes].find(name => name.startsWith('ds-log__line--')));
  assert.deepEqual(severities, [
    'ds-log__line--info', 'ds-log__line--warn', 'ds-log__line--error',
    'ds-log__line--error', 'ds-log__line--debug', 'ds-log__line--error',
  ], 'continuation lines keep the severity of the record above; a panic reads as an error');
  assert.equal(lines[0].querySelector('.ds-log__time').textContent, '2026-09-30T08:34:41Z');
  assert.equal(lines[0].querySelector('.ds-log__level').textContent, '[INFO]');
  assert.equal(lines[0].querySelector('.ds-log__scope').textContent, '[agent][server]');
  assert.equal(lines[1].textContent, '2026-09-30T08:34:42Z [WARN][agent][phone_bridge] fallback to HTTP', 'the text is unchanged');
}

/* --------------------------------------------------- agent log polling --- */

{
  const {agentLogPage} = await import(pathToFileURL(path.join(uiRoot, '../app/pages/advanced.js')).href);
  const realFetch = globalThis.fetch;
  const realSetTimeout = globalThis.setTimeout;
  const realClearTimeout = globalThis.clearTimeout;
  const timers = [];
  const calls = [];
  let releaseStatus;
  const statusGate = new Promise(resolve => { releaseStatus = resolve; });
  const respond = body => ({ok: true, status: 200, text: async () => JSON.stringify(body)});
  globalThis.fetch = async url => {
    calls.push(url);
    if (url === '/api/device/status') {
      // The first status read hangs, as the init script can on a busy board.
      if (calls.filter(entry => entry === url).length === 1) await statusGate;
      return respond({agent_status: {process_running: true, pid: 42, port_reachable: true}});
    }
    return respond({agent_log: {log: '2026-09-30T08:34:41Z [INFO][agent][server] ok'}});
  };
  globalThis.setTimeout = callback => timers.push(callback);
  globalThis.clearTimeout = () => {};
  globalThis.requestAnimationFrame = () => {};
  const settle = () => new Promise(resolve => setImmediate(resolve));
  const tick = async () => {
    const pending = timers.splice(0);
    pending.forEach(callback => callback());
    await settle();
  };
  try {
    // Racing a few event-loop turns: a page that waits for its requests would
    // otherwise hang this test instead of failing it.
    const page = await Promise.race([agentLogPage({header() {}, navigate() {}}), settle().then(settle).then(() => null)]);
    assert.ok(page, 'the page is returned while the status request is still pending');
    const body = page.childNodes[0];
    // The shell attaches the returned page synchronously.
    body.isConnected = true;
    await settle();
    assert.equal(timers.length, 1, 'the log read finished and scheduled the next poll');
    releaseStatus();
    for (let i = 0; i < 5; i += 1) await tick();
    assert.equal(calls.filter(url => url === '/api/logs/agent').length, 6, 'the log is polled while the page is shown');
    assert.equal(calls.filter(url => url === '/api/device/status').length, 2, 'the status is refreshed every fifth poll');

    body.isConnected = false;
    await tick();
    assert.equal(timers.length, 0, 'polling stops once the page is left');

    // Left before its first tick, e.g. a quick back: no timer outlives the page.
    const left = await agentLogPage({header() {}, navigate() {}});
    await settle();
    assert.equal(timers.length, 1);
    assert.ok(!left.childNodes[0].isConnected);
    await tick();
    assert.equal(timers.length, 0, 'a page that was never shown does not keep rescheduling');
  } finally {
    globalThis.fetch = realFetch;
    globalThis.setTimeout = realSetTimeout;
    globalThis.clearTimeout = realClearTimeout;
  }
}

process.stdout.write('config web UI component tests passed\n');
