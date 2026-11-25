import { useEffect, useId, useRef, useState } from 'react';
import type { JsonValue, TextSpec } from '../types';
import type { SaveOutcome } from '../hooks/settings';
import type { Field } from '../lib/schema';
import {
  fieldLabel,
  fieldHelp,
  matches,
  optionsFor,
  optionLabel,
  placeholderFor,
  rangeOptions,
} from '../lib/schema';
import { msg, resolve } from '../lib/i18n';
import { Button, Icon, Row, Switch, Text } from './ui';
import { toast } from './overlays';
import { ChoiceRow } from './choices';
export function QueryError({ error, retry }: { error: string; retry: () => void }) {
  return error ? (
    <div role="alert" className="ds-field__error">
      {error}
      <Button variant="quiet" onClick={retry}>
        <Text value={msg('ui.retry', '重试')} />
      </Button>
    </div>
  ) : null;
}
export function FieldInput({
  label,
  value,
  onChange,
  secret = false,
  multiline = false,
  placeholder = '',
  disabled = false,
  action,
}: {
  label: TextSpec;
  value: string;
  onChange: (value: string) => void;
  secret?: boolean;
  multiline?: boolean;
  placeholder?: string;
  disabled?: boolean;
  action?: string;
}) {
  const id = useId();
  const [reveal, setReveal] = useState(false);
  const common = {
    id,
    className: multiline ? 'ds-textarea' : 'ds-input',
    value,
    onChange: (event: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) =>
      onChange(event.target.value),
    placeholder,
    disabled,
    autoComplete: 'off',
    spellCheck: false,
    'data-action': action,
  };
  return (
    <div className="ds-form-card">
      <label className="ds-form-card__title" htmlFor={id}>
        <Text value={label} />
      </label>
      <div className={secret ? 'ds-input-wrap' : undefined}>
        {multiline ? (
          <textarea {...common} rows={8} />
        ) : (
          <input
            {...common}
            className={`ds-input ${secret ? 'ds-input--with-toggle' : ''}`}
            type={secret && !reveal ? 'password' : 'text'}
          />
        )}
        {secret && !multiline && (
          <button
            type="button"
            className="ds-input__toggle"
            onClick={() => setReveal(!reveal)}
            aria-label={resolve(
              reveal ? msg('model.hide_key', '隐藏密钥') : msg('model.show_key', '显示密钥'),
            )}
            aria-pressed={reveal}
          >
            <Icon name={reveal ? 'eyeOff' : 'eye'} size={18} />
          </button>
        )}
      </div>
    </div>
  );
}

export function ValueField({
  label,
  saved,
  onCommit,
  parse = (value) => value,
  description,
  secret = false,
  multiline = false,
  placeholder = '',
  action,
  suggestions,
  inline = false,
  wide = false,
  inputMode,
}: {
  label: TextSpec;
  saved: unknown;
  onCommit: (value: JsonValue) => Promise<SaveOutcome | void>;
  parse?: (value: string) => JsonValue | undefined;
  description?: TextSpec;
  secret?: boolean;
  multiline?: boolean;
  placeholder?: string;
  action?: string;
  suggestions?: string[];
  inline?: boolean;
  wide?: boolean;
  inputMode?: React.HTMLAttributes<HTMLInputElement>['inputMode'];
}) {
  const id = useId();
  const input = useRef<HTMLInputElement>(null);
  const [draft, setDraft] = useState<string | null>(null);
  const [error, setError] = useState('');
  const sequence = useRef(0);
  const staged = useRef(false);
  const confirmed = secret ? '' : Array.isArray(saved) ? saved.join('\n') : String(saved ?? '');
  useEffect(() => {
    setDraft((current) => {
      if (current === confirmed) {
        staged.current = false;
        return null;
      }
      return current;
    });
  }, [confirmed]);
  const text = draft ?? confirmed;
  async function commit() {
    if (draft === null || (text === confirmed && !staged.current) || (secret && !text)) return;
    const value = parse(text.trim());
    if (value === undefined) {
      setError(resolve(msg('config.invalid_value', '无效值')));
      return;
    }
    const id = ++sequence.current;
    setError('');
    const submitted = text;
    try {
      const result = await onCommit(value);
      if (sequence.current === id) staged.current = result === 'staged';
      if (sequence.current === id && result !== 'staged' && result !== 'superseded') {
        if (result === 'failed' || result === 'invalid')
          setError(resolve(msg('basic.save_failed', '保存失败')));
        else setDraft((current) => (current === submitted ? null : current));
      }
    } catch (error) {
      setError(error instanceof Error ? error.message : String(error));
    }
  }
  const common = {
    id,
    className: inline
      ? `ds-row__input ${wide ? 'ds-row__input--wide' : ''}`
      : multiline
        ? 'ds-textarea'
        : 'ds-input',
    value: text,
    onChange: (event: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => {
      sequence.current++;
      setDraft(event.target.value);
    },
    onBlur: () => void commit(),
    placeholder,
    autoComplete: 'off',
    spellCheck: false,
    'data-action': action,
  };
  const control = multiline ? (
    <textarea {...common} rows={6} />
  ) : (
    <input
      {...common}
      ref={input}
      aria-label={resolve(label)}
      inputMode={inputMode}
      type={secret ? 'password' : 'text'}
      list={suggestions ? `${id}-suggestions` : undefined}
      onKeyDown={(event) => {
        if (event.key === 'Enter' && !event.nativeEvent.isComposing) event.currentTarget.blur();
      }}
    />
  );
  const options = suggestions && (
    <datalist id={`${id}-suggestions`}>
      {suggestions.map((value) => (
        <option key={value} value={value} />
      ))}
    </datalist>
  );
  return inline ? (
    <div
      onClick={(event) => {
        if (event.target !== input.current) input.current?.focus();
      }}
    >
      <Row label={label} description={description} className="ds-row--editable">
        {control}
      </Row>
      {options}
      {error && (
        <p role="alert" className="ds-field__error">
          {error}
        </p>
      )}
    </div>
  ) : (
    <div className="ds-form-card">
      <label htmlFor={id} className="ds-form-card__title">
        <Text value={label} />
      </label>
      {description && <Text value={description} className="ds-form-card__help" />}
      {control}
      {options}
      {error && (
        <p role="alert" className="ds-field__error">
          {error}
        </p>
      )}
    </div>
  );
}

export function ConfigField({
  section,
  field,
  value,
  ctx = {},
  type = '',
  save,
  label: labelOverride,
  description,
}: {
  section: string;
  field: Field;
  value: unknown;
  ctx?: Record<string, unknown>;
  type?: string;
  save: (value: JsonValue) => Promise<SaveOutcome | void>;
  label?: TextSpec;
  description?: TextSpec;
}) {
  const [draft, setDraft] = useState<JsonValue | undefined>(undefined);
  const [busy, setBusy] = useState(false);
  const label = labelOverride ?? fieldLabel(section, field),
    help = description ?? fieldHelp(section, field);
  useEffect(
    () => setDraft((current) => (Object.is(current, value) ? undefined : current)),
    [value],
  );
  const shown = draft ?? value ?? field.default;
  async function choose(next: JsonValue) {
    if (busy) return;
    setDraft(next);
    setBusy(true);
    try {
      const result = await save(next);
      if (result !== 'staged') setDraft(undefined);
    } catch (error) {
      toast(error instanceof Error ? error.message : String(error));
      setDraft(undefined);
    } finally {
      setBusy(false);
    }
  }
  if (field.widget === 'boolean')
    return (
      <Row label={label} description={help}>
        <Switch
          label={label}
          checked={!!shown}
          disabled={busy}
          onChange={(next) => void choose(next)}
        />
      </Row>
    );
  const options = field.range ? rangeOptions(field) : optionsFor(field, type);
  if (
    field.widget === 'select' ||
    field.range ||
    (field.selectWhen && matches(field.selectWhen, ctx))
  )
    return (
      <ChoiceRow
        label={label}
        selected={String(shown ?? '')}
        options={options.map((option) => ({
          value: String(option.value),
          label: optionLabel(section, field.key, option),
        }))}
        disabled={busy}
        action={`open-${section}.${field.key}`}
        onSelect={(next) =>
          choose(
            field.widget === 'number' || typeof field.default === 'number' ? Number(next) : next,
          )
        }
      />
    );
  return (
    <ValueField
      label={label}
      description={help}
      inline={!field.secret && !['textarea', 'list'].includes(field.widget || '')}
      wide={field.widget !== 'number'}
      secret={field.secret}
      saved={value}
      multiline={field.widget === 'textarea' || field.widget === 'list'}
      placeholder={placeholderFor(field, ctx)}
      action={`edit-${section}.${field.key}`}
      parse={(value) =>
        field.widget === 'number'
          ? value === '' && field.nullable
            ? null
            : value !== '' && Number.isFinite(Number(value))
              ? Number(value)
              : undefined
          : field.widget === 'list'
            ? value.split('\n').filter(Boolean)
            : value
      }
      onCommit={save}
    />
  );
}
