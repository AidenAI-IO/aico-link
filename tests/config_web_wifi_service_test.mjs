/**
 * Wi-Fi service contract.
 *
 * The rules here are the ones that were wrong in the UI at least once, so they
 * live as pure functions and are pinned by tests:
 *
 *   - the Agent rejects `proxy_mode=proxy` without a `proxy_url`, so a proxy
 *     toggle must never submit that combination;
 *   - the `iw` scan dump is the only place the security of a nearby network is
 *     known, and its records are order-dependent.
 *
 * Live updates are exercised with delayed requests and a controllable clock.
 */

import assert from 'node:assert/strict';
import path from 'node:path';
import {fileURLToPath, pathToFileURL} from 'node:url';
import {test} from 'node:test';

const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const service = await import(
  pathToFileURL(path.join(repositoryRoot, 'src/config_web/web/assets/js/app/wifi-service.js')).href
);
const {otherNetworks, parseSecuredSsids, proxyChangeRequest, wifiSignalLevel} = service;

/* ----------------------------------------------------------------- proxy --- */

// 1. Turning the switch off always means "system": nothing else is required.
assert.deepEqual(
  proxyChangeRequest({enabled: false, url: 'http://proxy.local:8080'}),
  {ok: true, proxyMode: 'system', proxyUrl: ''},
  'disabling the proxy clears the mode and the URL',
);

// 2. Turning it on with no URL is the case that showed a spurious failure: the
//    Agent answers "proxy_url is required when proxy_mode is proxy".
assert.deepEqual(
  proxyChangeRequest({enabled: true, url: '', savedUrl: ''}),
  {ok: false, reason: 'proxy_url_required'},
  'a proxy with no URL must be refused before the request is made',
);
assert.deepEqual(
  proxyChangeRequest({enabled: true, url: '   ', savedUrl: ''}),
  {ok: false, reason: 'proxy_url_required'},
  'whitespace is not a URL',
);

// 3. A committed URL is sent, trimmed.
assert.deepEqual(
  proxyChangeRequest({enabled: true, url: '  http://proxy.local:8080 ', savedUrl: ''}),
  {ok: true, proxyMode: 'proxy', proxyUrl: 'http://proxy.local:8080'},
  'the URL is trimmed before it is sent',
);

// 4. Re-enabling a profile that already stores a URL is allowed without retyping.
assert.deepEqual(
  proxyChangeRequest({enabled: true, url: '', savedUrl: 'http://stored.local:3128'}),
  {ok: true, proxyMode: 'proxy', proxyUrl: 'http://stored.local:3128'},
  'a stored URL is reused, which is what the Agent does too',
);

// 5. A newly typed URL wins over the stored one.
assert.deepEqual(
  proxyChangeRequest({enabled: true, url: 'http://new.local:8080', savedUrl: 'http://old.local:3128'}),
  {ok: true, proxyMode: 'proxy', proxyUrl: 'http://new.local:8080'},
);

/* ------------------------------------------------------------ scan parse --- */

// A capability line precedes its SSID, so the parser has to hold it. Only
// networks advertising Privacy are secured.
const iwDump = `BSS aa:bb:cc:dd:ee:01(on wlan0)
\tfreq: 2437.0
\tcapability: ESS Privacy ShortSlotTime (0x0411)
\tSSID: Locked
BSS aa:bb:cc:dd:ee:02(on wlan0)
\tfreq: 2412.0
\tcapability: ESS ShortSlotTime (0x0401)
\tSSID: Open
BSS aa:bb:cc:dd:ee:03(on wlan0)
\tfreq: 5180.0
\tcapability: ESS Privacy (0x0411)
\tSSID: AlsoLocked
`;
const secured = parseSecuredSsids(iwDump);
assert.equal(secured.has('Locked'), true, 'a Privacy network is secured');
assert.equal(secured.has('AlsoLocked'), true, 'security follows the record it belongs to');
assert.equal(secured.has('Open'), false, 'a network without Privacy is open');

// A missing or unexpected dump must degrade to "no lock icons", never throw.
assert.equal(parseSecuredSsids(undefined).size, 0);
assert.equal(parseSecuredSsids('').size, 0);
assert.equal(parseSecuredSsids('nonsense\n').size, 0);

assert.equal(wifiSignalLevel({signalPercent: 90}), 3, 'strong Wi-Fi uses all arcs');
assert.equal(wifiSignalLevel({signalPercent: 50}), 2, 'medium Wi-Fi uses two arcs');
assert.equal(wifiSignalLevel({signalDbm: -80}), 1, 'weak Wi-Fi uses one arc');
assert.equal(wifiSignalLevel({}), null, 'missing signal leaves the icon neutral');

/* --------------------------------------------------------- other networks --- */

const discovered = [
  {ssid: 'Aico', secured: true},
  {ssid: 'Neighbour', secured: true},
  {ssid: 'Neighbour', secured: true},
  {ssid: '', secured: false},
  {ssid: 'Guest', secured: false},
];

assert.deepEqual(
  otherNetworks(discovered, ['Aico'], 'Aico').map(network => network.ssid),
  ['Neighbour', 'Guest'],
  'saved and connected networks are excluded, and duplicates collapse',
);
assert.deepEqual(
  otherNetworks(discovered, [], '').map(network => network.ssid),
  ['Aico', 'Neighbour', 'Guest'],
  'nothing is hidden when no profile is saved',
);
assert.deepEqual(otherNetworks(undefined, [], ''), []);

/* ---------------------------------------------------------- live updates --- */

const {watchWiFi} = await import(
  pathToFileURL(path.join(repositoryRoot, 'src/config_web/web/assets/js/app/wifi-status.js')).href
);

test('Wi-Fi observations recover, pause in the background, and reject stale reads after a save', async t => {
  const original = Object.fromEntries(['document', 'window', 'fetch', 'setTimeout', 'clearTimeout'].map(key => [key, globalThis[key]]));
  const timers = new Set();
  const reads = [];
  const changes = [];
  const sources = [];
  const browser = new EventTarget();
  browser.CustomEvent = CustomEvent;
  globalThis.window = browser;
  globalThis.document = new EventTarget();
  document.hidden = false;
  globalThis.setTimeout = (callback, ms) => {
    const timer = {callback, ms};
    timers.add(timer);
    return timer;
  };
  globalThis.clearTimeout = timer => timers.delete(timer);
  globalThis.fetch = (url, options) => new Promise((resolve, reject) => {
    reads.push({url, options, resolve: payload => resolve({ok: true, text: async () => typeof payload === 'string' ? payload : JSON.stringify(payload)}), reject});
  });
  let attached = true;
  const stop = watchWiFi((value, {source}) => { changes.push(value); sources.push(source); }, {isActive: () => attached});
  t.after(() => { stop(); Object.assign(globalThis, original); });
  const flush = () => new Promise(resolve => setImmediate(resolve));
  async function tick(ms) {
    for (const timer of [...timers]) {
      if (timer.ms === ms) { timers.delete(timer); timer.callback(); }
    }
    await flush();
  }
  const disconnected = {wifi: {networks: []}, wifi_status: {connected: false, ssid: '', state: 'DISCONNECTED'}};
  const connected = {wifi: {networks: [{ssid: 'Home'}]}, wifi_status: {connected: true, ssid: 'Home', state: 'COMPLETED', ip_address: '192.168.1.8'}};

  await tick(0);
  assert.equal(reads[0].url, '/api/network/wifi/status');
  assert.equal(reads[0].options.cache, 'no-store');
  reads[0].resolve(disconnected);
  await flush();
  assert.deepEqual(changes, [disconnected]);
  assert.equal(sources.at(-1), 'poll');

  await tick(1000);
  await tick(1000);
  assert.equal(reads.length, 2, 'a slow read never overlaps another poll');
  reads[1].resolve(disconnected);
  await flush();
  assert.equal(changes.length, 1, 'unchanged observations do not repaint the UI');

  await tick(1000);
  browser.dispatchEvent(new CustomEvent('aico:wifi-changed', {detail: connected}));
  assert.deepEqual(changes.at(-1), connected, 'a completed connection paints immediately');
  assert.equal(sources.at(-1), 'task');
  const published = changes.length;
  browser.dispatchEvent(new CustomEvent('aico:wifi-changed', {detail: connected}));
  assert.equal(changes.length, published + 1, 'identical task results still establish ordering against navigation reads');
  reads[2].resolve(disconnected);
  await flush();
  assert.deepEqual(changes.at(-1), connected, 'a pre-save read cannot revert the new connection');

  await tick(0);
  reads.at(-1).reject(new Error('device unreachable'));
  await flush();
  assert.equal(changes.at(-1).wifi_status.state, 'UNKNOWN', 'an outage cannot keep a stale connection badge');
  await tick(1000);
  reads.at(-1).resolve(connected);
  await flush();
  assert.deepEqual(changes.at(-1), connected, 'polling recovers after an outage');

  for (const malformed of ['not JSON', {wifi_status: {}}, {wifi_status: {connected: true, state: 'COMPLETED'}}]) {
    await tick(1000);
    reads.at(-1).resolve(malformed);
    await flush();
    assert.equal(changes.at(-1).wifi_status.state, 'UNKNOWN', 'an unreadable status cannot retain a connected badge');
    await tick(1000);
    reads.at(-1).resolve(connected);
    await flush();
    assert.deepEqual(changes.at(-1), connected, 'a valid response recovers from malformed data');
  }

  await tick(1000);
  const stale = reads.at(-1);
  stop.refresh();
  assert.equal(stale.options.signal.aborted, true, 'a fresh snapshot invalidates an older poll');
  const beforeRefresh = changes.length;
  stale.resolve(disconnected);
  await flush();
  assert.equal(changes.length, beforeRefresh);
  await tick(0);
  reads.at(-1).resolve(connected);
  await flush();
  assert.equal(changes.length, beforeRefresh + 1, 'the next observation is delivered even if it matches a pre-navigation poll');

  document.hidden = true;
  document.dispatchEvent(new Event('visibilitychange'));
  const count = reads.length;
  await tick(1000);
  assert.equal(reads.length, count, 'background tabs do not poll');
  document.hidden = false;
  document.dispatchEvent(new Event('visibilitychange'));
  await tick(0);
  assert.equal(reads.length, count + 1, 'returning to the page refreshes immediately');
  reads.at(-1).resolve(disconnected);
  await flush();
  assert.deepEqual(changes.at(-1), disconnected, 'external disconnects update without scanning');

  attached = false;
  const readsBeforeDetach = reads.length;
  const painted = changes.length;
  await tick(1000);
  assert.equal(reads.length, readsBeforeDetach, 'detaching stops polling even after repeated unchanged observations');
  browser.dispatchEvent(new CustomEvent('aico:wifi-changed', {detail: connected}));
  await tick(1000);
  assert.equal(changes.length, painted, 'a stopped watcher cannot repaint or restart');
  assert.equal(timers.size, 0);
  document.hidden = true;
  document.dispatchEvent(new Event('visibilitychange'));
  document.hidden = false;
  document.dispatchEvent(new Event('visibilitychange'));
  browser.dispatchEvent(new Event('online'));
  assert.equal(timers.size, 0, 'detachment also removes restart listeners');
});

process.stdout.write('config web Wi-Fi service tests passed\n');
