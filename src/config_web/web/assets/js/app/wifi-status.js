/** One lightweight Wi-Fi read at a time, shared by the shell and its page. */
import {request} from '../config/api.js';

const INTERVAL_MS = 1000;
const REQUEST_TIMEOUT_MS = 4000;

export function watchWiFi(changed, {isActive = () => true} = {}) {
  let stopped = false;
  let timer;
  let pending;
  let revision = 0;
  let signature = '';
  let immediate = false;

  function active() {
    if (!stopped && !isActive()) stop();
    return !stopped;
  }

  function publish(payload, source = 'poll') {
    const status = payload?.wifi_status;
    if (!status || typeof status.connected !== 'boolean' || typeof status.state !== 'string' || !status.state ||
        (status.connected && (typeof status.ssid !== 'string' || !status.ssid))) {
      payload = {wifi_status: {connected: false, ssid: '', ip_address: '', state: 'UNKNOWN'}};
    }
    const value = {wifi_status: payload.wifi_status};
    if (payload.wifi) value.wifi = payload.wifi;
    const next = JSON.stringify(value);
    // Task publications also establish ordering against pending navigation reads.
    if (next === signature && source !== 'task') return;
    signature = next;
    changed(value, {source});
  }

  async function poll() {
    clearTimeout(timer);
    if (!active() || document.hidden || pending) return;
    immediate = false;
    const version = revision;
    const controller = new AbortController();
    pending = controller;
    const timeout = setTimeout(() => controller.abort(), REQUEST_TIMEOUT_MS);
    try {
      const payload = await request('/api/network/wifi/status', {cache: 'no-store', signal: controller.signal});
      if (active() && version === revision) publish(payload);
    } catch (_error) {
      if (active() && version === revision && !document.hidden) {
        // A lost HTTP connection cannot establish the radio's state.
        publish({wifi_status: {connected: false, ssid: '', ip_address: '', state: 'UNKNOWN'}});
      }
    } finally {
      clearTimeout(timeout);
      pending = null;
      if (active() && !document.hidden) timer = setTimeout(poll, immediate ? 0 : INTERVAL_MS);
    }
  }

  function refresh() {
    if (!active()) return;
    revision += 1;
    immediate = true;
    clearTimeout(timer);
    if (pending) pending.abort();
    else timer = setTimeout(poll, 0);
  }

  function onResult(event) {
    if (!active()) return;
    revision += 1;
    if (event.detail?.wifi_status) publish(event.detail, 'task');
    refresh();
  }

  function onVisibility() {
    if (!document.hidden) refresh();
    else {
      revision += 1;
      clearTimeout(timer);
      if (pending) pending.abort();
    }
  }

  document.addEventListener('visibilitychange', onVisibility);
  window.addEventListener('online', refresh);
  window.addEventListener('aico:wifi-changed', onResult);
  refresh();

  function stop() {
    stopped = true;
    clearTimeout(timer);
    if (pending) pending.abort();
    document.removeEventListener('visibilitychange', onVisibility);
    window.removeEventListener('online', refresh);
    window.removeEventListener('aico:wifi-changed', onResult);
  }
  // A fresh aggregate snapshot supersedes all reads started before it settled.
  stop.refresh = () => { signature = ''; refresh(); };
  return stop;
}
