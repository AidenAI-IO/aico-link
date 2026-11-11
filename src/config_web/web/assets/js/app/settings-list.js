/**
 * The settings list.
 *
 * This is the single view the design describes, and it is used in two places:
 * as the desktop navigation rail on the left, and as the whole screen on a
 * phone at `/`. Both copies render from the same definition so they cannot
 * diverge; CSS decides which one is visible at a given width.
 */

import {connectedSsid, storageStatus, voiceMode} from './data.js';
import {shortVersion} from './ota-progress.js';
import {LOCAL_ITEMS, PRIMARY_ITEMS, SECONDARY_ITEMS} from './nav-items.js';
import {group, row, screen} from '../ui/list.js';
import {msg} from '../ui/text.js';

/** Trailing value for an entry, or null when the design shows none. */
function statusFor(item, snapshot) {
  if (!item.status || !snapshot) return null;

  if (item.status === 'wifi') {
    if (connectedSsid(snapshot)) return {value: msg('wifi.connected', '已连接'), tone: 'success'};
    return {value: msg('ui.not_connected', '未连接'), tone: null};
  }

  if (item.status === 'voice') {
    // The config has no `voice_mode`; the mode is `agent.input_mode`, or the
    // one the configured providers imply when it is unset.
    const mode = voiceMode(snapshot);
    if (!mode) return {value: msg('ui.voice_unset', '未启用'), tone: null};
    return {value: mode === 'realtime' ? msg('voice.mode_realtime', 'Realtime') : msg('voice.mode_classic', 'Classic'), tone: null};
  }

  if (item.status === 'firmware') {
    const firmware = snapshot.firmware || {};
    if (firmware.health_status === 'failed') return {value: msg('ota.health_failed', '失败'), tone: 'danger'};
    const version = shortVersion(firmware.current_version || firmware.version);
    return version ? {value: version, tone: null} : null;
  }

  if (item.status === 'storage') {
    if (storageStatus(snapshot) === 'mounted') {
      return {value: msg('wifi.connected', '已连接'), tone: 'success'};
    }
    return null;
  }

  return null;
}

function entryRow(item, snapshot, navigate, currentPath) {
  const status = statusFor(item, snapshot);
  const active = currentPath === item.route || currentPath.startsWith(`${item.route}/`);
  return row({
    label: item.label,
    value: status ? status.value : null,
    tone: status ? status.tone : null,
    chevron: true,
    extraClass: active ? 'nav__row nav__row--active' : 'nav__row',
    action: `open-${item.id}`,
    onPress: () => navigate(item.route),
  });
}

/**
 * Build the settings list.
 *
 * @param {object|null} snapshot - device snapshot, or null when unavailable.
 * @param {(path: string) => void} navigate
 * @param {string} [currentPath] - path used to highlight the active entry.
 * @returns {Node}
 */
export function settingsList(snapshot, navigate, currentPath = '', localAgent = false) {
  const build = item => entryRow(item, snapshot, navigate, currentPath);
  if (localAgent) return screen([group({rows: LOCAL_ITEMS.map(build)})]);
  return screen([
    group({rows: PRIMARY_ITEMS.map(build)}),
    group({rows: SECONDARY_ITEMS.map(build)}),
  ]);
}
