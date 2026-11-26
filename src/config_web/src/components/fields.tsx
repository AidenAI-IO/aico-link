import { useEffect, useId, useRef, useState } from 'react';
import type { JsonValue, TextSpec } from '../types';
import type { SaveOutcome } from '../hooks/settings';
import type { Field } from '../lib/schema';
import type { ModelOption } from '../state/model-history';
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
  suggestions?: ModelOption[];
  inline?: boolean;
  wide?: boolean;
  inputMode?: React.HTMLAttributes<HTMLInputElement>['inputMode'];
}) {
  const id = useId();
  const input = useRef<HTMLInputElement>(null);
  const [draft, setDraft] = useState<string | null>(null);
  const [error, setError] = useState('');
  const [showSuggestions, setShowSuggestions] = useState(false);
  const [activeSuggestion, setActiveSuggestion] = useState(-1);
  const sequence = useRef(0);
  const pending = useRef(0);
  const submission = useRef<{ text: string } | null>(null);
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
  const query = text.trim().toLowerCase();
  const matches = (suggestions || []).filter((entry) =>
    `${entry.id} ${entry.name || ''}`.toLowerCase().includes(query),
  );
  const expanded = showSuggestions && matches.length > 0;
  const active = expanded ? matches[activeSuggestion] : undefined;
  useEffect(() => {
    if (active)
      document
        .getElementById(`${id}-option-${activeSuggestion}`)
        ?.scrollIntoView?.({ block: 'nearest' });
  }, [active, activeSuggestion, id]);
  function closeSuggestions() {
    setShowSuggestions(false);
    setActiveSuggestion(-1);
  }
  async function commit(submitted = text) {
    if (
      submission.current?.text === submitted ||
      (draft === null && submitted === confirmed) ||
      (submitted === confirmed && !staged.current && pending.current === 0) ||
      (secret && !submitted)
    )
      return;
    const value = parse(submitted.trim());
    if (value === undefined) {
      setError(resolve(msg('config.invalid_value', '无效值')));
      return;
    }
    const id = ++sequence.current;
    const attempt = { text: submitted };
    submission.current = attempt;
    pending.current++;
    setError('');
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
    } finally {
      pending.current--;
      if (submission.current === attempt) submission.current = null;
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
      setShowSuggestions(true);
      setActiveSuggestion(-1);
    },
    onBlur: () => {
      closeSuggestions();
      void commit();
    },
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
      role={suggestions ? 'combobox' : undefined}
      aria-autocomplete={suggestions ? 'list' : undefined}
      aria-expanded={suggestions ? expanded : undefined}
      aria-controls={suggestions ? `${id}-suggestions` : undefined}
      aria-activedescendant={active ? `${id}-option-${activeSuggestion}` : undefined}
      onFocus={() => setShowSuggestions(true)}
      onKeyDown={(event) => {
        if (event.nativeEvent.isComposing) return;
        if (suggestions && ['ArrowDown', 'ArrowUp'].includes(event.key) && matches.length) {
          event.preventDefault();
          setShowSuggestions(true);
          setActiveSuggestion(
            (expanded ? activeSuggestion : -1) < 0
              ? event.key === 'ArrowDown'
                ? 0
                : matches.length - 1
              : (activeSuggestion + (event.key === 'ArrowDown' ? 1 : -1) + matches.length) %
                  matches.length,
          );
        } else if (event.key === 'Escape' && expanded) {
          event.preventDefault();
          event.stopPropagation();
          closeSuggestions();
        } else if (event.key === 'Enter') {
          event.preventDefault();
          if (active) chooseSuggestion(active.id);
          else event.currentTarget.blur();
        }
      }}
    />
  );
  function chooseSuggestion(value: string) {
    sequence.current++;
    setDraft(value);
    closeSuggestions();
    void commit(value);
  }
  const options = suggestions && (
    <div
      id={`${id}-suggestions`}
      className="ds-suggestions"
      role="listbox"
      aria-label={resolve(label)}
      hidden={!expanded}
    >
      {matches.map((entry, index) => (
        <button
          key={entry.id}
          id={`${id}-option-${index}`}
          type="button"
          role="option"
          tabIndex={-1}
          aria-selected={index === activeSuggestion}
          className={`ds-suggestion ${index === activeSuggestion ? 'ds-suggestion--active' : ''}`}
          onMouseDown={(event) => event.preventDefault()}
          onClick={() => chooseSuggestion(entry.id)}
        >
          <span className="ds-suggestion__name">{entry.name || entry.id}</span>
          {entry.name && entry.name !== entry.id && (
            <span className="ds-suggestion__id">{entry.id}</span>
          )}
        </button>
      ))}
    </div>
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
  suggestions,
}: {
  section: string;
  field: Field;
  value: unknown;
  ctx?: Record<string, unknown>;
  type?: string;
  save: (value: JsonValue) => Promise<SaveOutcome | void>;
  label?: TextSpec;
  description?: TextSpec;
  suggestions?: ModelOption[];
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
  const options = field.range
    ? rangeOptions(field)
    : suggestions
      ? suggestions.map((entry) => ({ value: entry.id, label: entry.name || entry.id }))
      : optionsFor(field, type);
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
      suggestions={suggestions}
    />
  );
}
