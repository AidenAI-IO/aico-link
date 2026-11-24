/**
 * TOML editor with syntax colouring.
 *
 * A transparent <textarea> sits exactly over a <pre> that renders the same
 * text with coloured spans; the two share font, padding and scroll position,
 * so editing is plain textarea editing (selection, IME, undo, paste) while
 * the colours follow as you type. No library is loaded: the settings page is
 * served by the device and may have no route to a CDN.
 *
 * The tokenizer is line-based and deliberately small: table headers, keys,
 * strings, numbers, booleans, dates and comments. Multi-line strings are
 * coloured line by line, which is as far as a highlighter (not a parser) needs
 * to go; the Agent validates the file on save.
 */

import { el } from './dom.js';

const escape = (value) => value.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
const span = (kind, value) => `<span class="ds-toml__${kind}">${escape(value)}</span>`;

/** Tokens a value may contain, tried in order at each position. */
const VALUE_TOKENS = [
  ['comment', /#.*/y],
  ['string', /"""[^]*?(?:"""|$)|'''[^]*?(?:'''|$)|"(?:[^"\\]|\\.)*"?|'[^']*'?/y],
  ['date', /\d{4}-\d{2}-\d{2}(?:[T ]\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:\d{2})?)?/y],
  [
    'number',
    /[+-]?(?:inf|nan|0x[\da-fA-F_]+|0o[0-7_]+|0b[01_]+|\d[\d_]*(?:\.[\d_]+)?(?:[eE][+-]?\d+)?)(?![\w.-])/y,
  ],
  ['boolean', /(?:true|false)(?![\w-])/y],
];

function highlightValue(text) {
  let out = '';
  let index = 0;
  while (index < text.length) {
    let matched = false;
    for (const [kind, pattern] of VALUE_TOKENS) {
      pattern.lastIndex = index;
      const match = pattern.exec(text);
      if (match && match[0]) {
        out += span(kind, match[0]);
        index += match[0].length;
        matched = true;
        break;
      }
    }
    if (!matched) {
      // Punctuation and bare words (inline-table keys) keep the base ink.
      const next = text.slice(index).search(/["'#\d+-]|true|false|inf|nan/);
      const end = next <= 0 ? index + 1 : index + next;
      out += escape(text.slice(index, end));
      index = end;
    }
  }
  return out;
}

/** HTML for one line of TOML. */
export function highlightLine(line) {
  const header = /^(\s*)(\[\[?[^\]#]*\]\]?)(.*)$/.exec(line);
  if (header) return escape(header[1]) + span('table', header[2]) + highlightValue(header[3]);
  const pair =
    /^(\s*)((?:"(?:[^"\\]|\\.)*"|'[^']*'|[A-Za-z0-9_.-]+)(?:\s*\.\s*(?:"(?:[^"\\]|\\.)*"|'[^']*'|[A-Za-z0-9_-]+))*)(\s*=)(.*)$/.exec(
      line,
    );
  if (pair)
    return (
      escape(pair[1]) + span('key', pair[2]) + span('punct', pair[3]) + highlightValue(pair[4])
    );
  return highlightValue(line);
}

/** HTML for a whole document. */
export function highlightToml(source) {
  return source.split('\n').map(highlightLine).join('\n');
}

/**
 * @param {object} props
 * @param {string} props.value
 * @param {string} [props.label] - accessible name of the textarea.
 * @param {(value: string) => void} [props.onInput]
 * @returns {{el: HTMLElement, textarea: HTMLTextAreaElement, setValue: (value: string) => void}}
 */
export function tomlEditor(props) {
  // `code` would take the browser's own `monospace` face unless told to
  // inherit; a face even slightly wider than the textarea's drifts the caret
  // further from the text with every character.
  const code = el('code', { class: 'ds-toml__code' });
  const highlight = el('pre', { class: 'ds-toml__highlight', attrs: { 'aria-hidden': 'true' } }, [
    code,
  ]);
  const textarea = el('textarea', {
    class: 'ds-toml__input',
    attrs: {
      spellcheck: 'false',
      autocapitalize: 'none',
      autocomplete: 'off',
      autocorrect: 'off',
      wrap: 'off',
      'aria-label': props.label || 'TOML',
    },
    data: { action: 'edit-toml' },
  });
  textarea.value = props.value || '';

  const paint = () => {
    // A trailing newline needs a character after it, or the last empty line
    // of the overlay collapses and the two layers drift apart by one line.
    code.innerHTML = `${highlightToml(textarea.value)}\n `;
  };
  const sync = () => {
    highlight.scrollTop = textarea.scrollTop;
    highlight.scrollLeft = textarea.scrollLeft;
  };
  textarea.addEventListener('input', () => {
    paint();
    sync();
    if (props.onInput) props.onInput(textarea.value);
  });
  textarea.addEventListener('scroll', sync);
  // Tab indents instead of leaving the field.
  textarea.addEventListener('keydown', (event) => {
    if (event.key !== 'Tab' || event.shiftKey) return;
    event.preventDefault();
    textarea.setRangeText('  ', textarea.selectionStart, textarea.selectionEnd, 'end');
    textarea.dispatchEvent(new Event('input'));
  });
  paint();

  return {
    el: el('div', { class: 'ds-toml' }, [highlight, textarea]),
    textarea,
    setValue(value) {
      textarea.value = value;
      paint();
      sync();
    },
  };
}
