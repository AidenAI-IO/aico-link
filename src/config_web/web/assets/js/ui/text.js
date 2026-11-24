/**
 * Translated text nodes.
 *
 * A text spec is either a literal string or `{key, fallback}`. The second form
 * renders immediately *and* carries the `data-i18n` attributes that
 * `config/i18n.js` re-translates on a locale change, so a route does not have to
 * re-render itself to follow the language switch.
 */

import { el } from './dom.js';
import { t } from '../config/i18n.js';

/** @typedef {string | {key: string, fallback?: string}} TextSpec */

/** Resolve a spec to its current-locale string. */
export function resolve(spec) {
  if (spec == null) return '';
  if (typeof spec === 'string') return spec;
  return t(spec.key, { defaultValue: spec.fallback });
}

/**
 * Build a `<span>` for a text spec.
 *
 * @param {TextSpec} spec
 * @param {string} [className]
 * @returns {HTMLElement}
 */
export function text(spec, className) {
  const options = { text: resolve(spec) };
  if (className) options.class = className;
  if (spec && typeof spec !== 'string' && spec.key) {
    options.attrs = { 'data-i18n': spec.key };
    if (spec.fallback != null) options.attrs['data-i18n-default'] = spec.fallback;
  }
  return el('span', options);
}

/** Build a text spec, for readability at call sites. */
export function msg(key, fallback) {
  return { key, fallback };
}
