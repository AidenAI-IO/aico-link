import type { Snapshot, WiFiData } from '../types';
export { request } from '../api/client';
export { applyLocale, initI18n, t } from './i18n';
import { applyLocale } from './i18n';
export function applyServerLocale(snapshot: Snapshot) {
  if (snapshot.config?.agent?.locale) applyLocale(snapshot.config.agent.locale);
}
export const connectedSsid = (snapshot: Partial<WiFiData>) =>
  snapshot.wifi_status?.connected ? snapshot.wifi_status.ssid || '' : '';
export const savedNetworks = (snapshot: Partial<WiFiData>) => snapshot.wifi?.networks || [];
export const savedNetwork = (snapshot: Partial<WiFiData>, ssid: string) =>
  savedNetworks(snapshot).find((network) => network.ssid === ssid);
export const voiceMode = (snapshot: Snapshot) =>
  ['stt', 'realtime'].includes(snapshot.config?.agent?.input_mode || '')
    ? snapshot.config?.agent?.input_mode
    : '';
export const storageStatus = (snapshot: Snapshot) =>
  snapshot.storage?.card?.mounted
    ? 'mounted'
    : snapshot.storage?.card?.present
      ? 'unusable'
      : 'none';
