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
 * The transport calls themselves are exercised on the board, not here.
 */

import assert from 'node:assert/strict';
import path from 'node:path';
import {fileURLToPath, pathToFileURL} from 'node:url';

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

process.stdout.write('config web Wi-Fi service tests passed\n');
