import { atom } from 'jotai/vanilla';
import type { createStore } from 'jotai/vanilla';

export interface ResourceState<T> {
  data: T | null;
  error: string | null;
  stale: boolean;
  busy: boolean;
  updatedAt: number | null;
}
interface ResourceOptions<T> {
  read: (signal: AbortSignal) => Promise<T>;
  interval?: number;
  timeout?: number;
  accept?: (next: T, previous: T | null) => boolean;
  observeDuringWrite?: boolean;
}

/** One request owner per resource, independent of the number of subscribers. */
export function createResource<T>(
  store: ReturnType<typeof createStore>,
  {
    read,
    interval = 5000,
    timeout = 8000,
    accept = () => true,
    observeDuringWrite = false,
  }: ResourceOptions<T>,
) {
  const state = atom<ResourceState<T>>({
    data: null,
    error: null,
    stale: true,
    busy: false,
    updatedAt: null,
  });
  let epoch = 0;
  let pending: Promise<void> | null = null;
  let controller: AbortController | null = null;
  let timer: ReturnType<typeof setTimeout> | null = null;
  let deadline: ReturnType<typeof setTimeout> | null = null;
  let rejectCancelled: ((reason: Error) => void) | null = null;
  let active = false;
  let writing = false;
  const equal = (a: unknown, b: unknown) => JSON.stringify(a) === JSON.stringify(b);
  const update = (patch: Partial<ResourceState<T>>) => {
    const previous = store.get(state);
    const next = { ...previous, ...patch };
    if (patch.data && equal(previous.data, patch.data)) next.data = previous.data;
    store.set(state, next);
  };
  function cancel() {
    epoch++;
    controller?.abort();
    rejectCancelled?.(new Error('Status request cancelled'));
    rejectCancelled = null;
    controller = null;
    pending = null;
    if (deadline) clearTimeout(deadline);
    if (timer) clearTimeout(timer);
  }
  function schedule() {
    if (timer) clearTimeout(timer);
    if (active && (!writing || observeDuringWrite)) timer = setTimeout(refresh, interval);
  }
  function publish(data: T) {
    if (!data || !accept(data, store.get(state).data)) return false;
    cancel();
    update({ data, error: null, stale: false, updatedAt: Date.now() });
    schedule();
    return true;
  }
  function refresh(): Promise<void> {
    if (pending || (writing && !observeDuringWrite)) return pending || Promise.resolve();
    if (timer) clearTimeout(timer);
    const current = ++epoch;
    const abort = new AbortController();
    controller = abort;
    // Reject even when a mocked or faulty transport ignores AbortSignal.
    const expiry = new Promise<never>((_, reject) => {
      rejectCancelled = reject;
      deadline = setTimeout(() => {
        abort.abort();
        reject(new Error('Status request timed out'));
      }, timeout);
    });
    pending = Promise.race([Promise.resolve().then(() => read(abort.signal)), expiry])
      .then((data) => {
        if (current !== epoch || abort.signal.aborted || !accept(data, store.get(state).data))
          return;
        update({ data, error: null, stale: false, updatedAt: Date.now() });
      })
      .catch((error) => {
        if (current === epoch) update({ error: error.message || String(error), stale: true });
      })
      .finally(() => {
        if (current !== epoch) return;
        if (deadline) clearTimeout(deadline);
        rejectCancelled = null;
        controller = pending = null;
        schedule();
      });
    return pending;
  }
  async function mutate<R = T>(
    send: () => Promise<R>,
    select: (value: R) => T | null = (value) => value as unknown as T,
  ): Promise<R> {
    if (writing) throw new Error('An operation is already in progress');
    cancel();
    writing = true;
    update({ busy: true });
    // Radio observations remain useful while a long connection task is pending.
    // Its final publication still cancels reads begun before task completion.
    if (active && observeDuringWrite) void refresh();
    try {
      const result = await send();
      const data = select(result);
      if (data) publish(data);
      return result;
    } finally {
      writing = false;
      update({ busy: false });
      // The operation can complete after the page is left; keep its result but
      // only run the follow-up read while this resource still has subscribers.
      if (active) void refresh();
    }
  }
  return {
    atom: state,
    get epoch() {
      return epoch;
    },
    refresh,
    publish,
    mutate,
    setActive(value: boolean) {
      if (active === value) return;
      active = value;
      if (value) void refresh();
      else cancel();
    },
    dispose() {
      active = false;
      cancel();
    },
  };
}

export type Resource<T> = ReturnType<typeof createResource<T>>;
