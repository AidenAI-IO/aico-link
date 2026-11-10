import {request} from '../data.js';
import {el, replace} from '../../ui/dom.js';
import {group, row} from '../../ui/list.js';
import {switchControl} from '../../ui/switch.js';
import {msg, resolve} from '../../ui/text.js';
import {toast} from '../../ui/toast.js';

const endpoint = '/api/network/usb-internet';

// Save failures keep the last confirmed state. A read started before a save
// must not overwrite its result when requests complete out of order.
export function usbInternetState(send, changed) {
  let state = null;
  let busy = false;
  let version = 0;
  return {
    get state() { return state; },
    get busy() { return busy; },
    async load() {
      if (busy) return;
      const revision = version;
      try {
        const result = await send(endpoint, {cache: 'no-store'});
        if (revision === version) {
          const different = state === null || ['enabled', 'active', 'host', 'via_usb'].some(key => state[key] !== result[key]);
          state = result;
          if (different) changed();
        }
      } catch (error) {
        if (revision === version && state !== null) { state = null; changed(); }
        throw error;
      }
    },
    async save(enabled) {
      if (busy || !state?.via_usb) return;
      version += 1;
      busy = true;
      changed();
      try {
        state = await send(endpoint, {method: 'PUT', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({enabled})});
      } finally {
        busy = false;
        changed();
      }
    },
  };
}

export function usbInternetSettings() {
  const root = el('div');
  const model = usbInternetState(request, render);
  function render() {
    const state = model.state;
    const control = switchControl({
      checked: state?.enabled === true,
      label: msg('usb_internet.title', 'USB 联网'),
      onChange: enabled => model.save(enabled).catch(error => toast(error.message || resolve(msg('usb_internet.save_failed', '保存失败')))),
    });
    control.disabled = model.busy || !state?.via_usb;
    const status = state == null ? msg('usb_internet.unavailable', '无法读取状态')
      : !state.enabled ? msg('usb_internet.off', '已关闭')
        : state.active ? (state.host === 'android' ? msg('usb_internet.android', '通过 Android 联网') : msg('usb_internet.mac', '通过 Mac 联网'))
          : msg('usb_internet.waiting', '等待主机共享网络');
    replace(root, [group({rows: [row({
      label: msg('usb_internet.title', 'USB 联网'),
      value: status,
      description: msg('usb_internet.help', 'macOS 需运行 scripts/usb-internet-macos.sh start；Android 需使用 Aico App；不支持 iOS。'),
      accessory: control,
      action: 'toggle-usb-internet',
    })], note: state && !state.via_usb ? msg('lan.usb_only', '只能在通过 USB 连接时修改这些设置。') : null})]);
  }
  function schedule() {
    setTimeout(async () => {
      if (!root.isConnected) return;
      await model.load().catch(() => {});
      schedule();
    }, 5000);
  }
  render();
  model.load().catch(() => {});
  schedule();
  return root;
}
