/**
 * Setting rows shared by the settings pages.
 */

import { el } from '../ui/dom.js';
import { row } from '../ui/list.js';
import { msg, resolve, text } from '../ui/text.js';
import { toast } from '../ui/toast.js';

/**
 * A row whose trailing value is edited in place. It reads as plain value text
 * until focused, and commits on blur or Enter through the page's saver; an
 * entry `parse` rejects is refused without saving and the saved value returns.
 *
 * @param {object} props
 * @param {import('../ui/text.js').TextSpec} props.label
 * @param {import('../ui/text.js').TextSpec} [props.description] - spell out the
 *   value that means "automatic", since -1 or 0 is not self-explanatory.
 * @param {() => string} props.saved - the saved value as text ('' when unset).
 * @param {(text: string) => any} props.parse - the value to save, or `undefined`
 *   when the text is invalid. `null` is a valid result: it clears the setting.
 * @param {import('../ui/text.js').TextSpec} props.invalid - message for a rejected entry.
 * @param {(value: any) => object} props.patch - the `config` patch for a value.
 * @param {string} props.key - saver key, e.g. `model.temperature`.
 * @param {{change: Function}} props.saver
 * @param {string} [props.inputmode]
 * @param {import('../ui/text.js').TextSpec} [props.placeholder] - shown while unset.
 * @param {boolean} [props.wide] - room for a word or ID rather than a number.
 */
export function inlineValueRow(props) {
  const input = el('input', {
    class: props.wide ? 'ds-row__input ds-row__input--wide' : 'ds-row__input',
    attrs: {
      type: 'text',
      inputmode: props.inputmode || 'decimal',
      value: props.saved(),
      placeholder: props.placeholder ? resolve(props.placeholder) : null,
      'aria-label': resolve(props.label),
      autocomplete: 'off',
      autocorrect: 'off',
      spellcheck: 'false',
    },
    data: { action: `edit-${props.key}` },
  });
  let committing = false;
  async function commit() {
    const raw = input.value.trim();
    if (committing || raw === props.saved()) return;
    const value = props.parse(raw);
    if (value === undefined) {
      toast(props.invalid);
      input.value = props.saved();
      return;
    }
    committing = true;
    const outcome = await props.saver.change(props.key, props.patch(value));
    committing = false;
    if (outcome === 'invalid' || outcome === 'failed') input.value = props.saved();
  }
  input.addEventListener('keydown', (event) => {
    if (event.key === 'Enter') input.blur();
  });
  input.addEventListener('change', commit);
  const node = row({
    label: props.label,
    description: props.description,
    accessory: input,
    extraClass: 'ds-row--editable',
  });
  // Tapping anywhere on the row edits its value, like a native settings cell.
  node.addEventListener('click', (event) => {
    if (event.target !== input) input.focus();
  });
  return node;
}

/** Parsers for `inlineValueRow`. Each returns `undefined` for invalid text. */
export const parse = {
  /** A whole number no smaller than `min`. */
  integer: (min) => (text) =>
    /^-?\d+$/.test(text) && Number(text) >= min ? Number(text) : undefined,
  /** 0, or a fraction in (0, 1). */
  fraction: (text) => {
    if (!/^\d*\.?\d+$/.test(text)) return undefined;
    const number = Number(text);
    return number === 0 || (number > 0 && number < 1) ? number : undefined;
  },
  /** A number in [min, max]; empty clears the setting. */
  optionalRange: (min, max) => (text) => {
    if (text === '') return null;
    if (!/^\d*\.?\d+$/.test(text)) return undefined;
    const number = Number(text);
    return number >= min && number <= max ? number : undefined;
  },
};

/**
 * A free-text entry for a value outside a list, such as a model the catalogue
 * does not know. Its 使用 button is enabled once something is typed; the
 * keyboard's Return does the same.
 *
 * @param {object} props
 * @param {import('../ui/text.js').TextSpec} props.title
 * @param {import('../ui/text.js').TextSpec} props.placeholder
 * @param {(value: string) => void} props.onUse
 * @param {string} [props.action] - `data-action` of the input.
 */
export function customValueField(props) {
  const input = el('input', {
    class: 'ds-input ds-input--with-action',
    attrs: {
      type: 'text',
      autocomplete: 'off',
      autocapitalize: 'none',
      spellcheck: 'false',
      enterkeyhint: 'done',
      placeholder: resolve(props.placeholder),
      'aria-label': resolve(props.title),
    },
    data: { action: props.action || 'edit-custom-value' },
  });
  const use = el(
    'button',
    {
      class: 'ds-input__action',
      attrs: { type: 'button', disabled: 'disabled' },
      data: { action: 'use-custom-value' },
    },
    [text(msg('fields.use', '使用'))],
  );
  // Using a value can take a moment (the model's capabilities are looked up
  // first), so the button stays disabled until it is done.
  const submit = async () => {
    const value = input.value.trim();
    if (!value || (use.disabled && use.dataset.busy)) return;
    use.disabled = true;
    use.dataset.busy = '1';
    try {
      await props.onUse(value);
    } finally {
      delete use.dataset.busy;
      use.disabled = !input.value.trim();
    }
  };
  input.addEventListener('input', () => {
    use.disabled = !input.value.trim();
  });
  input.addEventListener('keydown', (event) => {
    if (event.key === 'Enter') submit();
  });
  use.addEventListener('click', submit);
  return el('div', { class: 'ds-form-card' }, [
    text(props.title, 'ds-form-card__title'),
    el('div', { class: 'ds-input-wrap' }, [input, use]),
  ]);
}
