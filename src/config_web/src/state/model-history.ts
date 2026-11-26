import { atom, type Atom, type createStore } from 'jotai/vanilla';
import type { Configuration } from '../types';

export interface ModelOption {
  id: string;
  name?: string;
}
type History = Record<string, string[]>;
const object = (value: unknown): Record<string, unknown> =>
  value && typeof value === 'object' && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};

/** Provider records and custom endpoints have independent model namespaces. */
export function modelScope(section: string, provider: string, record: Record<string, unknown>) {
  return JSON.stringify([
    section,
    provider,
    record.type || '',
    record.base_url || '',
    record.endpoint || '',
    record.upstream_provider || '',
  ]);
}

export function withModelHistory(catalog: ModelOption[], history: string[] = []): ModelOption[] {
  const options = new Map(catalog.map((entry) => [entry.id, entry]));
  for (const id of history) if (!options.has(id)) options.set(id, { id });
  return [...options.values()];
}

/** Capture confirmed configurations only; drafts and rejected saves never enter history. */
export function createModelHistory(
  store: ReturnType<typeof createStore>,
  config: Atom<Configuration | null>,
  profile: string,
) {
  const key = `aico.config.model-history.v1.${profile}`;
  function read(): History {
    try {
      const value: unknown = JSON.parse(localStorage.getItem(key) || '{}');
      return Object.fromEntries(
        Object.entries(object(value)).map(([scope, ids]) => [
          scope,
          Array.isArray(ids)
            ? [
                ...new Set(
                  ids
                    .filter((id): id is string => typeof id === 'string' && !!id.trim())
                    .map((id) => id.trim()),
                ),
              ]
            : [],
        ]),
      );
    } catch {
      return {};
    }
  }
  const history = atom<History>(read());
  const unsubscribe = store.sub(config, () => {
    const confirmed = store.get(config);
    if (!confirmed) return;
    const previous = store.get(history);
    let next = previous;
    function remember(
      section: string,
      provider: string,
      record: Record<string, unknown>,
      model: unknown,
    ) {
      if (!provider || typeof model !== 'string' || !model.trim()) return;
      const id = model.trim();
      const scope = modelScope(section, provider, record);
      const ids = next[scope] || [];
      if (!ids.includes(id)) next = { ...next, [scope]: [...ids, id] };
    }
    const main = confirmed.model;
    if (main?.provider)
      remember(
        'model',
        main.provider,
        confirmed.model_providers?.[main.provider] || {},
        main.model,
      );
    const decision = object(confirmed.decision);
    remember('decision', String(decision.provider || 'typesafe'), decision, decision.model);
    for (const section of ['voice_model_providers', 'stt_providers', 'tts_providers']) {
      for (const [name, value] of Object.entries(object(confirmed[section]))) {
        const record = object(value);
        remember(section, name, record, record.model);
      }
    }
    if (next === previous) return;
    store.set(history, next);
    try {
      localStorage.setItem(key, JSON.stringify(next));
    } catch {
      // Storage restrictions must not prevent configuration saves or in-memory history.
    }
  });
  return { atom: history, dispose: unsubscribe };
}
