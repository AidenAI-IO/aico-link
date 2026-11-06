/**
 * Model provider marks.
 *
 * The files under /assets/icons/model/ are LobeHub Icons 1.95.1 static SVGs (MIT,
 * https://github.com/lobehub/lobe-icons), except `speko.svg` (Speko site icon,
 * https://speko.ai/icon.svg) and `moss.svg` (MOSS favicon from
 * https://www.moss.cn/favicon.svg). Single-colour marks are drawn black in
 * `currentColor`, which an <img> cannot recolour, so they sit on a dark disc
 * and are inverted by CSS; colour marks sit on a light disc as drawn.
 */

import {el} from './dom.js';

/**
 * Provider type -> {file, dark, full}. `dark` puts a monochrome mark on a black
 * disc; `full` is an icon that already carries its own background and fills
 * the mark edge to edge.
 */
const MARKS = {
  openai: {file: 'openai', dark: true},
  anthropic: {file: 'anthropic', dark: false},
  openrouter: {file: 'openrouter', dark: true},
  kimi: {file: 'kimi', dark: true},
  'kimi-cn': {file: 'kimi', dark: true},
  volcengine: {file: 'volcengine', dark: false},
  deepseek: {file: 'deepseek', dark: false},
  ollama: {file: 'ollama', dark: true},
  gemini: {file: 'gemini', dark: false},
  // Voice provider types (speech-to-text, text-to-speech, realtime).
  'openai-whisper': {file: 'openai', dark: true},
  qwen: {file: 'qwen', dark: false},
  'qwen-asr': {file: 'qwen', dark: false},
  alicloud: {file: 'alibabacloud', dark: false},
  minimax: {file: 'minimax', dark: false},
  'minimax-cn': {file: 'minimax', dark: false},
  'tencent-asr': {file: 'tencentcloud', dark: false},
  'google-cloud': {file: 'google', dark: false},
  'fish-audio': {file: 'fishaudio', dark: true},
  xai: {file: 'xai', dark: true},
  speko: {file: 'speko', full: true},
  moss: {file: 'moss', full: true},
};

/**
 * @param {string} type - provider type, e.g. `openai`. `custom` is the blue
 *   "others" disc; a type without a mark (such as `speko`) gets its initial.
 * @param {{size?: 'row'|'tile'}} [options]
 */
export function providerMark(type, options = {}) {
  const mark = MARKS[type];
  const size = options.size === 'tile' ? 'ds-mark--tile' : 'ds-mark--row';
  if (type === 'custom') {
    return el('span', {class: `ds-mark ${size} ds-mark--others`, attrs: {'aria-hidden': 'true'}}, [
      el('span', {class: 'ds-mark__dots', text: '•••'}),
    ]);
  }
  if (!mark) {
    return el('span', {class: `ds-mark ${size} ds-mark--initial`, attrs: {'aria-hidden': 'true'}}, [
      el('span', {class: 'ds-mark__initial', text: String(type || '?').charAt(0).toUpperCase()}),
    ]);
  }
  const tone = mark.full ? 'ds-mark--full' : mark.dark ? 'ds-mark--dark' : 'ds-mark--light';
  return el('span', {class: `ds-mark ${size} ${tone}`, attrs: {'aria-hidden': 'true'}}, [
    el('img', {
      class: mark.full ? 'ds-mark__img ds-mark__img--full' : mark.dark ? 'ds-mark__img ds-mark__img--inverted' : 'ds-mark__img',
      attrs: {src: `/assets/icons/model/${mark.file}.svg`, alt: ''},
    }),
  ]);
}
