import { useEffect, useId, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import type { TextSpec } from '../types';
import { msg, resolve } from '../lib/i18n';
import { Modal, toast } from './overlays';
import { Icon, Row, Text } from './ui';
import type { glyphs } from '../lib/glyphs';

export interface Choice {
  value: string;
  label: TextSpec;
  icon?: keyof typeof glyphs;
  leading?: ReactNode;
}
export function ChoiceRow({
  label,
  options,
  selected,
  onSelect,
  disabled,
  action,
  chevron,
  layout = 'list',
  className,
  leading,
  outlined = false,
}: {
  label: TextSpec;
  options: Choice[];
  selected: string;
  onSelect: (value: string) => unknown;
  disabled?: boolean;
  action?: string;
  chevron?: boolean;
  layout?: 'list' | 'cards';
  className?: string;
  leading?: ReactNode;
  outlined?: boolean;
}) {
  const [open, setOpen] = useState(false);
  const [draft, setDraft] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  useEffect(() => {
    if (disabled) setOpen(false);
  }, [disabled]);
  useEffect(() => {
    setDraft((current) => (current === selected ? null : current));
  }, [selected]);
  const shown = draft ?? selected;
  const current = options.find((option) => option.value === shown);
  return (
    <>
      <Row
        label={outlined ? (current?.label ?? shown) : label}
        value={outlined ? undefined : (current?.label ?? shown)}
        chevron={chevron}
        action={action}
        className={className}
        leading={leading}
        onPress={() => {
          if (!disabled && !saving) setOpen(true);
        }}
      />
      {open && (
        <Modal title={label} hideTitle onClose={() => setOpen(false)}>
          <div className={layout === 'cards' ? 'ds-option-list' : 'ds-choice-list'}>
            {options.map((option, index) => {
              const active = option.value === shown;
              const choose = async () => {
                setOpen(false);
                setDraft(option.value);
                setSaving(true);
                try {
                  const result = await onSelect(option.value);
                  if (result !== 'staged') setDraft(null);
                } catch (error) {
                  setDraft(null);
                  toast(error instanceof Error ? error.message : String(error));
                } finally {
                  setSaving(false);
                }
              };
              return layout === 'cards' ? (
                <button
                  key={option.value}
                  type="button"
                  className={`ds-option ${active ? 'ds-option--selected' : ''}`}
                  aria-pressed={active}
                  data-action={`choose-${option.value}`}
                  onClick={choose}
                >
                  {option.leading ??
                    (option.icon && (
                      <span className="ds-option__icon">
                        <Icon name={option.icon} size={18} />
                      </span>
                    ))}
                  <Text value={option.label} className="ds-option__label" />
                  {active && (
                    <span className="ds-option__check">
                      <Icon name="check" size={16} />
                    </span>
                  )}
                </button>
              ) : (
                <div key={option.value}>
                  {index > 0 && <div className="ds-separator" />}
                  <Row
                    label={option.label}
                    className="ds-row--plain ds-choice-row"
                    action={`choose-${option.value}`}
                    onPress={choose}
                  >
                    {active && <Icon name="check" className="ds-choice-row__check" size={24} />}
                  </Row>
                </div>
              );
            })}
          </div>
        </Modal>
      )}
    </>
  );
}
export function Dropdown({
  label,
  options,
  selected,
  onSelect,
  disabled,
  action,
}: {
  label: TextSpec;
  options: Choice[];
  selected: string;
  onSelect: (value: string) => unknown;
  disabled?: boolean;
  action?: string;
}) {
  const [open, setOpen] = useState(false);
  const root = useRef<HTMLDivElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const id = useId();
  useEffect(() => {
    if (!open) return;
    const outside = (event: PointerEvent) => {
      if (!root.current?.contains(event.target as Node)) setOpen(false);
    };
    const key = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        setOpen(false);
        trigger.current?.focus();
        return;
      }
      const options = [
        ...(root.current?.querySelectorAll<HTMLButtonElement>(
          'button[role="option"]:not(:disabled)',
        ) || []),
      ];
      if (!options.length) return;
      const index = options.indexOf(document.activeElement as HTMLButtonElement);
      let next: number;
      if (event.key === 'ArrowDown') next = index < 0 ? 0 : (index + 1) % options.length;
      else if (event.key === 'ArrowUp')
        next = index < 0 ? options.length - 1 : (index - 1 + options.length) % options.length;
      else if (event.key === 'Home') next = 0;
      else if (event.key === 'End') next = options.length - 1;
      else return;
      event.preventDefault();
      options[next]?.focus();
    };
    document.addEventListener('pointerdown', outside, true);
    document.addEventListener('keydown', key);
    const selected = root.current?.querySelector<HTMLButtonElement>(
      'button[role="option"][aria-selected="true"]:not(:disabled)',
    );
    (
      selected ||
      root.current?.querySelector<HTMLButtonElement>('button[role="option"]:not(:disabled)')
    )?.focus();
    selected?.scrollIntoView?.({ block: 'nearest' });
    return () => {
      document.removeEventListener('pointerdown', outside, true);
      document.removeEventListener('keydown', key);
    };
  }, [open]);
  return (
    <div ref={root} className="ds-dropdown">
      <button
        ref={trigger}
        type="button"
        className={`ds-dropdown__trigger ${open ? 'ds-dropdown__trigger--open' : ''}`}
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-controls={id}
        disabled={disabled}
        data-action={action}
        onClick={() => setOpen(!open)}
      >
        <Text value={label} className="ds-dropdown__label" />
        <Text
          value={options.find((option) => option.value === selected)?.label ?? selected}
          className="ds-dropdown__value"
        />
        <Icon
          name="chevron"
          className={`ds-dropdown__chevron ${open ? 'ds-dropdown__chevron--open' : ''}`}
        />
      </button>
      <div
        id={id}
        className="ds-dropdown__list"
        role="listbox"
        aria-label={resolve(label)}
        hidden={!open}
      >
        {options.map((option) => (
          <button
            key={option.value}
            type="button"
            role="option"
            disabled={disabled}
            aria-selected={option.value === selected}
            className={`ds-dropdown__option ${option.value === selected ? 'ds-dropdown__option--selected' : ''}`}
            onClick={() => {
              setOpen(false);
              trigger.current?.focus();
              if (option.value !== selected) void onSelect(option.value);
            }}
          >
            <Text value={option.label} className="ds-dropdown__option-label" />
            {option.value === selected && (
              <Icon name="check" className="ds-choice-row__check" size={20} />
            )}
          </button>
        ))}
      </div>
    </div>
  );
}
export function CustomValue({
  label,
  placeholder,
  onUse,
}: {
  label: TextSpec;
  placeholder: TextSpec;
  onUse: (value: string) => Promise<unknown>;
}) {
  const [value, setValue] = useState('');
  const [busy, setBusy] = useState(false);
  const id = useId();
  async function submit() {
    if (!value.trim() || busy) return;
    setBusy(true);
    try {
      await onUse(value.trim());
    } finally {
      setBusy(false);
    }
  }
  return (
    <form
      className="ds-form-card"
      onSubmit={(event) => {
        event.preventDefault();
        void submit();
      }}
    >
      <label htmlFor={id} className="ds-form-card__title">
        <Text value={label} />
      </label>
      <div className="ds-input-wrap">
        <input
          id={id}
          className="ds-input ds-input--with-action"
          value={value}
          placeholder={resolve(placeholder)}
          onChange={(event) => setValue(event.target.value)}
          autoComplete="off"
          spellCheck={false}
        />
        <button type="submit" className="ds-input__action" disabled={!value.trim() || busy}>
          <Text value={msg('fields.use', '使用')} />
        </button>
      </div>
    </form>
  );
}
