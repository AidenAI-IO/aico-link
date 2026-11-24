/**
 * Buttons.
 *
 * `primary` is the black pill from the design; `quiet` is a link-style action.
 * `accent` and `accent-outline` are the blue pair of the storage page; `danger`
 * confirms a destructive action and `danger-soft` offers one.
 */

import { el } from './dom.js';
import { text } from './text.js';

/**
 * @param {object} props
 * @param {import('./text.js').TextSpec} props.label
 * @param {'primary'|'secondary'|'quiet'|'accent'|'accent-outline'|'danger'|'danger-soft'} [props.variant]
 * @param {boolean} [props.block] - fill the available width.
 * @param {boolean} [props.disabled]
 * @param {string} [props.action] - `data-action` value.
 * @param {() => void} [props.onPress]
 */
export function button(props) {
  const variant = props.variant || 'primary';
  const classes = ['ds-btn', `ds-btn--${variant}`];
  if (props.block) classes.push('ds-btn--block');

  const node = el(
    'button',
    {
      class: classes.join(' '),
      attrs: { type: 'button' },
      data: { action: props.action },
      on: props.onPress ? { click: props.onPress } : {},
    },
    [text(props.label)],
  );
  if (props.disabled) node.disabled = true;
  return node;
}

/** Toggle the disabled state of a button built by `button()`. */
export function setDisabled(node, disabled) {
  node.disabled = Boolean(disabled);
  return node;
}
