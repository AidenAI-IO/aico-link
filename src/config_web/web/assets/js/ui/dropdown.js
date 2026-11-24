/**
 * Dropdown: a settings row that opens a list directly beneath it.
 *
 * The design draws this for the time zone: the row takes an accent outline
 * and a down chevron while open, and the options sit in a floating card under
 * it, scrolling inside a fixed height. Choosing an option, tapping the row
 * again, tapping anywhere else, or pressing Escape closes it.
 *
 * Unlike a bottom sheet it keeps the page in view, which suits a long list of
 * plain values the user scans for one item.
 */

import { el } from './dom.js';
import { icon } from './icon.js';
import { resolve, text } from './text.js';

/**
 * @param {object} props
 * @param {import('./text.js').TextSpec} props.label
 * @param {Array<{value: string, label: import('./text.js').TextSpec}>} props.options
 * @param {string} props.selected
 * @param {(value: string) => void} props.onSelect
 * @param {string} [props.action] - `data-action` of the row.
 * @returns {HTMLElement}
 */
export function dropdown(props) {
  const current = props.options.find((option) => option.value === props.selected);
  const chevron = icon('chevron', { class: 'ds-dropdown__chevron' });
  const trigger = el(
    'button',
    {
      class: 'ds-dropdown__trigger',
      attrs: { type: 'button', 'aria-haspopup': 'listbox', 'aria-expanded': 'false' },
      data: { action: props.action || 'open-dropdown' },
    },
    [
      text(props.label, 'ds-dropdown__label'),
      text(current ? current.label : props.selected || '', 'ds-dropdown__value'),
      chevron,
    ],
  );

  const list = el(
    'div',
    {
      class: 'ds-dropdown__list',
      attrs: { role: 'listbox', 'aria-label': resolve(props.label), hidden: 'hidden' },
    },
    props.options.map((option) => {
      const active = option.value === props.selected;
      return el(
        'button',
        {
          class: active
            ? 'ds-dropdown__option ds-dropdown__option--selected'
            : 'ds-dropdown__option',
          attrs: { type: 'button', role: 'option', 'aria-selected': active ? 'true' : 'false' },
          data: { action: `choose-${option.value}` },
          on: {
            click: () => {
              close();
              if (!active) props.onSelect(option.value);
            },
          },
        },
        [
          text(option.label, 'ds-dropdown__option-label'),
          active ? icon('check', { class: 'ds-choice-row__check', size: 20 }) : null,
        ],
      );
    }),
  );

  const root = el('div', { class: 'ds-dropdown' }, [trigger, list]);

  function onOutside(event) {
    if (!root.contains(event.target)) close();
  }
  function onKeydown(event) {
    if (event.key === 'Escape') {
      close();
      trigger.focus();
    }
  }
  function open() {
    list.hidden = false;
    trigger.classList.add('ds-dropdown__trigger--open');
    chevron.classList.add('ds-dropdown__chevron--open');
    trigger.setAttribute('aria-expanded', 'true');
    document.addEventListener('pointerdown', onOutside, true);
    document.addEventListener('keydown', onKeydown);
    // Start at the chosen item, as a native picker does.
    const selected = list.querySelector('.ds-dropdown__option--selected');
    if (selected)
      list.scrollTop = selected.offsetTop - list.clientHeight / 2 + selected.offsetHeight / 2;
  }
  function close() {
    list.hidden = true;
    trigger.classList.remove('ds-dropdown__trigger--open');
    chevron.classList.remove('ds-dropdown__chevron--open');
    trigger.setAttribute('aria-expanded', 'false');
    document.removeEventListener('pointerdown', onOutside, true);
    document.removeEventListener('keydown', onKeydown);
  }
  trigger.addEventListener('click', () => (list.hidden ? open() : close()));
  return root;
}
