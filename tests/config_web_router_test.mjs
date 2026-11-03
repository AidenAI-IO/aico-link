/**
 * Router contract.
 *
 * The first assertion here guards a defect that shipped: `createRouter` rebuilt
 * each entry from the matcher alone, so a route's own metadata — `name`, and with
 * it the flag the desktop layout uses to keep the detail panel empty — was
 * dropped and every route silently behaved like the fallback.
 */

import assert from 'node:assert/strict';
import path from 'node:path';
import {fileURLToPath, pathToFileURL} from 'node:url';

const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const {createRouter} = await import(
  pathToFileURL(path.join(repositoryRoot, 'src/config_web/web/assets/js/app/router.js')).href
);

/** Minimal browser surface: the router only reads location and writes history. */
function stubWindow(pathname, search = '') {
  const pushes = [];
  globalThis.window = {
    location: {pathname, search, origin: 'http://board.local'},
    history: {
      pushState: (_state, _title, url) => pushes.push(url),
      replaceState: (_state, _title, url) => pushes.push(url),
    },
    addEventListener() {},
  };
  return pushes;
}

const pages = {
  home: async () => 'home',
  wifi: async () => 'wifi',
  detail: async () => 'detail',
};

const routes = [
  {path: '/', name: 'home', page: pages.home},
  {path: '/wifi', name: 'wifi', page: pages.wifi},
  {path: '/wifi/:ssid', name: 'wifi-detail', page: pages.detail},
];

/** Run one navigation and capture the render context. */
async function navigateTo(target) {
  stubWindow(target);
  const seen = [];
  const router = createRouter({
    routes,
    render: async context => {
      seen.push(context);
    },
  });
  await router.navigate(target, {silent: true});
  return seen[0];
}

// 1. A route's declared metadata must survive route compilation.
{
  const context = await navigateTo('/wifi');
  assert.equal(context.name, 'wifi', 'a route name must reach the render context');
  assert.equal(context.page, pages.wifi, 'the declared page must reach the render context');
  assert.equal(context.pattern, '/wifi');
}

// 2. Parameters are extracted and decoded.
{
  const context = await navigateTo('/wifi/Aico%20IoT');
  assert.equal(context.name, 'wifi-detail');
  assert.equal(context.params.ssid, 'Aico IoT', 'path parameters are URL-decoded');
}

// 3. Query strings are parsed alongside the path.
{
  const context = await navigateTo('/wifi?focus=password');
  assert.equal(context.name, 'wifi');
  assert.equal(context.query.focus, 'password');
}

// 4. Longer literal routes do not swallow their own sub-routes.
{
  assert.equal((await navigateTo('/wifi')).name, 'wifi');
  assert.equal((await navigateTo('/wifi/Aico')).name, 'wifi-detail');
}

// 5. An unknown path falls back to the root route, so a stale link cannot blank
//    the page.
{
  const context = await navigateTo('/does-not-exist');
  assert.equal(context.name, 'home');
  assert.equal(context.path, '/');
}

// 6. Navigating to a different path records exactly one history entry.
{
  stubWindow('/');
  const router = createRouter({routes, render: async () => {}});
  await router.navigate('/wifi');
  assert.equal(window.history.pushState === undefined, false);
  // `navigate` pushes before rendering; the stub collects the entries.
  const pushes = [];
  globalThis.window.history.pushState = (_state, _title, url) => pushes.push(url);
  await router.navigate('/wifi/Aico');
  assert.deepEqual(pushes, ['/wifi/Aico'], 'one entry per navigation');
}

// 8. A malformed escape in a deep link falls back to the settings list
// instead of throwing out of navigate() and leaving the page blank.
{
  const context = await navigateTo('/wifi/%E0');
  assert.equal(context.name, 'home');
}

// 7. Link clicks: in-app links navigate; a download link (how the backup
// archive is fetched) and a new-tab link are left to the browser. Intercepting
// the download once replaced the storage page with the archive's URL.
{
  const pushes = stubWindow('/storage');
  let handler = null;
  globalThis.document = {addEventListener: (type, fn) => { if (type === 'click') handler = fn; }};
  const router = createRouter({routes, render: async () => {}});
  await router.start();
  const click = attributes => {
    const anchor = {
      getAttribute: name => attributes[name] ?? null,
      hasAttribute: name => name in attributes,
      target: attributes.target || '',
    };
    const event = {button: 0, defaultPrevented: false, target: {closest: () => anchor},
      preventDefault() { this.defaultPrevented = true; }};
    handler(event);
    return event.defaultPrevented;
  };
  assert.equal(click({href: '/api/backup/jobs/b1/archive', download: 'x.aico-backup'}), false, 'downloads are not routed');
  assert.equal(click({href: '/webtty/', target: '_blank'}), false, 'new-tab links are not routed');
  assert.equal(click({href: '/wifi'}), true, 'in-app links are routed');
  assert.deepEqual(pushes, ['/wifi']);
  delete globalThis.document;
}

process.stdout.write('config web router tests passed\n');
