import type { ApplicationData, Transport } from '../types';

export class APIError extends Error {
  status = 0;
  error = '';
  code = '';
  persisted = false;
  config?: ApplicationData['config'];
  apply?: string;
}
export function apiError(value: unknown): APIError {
  if (value instanceof APIError) return value;
  if (value instanceof Error) return Object.assign(new APIError(value.message), value);
  if (value && typeof value === 'object') {
    const data = value as Record<string, unknown>;
    return Object.assign(
      new APIError(String(data.message || data.error || data.code || 'Request failed')),
      data,
    );
  }
  return new APIError(String(value));
}
let writes: Promise<unknown> = Promise.resolve();
export async function performRequest<T>(path: string, options?: RequestInit): Promise<T> {
  const response = await fetch(path, options);
  const source = await response.text();
  let body: Record<string, unknown>;
  try {
    const parsed: unknown = source ? JSON.parse(source) : {};
    body =
      parsed && typeof parsed === 'object' && !Array.isArray(parsed)
        ? (parsed as Record<string, unknown>)
        : {};
  } catch {
    body = { error: source || response.statusText };
  }
  const write =
    ['/api/config', '/api/config/locale', '/api/config/backup'].includes(path) &&
    options?.method &&
    options.method !== 'GET';
  if (write && (response.ok || body.persisted))
    window.dispatchEvent(new CustomEvent('aico:config-saved', { detail: body }));
  if (!response.ok) {
    if (response.status === 401 && body.error === 'login_required') {
      const next = window.location.pathname + window.location.search + window.location.hash;
      window.location.assign('/login?next=' + encodeURIComponent(next));
    }
    throw Object.assign(new APIError(String(body.error || `HTTP ${response.status}`)), body, {
      status: response.status,
    });
  }
  return body as T;
}
export function request<T = Record<string, unknown>>(
  path: string,
  options?: RequestInit,
): Promise<T> {
  if (
    ['/api/config', '/api/config/locale', '/api/config/backup'].includes(path) &&
    options?.method &&
    options.method !== 'GET'
  ) {
    const next = writes.then(() => performRequest<T>(path, options));
    writes = next.catch(() => {});
    return next;
  }
  return performRequest<T>(path, options);
}
export const transport: Transport = request;
export const json = (body: unknown): Pick<RequestInit, 'headers' | 'body'> => ({
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify(body),
});
