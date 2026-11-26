import type { TextSpec } from '../types';
import { messages } from './messages';
const KEY = 'aico.config.locale';
let locale =
  typeof navigator !== 'undefined' && navigator.language?.toLowerCase().startsWith('zh')
    ? 'zh-CN'
    : 'en-US';
try {
  locale = localStorage.getItem(KEY) || locale;
} catch {
  /* Storage can be unavailable in WebViews. */
}
let revision = 0;
export const getActiveLocale = () => locale;
export const getLocaleRevision = () => revision;
export const msg = (key: string, fallback = ''): TextSpec => ({ key, fallback });
export function t(key: string, params: Record<string, unknown> = {}): string {
  const template =
    messages[locale]?.[key] ?? messages['en-US'][key] ?? String(params.defaultValue ?? key);
  return template.replace(/\{\{\s*(\w+)\s*\}\}/g, (_, name: string) => String(params[name] ?? ''));
}
export function resolve(value?: TextSpec | null): string {
  return typeof value === 'string'
    ? value
    : value
      ? t(value.key, { defaultValue: value.fallback || '' })
      : '';
}
export function applyLocale(value: string, remember = false) {
  const next = value.toLowerCase().startsWith('zh') ? 'zh-CN' : 'en-US';
  const changed = locale !== next;
  locale = next;
  if (remember) {
    try {
      localStorage.setItem(KEY, next);
    } catch {
      /* Optional preference cache. */
    }
  }
  document.documentElement.lang = next;
  if (changed) {
    revision++;
    document.dispatchEvent(new CustomEvent('aico:locale-changed'));
  }
}
export function initI18n() {
  document.documentElement.lang = locale;
}
