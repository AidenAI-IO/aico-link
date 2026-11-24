/**
 * Switch control.
 *
 * A `role="switch"` button rather than a checkbox, so the markup maps directly
 * onto React Native's `Switch` and onto a `Pressable` when the native design
 * needs the same custom track.
 *
 * State ownership: there is no reactive re-render in this UI, so the component
 * owns its own painted state. A press flips the visual immediately and then
 * reports the new value through `onChange`; a caller that rejects the change
 * (a failed request, say) calls `setChecked` to put it back. That way a call
 * site only has to describe intent, and the control is usable in any number of
 * places without each one remembering to repaint it.
 */

import { el } from './dom.js';
import { resolve } from './text.js';

/** Painted state per switch node, so `setChecked` can also record it. */
const state = new WeakMap();

function paint(node, knob, on) {
  node.classList.toggle('ds-switch--on', on);
  if (knob) knob.classList.toggle('ds-switch__knob--on', on);
  node.setAttribute('aria-checked', String(on));
}

/**
 * @param {object} props
 * @param {boolean} props.checked - initial state.
 * @param {import('./text.js').TextSpec} [props.label] - accessible name.
 * @param {(next: boolean) => void} [props.onChange] - called after the visual flips.
 * @returns {HTMLButtonElement}
 */
export function switchControl(props) {
  const knob = el('span', { class: 'ds-switch__knob' });
  const node = el(
    'button',
    {
      class: 'ds-switch',
      attrs: {
        type: 'button',
        role: 'switch',
        'aria-label': props.label ? resolve(props.label) : null,
      },
      data: { action: 'toggle-switch' },
    },
    [knob],
  );

  const entry = { checked: Boolean(props.checked) };
  state.set(node, entry);
  paint(node, knob, entry.checked);

  node.addEventListener('click', () => {
    entry.checked = !entry.checked;
    paint(node, knob, entry.checked);
    if (props.onChange) props.onChange(entry.checked);
  });

  return node;
}

/**
 * Set the state of a switch built by `switchControl()`, for callers that need to
 * apply or revert a change outside of a press.
 *
 * @param {HTMLElement} node
 * @param {boolean} checked
 */
export function setChecked(node, checked) {
  const entry = state.get(node) || {};
  entry.checked = Boolean(checked);
  state.set(node, entry);
  paint(node, node.querySelector('.ds-switch__knob'), entry.checked);
  return node;
}
