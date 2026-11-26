import type { JsonValue, TextSpec } from '../types';
import { getActiveLocale, msg, t } from './i18n';
export interface Condition {
  field: string;
  op: string;
  value?: string;
  values?: string[];
}
export interface Rule {
  all?: Condition[];
  any?: Condition[];
}
export interface Option {
  value: string;
  label?: string;
  providers?: string[];
  excludeProviders?: string[];
}
export interface Field {
  key: string;
  label?: string;
  help?: string;
  widget?: string;
  enum?: Option[];
  range?: { min: number; max: number; step: number; precision?: number };
  default?: JsonValue;
  placeholder?: string;
  placeholderWhen?: { when: Rule; value: unknown }[];
  secret?: boolean;
  nullable?: boolean;
  advanced?: boolean;
  visibleWhen?: Rule;
  selectWhen?: Rule;
}
export interface Schema {
  sections: { name: string; fields: Field[] }[];
}
export const sectionFields = (schema: Schema | null | undefined, name: string): Field[] =>
  schema?.sections.find((section) => section.name === name)?.fields || [];
export function matches(rule: Rule | undefined, ctx: Record<string, unknown>): boolean {
  if (!rule) return true;
  const test = (condition: Condition) => {
    const value = ctx[condition.field],
      text = String(value ?? '');
    switch (condition.op) {
      case 'eq':
        return text === String(condition.value ?? '');
      case 'ne':
        return text !== String(condition.value ?? '');
      case 'in':
        return (condition.values || []).includes(text);
      case 'notIn':
        return !(condition.values || []).includes(text);
      case 'truthy':
        return !!value;
      case 'providerType':
        return ctx[condition.field + '.__type'] === condition.value;
      default:
        return true;
    }
  };
  return rule.all ? rule.all.every(test) : rule.any ? rule.any.some(test) : true;
}
export function placeholderFor(field: Field, ctx: Record<string, unknown>): string {
  for (const entry of field.placeholderWhen || [])
    if (matches(entry.when, ctx)) return String(entry.value ?? '');
  return field.placeholder || String(field.default ?? '');
}
export const optionsFor = (field: Field, type: string) =>
  (field.enum || []).filter(
    (option) =>
      (!option.providers?.length || option.providers.includes(type)) &&
      !option.excludeProviders?.includes(type),
  );
export function rangeOptions(field: Field) {
  const range = field.range;
  if (!range || range.step <= 0 || range.max < range.min) return [];
  const values = [];
  for (let value = range.min; value <= range.max + range.step / 2; value += range.step)
    values.push({
      value: String(Number(value.toFixed(range.precision ?? 2))),
      label: String(Number(value.toFixed(range.precision ?? 2))),
    });
  return values;
}
export const fieldLabel = (section: string, field: Field): TextSpec =>
  msg(`config.fields.${section}.${field.key}.label`, field.label || field.key);
export function fieldHelp(section: string, field: Field): TextSpec | undefined {
  const key = `config.fields.${section}.${field.key}.help`;
  if (!field.help) return;
  if (t(key, { defaultValue: '' })) return msg(key, field.help);
  return getActiveLocale().startsWith('en') ? field.help : undefined;
}
export function optionLabel(section: string, key: string, option: Option): TextSpec {
  const suffix =
    option.value
      .trim()
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, '_')
      .replace(/^_+|_+$/g, '') || 'default';
  return msg(`config.fields.${section}.${key}.options.${suffix}`, option.label || option.value);
}
