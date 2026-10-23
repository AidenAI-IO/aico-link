/**
 * Data access for the settings routes.
 *
 * This layer reuses the settled, contract-tested pieces of the classic page —
 * the `request()` wrapper that serialises config writes and knows the canonical
 * routes, and the message catalogue — and keeps every route's rendering free of
 * `fetch` calls.
 */

import {request} from '../config/api.js';
import {applyLocale, getActiveLocale, initI18n, t} from '../config/i18n.js';

export {applyLocale, request, t, initI18n};

/**
 * Adopt the language the device is configured with.
 *
 * The catalogue otherwise falls back to the browser's stored choice, which on a
 * fresh WebView is the built-in default; the device setting is authoritative, so
 * a language changed in the app has to win.
 *
 * @param {object|null} snapshot
 * @returns {boolean} true when the active locale changed.
 */
export function applyServerLocale(snapshot) {
  const configured = snapshot && snapshot.config && snapshot.config.agent && snapshot.config.agent.locale;
  if (!configured || configured === getActiveLocale()) return false;
  applyLocale(configured, false);
  return true;
}

/** Read the aggregate device snapshot that backs the settings home and Wi-Fi. */
export async function fetchSnapshot() {
  return request('/api/device/snapshot');
}

/**
 * The snapshot a route was opened with, or a fresh one; a page that cannot
 * read it still renders, with an empty config.
 */
export async function ensureSnapshot(context) {
  return context.snapshot || fetchSnapshot().catch(() => ({config: {}}));
}

/**
 * A saver `onSaved` handler that adopts the saved config into `snapshot`, then
 * runs `after` when given.
 */
export function adoptSaved(snapshot, after) {
  return payload => {
    if (payload && payload.config) snapshot.config = payload.config;
    if (after) after();
  };
}

/**
 * Read the snapshot without failing the caller.
 *
 * Every settings surface is still usable without live values, so a snapshot
 * problem degrades the status accessories instead of blanking the screen.
 *
 * @returns {Promise<object|null>}
 */
export async function loadSnapshot() {
  try {
    return await fetchSnapshot();
  } catch (error) {
    console.warn('[settings] device snapshot unavailable:', error && error.message);
    return null;
  }
}

/** Saved Wi-Fi profiles, as reported by the snapshot. */
export function savedNetworks(snapshot) {
  const networks = snapshot && snapshot.wifi && snapshot.wifi.networks;
  return Array.isArray(networks) ? networks : [];
}

/** The currently associated SSID, or an empty string. */
export function connectedSsid(snapshot) {
  const status = (snapshot && snapshot.wifi_status) || {};
  return status.connected && status.ssid ? status.ssid : '';
}

/** Look up one saved profile by SSID. */
export function savedNetwork(snapshot, ssid) {
  return savedNetworks(snapshot).find(network => network.ssid === ssid) || null;
}

/**
 * The voice mode the Agent runs: `agent.input_mode`, or when unset the one its
 * configured providers imply (`Config.InputModeOrDefault` in the Agent): the
 * classic pair when both STT and TTS name a record, realtime when the selected
 * realtime record has a credential, otherwise none (`''`).
 */
export function voiceMode(snapshot) {
  const config = (snapshot && snapshot.config) || {};
  const explicit = String((config.agent || {}).input_mode || '').toLowerCase();
  if (explicit) return explicit;
  const selected = (ref, records) => {
    const name = (config[ref] || {}).provider;
    return name && config[records] && config[records][name] ? config[records][name] : null;
  };
  if (selected('stt', 'stt_providers') && selected('tts', 'tts_providers')) return 'stt';
  const realtime = selected('voice_model', 'voice_model_providers');
  return realtime && realtime.has_api_key ? 'realtime' : '';
}

/** Storage figures for the storage row's status text. */
export function storageStatus(snapshot) {
  const storage = (snapshot && snapshot.storage) || null;
  if (!storage) return null;
  if (storage.card && storage.card.mounted) return 'mounted';
  const internal = storage.internal || {};
  if (internal.available) return 'internal';
  return 'unavailable';
}
