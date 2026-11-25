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
const WRITE_TIMEOUT_MS = 30000;
export async function performRequest<T>(
  path: string,
  options?: RequestInit,
  timeoutMs = 0,
): Promise<T> {
  const controller = timeoutMs ? new AbortController() : null;
  const upstream = options?.signal;
  const relayAbort = () => controller?.abort(upstream?.reason);
  if (upstream?.aborted) relayAbort();
  else upstream?.addEventListener('abort', relayAbort, { once: true });
  let timer: ReturnType<typeof setTimeout> | undefined;
  const send = async () => {
    const response = await fetch(
      path,
      controller ? { ...options, signal: controller.signal } : options,
    );
    const source = await response.text();
    if (controller?.signal.aborted) throw new APIError('Request aborted');
    return { response, source };
  };
  let response: Response;
  let source: string;
  try {
    ({ response, source } = await (timeoutMs
      ? Promise.race([
          send(),
          new Promise<never>((_, reject) => {
            timer = setTimeout(() => {
              controller?.abort();
              reject(Object.assign(new APIError('Request timed out'), { code: 'request_timeout' }));
            }, timeoutMs);
          }),
        ])
      : send()));
  } finally {
    if (timer) clearTimeout(timer);
    upstream?.removeEventListener('abort', relayAbort);
  }
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
  timeoutMs = 0,
): Promise<T> {
  if (
    ['/api/config', '/api/config/locale', '/api/config/backup'].includes(path) &&
    options?.method &&
    options.method !== 'GET'
  ) {
    const next = writes.then(() => performRequest<T>(path, options, WRITE_TIMEOUT_MS));
    writes = next.catch(() => {});
    return next;
  }
  return performRequest<T>(path, options, timeoutMs);
}
export const transport: Transport = request;
export const json = (body: unknown): Pick<RequestInit, 'headers' | 'body'> => ({
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify(body),
});
