import type { Network, WiFiData, WiFiProfile } from '../types';
export interface ConnectionProfile {
  ssid: string;
  psk?: string;
  keepProxy?: boolean;
  proxyMode?: string;
  proxyUrl?: string;
  noProxy?: string;
}
interface WiFiTask extends Partial<WiFiData> {
  ok?: boolean;
  status?: string;
  task_id?: string;
  deadline_seconds?: number;
  failure_reason?: string;
}
interface ScanPayload {
  network_details?: {
    ssid: string;
    secured?: boolean;
    signal_dbm?: number;
    signal_percent?: number;
  }[];
  networks?: string[];
  output?: string;
}
/**
 * Wi-Fi operations.
 *
 * The transport protocol is lifted from the classic page unchanged: a scan is a
 * POST, saving a profile is a PUT that may hand back a background task id, and
 * forgetting is a DELETE keyed by SSID. Keeping it here means the Wi-Fi route
 * renders state and never speaks to the network itself.
 */

import { request } from './client';

const CONNECTION_URL = '/api/network/wifi/connection';
const SCAN_URL = '/api/network/wifi/scan';
/** How often a running connection task is polled, in milliseconds. */
const POLL_INTERVAL_MS = 1000;

const wait = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

function publishWiFi(payload: WiFiTask) {
  if (
    payload?.wifi_status &&
    typeof window !== 'undefined' &&
    typeof window.CustomEvent === 'function'
  ) {
    window.dispatchEvent(new window.CustomEvent('aico:wifi-changed', { detail: payload }));
  }
}

/** Trigger a scan and return the discovered networks in scan order. */
export async function scan() {
  const payload = await request<ScanPayload>(SCAN_URL, { method: 'POST' });
  const details = Array.isArray(payload && payload.network_details)
    ? (payload.network_details || [])
        .filter((network) => network && typeof network.ssid === 'string' && network.ssid !== '')
        .map((network) => ({
          ssid: network.ssid,
          secured: network.secured === true,
          signalDbm:
            network.signal_dbm != null && Number.isFinite(Number(network.signal_dbm))
              ? Number(network.signal_dbm)
              : null,
          signalPercent:
            network.signal_percent != null && Number.isFinite(Number(network.signal_percent))
              ? Math.max(0, Math.min(100, Number(network.signal_percent)))
              : null,
        }))
    : [];
  if (details.length) return details;
  const names = Array.isArray(payload && payload.networks) ? payload.networks || [] : [];
  const secured = parseSecuredSsids(payload.output);
  return names.map((ssid) => ({
    ssid,
    secured: secured.has(ssid),
    signalDbm: null,
    signalPercent: null,
  }));
}

/** Map a dBm/percentage observation to the three visible radio levels. */
export function wifiSignalLevel(network: Partial<Network> | null) {
  const percent = network && network.signalPercent != null ? Number(network.signalPercent) : NaN;
  if (Number.isFinite(percent)) {
    if (percent >= 67) return 3;
    if (percent >= 34) return 2;
    if (percent > 0) return 1;
    return 0;
  }
  const dbm = network && network.signalDbm != null ? Number(network.signalDbm) : NaN;
  if (!Number.isFinite(dbm)) return null;
  if (dbm >= -55) return 3;
  if (dbm >= -70) return 2;
  if (dbm >= -85) return 1;
  return 0;
}

/**
 * Read the `iw` scan dump the scan response carries alongside the SSID list, to
 * learn which networks are encrypted. A BSS block states its capability before
 * its SSID, so the capability is held until the matching SSID line arrives.
 * A missing or unexpected dump simply yields no lock icons.
 *
 * @param {string} [output]
 * @returns {Set<string>} SSIDs advertising the Privacy capability.
 */
export function parseSecuredSsids(output?: string) {
  const secured = new Set<string>();
  if (typeof output !== 'string' || output === '') return secured;

  let currentSecured = false;
  for (const rawLine of output.split('\n')) {
    const line = rawLine.trim();
    if (line.startsWith('BSS ')) {
      currentSecured = false;
      continue;
    }
    if (line.startsWith('capability:')) {
      currentSecured = line.includes('Privacy');
      continue;
    }
    if (line.startsWith('SSID:')) {
      const ssid = line.slice('SSID:'.length).trim();
      if (ssid && currentSecured) secured.add(ssid);
    }
  }
  return secured;
}

/**
 * Save and activate a profile, waiting for the background task to settle.
 *
 * @param {object} profile
 * @param {string} profile.ssid
 * @param {string} [profile.psk] - omit to keep the stored key.
 * @param {boolean} [profile.keepProxy] - leave the saved network's proxy
 *   setting as it is (switching to a saved network changes nothing else).
 * @param {'system'|'direct'|'proxy'} [profile.proxyMode]
 * @param {string} [profile.proxyUrl]
 * @param {string} [profile.noProxy]
 * @returns {Promise<{ok: boolean, failureReason: string, payload: object}>}
 *   `payload` is the final task result, which carries `wifi`, `wifi_status`,
 *   and (on failure) the protocol's `failure_reason`.
 */
export async function connect(profile: ConnectionProfile) {
  // An absent proxy_mode tells the device to keep the saved proxy setting.
  const body: {
    ssid: string;
    proxy_mode?: string;
    psk?: string;
    proxy_url?: string;
    no_proxy?: string;
  } = profile.keepProxy
    ? { ssid: profile.ssid }
    : { ssid: profile.ssid, proxy_mode: profile.proxyMode || 'system' };
  if (profile.psk !== undefined) body.psk = profile.psk;
  if (body.proxy_mode === 'proxy') {
    if (profile.proxyUrl) body.proxy_url = profile.proxyUrl;
    if (profile.noProxy) body.no_proxy = profile.noProxy;
  }

  const started = await request<WiFiTask>(CONNECTION_URL, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });

  const payload = started && started.status === 'running' ? await waitForTask(started) : started;
  publishWiFi(payload);
  const status = payload.wifi_status;
  const ok =
    Boolean(payload && payload.ok) && status?.connected === true && status.ssid === profile.ssid;
  return {
    ok,
    failureReason:
      payload && typeof payload.failure_reason === 'string' ? payload.failure_reason : '',
    payload: payload || {},
  };
}

/** Remove a saved profile. */
export async function forget(ssid: string) {
  const payload = await request<WiFiTask>(`${CONNECTION_URL}?ssid=${encodeURIComponent(ssid)}`, {
    method: 'DELETE',
  });
  publishWiFi(payload);
  return { ok: payload && payload.ok !== false, payload: payload || {} };
}

/** Poll a running connection task until it stops, or its deadline passes. */
async function waitForTask(started: WiFiTask) {
  const taskId = started.task_id;
  if (!taskId) throw new Error('Missing Wi-Fi task ID');
  const deadline = Date.now() + ((Number(started.deadline_seconds) || 120) + 5) * 1000;
  while (Date.now() < deadline) {
    await wait(POLL_INTERVAL_MS);
    const payload = await request<WiFiTask>(
      `${CONNECTION_URL}?task_id=${encodeURIComponent(taskId)}`,
      {
        method: 'GET',
      },
    );
    if (payload.status !== 'running') return payload;
  }
  throw new Error('Wi-Fi connection task timed out');
}

/**
 * Resolve a proxy choice into a request the Agent will accept.
 *
 * The Agent requires `proxy_url` whenever `proxy_mode` is `proxy`, unless the
 * profile already stores one (it then reuses the stored value). Sending
 * `proxy_mode=proxy` with no URL is therefore rejected outright, which is why
 * flipping a proxy switch must not submit anything until a URL exists — asking
 * for that up front is what made a per-network proxy toggle look broken.
 *
 * @param {object} choice
 * @param {boolean} choice.enabled - the switch's new position.
 * @param {string} [choice.url] - URL currently in the field.
 * @param {string} [choice.savedUrl] - URL already stored for this profile.
 * @returns {{ok: true, proxyMode: string, proxyUrl: string} | {ok: false, reason: string}}
 */
export function proxyChangeRequest({
  enabled,
  url,
  savedUrl = '',
}: {
  enabled: boolean;
  url?: string;
  savedUrl?: string;
}) {
  const trimmed = (url || '').trim();
  const stored = (savedUrl || '').trim();

  if (!enabled) return { ok: true, proxyMode: 'system', proxyUrl: '' };
  if (!trimmed && !stored) return { ok: false, reason: 'proxy_url_required' };
  return { ok: true, proxyMode: 'proxy', proxyUrl: trimmed || stored };
}

/**
 * Networks to show under "Other networks": everything discovered, minus the
 * profiles already listed above, de-duplicated, in scan order.
 *
 * @param {Array<{ssid: string, secured: boolean, signalDbm?: number|null, signalPercent?: number|null}>} discovered
 * @param {string[]} savedSsids
 * @param {string} connected
 * @returns {Array<{ssid: string, secured: boolean}>}
 */
export function otherNetworks(discovered: Network[], savedSsids: string[], connected: string) {
  const seen = new Set([...savedSsids, connected].filter(Boolean));
  const result = [];
  for (const network of discovered || []) {
    if (!network || !network.ssid || seen.has(network.ssid)) continue;
    seen.add(network.ssid);
    result.push(network);
  }
  return result;
}

/**
 * Profiles to show under "Current network": the connected profile first, then
 * the remaining saved profiles by descending priority.
 */
export function currentNetworks(networks: WiFiProfile[], connected: string) {
  const sorted = [...(networks || [])].sort(
    (a, b) => Number(b.priority || 0) - Number(a.priority || 0),
  );
  if (!connected) return sorted;
  const match = sorted.find((network) => network.ssid === connected);
  if (!match) return sorted;
  return [match, ...sorted.filter((network) => network.ssid !== connected)];
}
