import type { DeviceState } from '../state/device';
import type { Navigate, Snapshot, TextSpec } from '../types';
import { useAtomValue } from 'jotai';
import { PRIMARY_ITEMS, SECONDARY_ITEMS, LOCAL_ITEMS, CONTAINER_ITEMS } from './navItems';
import { voiceMode, storageStatus } from '../lib/data';
import { shortVersion } from '../lib/ota';
import { msg } from '../lib/i18n';
import { Group, Row } from '../components/ui';
import { isInitialLoading } from '../state/resource';

function status(item: { status?: string }, snapshot: Snapshot, model: DeviceState) {
  const unknown = msg('status.state.unknown', '未知');
  if (item.status === 'wifi') {
    if (!snapshot.wifi_status || snapshot.wifi_status.state === 'UNKNOWN')
      return { value: unknown };
    return snapshot.wifi_status?.connected
      ? { value: msg('wifi.connected', '已连接'), tone: 'success' }
      : { value: msg('ui.not_connected', '未连接') };
  }
  if (item.status === 'voice' && snapshot.config) {
    const mode = voiceMode(snapshot);
    return {
      value:
        mode === 'realtime'
          ? msg('voice.mode_realtime', 'Realtime')
          : mode
            ? msg('voice.mode_classic', 'Classic')
            : msg('ui.voice_unset', '未启用'),
    };
  }
  if (item.status === 'voice') return { value: unknown };
  if (item.status === 'storage') {
    if (model.store.get(model.resources.storage.atom).stale) return { value: unknown };
    return storageStatus(snapshot) === 'mounted'
      ? { value: msg('wifi.connected', '已连接'), tone: 'success' }
      : {};
  }
  if (item.status === 'firmware') {
    if (model.store.get(model.resources.firmware.atom).stale) return { value: unknown };
    const firmware = snapshot.firmware || {};
    return firmware.health_status === 'failed'
      ? { value: msg('ota.health_failed', '失败'), tone: 'danger' }
      : { value: shortVersion(firmware.current_version || firmware.version) };
  }
  return {};
}

export function Navigation({
  model,
  navigate,
  path,
}: {
  model: DeviceState;
  navigate: Navigate;
  path: string;
}) {
  const snapshot = useAtomValue(model.current);
  const wifi = useAtomValue(model.resources.wifi.view);
  const storage = useAtomValue(model.resources.storage.view);
  const device = useAtomValue(model.resources.firmware.view);
  const config = useAtomValue(model.resources.snapshot.view);
  const loading: Record<string, boolean> = {
    wifi: isInitialLoading(wifi),
    storage: isInitialLoading(storage),
    firmware: isInitialLoading(device),
    voice: !snapshot.config && isInitialLoading(config),
  };
  const build = (item: { id: string; label: TextSpec; route: string; status?: string }) => (
    <Row
      key={item.id}
      label={item.label}
      {...status(item, snapshot, model)}
      loading={!!item.status && loading[item.status]}
      chevron
      className={`nav__row ${path === item.route || path.startsWith(`${item.route}/`) ? 'nav__row--active' : ''}`}
      action={`open-${item.id}`}
      onPress={() => navigate(item.route)}
    />
  );
  return (
    <div className="ds-screen">
      {model.local ? (
        <Group>{(model.container ? CONTAINER_ITEMS : LOCAL_ITEMS).map(build)}</Group>
      ) : (
        <>
          <Group>{PRIMARY_ITEMS.map(build)}</Group>
          <Group>{SECONDARY_ITEMS.map(build)}</Group>
        </>
      )}
    </div>
  );
}
