import {iphoneMirroringCard} from './iphone-mirroring.js';
import {ensureSnapshot, request} from '../data.js';
import {LOCAL_ITEMS} from '../nav-items.js';
import {el, replace} from '../../ui/dom.js';
import {group, row, screen} from '../../ui/list.js';
import {msg, text} from '../../ui/text.js';

export function applicationLabel(status) {
  if (!status) return msg('local.apply_unavailable', '暂时无法确认配置状态；Agent 启动后会读取已保存的配置');
  if (status.state === 'failed' || status.error) return msg('local.apply_failed', '配置尚未生效，请检查 Agent 日志');
  if (status.pending || status.state === 'pending') return msg('local.apply_pending', '配置已保存，正在等待 Agent 应用');
  if (status.applied && status.state === 'applied') return msg('local.applied', '配置已生效');
  if (status.reboot_required || status.state === 'reboot_required') return msg('local.restart_required', '存在启动配置变更，请重新启动本机 Agent');
  return msg('local.apply_unknown', '配置状态待确认');
}

/** Always visible across routes, including direct locale/provider saves. */
export function localApplicationStatus() {
  const node = el('div', {class: 'local-application', attrs: {role: 'status', 'aria-live': 'polite'}});
  let generation = 0;
  function render(status) {
    replace(node, [text(applicationLabel(status))]);
  }
  async function poll() {
    const current = ++generation;
    const status = await request('/api/config/application').catch(() => null);
    if (current === generation) render(status);
    if (node.isConnected) setTimeout(poll, 2500);
  }
  window.addEventListener('aico:config-saved', event => {
    generation++;
    render(event.detail);
  });
  render(null);
  // The shell attaches this node synchronously before the first timer runs.
  setTimeout(poll, 0);
  return node;
}

export async function localOverviewPage(context) {
  const snapshot = await ensureSnapshot(context);
  const statusHost = el('div');
  const mirroring = iphoneMirroringCard();
  void mirroring.refresh();
  const config = snapshot.config || {};
  const model = config.model || {};
  const provider = (config.model_providers || {})[model.provider];
  const configured = Boolean(provider?.has_api_key);
  const initialStatus = snapshot.agent_status || {};
  const readError = snapshot.snapshot_error;
  if (context.header) context.header({title: msg('local.overview', '运行概览')});

  function showStatus(status) {
    const running = Boolean(status.process_running && status.port_reachable);
    replace(statusHost, [group({rows: [
      row({label: msg('advanced.agent_status', 'Agent 状态'),
        value: running ? msg('advanced.agent_running', '运行中') : msg('local.offline', '无法连接'),
        tone: running ? 'success' : 'danger',
        description: running ? status.url : readError ? msg('local.config_read_unavailable', '请先恢复配置读取，再保存设置。')
          : msg('local.offline_help', '可以保存配置；Agent 启动后会加载它。')}),
      row({label: msg('local.target', '当前目标设备'), value: status.device_type || '—',
        description: msg('local.target_help', '由 Agent 启动参数决定；设备连接状态请在 Agent 界面查看。')}),
    ]})]);
  }
  showStatus(initialStatus);

  const agentLink = initialStatus.url ? el('a', {class: 'local-agent-link', attrs: {
    href: initialStatus.url, target: '_blank', rel: 'noopener noreferrer',
  }}, [text(msg('local.open_agent', '打开 Agent 界面'))]) : null;
  const shortcuts = el('div', {class: 'local-shortcuts'}, [group({rows: LOCAL_ITEMS.filter(item => item.route !== '/').map(item => row({
    label: item.label, chevron: true, action: `open-${item.id}`, onPress: () => context.navigate(item.route),
  }))})]);
  const body = screen([
    el('div', {class: 'basic-intro'}, [
      text(msg('local.title', '本机 Agent'), 'page__title'),
      text(msg('local.intro', '管理模型、对话和运行偏好。设置会保存到本机，并自动应用到正在运行的 Agent。'), 'page__description'),
    ]),
    readError ? group({rows: [row({label: msg('local.config_read_failed', '无法读取配置'), tone: 'danger',
      description: readError.code === 'config_read_failed'
        ? msg('local.config_read_help', '请在配置界面之外修复 TOML 配置文件，然后刷新此页面。') : readError.message})]}) : null,
    !readError && snapshot.config_valid === false ? group({rows: [row({label: msg('local.config_invalid', '配置需要修复'),
      description: (snapshot.config_errors || []).map(error => error.message).join('\n'), tone: 'danger'})]}) : null,
    statusHost,
    mirroring.element,
    group({rows: [
      row({label: msg('local.main_model', '当前主模型'), value: model.model || '—', description: model.provider || null,
        chevron: true, onPress: () => context.navigate('/model/main')}),
      row({label: 'API Key', value: configured ? msg('local.key_ready', '已配置') : msg('local.key_missing', '未配置'),
        tone: configured ? 'success' : 'muted', chevron: true, onPress: () => context.navigate('/model/providers')}),
      row({label: msg('local.config_file', '配置文件'), description: snapshot.paths?.agent_config || '—', descriptionTone: 'code'}),
    ]}),
    agentLink ? el('div', {class: 'ds-group'}, [agentLink]) : null,
    shortcuts,
  ]);
  async function refresh() {
    if (!body.isConnected) return;
    const [payload] = await Promise.all([request('/api/device/status').catch(() => null), mirroring.refresh()]);
    if (body.isConnected) {
      showStatus(payload?.agent_status || {});
      setTimeout(refresh, 5000);
    }
  }
  setTimeout(refresh, 5000);
  return body;
}
