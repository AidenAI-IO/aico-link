import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';
import { useAtomValue } from 'jotai';
import type { DeviceState } from '../state/device';
export function useQuery<T>(
  load: (signal: AbortSignal) => Promise<T>,
  dependencies: readonly unknown[],
  interval = 0,
) {
  const source = useRef(load);
  useLayoutEffect(() => {
    source.current = load;
  }, [load]);
  const identity = useRef({ dependencies, id: 0 });
  if (
    dependencies.length !== identity.current.dependencies.length ||
    dependencies.some((value, index) => !Object.is(value, identity.current.dependencies[index]))
  )
    identity.current = { dependencies, id: identity.current.id + 1 };
  const id = identity.current.id;
  const [state, setState] = useState<{
    id: number;
    data: T | null;
    error: string;
    loading: boolean;
  }>({ id, data: null, error: '', loading: true });
  const [version, setVersion] = useState(0);
  useEffect(() => {
    let disposed = false,
      resume = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let controller: AbortController | null = null;
    async function refresh() {
      if (disposed || controller || document.hidden) return;
      const request = new AbortController();
      controller = request;
      const deadline = setTimeout(() => request.abort(), 20000);
      try {
        const data = await source.current(request.signal);
        if (!disposed && !request.signal.aborted) setState({ id, data, error: '', loading: false });
      } catch (error) {
        if (!disposed && (!request.signal.aborted || !document.hidden))
          setState((previous) => ({
            id,
            data: previous.id === id ? previous.data : null,
            error: error instanceof Error ? error.message : String(error),
            loading: false,
          }));
      } finally {
        clearTimeout(deadline);
        controller = null;
        if (!disposed) {
          if (resume && !document.hidden) {
            resume = false;
            void refresh();
          } else if (interval) timer = setTimeout(refresh, interval);
        }
      }
    }
    const activate = () => {
      clearTimeout(timer);
      if (document.hidden) controller?.abort();
      else if (controller) resume = true;
      else void refresh();
    };
    void refresh();
    document.addEventListener('visibilitychange', activate);
    window.addEventListener('online', activate);
    return () => {
      disposed = true;
      controller?.abort();
      clearTimeout(timer);
      document.removeEventListener('visibilitychange', activate);
      window.removeEventListener('online', activate);
    };
  }, [id, version, interval]);
  const retry = useCallback(() => setVersion((value) => value + 1), []);
  return { ...(state.id === id ? state : { id, data: null, error: '', loading: true }), retry };
}
export function useSchema(model: DeviceState) {
  const resource = model.resources.schema;
  const state = useAtomValue(resource.view);
  useEffect(() => {
    const read = () => {
      if (!document.hidden && !model.store.get(resource.atom).data) void resource.refresh();
    };
    read();
    document.addEventListener('visibilitychange', read);
    window.addEventListener('online', read);
    return () => {
      document.removeEventListener('visibilitychange', read);
      window.removeEventListener('online', read);
    };
  }, [model, resource]);
  return {
    data: state.data,
    error: state.error || '',
    loading: state.data === null && !state.error,
    retry: useCallback(() => void resource.refresh(), [resource]),
  };
}
