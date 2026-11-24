import { request } from '../data.js';
import { button } from '../../ui/button.js';
import { confirmSheet } from '../../ui/confirm.js';
import { el, replace } from '../../ui/dom.js';
import { group, row } from '../../ui/list.js';
import { sheet } from '../../ui/sheet.js';
import { switchControl } from '../../ui/switch.js';
import { msg, resolve, text } from '../../ui/text.js';
import { toast } from '../../ui/toast.js';

const API = '/api/mcp/servers';

export function mcpServersCard() {
  const element = el('div');
  let servers = [];
  let error = '';
  let busy = false;
  let refreshGeneration = 0;

  async function refresh() {
    const generation = ++refreshGeneration;
    try {
      const result = await request(API, { cache: 'no-store' });
      if (generation !== refreshGeneration) return;
      servers = result.servers || [];
      error = '';
    } catch (failure) {
      if (generation !== refreshGeneration) return;
      error = failure.message;
    }
    render();
  }

  async function change(server, method, enabled) {
    if (busy) return;
    busy = true;
    render();
    try {
      await request(`${API}/${encodeURIComponent(server.name)}`, {
        method,
        headers: { 'Content-Type': 'application/json' },
        ...(method === 'PATCH' ? { body: JSON.stringify({ enabled }) } : {}),
      });
    } catch (failure) {
      toast(failure.message);
    } finally {
      busy = false;
      await refresh();
    }
  }

  function details(server) {
    const enabled = switchControl({
      checked: server.enabled,
      label: msg('mcp.install_enabled', '启用'),
      onChange: (value) => {
        view.close();
        void change(server, 'PATCH', value);
      },
    });
    const reconnect = button({
      label: msg('mcp.install_reconnect', '重新连接'),
      block: true,
      action: 'reconnect-mcp-server',
      onPress: () => {
        view.close();
        void change(server, 'PATCH', true);
      },
    });
    const remove = button({
      label: msg('mcp.install_remove', '卸载'),
      block: true,
      action: 'uninstall-mcp-server',
      onPress: async () => {
        const confirmed = await confirmSheet({
          title: msg('mcp.install_remove_title', '卸载 MCP？'),
          body: msg('mcp.install_remove_help', 'Agent 将停止使用此服务并删除保存的连接配置。'),
          confirmLabel: msg('mcp.install_remove', '卸载'),
          danger: true,
        });
        if (confirmed) {
          view.close();
          await change(server, 'DELETE');
        }
      },
    });
    const view = sheet({
      title: server.name,
      body: [
        group({
          rows: [
            row({ label: msg('mcp.install_enabled', '启用'), accessory: enabled }),
            ...(server.tools || []).map((name) => row({ label: name, descriptionTone: 'code' })),
          ],
        }),
        el('div', { class: 'page__footer' }, [reconnect, remove]),
      ],
    });
    view.open();
  }

  function install() {
    const input = el('textarea', {
      class: 'ds-input',
      attrs: {
        rows: '12',
        spellcheck: 'false',
        autocomplete: 'off',
        autocapitalize: 'off',
        'aria-label': resolve(msg('mcp.install_config', 'MCP JSON 配置')),
        placeholder: '{"mcpServers":{"example":{"type":"http","url":"https://example.com/mcp"}}}',
      },
    });
    const failure = el('div', { class: 'ds-field__error', attrs: { role: 'alert' } });
    const submit = button({
      label: msg('mcp.install_connect', '安装并连接'),
      variant: 'primary',
      block: true,
      action: 'install-mcp-submit',
    });
    const view = sheet({
      title: msg('mcp.install_add', '安装 MCP'),
      body: [
        text(
          msg(
            'mcp.install_paste',
            '粘贴服务提供方的 mcpServers JSON 配置。支持 stdio 和 Streamable HTTP；命令及其依赖须已安装在 Agent 所在设备。',
          ),
          'ds-group__note',
        ),
        text(
          msg('mcp.install_trust', '仅安装你信任的服务。stdio 命令会以 Agent 的权限在设备上运行。'),
          'ds-group__note',
        ),
        input,
        failure,
        el('div', { class: 'page__footer' }, [submit]),
      ],
    });
    submit.addEventListener('click', async () => {
      submit.disabled = true;
      failure.textContent = '';
      try {
        let config;
        try {
          config = JSON.parse(input.value);
        } catch (_) {
          throw new Error(resolve(msg('mcp.install_invalid', '请输入有效的 JSON 配置')));
        }
        await request(API, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(config),
        });
        input.value = '';
        view.close();
        toast(msg('mcp.install_done', '已安装，Agent 可在下次请求中使用这些工具'));
        await refresh();
      } catch (error) {
        failure.textContent = error.message;
      } finally {
        submit.disabled = false;
      }
    });
    view.open();
  }

  function render() {
    const stateLabel = (server) => {
      if (server.state === 'connected') return msg('mcp.install_connected', '已连接');
      if (!server.enabled) return msg('mcp.install_disabled', '已停用');
      if (server.state === 'error') return msg('mcp.install_error', '连接失败');
      return msg('mcp.install_disconnected', '待连接');
    };
    replace(element, [
      group({
        caption: msg('mcp.install_title', '给 Agent 安装 MCP'),
        rows: [
          ...servers.map((server) =>
            row({
              label: server.name,
              value: stateLabel(server),
              tone:
                server.state === 'error'
                  ? 'danger'
                  : server.state === 'connected'
                    ? 'success'
                    : 'muted',
              description:
                server.error || `${server.transport} · ${(server.tools || []).length} tools`,
              chevron: true,
              onPress: busy ? undefined : () => details(server),
            }),
          ),
          error
            ? row({
                label: msg('mcp.install_unavailable', '无法读取 MCP 服务'),
                description: error,
                tone: 'danger',
              })
            : null,
          row({
            label: msg('mcp.install_add', '安装 MCP'),
            labelTone: 'accent',
            action: 'install-mcp-server',
            onPress: busy ? undefined : install,
          }),
          row({
            label: msg('mcp.install_refresh', '刷新连接状态'),
            action: 'refresh-mcp-servers',
            onPress: busy ? undefined : refresh,
          }),
        ],
        note: msg(
          'mcp.install_help',
          '安装后 Agent 可调用服务提供的工具。点击服务可查看工具、启停、重连或卸载。',
        ),
      }),
    ]);
  }
  render();
  return { element, refresh };
}
