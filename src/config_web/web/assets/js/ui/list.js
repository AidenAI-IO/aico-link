/**
 * Inset grouped list: the primary surface of every settings route.
 *
 * `group()` renders the white rounded card, `row()` renders one line inside it,
 * and the group inserts hairline separators between rows — never after the last
 * one, matching the design.
 */

import { el, append } from './dom.js';
import { icon } from './icon.js';
import { text } from './text.js';

/** A hairline between two rows, inset to the label's left edge. */
export function separator() {
  return el('div', { class: 'ds-separator' });
}

/**
 * A section caption above a card, e.g. "Other networks".
 * @param {import('./text.js').TextSpec} label
 */
export function caption(label, marker) {
  const children = [text(label)];
  if (marker)
    children.unshift(el('span', { class: `ds-group__marker ds-group__marker--${marker}` }));
  return el(
    'div',
    { class: marker ? 'ds-group__caption ds-group__caption--marked' : 'ds-group__caption' },
    children,
  );
}

/**
 * A row inside an inset group.
 *
 * @param {object} props
 * @param {import('./text.js').TextSpec} props.label
 * @param {import('./text.js').TextSpec} [props.description] - second line under the label.
 * @param {'code'} [props.descriptionTone] - `code` sets it in monospace and lets it break anywhere.
 * @param {import('./text.js').TextSpec} [props.value] - trailing value text.
 * @param {'default'|'success'|'danger'|'strong'|'muted'} [props.tone] - value colour.
 * @param {'default'|'accent'|'strong'|'danger'} [props.labelTone] - label colour and weight.
 * @param {boolean} [props.chevron] - show the disclosure accessory.
 * @param {keyof import('./icon.js').glyphs} [props.icon] - leading glyph.
 * @param {number} [props.signalLevel] - signal level for a Wi-Fi leading glyph.
 * @param {string} [props.iconClass] - class for the leading glyph, for tone.
 * @param {Node} [props.leading] - arbitrary leading node, e.g. a provider logo.
 * @param {Node} [props.accessory] - arbitrary trailing node, e.g. a switch.
 * @param {() => void} [props.onPress]
 * @param {string} [props.action] - `data-action` value used by tests and delegation.
 * @param {string} [props.extraClass] - extra class for context-specific styling.
 */
export function row(props) {
  const classes = ['ds-row'];
  if (props.onPress) classes.push('ds-row--tappable');
  if (props.extraClass) classes.push(props.extraClass);

  const labelModifier = {
    accent: 'ds-row__label--accent',
    strong: 'ds-row__label--strong',
    danger: 'ds-row__label--danger',
  }[props.labelTone];
  const labelClass = labelModifier ? `ds-row__label ${labelModifier}` : 'ds-row__label';
  const body = el('div', { class: 'ds-row__body' }, [text(props.label, labelClass)]);
  if (props.description != null) {
    const tone = props.descriptionTone === 'code' ? ' ds-row__description--code' : '';
    body.appendChild(text(props.description, `ds-row__description${tone}`));
  }

  const children = [];
  if (props.leading) children.push(props.leading);
  if (props.icon) {
    const iconOptions = {
      class: props.iconClass || 'ds-row__icon',
      signalLevel: props.signalLevel,
    };
    children.push(icon(props.icon, iconOptions));
  }
  children.push(body);

  const accessory = [];
  if (props.value != null) {
    const toneClass = {
      success: 'ds-row__value--success',
      danger: 'ds-row__value--danger',
      strong: 'ds-row__value--strong',
      muted: 'ds-row__value--muted',
    }[props.tone];
    accessory.push(text(props.value, toneClass ? `ds-row__value ${toneClass}` : 'ds-row__value'));
  }
  if (props.accessory) accessory.push(props.accessory);
  if (props.chevron) accessory.push(icon('chevron', { class: 'ds-row__chevron' }));
  if (accessory.length) {
    children.push(el('div', { class: 'ds-row__accessory' }, accessory));
  }

  const node = el('div', { class: classes.join(' '), data: { action: props.action } }, children);
  if (props.onPress) {
    node.setAttribute('role', 'button');
    node.setAttribute('tabindex', '0');
    node.addEventListener('click', props.onPress);
    node.addEventListener('keydown', (event) => {
      // Only keys aimed at the row itself. A control inside it (the Wi-Fi ⓘ,
      // a switch) handles its own Enter/Space; letting it bubble here would
      // both run the row's action and cancel the control's own activation.
      if (event.target !== node) return;
      if (event.key === 'Enter' || event.key === ' ') {
        event.preventDefault();
        props.onPress();
      }
    });
  }
  return node;
}

/**
 * A white rounded card holding rows and separators.
 *
 * @param {object} props
 * @param {import('./text.js').TextSpec} [props.caption] - caption rendered above the card.
 * @param {'success'|'muted'} [props.captionMarker] - a short status bar before the caption.
 * @param {Array<Node|null|false>} props.rows
 * @param {boolean} [props.joined] - omit the separators, for a row that owns the
 *   control directly beneath it.
 * @param {import('./text.js').TextSpec} [props.note] - muted footnote inside the card.
 */
export function group(props) {
  const entries = (props.rows || []).filter(Boolean);
  const inner = [];
  entries.forEach((entry, index) => {
    if (index > 0 && !props.joined) inner.push(separator());
    inner.push(entry);
  });
  if (props.note != null) {
    inner.push(text(props.note, 'ds-group__note'));
  }

  const card = el('div', { class: 'ds-group' }, inner);
  if (props.caption == null) return card;
  return el('div', {}, [caption(props.caption, props.captionMarker), card]);
}

/** A stack of cards and captions, spaced by the caller. */
export function screen(children, extraClass) {
  return el('div', { class: extraClass ? `ds-screen ${extraClass}` : 'ds-screen' }, children);
}

export { append };
