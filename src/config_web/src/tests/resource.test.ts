import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createStore } from 'jotai/vanilla';
import { createResource } from '../state/resource';
import { createDeviceState } from '../state/device';
import { applyLocale, getActiveLocale } from '../lib/i18n';

const deferred = () => {
  let resolve!: (value: unknown) => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<unknown>((yes, no) => {
    resolve = yes;
    reject = no;
  });
  return { promise, resolve, reject };
};
const flush = async () => {
  for (let i = 0; i < 12; i++) await Promise.resolve();
};
beforeEach(() => vi.useFakeTimers());
afterEach(() => {
  vi.clearAllTimers();
  vi.useRealTimers();
});

it('applies the saved device locale before publishing the first config snapshot', async () => {
  applyLocale('en-US', true);
  const model = createDeviceState(
    async () => ({ config: { agent: { locale: 'zh-CN' } } }) as never,
  );
  let localeAtPublish = '';
  model.store.sub(model.config, () => {
    localeAtPublish ||= getActiveLocale();
  });
  await model.start();
  expect(localeAtPublish).toBe('zh-CN');
  model.dispose();
  applyLocale('en-US', true);
});

describe('resource lifecycle', () => {
  it('observes radio state during a long task and rejects observations older than its final result', async () => {
    const task = deferred();
    const lateRead = deferred();
    const store = createStore();
    let reads = 0;
    const resource = createResource(store, {
      interval: 1000,
      observeDuringWrite: true,
      read: async () => (++reads < 3 ? { connected: reads === 2 } : lateRead.promise),
    });
    resource.setActive(true);
    await flush();
    const operation = resource.mutate(() => task.promise);
    await flush();
    expect(store.get(resource.atom)).toMatchObject({ data: { connected: true }, busy: true });
    await vi.advanceTimersByTimeAsync(1000);
    task.resolve({ connected: false });
    await operation;
    lateRead.resolve({ connected: true });
    // Pause the follow-up observation to inspect the confirmed task result.
    resource.setActive(false);
    await flush();
    expect(store.get(resource.atom)).toMatchObject({ data: { connected: false }, busy: false });
    resource.dispose();
  });
  it('shares an in-flight read and schedules the next one only after it settles', async () => {
    const read = deferred();
    const send = vi.fn(() => read.promise);
    const store = createStore();
    const resource = createResource(store, { read: send, interval: 1000, timeout: 10000 });
    resource.setActive(true);
    const one = resource.refresh();
    expect(resource.refresh()).toBe(one);
    await vi.advanceTimersByTimeAsync(3000);
    expect(send).toHaveBeenCalledTimes(1);
    read.resolve({ connected: true });
    await one;
    await vi.advanceTimersByTimeAsync(999);
    expect(send).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(send).toHaveBeenCalledTimes(2);
    resource.dispose();
  });
  it('a write invalidates a delayed read even when the transport ignores cancellation', async () => {
    const delayed = deferred();
    const store = createStore();
    const resource = createResource(store, { read: () => delayed.promise });
    const old = resource.refresh();
    await flush();
    await resource.mutate(async () => ({ active: false }));
    delayed.resolve({ active: true });
    await old;
    expect(store.get(resource.atom).data).toEqual({ active: false });
    expect(store.get(resource.atom).busy).toBe(false);
    resource.dispose();
  });
  it('pauses and cancels when hidden, and reads immediately when shown again', async () => {
    const delayed = deferred();
    const send = vi.fn(() => delayed.promise);
    const store = createStore();
    const resource = createResource(store, { read: send });
    resource.setActive(true);
    await flush();
    resource.setActive(false);
    delayed.resolve({ active: true });
    await flush();
    expect(store.get(resource.atom).data).toBe(null);
    await vi.advanceTimersByTimeAsync(15000);
    expect(send).toHaveBeenCalledTimes(1);
    resource.setActive(true);
    await flush();
    expect(send).toHaveBeenCalledTimes(2);
    resource.dispose();
  });
  it('times out, reports stale data, and recovers without losing the confirmed value', async () => {
    const store = createStore();
    const send = vi.fn(() => new Promise(() => {}));
    const resource = createResource(store, { read: send, timeout: 1000 });
    resource.publish({ active: true });
    const request = resource.refresh();
    await vi.advanceTimersByTimeAsync(1000);
    await request;
    expect(store.get(resource.atom)).toMatchObject({
      data: { active: true },
      stale: true,
      error: 'Status request timed out',
    });
    send.mockResolvedValue({ active: false });
    await resource.refresh();
    expect(store.get(resource.atom)).toMatchObject({
      data: { active: false },
      stale: false,
      error: null,
    });
    resource.dispose();
  });
  it('keeps the data identity on identical observations', async () => {
    const store = createStore();
    const resource = createResource(store, { read: async () => ({ active: true }) });
    await resource.refresh();
    const first = store.get(resource.atom).data;
    await resource.refresh();
    expect(store.get(resource.atom).data).toBe(first);
    resource.dispose();
  });
});

describe('device state', () => {
  it.each([false, true])(
    'retries canonical validation after a post-save read fails (container: %s)',
    async (container) => {
      let revision = 'one';
      let reads = 0;
      const canonical = deferred();
      let signal: AbortSignal | null | undefined;
      const model = createDeviceState(
        async (path, options) => {
          if (path === '/api/config/application')
            return { state: 'applied', revision, persisted_revision: revision };
          if (path === '/api/config') {
            reads++;
            if (reads === 2) throw new Error('temporary config read failure');
            if (reads === 3) {
              signal = options?.signal;
              return canonical.promise;
            }
            return { config: {}, revision, config_valid: true };
          }
          return {};
        },
        { container },
      );
      await model.start();
      await model.resources.application.refresh();
      revision = 'two';
      model.publishSave({ state: 'applied', config: {}, revision });
      await model.resources.snapshot.refresh();
      expect(model.store.get(model.resources.snapshot.atom).stale).toBe(true);
      expect(model.store.get(model.current).config_valid).toBeUndefined();
      await model.resources.application.refresh();
      await flush();
      expect(reads).toBe(3);
      for (let i = 0; i < 3; i++) await model.resources.application.refresh();
      expect(reads).toBe(3);
      expect(signal?.aborted).toBe(false);
      canonical.resolve({ config: {}, revision, config_valid: true, config_errors: [] });
      await model.resources.snapshot.refresh();
      expect(model.store.get(model.resources.snapshot.atom).stale).toBe(false);
      expect(model.store.get(model.current).config_valid).toBe(true);
      await model.resources.application.refresh();
      expect(reads).toBe(3);
      model.dispose();
    },
  );
  it.each([false, true])(
    'observes persisted edits independently of runtime (offline: %s)',
    async (offline) => {
      let revision = 'one';
      let failNextConfig = false;
      const send = vi.fn(async (path) => {
        if (path === '/api/config/application') {
          const status = { state: 'applied', revision: 'runtime', persisted_revision: revision };
          if (offline) throw Object.assign(new Error('offline'), status);
          return status;
        }
        if (failNextConfig) {
          failNextConfig = false;
          throw new Error('temporary config read failure');
        }
        return { config: { model: { model: revision } }, revision };
      });
      const model = createDeviceState(send);
      await model.start();
      await model.resources.application.refresh();
      revision = 'two';
      failNextConfig = true;
      await model.resources.application.refresh();
      await flush();
      expect(model.store.get(model.config)?.model?.model).toBe('one');
      await model.resources.application.refresh();
      await flush();
      expect(model.store.get(model.config)?.model?.model).toBe('two');
      await model.resources.application.refresh();
      expect(send.mock.calls.filter(([path]) => path === '/api/config')).toHaveLength(3);
      expect(model.store.get(model.resources.application.atom).stale).toBe(offline);
      model.dispose();
    },
  );
  it('reconciles canonical validation after a repair', async () => {
    let repaired = false;
    const send = vi.fn(async () => ({
      config: { agent: { input_mode: repaired ? 'stt' : 'invalid' } },
      revision: repaired ? 'two' : 'one',
      config_valid: repaired,
      config_errors: repaired ? [] : [{ message: 'invalid mode' }],
    }));
    const model = createDeviceState(send);
    await model.start();
    expect(model.store.get(model.current).config_valid).toBe(false);
    repaired = true;
    const saved = {
      config: { agent: { input_mode: 'stt' } },
      revision: 'two',
      state: 'applied',
    };
    model.publishSave(saved);
    model.publishSave(saved);
    await flush();
    expect(model.store.get(model.current)).toMatchObject({ config_valid: true, config_errors: [] });
    expect(send).toHaveBeenCalledTimes(2);
    model.dispose();
  });
  it('keeps OTA background status light and cancels log reads outside firmware routes', async () => {
    const send = vi.fn(async (_path: string) => ({ ota_update_running: false }));
    const model = createDeviceState(send);
    model.activate('/');
    await flush();
    expect(send.mock.calls.some(([path]) => path === '/api/ota/status')).toBe(false);
    model.activate('/firmware');
    await flush();
    expect(send.mock.calls.filter(([path]) => path === '/api/ota/status')).toHaveLength(1);
    model.activate('/voice');
    await vi.advanceTimersByTimeAsync(5000);
    expect(send.mock.calls.filter(([path]) => path === '/api/ota/status')).toHaveLength(1);
    expect(
      send.mock.calls.filter(([path]) => path === '/api/ota/status?logs=0').length,
    ).toBeGreaterThan(1);
    model.dispose();
  });
  it('ignores observation timestamps in views while enforcing their ordering', async () => {
    const model = createDeviceState(async () => ({}));
    const observation = (time: number, connected = true) => ({
      wifi_status: {
        connected,
        state: connected ? 'COMPLETED' : 'DISCONNECTED',
        observed_at_unix_us: time,
      },
    });
    model.publishWiFi(observation(100));
    const notified = vi.fn();
    const stop = model.store.sub(model.current, notified);
    model.publishWiFi(observation(200));
    expect(notified).not.toHaveBeenCalled();
    model.publishWiFi(observation(150, false));
    expect(notified).not.toHaveBeenCalled();
    await model.resources.wifi.refresh();
    expect(model.store.get(model.current).wifi_status?.state).toBe('UNKNOWN');
    model.publishWiFi(observation(300, false));
    expect(model.store.get(model.current).wifi_status?.state).toBe('DISCONNECTED');
    expect(notified).toHaveBeenCalledTimes(2);
    stop();
    model.dispose();
  });
  it.each([false, true])(
    'keeps board polling out of PC routes (container: %s)',
    async (container) => {
      const send = vi.fn(async (path) => {
        if (path === '/api/config/application') return { state: 'applied' };
        if (path === '/api/mcp/servers') return { servers: [] };
        if (path === '/api/device/iphone-mirroring') return { supported: false };
        return { config: {}, agent_status: {} };
      });
      const model = createDeviceState(send, { local: !container, container });
      for (const route of ['/', '/advanced/logs', '/advanced/mcp', '/voice', '/memory']) {
        model.activate(route);
        await flush();
        await vi.advanceTimersByTimeAsync(10000);
      }
      const requested = send.mock.calls.map(([path]) => path);
      expect(requested).toContain('/api/config/application');
      expect(requested).toContain('/api/mcp/servers');
      expect(requested.some((path) => /^\/api\/(network|storage|ota|security)\//.test(path))).toBe(
        false,
      );
      model.dispose();
    },
  );
  it('refreshes active route resources immediately on network recovery', async () => {
    const send = vi.fn(async (path) =>
      path === '/api/network/usb-internet' ? { enabled: true, via_usb: true } : { config: {} },
    );
    const model = createDeviceState(send);
    model.activate('/advanced');
    await flush();
    const count = () =>
      send.mock.calls.filter(([path]) => path === '/api/network/usb-internet').length;
    expect(count()).toBe(1);
    model.setVisible(true);
    await flush();
    expect(count()).toBe(2);
    model.setVisible(false);
    await vi.advanceTimersByTimeAsync(20000);
    expect(count()).toBe(2);
    model.setVisible(true);
    await flush();
    expect(count()).toBe(3);
    model.dispose();
  });
  it('retains confirmed USB data when the endpoint returns malformed JSON data', async () => {
    const model = createDeviceState(async () => ({}));
    model.resources.usb.publish({ enabled: true, via_usb: true });
    await model.resources.usb.refresh();
    expect(model.store.get(model.resources.usb.atom)).toMatchObject({
      data: { enabled: true },
      stale: true,
      error: 'usb status unavailable',
    });
    model.dispose();
  });
  it.each([true, false])(
    'joins the initial config read without aborting it (application arrives first: %s)',
    async (applicationFirst) => {
      const read = deferred();
      const send = vi.fn<import('../types').Transport>(() => read.promise);
      const model = createDeviceState(send);
      const request = model.start();
      await flush();
      const application = () =>
        model.resources.application.publish({ state: 'applied', revision: 'one' });
      if (applicationFirst) application();
      read.resolve({ config: { agent: { input_mode: 'realtime' } }, revision: 'one' });
      await request;
      if (!applicationFirst) application();
      await flush();
      expect(send).toHaveBeenCalledTimes(1);
      expect(send.mock.calls[0][1]?.signal?.aborted).toBe(false);
      expect(model.store.get(model.current).config?.agent?.input_mode).toBe('realtime');
      model.dispose();
    },
  );
  it('reconciles an external edit racing the first application observation', async () => {
    const read = deferred();
    let reads = 0;
    const model = createDeviceState(async () =>
      ++reads === 1
        ? read.promise
        : { config: { agent: { input_mode: 'realtime' } }, revision: 'two' },
    );
    const request = model.start();
    await flush();
    model.resources.application.publish({ state: 'applied', revision: 'two' });
    read.resolve({ config: { agent: { input_mode: 'stt' } }, revision: 'one' });
    await request;
    await flush();
    expect(reads).toBe(2);
    expect(model.store.get(model.current).config?.agent?.input_mode).toBe('realtime');
    model.dispose();
  });
  it('starts a fresh aggregate read when the external revision changes during a read', async () => {
    const old = deferred();
    let snapshotReads = 0;
    const model = createDeviceState(async () =>
      ++snapshotReads === 1 ? old.promise : { config: { model: { model: 'new' } } },
    );
    model.publishSave({ config: {}, revision: 'one', state: 'applied' });
    const request = model.resources.snapshot.refresh();
    await flush();
    model.resources.application.publish({ state: 'applied', revision: 'two' });
    await flush();
    old.resolve({ config: { model: { model: 'old' } } });
    await request;
    expect(model.store.get(model.current).config?.model?.model).toBe('new');
    expect(snapshotReads).toBe(2);
    model.dispose();
  });
  it('a delayed aggregate cannot overwrite a newer disconnected Wi-Fi observation', async () => {
    const read = deferred();
    const model = createDeviceState(() => read.promise);
    const request = model.resources.snapshot.refresh();
    await flush();
    model.publishWiFi({
      wifi_status: { connected: false, state: 'DISCONNECTED', observed_at_unix_us: 200 },
    });
    read.resolve({
      config: {},
      wifi_status: { connected: true, state: 'COMPLETED', observed_at_unix_us: 100 },
    });
    await request;
    expect(model.store.get(model.current).wifi_status!.connected).toBe(false);
    model.publishWiFi({
      wifi_status: { connected: true, state: 'COMPLETED', observed_at_unix_us: 150 },
    });
    expect(model.store.get(model.current).wifi_status!.connected).toBe(false);
    model.dispose();
  });
  it('does not reseed a resource after a newer dedicated read commits', async () => {
    const read = deferred();
    const model = createDeviceState(() => read.promise);
    const request = model.resources.snapshot.refresh();
    await flush();
    model.resources.storage.publish({ card: { present: false, mounted: false } });
    read.resolve({ config: {}, storage: { card: { present: true, mounted: true } } });
    await request;
    expect(model.store.get(model.resources.storage.atom).data?.card?.present).toBe(false);
    model.dispose();
  });
  it('preserves a newer saved config when an older aggregate arrives', async () => {
    const read = deferred();
    let reads = 0;
    const model = createDeviceState(async () =>
      ++reads === 1
        ? read.promise
        : {
            config: { agent: { input_mode: 'realtime' } },
            revision: 'new',
            config_valid: true,
            config_errors: [],
          },
    );
    const request = model.resources.snapshot.refresh();
    await flush();
    model.publishSave({
      config: { agent: { input_mode: 'realtime' } },
      state: 'pending',
      pending: true,
      revision: 'new',
    });
    read.resolve({ config: { agent: { input_mode: 'stt' } } });
    await request;
    expect(model.store.get(model.current).config!.agent!.input_mode).toBe('realtime');
    expect(model.store.get(model.resources.application.atom).data!.state).toBe('pending');
    model.dispose();
  });
  it('tracks asynchronous application failures and external config revisions', async () => {
    const send = vi.fn(async (url) =>
      url === '/api/config' ? { config: { agent: { input_mode: 'realtime' } } } : {},
    );
    const model = createDeviceState(send);
    await model.start();
    model.publishSave({
      config: { agent: { input_mode: 'stt' } },
      state: 'pending',
      revision: 'one',
    });
    model.resources.application.publish({
      state: 'failed',
      error: 'voice session failed',
      revision: 'one',
    });
    expect(model.store.get(model.resources.application.atom).data!.state).toBe('failed');
    model.resources.application.publish({ state: 'applied', revision: 'two' });
    await flush();
    expect(model.store.get(model.current).config!.agent!.input_mode).toBe('realtime');
    model.dispose();
  });
  it('shows unknown after a radio read fails and returns to confirmed state on recovery', async () => {
    let offline = true;
    const model = createDeviceState(async () => {
      if (offline) throw new Error('offline');
      return { wifi_status: { connected: true, ssid: 'Home', state: 'COMPLETED' } };
    });
    model.publishWiFi({ wifi_status: { connected: true, ssid: 'Home', state: 'COMPLETED' } });
    await model.resources.wifi.refresh();
    expect(model.store.get(model.current).wifi_status!.state).toBe('UNKNOWN');
    offline = false;
    await model.resources.wifi.refresh();
    expect(model.store.get(model.current).wifi_status!.connected).toBe(true);
    model.dispose();
  });
});
