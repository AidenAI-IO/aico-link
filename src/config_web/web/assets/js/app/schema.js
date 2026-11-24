/**
 * Config schema access for schema-driven pages.
 *
 * `GET /api/config/schema` is the Agent's own field catalogue (config_meta.go):
 * widgets, enums, per-provider option scoping, defaults, placeholders and the
 * `visibleWhen` rules that decide which fields a provider type has. Pages that
 * render provider records read it instead of keeping per-type field lists, so a
 * provider added in Go shows up here without a JavaScript change.
 */

import { request, t } from './data.js';
import { getActiveLocale } from '../config/i18n.js';
import { msg } from '../ui/text.js';

let pending = null;

/** The schema, fetched once per page load. */
export function loadSchema() {
  if (!pending) {
    pending = request('/api/config/schema').catch((error) => {
      pending = null;
      throw error;
    });
  }
  return pending;
}

/** Fields of one schema section, e.g. `tts_providers`. */
export function sectionFields(schema, name) {
  const section = ((schema && schema.sections) || []).find((entry) => entry.name === name);
  return (section && section.fields) || [];
}

/**
 * Evaluate a `visibleWhen`/`when` rule against `ctx`, a map of dotted field
 * paths (`tts_providers.type`) to values. Mirrors the classic page's evaluator
 * (config-meta.js `evalRule`): `all` or `any` of `eq`, `ne`, `in`, `notIn` and
 * `truthy`. Its `providerType` op resolves main-model provider records, which
 * no record section here uses, so it is treated as not matching.
 */
export function matches(rule, ctx) {
  if (!rule) return true;
  const test = (condition) => {
    const value = ctx[condition.field];
    const text = value == null ? '' : String(value);
    switch (condition.op) {
      case 'eq':
        return text === String(condition.value ?? '');
      case 'ne':
        return text !== String(condition.value ?? '');
      case 'in':
        return (condition.values || []).map(String).includes(text);
      case 'notIn':
        return !(condition.values || []).map(String).includes(text);
      case 'truthy':
        return Boolean(value);
      case 'providerType':
        return false;
      default:
        return true;
    }
  };
  if (rule.all) return rule.all.every(test);
  if (rule.any) return rule.any.some(test);
  return true;
}

/** The placeholder a field shows for `ctx`, falling back to its default. */
export function placeholderFor(field, ctx) {
  for (const entry of field.placeholderWhen || []) {
    if (matches(entry.when, ctx)) return String(entry.value);
  }
  if (field.placeholder) return field.placeholder;
  return field.default == null || field.default === '' ? '' : String(field.default);
}

/** Enum options offered to a provider type. */
export function optionsFor(field, type) {
  return (field.enum || []).filter(
    (option) =>
      (!option.providers || !option.providers.length || option.providers.includes(type)) &&
      !(option.excludeProviders || []).includes(type),
  );
}

/** Options for a `range` select such as speech speed: min..max by step. */
export function rangeOptions(field) {
  const { min, max, step, precision } = field.range || {};
  if (!(step > 0) || max < min) return [];
  const values = [];
  for (let value = min; value <= max + step / 2; value += step)
    values.push(Number(value.toFixed(precision ?? 2)));
  return values.map((value) => ({ value, label: String(value) }));
}

/* The classic page's catalogue already translates these; reuse its keys. */

export function fieldLabel(section, field) {
  return msg(`config.fields.${section}.${field.key}.label`, field.label || field.key);
}

/**
 * A field's help line. The schema's own help is English, so outside English it
 * is shown only when the catalogue translates it; an English line under a
 * Chinese label reads as a defect rather than as help.
 */
export function fieldHelp(section, field) {
  if (!field.help) return null;
  const key = `config.fields.${section}.${field.key}.help`;
  if (t(key, { defaultValue: '' })) return msg(key, field.help);
  return String(getActiveLocale() || '').startsWith('en') ? field.help : null;
}

export function optionLabel(section, key, option, fallback) {
  const suffix =
    String(option.value ?? '')
      .trim()
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, '_')
      .replace(/^_+|_+$/g, '') || 'default';
  return msg(
    `config.fields.${section}.${key}.options.${suffix}`,
    option.label || fallback || String(option.value),
  );
}
