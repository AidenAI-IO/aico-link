/**
 * Aico Link's text wordmark for standalone settings pages.
 * The companion app supplies its own chrome when this page is embedded.
 */

import {el} from './dom.js';

/**
 * @param {width?: number, class?: string} [options]
 * @returns {HTMLElement}
 */
export function aicoLinkLogo(options = {}) {
  const width = options.width ?? 116;
  return el('span', {
    class: ['app__wordmark', options.class].filter(Boolean).join(' '),
    text: 'Aico Link',
    attrs: {style: `font-size: ${(width * 22) / 116}px`},
  });
}
