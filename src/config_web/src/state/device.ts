import { atom, createStore } from 'jotai/vanilla';
import { createResource } from './resource';
import { applyLocale } from '../lib/i18n';
import type { Schema } from '../lib/schema';
import type {
  ApplicationData,
  Configuration,
  DeviceData,
  LANData,
  MCPData,
  MirroringData,
  OTAData,
  Revision,
  Snapshot,
  StorageData,
  Transport,
  USBData,
  VoiceMode,
  WiFiData,
} from '../types';

const paths = {
  wifi: '/api/network/wifi/status',
  storage: '/api/storage/status',
  ota: '/api/ota/status?logs=0',
  otaLogs: '/api/ota/status',
  application: '/api/config/application',
  usb: '/api/network/usb-internet',
  mcp: '/api/mcp/servers',
  lan: '/api/security/lan-access',
  device: '/api/device/status',
  firmware: '/api/device/firmware',
  mirroring: '/api/device/iphone-mirroring',
};
const frequency: Partial<Record<keyof typeof paths, number>> = {
  wifi: 1000,
  application: 2000,
  storage: 3000,
  ota: 2000,
  otaLogs: 2000,
  device: 10000,
  firmware: 10000,
};
const record = (value: unknown): value is Record<string, unknown> =>
  value !== null && typeof value === 'object' && !Array.isArray(value);
const validSnapshot = (value: unknown): value is Snapshot => record(value) && record(value.config);
const validStatus = (name: keyof typeof paths, value: unknown): boolean => {
  if (!record(value)) return false;
  switch (name) {
    case 'wifi':
      return validWiFi(value);
    case 'usb':
      return typeof value.enabled === 'boolean' && typeof value.via_usb === 'boolean';
    case 'lan':
      return typeof value.enabled === 'boolean' && typeof value.password_set === 'boolean';
    case 'storage':
      return record(value.card) && typeof value.card.present === 'boolean';
    case 'ota':
    case 'otaLogs':
      return typeof value.ota_update_running === 'boolean';
    case 'application':
      return typeof value.state === 'string';
    case 'mcp':
      return (
        Array.isArray(value.servers) &&
        value.servers.every(
          (server) =>
            record(server) &&
            typeof server.name === 'string' &&
            typeof server.enabled === 'boolean' &&
            (!server.tools ||
              (Array.isArray(server.tools) &&
                server.tools.every((tool) => typeof tool === 'string'))),
        )
      );
    case 'firmware':
      return record(value.firmware);
    case 'device':
      return record(value.agent_status) || record(value.firmware);
    case 'mirroring':
      return typeof value.supported === 'boolean';
  }
};
export const validWiFi = (data: unknown): data is WiFiData =>
  !!data &&
  typeof data === 'object' &&
  'wifi_status' in data &&
  typeof (data.wifi_status as WiFiData['wifi_status'])?.connected === 'boolean';
const wifiOrder = (next: WiFiData, previous: WiFiData | null) => {
  const a = next?.wifi_status?.observed_at_unix_us;
  const b = previous?.wifi_status?.observed_at_unix_us;
  return !Number.isSafeInteger(a) || !Number.isSafeInteger(b) || a! >= b!;
};
const wifiView = (value: WiFiData | null) => {
  if (!value) return value;
  const { observed_at_unix_us: _observed, ...status } = value.wifi_status;
  return { ...value, wifi_status: status };
};

/** Stores belong to one app, not a module singleton (also isolates tests). */
export function createDeviceState(
  send: Transport,
  { local: native = false, container = false } = {},
) {
  const local = native || container;
  const store = createStore();
  const config = atom<Configuration | null>(null);
  const voiceTab = atom<VoiceMode | null>(null);
  const voiceMode = atom(
    (get): VoiceMode =>
      get(voiceTab) ?? (get(config)?.agent?.input_mode === 'stt' ? 'stt' : 'realtime'),
    (_get, set, value: VoiceMode) => set(voiceTab, value),
  );
  const snapshotFailure = atom<Snapshot['snapshot_error'] | undefined>(undefined);
  let canonicalReadPending = false;
  const locale = atom(0);
  const request = <T>(url: string, options?: RequestInit) => send(url, options) as Promise<T>;
  function resource<T>(name: keyof typeof paths) {
    return createResource<T>(store, {
      interval: frequency[name] || 5000,
      observeDuringWrite: name === 'wifi',
      equalViewData:
        name === 'wifi'
          ? (a, b) =>
              JSON.stringify(wifiView(a as WiFiData | null)) ===
              JSON.stringify(wifiView(b as WiFiData | null))
          : Object.is,
      timeout: name === 'mcp' ? 45000 : 8000,
      accept: (next, previous) =>
        validStatus(name, next) &&
        (name !== 'wifi' || wifiOrder(next as WiFiData, previous as WiFiData | null)),
      async read(signal) {
        let result: T;
        try {
          result = await request<T>(paths[name], { cache: 'no-store', signal });
        } catch (error) {
          // A 503 can still describe the file revision while Agent is offline.
          if (name === 'application' && !signal.aborted && record(error))
            observeApplication(error as ApplicationData);
          throw error;
        }
        if (!validStatus(name, result)) throw new Error(`${name} status unavailable`);
        return result;
      },
    });
  }
  const resources = {
    wifi: resource<WiFiData>('wifi'),
    storage: resource<StorageData>('storage'),
    ota: resource<OTAData>('ota'),
    otaLogs: resource<OTAData>('otaLogs'),
    application: resource<ApplicationData>('application'),
    usb: resource<USBData>('usb'),
    mcp: resource<MCPData>('mcp'),
    lan: resource<LANData>('lan'),
    device: resource<DeviceData>('device'),
    firmware: resource<Pick<DeviceData, 'firmware'>>('firmware'),
    mirroring: resource<MirroringData>('mirroring'),
  };
  const schema = createResource<Schema>(store, {
    async read(signal) {
      const value = await request<Schema>('/api/config/schema', { signal });
      if (!Array.isArray(value.sections)) throw new Error('Configuration schema unavailable');
      return value;
    },
  });
  const snapshot = createResource<Snapshot>(store, {
    timeout: 20000,
    async read(signal) {
      const versions = Object.fromEntries(
        Object.entries(resources).map(([key, resource]) => [
          key,
          { epoch: resource.epoch, version: resource.version },
        ]),
      );
      let value: Snapshot;
      try {
        value = await request<Snapshot>('/api/config', { cache: 'no-store', signal });
        if (!validSnapshot(value)) throw new Error('Configuration unavailable');
      } catch (error) {
        if (!signal.aborted)
          store.set(snapshotFailure, {
            message: error instanceof Error ? error.message : String(error),
            ...(record(error) && typeof error.code === 'string' ? { code: error.code } : {}),
          });
        throw error;
      }
      if (signal.aborted) return value;
      canonicalReadPending = false;
      store.set(snapshotFailure, undefined);
      if (
        value.wifi_status &&
        resources.wifi.epoch === versions.wifi.epoch &&
        resources.wifi.version === versions.wifi.version
      )
        resources.wifi.publish({ wifi_status: value.wifi_status, wifi: value.wifi });
      if (
        value.storage &&
        resources.storage.epoch === versions.storage.epoch &&
        resources.storage.version === versions.storage.version
      )
        resources.storage.publish(value.storage);
      if (
        (value.firmware || value.agent_status) &&
        resources.device.epoch === versions.device.epoch &&
        resources.device.version === versions.device.version
      )
        resources.device.publish({ firmware: value.firmware, agent_status: value.agent_status });
      if (value.config?.agent?.locale) applyLocale(value.config.agent.locale, true);
      store.set(config, value.config || null);
      return value;
    },
  });
  const allResources = { ...resources, snapshot, schema };
  const current = atom<Snapshot>((get) => {
    const base = get(snapshot.view).data || {};
    const wifi = get(resources.wifi.view);
    const storage = get(resources.storage.view);
    const device = get(resources.device.view);
    const firmware = get(resources.firmware.view);
    return {
      ...base,
      config: get(config),
      snapshot_error: get(snapshotFailure),
      ...(wifi.data || {}),
      ...(wifi.stale ? { wifi_status: { connected: false, state: 'UNKNOWN' } } : {}),
      ...(storage.data ? { storage: storage.data } : {}),
      ...(device.data || {}),
      ...(firmware.data || {}),
    };
  });
  const configLocale = atom((get) => get(config)?.agent?.locale);
  let selected = new Set<string>();
  let visible = true;
  let savedRevision: Revision | null = null;
  let runtime: string | null = null;
  const publishedSaves = new WeakSet<ApplicationData>();
  function activate(route = '/') {
    const names = local ? ['application'] : ['wifi', 'storage', 'ota', 'application', 'firmware'];
    if (route === '/' && local) {
      names.push('device');
      if (!container) names.push('mirroring');
    }
    if (!local && route.startsWith('/advanced')) names.push('lan');
    if (!local && route === '/advanced') names.push('usb');
    if (route === '/advanced/logs/agent' || (container && route.startsWith('/voice')))
      names.push('device');
    if (route === '/advanced/mcp') names.push('mcp');
    if (!local && route.startsWith('/firmware')) names.push('otaLogs');
    selected = new Set(names);
    sync();
  }
  function sync() {
    for (const [name, resource] of Object.entries(allResources)) {
      if (name !== 'snapshot' && name !== 'schema')
        resource.setActive(visible && selected.has(name));
    }
  }
  function publishSave(value: ApplicationData) {
    // The transport event and the saving form can publish the same response.
    // Join its validation read rather than cancelling and restarting it.
    if (publishedSaves.has(value)) return;
    publishedSaves.add(value);
    // An aggregate read begun before this write cannot replace the new config.
    snapshot.dispose();
    canonicalReadPending = true;
    store.set(snapshotFailure, undefined);
    const savedLocale = value.locale || value.config?.agent?.locale;
    if (savedLocale) applyLocale(savedLocale, true);
    if (value.config) store.set(config, value.config);
    if (value.locale)
      store.set(config, (previous) => ({
        ...previous,
        agent: { ...previous?.agent, locale: value.locale },
      }));
    savedRevision = value.revision ?? savedRevision;
    runtime = value.runtime_id || runtime;
    if (value.config || value.locale)
      snapshot.publish({
        ...store.get(current),
        config: store.get(config),
        revision: value.revision,
        config_valid: undefined,
        config_errors: undefined,
      });
    resources.application.publish(value);
    // Saves and imports must reconcile canonical validation for the saved file.
    void snapshot.refresh();
  }
  function observeApplication(value: ApplicationData) {
    const revision = value.persisted_revision ?? value.revision;
    if (revision === undefined) return;
    const initial = savedRevision === null;
    const changed =
      savedRevision !== revision || (value.runtime_id && runtime && value.runtime_id !== runtime);
    savedRevision = revision;
    runtime = value.runtime_id || runtime;
    if (initial) {
      // Establish the first baseline without cancelling the initial config read.
      // If an external edit raced that read, reconcile once it has completed.
      const data = store.get(snapshot.atom).data;
      if (data) {
        if (data.revision !== undefined && data.revision !== revision) void snapshot.refresh();
        return;
      }
      void snapshot.refresh().then(() => {
        const data = store.get(snapshot.atom).data;
        if (
          visible &&
          savedRevision === revision &&
          data?.revision !== undefined &&
          data.revision !== revision
        )
          void snapshot.refresh();
      });
    } else if (changed) {
      snapshot.dispose();
      void snapshot.refresh();
    } else if (
      canonicalReadPending ||
      (value.persisted_revision !== undefined &&
        store.get(snapshot.atom).data?.revision !== revision)
    ) {
      // Retry a failed reconciliation without requiring another file edit.
      // refresh() joins an existing read instead of restarting it each poll.
      void snapshot.refresh();
    }
  }
  const unsubscribe = store.sub(resources.application.atom, () => {
    const { data, stale } = store.get(resources.application.atom);
    // Every successful poll can retry a failed config reconciliation, including
    // identical status data. A failed read must not replay older runtime data.
    if (data && !stale) observeApplication(data);
  });
  return {
    store,
    resources: allResources,
    config,
    voiceMode,
    current,
    locale,
    configLocale,
    local,
    container,
    send: request,
    activate,
    publishSave,
    setVisible(value: boolean) {
      visible = value;
      sync();
      if (value) {
        for (const name of selected) void resources[name as keyof typeof resources].refresh();
        void snapshot.refresh();
      } else snapshot.dispose();
    },
    publishWiFi(value: unknown) {
      if (validWiFi(value)) resources.wifi.publish(value);
    },
    async start() {
      if (visible) await snapshot.refresh();
    },
    dispose() {
      visible = false;
      unsubscribe();
      Object.values(allResources).forEach((resource) => resource.dispose());
    },
  };
}

export type DeviceState = ReturnType<typeof createDeviceState>;
