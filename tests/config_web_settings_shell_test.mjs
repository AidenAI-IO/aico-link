/**
 * Routed settings shell contract.
 *
 * The refactor this guards against shipped a page tree whose stylesheets and
 * scripts pointed at directories that did not exist, so it rendered unstyled and
 * half-dead. These assertions fail on exactly that class of defect:
 *
 *   - every asset a document references exists on disk,
 *   - every relative import in the new UI modules resolves,
 *   - every `var(--token)` is defined,
 *   - every settings entry in the navigation has a route,
 *   - the modules stay import-safe so the Node VM harness can load them.
 */

import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import path from 'node:path';
import {fileURLToPath, pathToFileURL} from 'node:url';

const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const webRoot = path.join(repositoryRoot, 'src/config_web/web');
const uiRoot = path.join(webRoot, 'assets/js');

const exists = async target => {
  try {
    await fs.access(target);
    return true;
  } catch (error) {
    if (error?.code === 'ENOENT') return false;
    throw error;
  }
};

async function listFiles(root, extension) {
  const found = [];
  for (const entry of await fs.readdir(root, {withFileTypes: true})) {
    const full = path.join(root, entry.name);
    if (entry.isDirectory()) found.push(...(await listFiles(full, extension)));
    else if (entry.name.endsWith(extension)) found.push(full);
  }
  return found;
}

// 1. Documents only reference assets that exist. Client routes are served by the
//    Go fallback, so they are checked against the route table instead.
const documentNames = ['index.html', 'local.html', 'llm-logs.html'];
for (const name of documentNames) {
  const documentPath = path.join(webRoot, name);
  const source = await fs.readFile(documentPath, 'utf8');
  const referenced = [...source.matchAll(/(?:href|src)="(\/assets\/[^"]+)"/g)].map(match => match[1]);
  assert.ok(referenced.length > 0, `${name} references no assets`);
  for (const reference of referenced) {
    const target = path.join(webRoot, reference.replace(/^\//, ''));
    assert.ok(await exists(target), `${name} references a missing asset: ${reference}`);
  }
}

// 2. Every relative import in the layered UI modules resolves.
const modules = [
  ...(await listFiles(path.join(uiRoot, 'ui'), '.js')),
  ...(await listFiles(path.join(uiRoot, 'app'), '.js')),
];
assert.ok(modules.length >= 12, 'expected the new UI module layers to be present');

for (const modulePath of modules) {
  const source = await fs.readFile(modulePath, 'utf8');
  const specifiers = [...source.matchAll(/from\s+'(\.[^']+)'/g)].map(match => match[1]);
  for (const specifier of specifiers) {
    const target = path.resolve(path.dirname(modulePath), specifier);
    assert.ok(
      await exists(target),
      `${path.relative(repositoryRoot, modulePath)} imports a missing module: ${specifier}`,
    );
  }
}

// 3. Stylesheets only use tokens that tokens.css defines.
const tokenCss = await fs.readFile(path.join(webRoot, 'assets/css/tokens.css'), 'utf8');
const defined = new Set([...tokenCss.matchAll(/^\s*(--[\w-]+):/gm)].map(match => match[1]));
// Custom properties a stylesheet defines for itself are legitimately local.
const stylesheets = ['base.css', 'layout.css', 'ds.css'];
for (const name of stylesheets) {
  const source = await fs.readFile(path.join(webRoot, 'assets/css', name), 'utf8');
  const local = new Set([...source.matchAll(/^\s*(--[\w-]+):/gm)].map(match => match[1]));
  for (const match of source.matchAll(/var\((--[\w-]+)/g)) {
    const property = match[1];
    assert.ok(
      defined.has(property) || local.has(property),
      `${name} uses an undefined design token: ${property}`,
    );
  }
}

// 4. Component styling stays flat: React Native has no cascade, so selectors
//    that rely on ancestry cannot be ported mechanically.
const dsCss = await fs.readFile(path.join(webRoot, 'assets/css/ds.css'), 'utf8');
const compoundSelectors = [...dsCss.matchAll(/^([^\s@/{][^{\n]*)\{/gm)]
  .map(match => match[1].trim())
  .filter(selector => /[\s>+~]/.test(selector) && !selector.startsWith('.ds-switch--on .ds-switch__knob'));
assert.deepEqual(
  compoundSelectors,
  [],
  `ds.css must not use descendant or sibling selectors: ${compoundSelectors.join(', ')}`,
);

// 4b. Form fields must not stack the shared keyboard ring on top of their own
//     focus border: doing both rendered a double ring the design does not have.
const baseCss = await fs.readFile(path.join(webRoot, 'assets/css/base.css'), 'utf8');
assert.match(
  baseCss,
  /input:focus-visible,[\s\S]{0,120}?outline:\s*none/,
  'form fields must be excluded from the shared :focus-visible ring',
);
assert.match(
  baseCss,
  /:focus-visible\s*\{[\s\S]*?outline:\s*2px solid var\(--color-accent\)/,
  'non-field controls must keep a visible keyboard focus ring',
);

// 5. Every settings entry is reachable through a declared route.
const {ALL_ITEMS} = await import(pathToFileURL(path.join(uiRoot, 'app/nav-items.js')).href);
const {routes} = await import(pathToFileURL(path.join(uiRoot, 'app/routes.js')).href);
const declared = new Set(routes.map(route => route.path));
for (const item of ALL_ITEMS) {
  assert.ok(declared.has(item.route), `settings entry ${item.id} has no route for ${item.route}`);
}
assert.ok(declared.has('/wifi/:ssid'), 'the Wi-Fi detail route must exist');
assert.ok(declared.has('/basic'), 'the basic settings route must exist');
assert.ok(declared.has('/basic/language'), 'the language and time zone route must exist');

// 6. Sub-pages get a back target one level up and the nav label as the title;
//    the settings home carries only the wordmark.
const {defaultHeader, parentPath} = await import(pathToFileURL(path.join(uiRoot, 'app/shell.js')).href);
assert.equal(parentPath('/basic/language'), '/basic');
assert.equal(parentPath('/basic'), '/');
assert.equal(parentPath('/wifi/'), '/');
assert.equal(defaultHeader('/', () => {}), null, 'the home route has no back button');
const visited = [];
const basicHeader = defaultHeader('/basic', target => visited.push(target));
assert.equal(basicHeader.title.key, 'ui.nav_basic');
basicHeader.onBack();
assert.deepEqual(visited, ['/']);

// 6b. `text()` builds a span, so any class that relies on vertical padding or
//     margin has to opt into a block box: an inline box drops those silently,
//     which is how the sheet's top inset once measured zero.
for (const selector of ['.ds-sheet__title', '.ds-row__description', '.ds-group__note']) {
  // The selector may stand alone or share a rule through a selector list.
  const rule = new RegExp(`${selector.replace('.', '\\.')}\\s*(?:,[^{]*)?\\{[^}]*display:\\s*block`);
  assert.match(dsCss, rule, `${selector} must be display:block to honour vertical spacing`);
}

// 7. Modules must be import-safe: the VM harness and the browser both load them
//    without a document, so no module may touch the DOM at import time.
for (const modulePath of modules) {
  const source = await fs.readFile(modulePath, 'utf8');
  const topLevel = source
    .split('\n')
    .filter(line => /^[A-Za-z]/.test(line) && !line.startsWith('import ') && !line.startsWith('export '));
  for (const line of topLevel) {
    assert.doesNotMatch(
      line,
      /\bdocument\.|\bwindow\./,
      `${path.relative(repositoryRoot, modulePath)} touches the DOM at import time: ${line.trim()}`,
    );
  }
}

process.stdout.write('config web settings shell tests passed\n');

// The host profile has its own route/navigation boundary, even if snapshot loading fails.
const {LOCAL_ITEMS} = await import(pathToFileURL(path.join(uiRoot, 'app/nav-items.js')).href);
const {localRoutes} = await import(pathToFileURL(path.join(uiRoot, 'app/routes.js')).href);
const localPaths = new Set(localRoutes.map(route => route.path));
for (const item of LOCAL_ITEMS) assert.ok(localPaths.has(item.route));
for (const retired of ['/wifi', '/voice', '/storage', '/firmware', '/memory', '/advanced', '/basic']) {
  assert.ok(!localPaths.has(retired), `host route exposes ${retired}`);
}
assert.ok(localPaths.has('/model/providers/edit'));
assert.ok(localPaths.has('/advanced/logs/agent'));
const {applicationLabel} = await import(pathToFileURL(path.join(uiRoot, 'app/pages/local.js')).href);
assert.equal(applicationLabel({persisted: true, applied: false, pending: true}).key, 'local.apply_pending');
assert.equal(applicationLabel({state: 'applied', applied: true}).key, 'local.applied');
assert.equal(applicationLabel({state: 'failed', persisted: true}).key, 'local.apply_failed');
assert.equal(applicationLabel(null).key, 'local.apply_unavailable');
