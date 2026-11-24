import { atom, createStore } from 'jotai/vanilla';
import { createResource } from './resource';
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
  WiFiData,
} from './types';

const paths = {
  wifi: '/api/network/wifi/status',
  storage: '/api/storage/status',
  ota: '/api/ota/status',
  application: '/api/config/application',
  usb: '/api/network/usb-internet',
  mcp: '/api/mcp/servers',
  lan: '/api/security/lan-access',
  device: '/api/device/status',
  mirroring: '/api/device/iphone-mirroring',
};
const frequency: Partial<Record<keyof typeof paths, number>> = {
  wifi: 1000,
  application: 2000,
  storage: 3000,
  ota: 2000,
  device: 10000,
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

/** Stores belong to one app, not a module singleton (also isolates tests). */
export function createDeviceState(
  send: Transport,
  { local: native = false, container = false } = {},
) {
  const local = native || container;
  const store = createStore();
  const config = atom<Configuration | null>(null);
  const snapshotFailure = atom<Snapshot['snapshot_error'] | undefined>(undefined);
  const locale = atom(0);
  const request = <T>(url: string, options?: RequestInit) => send(url, options) as Promise<T>;
  function resource<T>(name: keyof typeof paths) {
    return createResource<T>(store, {
      interval: frequency[name] || 5000,
      observeDuringWrite: name === 'wifi',
      timeout: name === 'mcp' ? 45000 : 8000,
      accept: (next, previous) =>
        validStatus(name, next) &&
        (name !== 'wifi' || wifiOrder(next as WiFiData, previous as WiFiData | null)),
      async read(signal) {
        const result = await request<T>(paths[name], { cache: 'no-store', signal });
        if (!validStatus(name, result)) throw new Error(`${name} status unavailable`);
        return result;
      },
    });
  }
  const resources = {
    wifi: resource<WiFiData>('wifi'),
    storage: resource<StorageData>('storage'),
    ota: resource<OTAData>('ota'),
    application: resource<ApplicationData>('application'),
    usb: resource<USBData>('usb'),
    mcp: resource<MCPData>('mcp'),
    lan: resource<LANData>('lan'),
    device: resource<DeviceData>('device'),
    mirroring: resource<MirroringData>('mirroring'),
  };
  const snapshot = createResource<Snapshot>(store, {
    timeout: 20000,
    async read(signal) {
      const versions = Object.fromEntries(
        Object.entries(resources).map(([key, resource]) => [key, resource.epoch]),
      );
      let value: Snapshot;
      try {
        value = await request<Snapshot>('/api/device/snapshot', { cache: 'no-store', signal });
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
      store.set(snapshotFailure, undefined);
      if (value.wifi_status && resources.wifi.epoch === versions.wifi)
        resources.wifi.publish({ wifi_status: value.wifi_status, wifi: value.wifi });
      if (value.storage && resources.storage.epoch === versions.storage)
        resources.storage.publish(value.storage);
      if ((value.firmware || value.agent_status) && resources.device.epoch === versions.device)
        resources.device.publish({ firmware: value.firmware, agent_status: value.agent_status });
      store.set(config, value.config || null);
      return value;
    },
  });
  const allResources = { ...resources, snapshot };
  const current = atom<Snapshot>((get) => {
    const base = get(snapshot.atom).data || {};
    const wifi = get(resources.wifi.atom);
    const storage = get(resources.storage.atom);
    const device = get(resources.device.atom);
    return {
      ...base,
      config: get(config),
      snapshot_error: get(snapshotFailure),
      ...(wifi.data || {}),
      ...(wifi.stale ? { wifi_status: { connected: false, state: 'UNKNOWN' } } : {}),
      ...(storage.data ? { storage: storage.data } : {}),
      ...(device.data || {}),
    };
  });
  let selected = new Set<string>();
  let visible = true;
  let savedRevision: Revision | null = null;
  let runtime: string | null = null;
  function activate(route = '/') {
    const names = local ? ['application'] : ['wifi', 'storage', 'ota', 'application', 'device'];
    if (route === '/' && local) {
      names.push('device');
      if (!container) names.push('mirroring');
    }
    if (!local && route.startsWith('/advanced')) names.push('lan');
    if (!local && route === '/advanced') names.push('usb');
    if (route === '/advanced/mcp') names.push('mcp');
    if (route.startsWith('/firmware')) names.push('device');
    selected = new Set(names);
    sync();
  }
  function sync() {
    for (const [name, resource] of Object.entries(allResources)) {
      if (name !== 'snapshot') resource.setActive(visible && selected.has(name));
    }
  }
  function publishSave(value: ApplicationData) {
    // An aggregate read begun before this write cannot replace the new config.
    snapshot.dispose();
    store.set(snapshotFailure, undefined);
    if (value.config) store.set(config, value.config);
    if (value.locale)
      store.set(config, (previous) => ({
        ...previous,
        agent: { ...previous?.agent, locale: value.locale },
      }));
    savedRevision = value.revision ?? savedRevision;
    runtime = value.runtime_id || runtime;
    if (value.config || value.locale)
      snapshot.publish({ ...store.get(current), config: store.get(config) });
    resources.application.publish(value);
    // Imports may not include the parsed config in their response.
    if (!value.config && !value.locale) void snapshot.refresh();
  }
  const unsubscribe = store.sub(resources.application.atom, () => {
    const value = store.get(resources.application.atom).data;
    if (value?.revision === undefined) return;
    const changed =
      savedRevision === null ||
      savedRevision !== value.revision ||
      (runtime && value.runtime_id !== runtime);
    savedRevision = value.revision;
    runtime = value.runtime_id || null;
    if (changed) {
      snapshot.dispose();
      void snapshot.refresh();
    }
  });
  return {
    store,
    resources: allResources,
    config,
    current,
    locale,
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
      unsubscribe();
      Object.values(allResources).forEach((resource) => resource.dispose());
    },
  };
}

export type DeviceState = ReturnType<typeof createDeviceState>;
