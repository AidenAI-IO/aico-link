#!/usr/bin/env node
/**
 * Generate the Config Web design-token targets from the single source of truth.
 *
 *   node scripts/gen_config_web_tokens.mjs           # write generated files
 *   node scripts/gen_config_web_tokens.mjs --check   # verify they are in sync
 *
 * Outputs:
 *   src/config_web/src/styles/tokens.css    CSS custom properties for the web UI
 *   src/config_web/design/tokens.native.js      React Native values for the companion app
 *
 * Both outputs are committed and consumed by the frontend build and companion
 * app. `tests/config_web_tokens_test.mjs`
 * fails when a generated file drifts from `design/tokens.mjs`.
 */

import fs from 'node:fs/promises';
import path from 'node:path';
import {fileURLToPath, pathToFileURL} from 'node:url';

const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const sourcePath = path.join(repositoryRoot, 'src/config_web/design/tokens.mjs');
const cssPath = path.join(repositoryRoot, 'src/config_web/src/styles/tokens.css');
const nativePath = path.join(repositoryRoot, 'src/config_web/design/tokens.native.js');

const HEADER =
  'GENERATED FILE — do not edit by hand. Edit src/config_web/design/tokens.mjs\n' +
  ' * and run: node scripts/gen_config_web_tokens.mjs';

/** camelCase to kebab-case, so `inkSecondary` becomes `--ink-secondary`. */
function kebab(name) {
  return name.replace(/([a-z0-9])([A-Z])/g, '$1-$2').toLowerCase();
}

/**
 * CSS unit for a numeric token. Defaults to pixels; a duration is milliseconds
 * and a font weight is unitless.
 */
function cssUnit(group, name, value) {
  if (group === 'font' && name.startsWith('weight')) return '';
  if (group === 'motion' && (name === 'sheet' || name === 'fast')) return 'ms';
  return 'px';
}

/**
 * Flatten nested token groups into ordered `--<group>-<name>` custom properties.
 * Arrays render as `cubic-bezier(...)`, shadow tokens as a CSS shadow, and
 * everything else is a number with the unit implied by its group, or a literal.
 */
function cssCustomProperties(tokens) {
  const declarations = [];
  for (const [group, entries] of Object.entries(tokens)) {
    for (const [name, value] of Object.entries(entries)) {
      const property = `--${kebab(group)}-${kebab(name)}`;
      let rendered;
      if (Array.isArray(value)) {
        rendered = `cubic-bezier(${value.join(', ')})`;
      } else if (value && typeof value === 'object') {
        const {offsetX, offsetY, blur, color: shadowColor} = value;
        rendered = `${offsetX}px ${offsetY}px ${blur}px ${shadowColor}`;
      } else if (typeof value === 'number') {
        rendered = `${value}${cssUnit(group, name, value)}`;
      } else {
        rendered = String(value);
      }
      declarations.push({group, property, rendered});
    }
  }
  return declarations;
}

function renderCss(tokens) {
  const declarations = cssCustomProperties(tokens);
  const lines = [];
  let currentGroup = null;
  for (const {group, property, rendered} of declarations) {
    if (group !== currentGroup) {
      if (currentGroup !== null) lines.push('');
      currentGroup = group;
    }
    lines.push(`  ${property}: ${rendered};`);
  }
  return `/*\n * ${HEADER}\n *\n * Custom properties cannot be used inside a media query condition, so\n * \`--layout-breakpoint-desktop\` is for JavaScript (matchMedia) only; the\n * stylesheets repeat that width in their own \`@media\` rules.\n */\n\n:root {\n${lines.join('\n')}\n}\n`;
}

function renderNative(tokens) {
  const body = JSON.stringify(tokens, null, 2)
    // Keep the JSON output valid JavaScript module syntax.
    .replace(/"([A-Za-z_$][\w$]*)":/g, '$1:');

  return `/**\n * ${HEADER}\n *\n * Import these values in the companion app so a WebView-rendered settings\n * surface and a native screen cannot drift apart:\n *\n *   import {tokens} from './tokens.native';\n *\n * Notes for the React Native side:\n *   - \`size.hairline\` is 0.5 for the web. Use \`StyleSheet.hairlineWidth\` natively.\n *   - \`motion.sheetBezier\` feeds \`Easing.bezier(...)\`; \`motion.sheet\` is in ms.\n *   - \`font.family\` is a CSS stack. Native uses the platform system font,\n *     which is the same typeface the stack resolves to on iOS.\n */\n\n/** @type {const} */\nexport const tokens = ${body};\n\nexport const {color, space, radius, size, font, motion, layout, shadow} = tokens;\n\nexport default tokens;\n`;
}

async function main() {
  const check = process.argv.includes('--check');
  const {tokens} = await import(pathToFileURL(sourcePath).href);

  const outputs = [
    {path: cssPath, content: renderCss(tokens)},
    {path: nativePath, content: renderNative(tokens)},
  ];

  const drifted = [];
  for (const output of outputs) {
    const relative = path.relative(repositoryRoot, output.path);
    if (check) {
      let existing = null;
      try {
        existing = await fs.readFile(output.path, 'utf8');
      } catch (error) {
        if (error?.code !== 'ENOENT') throw error;
      }
      if (existing !== output.content) drifted.push(relative);
      continue;
    }
    await fs.mkdir(path.dirname(output.path), {recursive: true});
    await fs.writeFile(output.path, output.content);
    process.stdout.write(`wrote ${relative}\n`);
  }

  if (check) {
    if (drifted.length) {
      process.stderr.write(
        `generated design tokens are stale: ${drifted.join(', ')}\n` +
          'run: node scripts/gen_config_web_tokens.mjs\n',
      );
      process.exitCode = 1;
      return;
    }
    process.stdout.write('design tokens are in sync\n');
  }
}

await main();
