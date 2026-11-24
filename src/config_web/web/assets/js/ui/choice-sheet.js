/** Reusable single-choice bottom sheet for settings values. */

import { el } from './dom.js';
import { icon } from './icon.js';
import { row, separator } from './list.js';
import { msg, resolve, text } from './text.js';
import { sheet } from './sheet.js';

/**
 * @param {object} props
 * @param {import('./text.js').TextSpec} [props.title] - omitted when the row that
 *   opened the sheet already names the choice, as on the device-type sheet.
 * @param {Array<{value:string,label:import('./text.js').TextSpec,icon?:string,leading?:() => Node}>} props.options
 *   `leading` builds an arbitrary mark, such as a provider logo, instead of a glyph.
 * @param {string} props.selected
 * @param {(value:string) => void} props.onSelect
 * @param {'list'|'cards'} [props.layout] - `cards` is the design's outlined option
 *   stack for short choices; `list` suits long ones such as the time zone.
 */
export function choiceSheet({ title, options, selected, onSelect, layout = 'list' }) {
  let view;
  const choose = (value) => {
    onSelect(value);
    view.close();
  };
  const body =
    layout === 'cards'
      ? el(
          'div',
          { class: 'ds-option-list' },
          options.map((option) => optionCard(option, option.value === selected, choose)),
        )
      : el('div', { class: 'ds-choice-list' }, listRows(options, selected, choose));
  view = sheet({ title, body: [body] });
  view.open();
  return view;
}

function listRows(options, selected, choose) {
  const entries = [];
  options.forEach((option, index) => {
    if (index > 0) entries.push(separator());
    entries.push(
      row({
        label: option.label,
        extraClass: 'ds-row--plain ds-choice-row',
        action: `choose-${option.value}`,
        onPress: () => choose(option.value),
        accessory:
          option.value === selected
            ? icon('check', { class: 'ds-choice-row__check', size: 24 })
            : null,
      }),
    );
  });
  return entries;
}

/** One outlined option: optional icon tile, label, and the check pill when chosen. */
function optionCard(option, active, choose) {
  return el(
    'button',
    {
      class: active ? 'ds-option ds-option--selected' : 'ds-option',
      attrs: { type: 'button', 'aria-pressed': active ? 'true' : 'false' },
      data: { action: `choose-${option.value}` },
      on: { click: () => choose(option.value) },
    },
    [
      option.leading
        ? option.leading()
        : option.icon
          ? el('span', { class: 'ds-option__icon' }, [icon(option.icon, { size: 18 })])
          : null,
      text(option.label, 'ds-option__label'),
      active ? el('span', { class: 'ds-option__check' }, [icon('check', { size: 16 })]) : null,
    ],
  );
}

/**
 * Each language is named in itself, as native language pickers do, so the
 * labels are literals rather than catalogue entries: a reader who cannot read
 * the current UI language can still find their own.
 */
export const localeOptions = [
  { value: 'en-US', label: 'English' },
  { value: 'zh-CN', label: '简体中文' },
];

export function optionLabel(options, value) {
  const option = options.find((entry) => entry.value === value);
  return option ? resolve(option.label) : value;
}
