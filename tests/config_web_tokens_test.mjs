/**
 * Design-token contract.
 *
 * `src/config_web/design/tokens.mjs` is the single source of truth for both the
 * WebView UI and the companion app's native screens. The generated targets are
 * committed so the Debian packaging step needs no Node, which means they can
 * drift; this test is what stops that.
 */

import assert from 'node:assert/strict';
import {execFile} from 'node:child_process';
import fs from 'node:fs/promises';
import path from 'node:path';
import {fileURLToPath, pathToFileURL} from 'node:url';
import {promisify} from 'node:util';

const run = promisify(execFile);
const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const designRoot = path.join(repositoryRoot, 'src/config_web/design');
const webRoot = path.join(repositoryRoot, 'src/config_web');

const {tokens} = await import(pathToFileURL(path.join(designRoot, 'tokens.mjs')).href);

// 1. The committed outputs must match the source.
try {
  await run(process.execPath, [path.join(repositoryRoot, 'scripts/gen_config_web_tokens.mjs'), '--check']);
} catch (error) {
  assert.fail(
    'generated design tokens are stale; run `node scripts/gen_config_web_tokens.mjs`\n' +
      `${error.stdout || ''}${error.stderr || ''}`,
  );
}

const css = await fs.readFile(path.join(webRoot, 'src/styles/tokens.css'), 'utf8');
const native = await fs.readFile(path.join(designRoot, 'tokens.native.js'), 'utf8');

// 2. Every colour and size token reaches the stylesheet.
for (const [name, value] of Object.entries(tokens.color)) {
  const property = `--color-${name.replace(/([a-z0-9])([A-Z])/g, '$1-$2').toLowerCase()}`;
  assert.ok(css.includes(`${property}: ${value};`), `tokens.css is missing ${property}`);
}

// 3. Durations carry milliseconds and lengths carry pixels.
assert.match(css, /--motion-sheet: 320ms;/, 'sheet duration must be rendered in ms');
assert.match(css, /--motion-fast: 180ms;/, 'fast duration must be rendered in ms');
assert.match(css, /--size-hairline: 0\.5px;/, 'the hairline token must render as 0.5px');
assert.match(css, /--font-weight-semibold: 600;/, 'font weights must stay unitless');

// 4. Values sampled from the design comps must not silently change.
assert.equal(tokens.color.canvas, '#F6F8FA', 'page canvas changed from the sampled design value');
assert.equal(tokens.color.ink, '#111214', 'ink changed from the sampled design value');
assert.equal(tokens.color.success, '#34C759', 'success green changed from the sampled design value');
assert.equal(tokens.color.accent, '#4B8EFF', 'accent must match the supplied Wi-Fi glyph SVG');
assert.equal(tokens.color.primaryFill, '#000000', 'the primary action pill is pure black in the design');

// 5. The native target exposes the same shape, so the app can import one object.
for (const group of ['color', 'space', 'radius', 'size', 'font', 'motion', 'shadow', 'layout']) {
  assert.ok(native.includes(`${group}: {`), `tokens.native.js is missing the ${group} group`);
}
assert.ok(
  // Every group, shadow included: importing a name the module does not
  // export is a link-time SyntaxError in ESM, not undefined.
  native.includes('export const {color, space, radius, size, font, motion, layout, shadow} = tokens;'),
  'tokens.native.js must offer a named export for every group',
);
assert.ok(
  /size: \{[\s\S]*?hairline: 0\.5\b/.test(native),
  'the native hairline stays a number so RN can use StyleSheet.hairlineWidth',
);

process.stdout.write('config web design token tests passed\n');
