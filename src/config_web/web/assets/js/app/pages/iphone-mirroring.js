import {request} from '../data.js';
import {el, replace} from '../../ui/dom.js';
import {button} from '../../ui/button.js';
import {group, row} from '../../ui/list.js';
import {msg} from '../../ui/text.js';

export function mirroringConnection(send, changed) {
  const state = {supported: false, busy: false, restored: false, error: null, readError: null};
  let generation = 0;
  const update = patch => {
    const next = {...state, ...patch};
    if (JSON.stringify(next) === JSON.stringify(state)) return;
    Object.assign(state, next);
    changed(state);
  };
  return {
    state,
    async load() {
      if (state.busy) return;
      const current = ++generation;
      try {
        const result = await send('/api/device/iphone-mirroring');
        if (current !== generation) return;
        const supported = result.ok === true && result.supported === true;
        update({supported, readError: null, restored: supported && state.supported && state.restored});
      } catch (error) {
        if (current === generation) update({readError: error.code || 'agent_unavailable', restored: false});
      }
    },
    async reconnect() {
      if (state.busy || !state.supported) return;
      generation++;
      update({busy: true, restored: false, error: null, readError: null});
      try {
        const result = await send('/api/device/iphone-mirroring/reconnect', {
          method: 'POST', headers: {'Content-Type': 'application/json'}, body: '{}',
        });
        if (!result.ok || !result.restored || !result.screenshot_available) {
          update({error: result.code || 'capture_failed'});
        } else update({restored: true});
      } catch (error) {
        update({error: error.code || 'agent_unavailable'});
      } finally {
        update({busy: false});
      }
    },
  };
}

const errors = {
  agent_unavailable: msg('local.mirror_agent_unavailable', '无法连接本机 Agent，请启动 Agent 后重试。'),
  bridge_unavailable: msg('local.mirror_bridge_unavailable', '无法连接镜像桥，请启动 iPhone 镜像桥后重试。'),
  mirror_unavailable: msg('local.mirror_unavailable', '请在 Mac 打开 iPhone 镜像并连接手机，确认录屏权限已开启，然后重试。'),
  input_permission_required: msg('local.mirror_input_permission', '镜像桥缺少辅助功能权限，请在 macOS 系统设置中授权后重试。'),
  session_busy: msg('local.mirror_session_busy', 'iPhone 镜像正被其他会话使用，请结束该会话后重试。'),
  reconnect_busy: msg('local.mirror_busy', '正在恢复连接，请稍候。'),
  not_iphone_mirroring: msg('local.mirror_wrong_mode', '当前 Agent 已不在 iPhone 镜像模式，请刷新页面。'),
  setup_failed: msg('local.mirror_setup_failed', '恢复连接失败，请检查镜像桥运行状态后重试。'),
  capture_failed: msg('local.mirror_capture_failed', '未能取得镜像截图，请确认 iPhone 镜像已连接、录屏权限已开启，然后重试。'),
};

export function iphoneMirroringCard() {
  const element = el('div');
  const model = mirroringConnection(request, state => {
    if (!state.supported) { replace(element, []); return; }
    const error = state.readError || state.error;
    const description = error ? errors[error] || errors.setup_failed
      : state.restored ? msg('local.mirror_restored', '会话已恢复，并已获取镜像画面。')
      : msg('local.mirror_help', '请先在 Mac 上连接 iPhone 镜像。空闲后无法操作时，可在这里恢复连接。');
    replace(element, [group({rows: [
      row({label: msg('local.mirror_title', 'iPhone 镜像'), description}),
      row({label: state.restored ? msg('local.mirror_frame_ready', '镜像画面可用') : null,
        accessory: button({
          label: state.busy ? msg('local.mirror_busy', '正在恢复连接，请稍候。') : msg('local.mirror_reconnect', '恢复 iPhone 镜像连接'),
          variant: 'accent', disabled: state.busy, action: 'reconnect-iphone-mirroring', onPress: () => model.reconnect(),
        })}),
    ]})]);
    element.setAttribute('aria-busy', String(state.busy));
  });
  return {element, refresh: () => model.load()};
}
