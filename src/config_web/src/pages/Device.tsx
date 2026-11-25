import type { FormEvent } from 'react';
import type { DeviceState } from '../state/device';
import type { PageProps, MirroringData, OperationResult } from '../types';
import { useEffect, useState } from 'react';
import { useAtomValue } from 'jotai';
import { Modal, confirmSheet } from '../components/overlays';
import { msg, resolve, t } from '../lib/i18n';
import { toast } from '../components/overlays';
import { isEmbedded } from '../lib/environment';
import { CONTAINER_ITEMS, LOCAL_ITEMS } from '../app/navItems';
import { terminalUrl } from './Advanced';
import { ChoiceRow } from '../components/choices';
import { FILESYSTEMS, BackupPanel } from './Backup';
import { MCPTokensPanel } from './MCP';
import { Button, Group, ResourceError, Row, Switch, Text } from '../components/ui';
import { Loading } from '../components/Loading';
import { isInitialLoading } from '../state/resource';

const report = (error: unknown) => toast(error instanceof Error ? error.message : String(error));
const json = (body: unknown) => ({
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify(body),
});

export function USBInternet({ model }: { model: DeviceState }) {
  const resource = useAtomValue(model.resources.usb.view);
  const { data, stale, busy } = resource;
  const loading = isInitialLoading(resource);
  const label = msg('usb_internet.title', 'USB 联网');
  const status = stale
    ? msg('usb_internet.unavailable', '无法读取状态')
    : !data?.enabled
      ? msg('usb_internet.off', '已关闭')
      : data.active
        ? data.host === 'android'
          ? msg('usb_internet.android', '通过 Android 联网')
          : msg('usb_internet.mac', '通过 Mac 联网')
        : msg('usb_internet.waiting', '等待主机共享网络');
  return (
    <Group note={data && !data.via_usb ? msg('lan.usb_only', '只能通过 USB 连接修改') : undefined}>
      <Row label={label} description={loading ? undefined : status} loading={loading}>
        <Switch
          checked={data?.enabled}
          label={label}
          disabled={busy || stale || !data?.via_usb}
          action="toggle-usb-internet"
          onChange={(enabled) =>
            model.resources.usb
              .mutate(() =>
                model.send<import('../types').USBData>('/api/network/usb-internet', {
                  method: 'PUT',
                  ...json({ enabled }),
                }),
              )
              .catch(report)
          }
        />
      </Row>
      <Text
        value={msg(
          'usb_internet.help',
          'Android：打开 Aico App。\nMac：运行 scripts/usb-internet-macos.sh start\niOS：暂不支持。',
        )}
        className="ds-group__note ds-usb-internet-help"
      />
    </Group>
  );
}

export function AdvancedPage({ model, navigate }: PageProps) {
  const snapshot = useAtomValue(model.current);
  const lanResource = useAtomValue(model.resources.lan.view);
  const { data: lan, stale } = lanResource;
  const config = useAtomValue(model.resources.snapshot.view);
  return (
    <div className="ds-screen">
      <Group>
        <Row
          label={msg('advanced.logs', '日志')}
          description={msg('advanced.logs_hint', '管理日志记录和导出')}
          chevron
          onPress={() => navigate('/advanced/logs')}
        />
        <Row
          label={msg('advanced.manual_config', '手动编辑配置')}
          description={msg('advanced.manual_config_hint', '编辑 Agent TOML 配置文件')}
          chevron
          onPress={() => navigate('/advanced/config')}
        />
        <Row
          label={msg('advanced.terminal', '终端')}
          description={msg('advanced.terminal_hint', '设备命令行')}
          chevron
          onPress={() => {
            if (isEmbedded()) window.location.href = terminalUrl();
            else window.open(terminalUrl(), '_blank', 'noopener');
          }}
        />
        <Row
          label={msg('lan.title', '局域网访问')}
          loading={isInitialLoading(lanResource)}
          value={
            stale
              ? msg('status.state.unknown', '未知')
              : lan?.enabled
                ? msg('lan.on', '已开启')
                : msg('lan.off', '已关闭')
          }
          chevron
          onPress={() => navigate('/advanced/lan')}
        />
        <Row
          label={msg('lan.password', '设备密码')}
          loading={isInitialLoading(lanResource)}
          value={
            stale
              ? msg('status.state.unknown', '未知')
              : lan?.password_set
                ? msg('lan.password_set', '已设置')
                : msg('lan.password_unset', '未设置')
          }
          chevron
          onPress={() => navigate('/advanced/password')}
        />
        <Row
          label="MCP"
          loading={!snapshot.config && isInitialLoading(config)}
          value={
            !snapshot.config
              ? msg('status.state.unknown', '未知')
              : snapshot.config.agent?.mcp_enabled === false
                ? msg('lan.off', '已关闭')
                : msg('lan.on', '已开启')
          }
          chevron
          onPress={() => navigate('/advanced/mcp')}
        />
      </Group>
      <USBInternet model={model} />
    </div>
  );
}

export function StoragePage({ model }: PageProps) {
  const resource = useAtomValue(model.resources.storage.view);
  const { data, stale, busy } = resource;
  const loading = isInitialLoading(resource);
  const [fs, setFS] = useState('fat32');
  const status = data || {};
  const internal = status.internal || {};
  const card = status.card || {};
  const formatting = status.format_job?.status === 'running';
  const mode =
    status.effective_mode === 1
      ? msg('storage.mode.emmc', '仅 eMMC')
      : status.effective_mode === 2
        ? msg('storage.mode.dual', '双存储')
        : msg('storage.mode.auto', '自动');
  const size = (value?: number) =>
    Number.isFinite(value) ? `${((value || 0) / 1e9).toFixed(1)} GB` : '—';
  async function operation(kind: 'format' | 'eject') {
    const format = kind === 'format';
    const title = format
      ? msg('storage.format_confirm_title', '格式化存储卡？')
      : msg('storage.eject_confirm_title', '安全弹出存储卡？');
    if (
      !(await confirmSheet({
        title,
        body: format
          ? `${resolve(msg('storage.format_warning', '此操作会清除存储卡上的所有数据。'))} ${fs.toUpperCase()}`
          : msg('storage.eject_confirm', '同步并卸载 SD 卡，以便安全取出？'),
        confirmLabel: format
          ? msg('storage.format_action', '格式化')
          : msg('storage.safe_eject', '安全弹出'),
        danger: format,
        action: `confirm-${kind}`,
      }))
    )
      return;
    try {
      const result = await model.resources.storage.mutate(
        () =>
          model.send<OperationResult>(`/api/storage/${kind}`, {
            method: 'POST',
            ...json(format ? { fs, confirm: 'format-sd-card' } : {}),
          }),
        () => null,
      );
      if (result.warning_code === 'storage_flush_failed')
        toast(
          msg('storage.ejected_with_warning', '存储卡已弹出，可以取出；部分待写入数据可能未保存。'),
        );
    } catch (error) {
      report(error);
    }
  }
  const used = (internal.total_bytes || 0) - (internal.free_bytes || 0);
  return (
    <div className="ds-screen">
      <Group
        caption={msg('storage.status_caption', '存储状态')}
        captionAction={
          <button
            type="button"
            className="ds-caption-action"
            data-action="refresh-storage"
            disabled={busy}
            onClick={() => model.resources.storage.refresh()}
          >
            <Text value={msg('storage.refresh', '刷新状态')} />
          </button>
        }
      >
        {internal.available && (
          <div className="ds-meter-block">
            <div className="ds-meter">
              <div
                className="ds-meter__fill"
                style={{
                  width: `${Math.max(0, Math.min(100, (used / (internal.total_bytes || 1)) * 100))}%`,
                }}
              />
            </div>
            <div className="ds-meter__legend">
              <span>{t('storage.used', { size: size(used) })}</span>
              <span>{t('storage.remaining', { size: size(internal.free_bytes) })}</span>
            </div>
          </div>
        )}
        <Row
          label={msg('storage.total_space', '总空间')}
          value={size(internal.total_bytes)}
          loading={loading}
        />
        <Row
          label={msg('storage.available_space', '可用空间')}
          value={size(internal.free_bytes)}
          loading={loading}
        />
      </Group>
      <ResourceError model={model} name="storage" />
      <Group caption={msg('storage.microsd_caption', 'microSD 设置')}>
        <Row
          label={msg('storage.running_mode', '运行模式')}
          loading={loading}
          value={
            stale
              ? msg('status.state.unknown', '未知')
              : `${resolve(mode)} — ${resolve(card.mounted ? msg('storage.card_state_mounted', 'SD 卡已挂载') : card.present ? msg('storage.card_state_unusable', 'SD 卡不可用') : msg('storage.card_state_none', '未检测到 SD 卡'))}`
          }
        />
        <ChoiceRow
          label={msg('storage.format_as', '格式化为')}
          options={FILESYSTEMS}
          selected={fs}
          onSelect={setFS}
          disabled={busy || formatting}
        />
        {formatting && (
          <p role="status">
            <Text value={msg('storage.format_in_progress', '正在格式化存储卡…')} />
          </p>
        )}
        {status.format_job?.status === 'failed' && (
          <p role="alert" className="ds-field__error">
            {status.format_job.error || resolve(msg('storage.format_failed', '格式化失败'))}
          </p>
        )}
        <div className="ds-button-pair">
          <Button
            variant="danger-soft"
            block
            className="ds-btn ds-btn--danger-soft ds-btn--block ds-button-pair__item"
            action="format-card"
            disabled={stale || busy || formatting || !card.present}
            onClick={() => operation('format')}
          >
            <Text value={msg('storage.format_card', '格式化存储卡')} />
          </Button>
          <Button
            variant="accent-outline"
            block
            className="ds-btn ds-btn--accent-outline ds-btn--block ds-button-pair__item"
            action="eject-card"
            disabled={stale || busy || formatting || !card.mounted}
            onClick={() => operation('eject')}
          >
            <Text value={msg('storage.safe_eject', '安全弹出')} />
          </Button>
        </div>
        {status.last_warning && (
          <Text
            value={msg(
              'storage.last_flush_warning',
              '此前存储卡同步失败，部分待写入数据可能未保存。',
            )}
            className="ds-card__note ds-card__note--danger"
          />
        )}
      </Group>
      <BackupPanel model={model} />
    </div>
  );
}

export function MCPPage({ model, navigate, header }: PageProps) {
  const resource = useAtomValue(model.resources.mcp.view);
  const { data, stale, busy } = resource;
  const [draft, setDraft] = useState('');
  const [error, setError] = useState('');
  const [installing, setInstalling] = useState(false);
  const [detailName, setDetailName] = useState<string | null>(null);
  const detail = data?.servers?.find((server) => server.name === detailName);
  async function change(name: string, method: 'PATCH' | 'DELETE', enabled?: boolean) {
    if (
      method === 'DELETE' &&
      !(await confirmSheet({
        title: name,
        body: msg('mcp.install_remove_help', 'Agent 将停止使用此服务并删除保存的连接配置。'),
        confirmLabel: msg('mcp.install_remove', '卸载'),
        danger: true,
      }))
    )
      return;
    try {
      await model.resources.mcp.mutate(
        () =>
          model.send(`/api/mcp/servers/${encodeURIComponent(name)}`, {
            method,
            ...(method === 'PATCH' ? json({ enabled }) : {}),
          }),
        () => null,
      );
      if (method === 'DELETE') setDetailName(null);
    } catch (failure) {
      report(failure);
    }
  }
  async function install(event: FormEvent) {
    event.preventDefault();
    setError('');
    try {
      const value = JSON.parse(draft);
      await model.resources.mcp.mutate(
        () => model.send('/api/mcp/servers', { method: 'POST', ...json(value) }),
        () => null,
      );
      setDraft('');
      setInstalling(false);
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : String(failure));
    }
  }
  return (
    <div className="ds-screen">
      <Group
        caption={msg('mcp.install_title', '给 Agent 安装 MCP')}
        note={msg(
          'mcp.install_help',
          '安装后 Agent 可调用服务提供的工具。点击服务可查看工具、启停、重连或卸载。',
        )}
      >
        {isInitialLoading(resource) && <Loading block />}
        {(data?.servers || []).map((server) => (
          <Row
            key={server.name}
            label={server.name}
            value={
              stale
                ? msg('status.state.unknown', '未知')
                : server.state === 'connected'
                  ? msg('mcp.install_connected', '已连接')
                  : !server.enabled
                    ? msg('mcp.install_disabled', '已停用')
                    : server.state === 'error'
                      ? msg('mcp.install_error', '连接失败')
                      : msg('mcp.install_disconnected', '待连接')
            }
            tone={
              server.state === 'error'
                ? 'danger'
                : server.state === 'connected'
                  ? 'success'
                  : 'muted'
            }
            description={
              server.error || `${server.transport || ''} · ${(server.tools || []).length} tools`
            }
            chevron
            onPress={busy ? undefined : () => setDetailName(server.name)}
          />
        ))}
        <ResourceError model={model} name="mcp" />
        <Row
          label={msg('mcp.install_add', '安装 MCP')}
          labelTone="accent"
          action="install-mcp-server"
          onPress={busy ? undefined : () => setInstalling(true)}
        />
        <Row
          label={msg('mcp.install_refresh', '刷新连接状态')}
          action="refresh-mcp-servers"
          onPress={busy ? undefined : () => void model.resources.mcp.refresh()}
        />
      </Group>
      {detail && (
        <Modal title={detail.name} onClose={() => setDetailName(null)} busy={busy}>
          <Group>
            <Row label={msg('mcp.install_enabled', '启用')}>
              <Switch
                label={msg('mcp.install_enabled', '启用')}
                checked={detail.enabled}
                disabled={busy || stale}
                onChange={(enabled) => change(detail.name, 'PATCH', enabled)}
              />
            </Row>
            {(detail.tools || []).map((tool) => (
              <Row key={tool} label={tool} descriptionTone="code" />
            ))}
          </Group>
          <div className="page__footer">
            <Button
              block
              disabled={busy || stale}
              onClick={() => change(detail.name, 'PATCH', true)}
            >
              <Text value={msg('mcp.install_reconnect', '重新连接')} />
            </Button>
            <Button block disabled={busy || stale} onClick={() => change(detail.name, 'DELETE')}>
              <Text value={msg('mcp.install_remove', '卸载')} />
            </Button>
          </div>
        </Modal>
      )}
      {installing && (
        <Modal
          title={msg('mcp.install_add', '安装 MCP')}
          onClose={() => setInstalling(false)}
          busy={busy}
        >
          <form onSubmit={install}>
            <Text
              value={msg(
                'mcp.install_paste',
                '粘贴服务提供方的 mcpServers JSON 配置。支持 stdio 和 Streamable HTTP；命令及其依赖须已安装在 Agent 所在设备。',
              )}
              className="ds-group__note"
            />
            <Text
              value={msg(
                'mcp.install_trust',
                '仅安装你信任的服务。stdio 命令会以 Agent 的权限在设备上运行。',
              )}
              className="ds-group__note"
            />
            <textarea
              className="ds-input"
              rows={12}
              value={draft}
              disabled={busy}
              onChange={(event) => setDraft(event.target.value)}
              autoComplete="off"
              spellCheck={false}
              aria-label={resolve(msg('mcp.install_config', 'MCP JSON 配置'))}
              placeholder='{"mcpServers":{"example":{"type":"http","url":"https://example.com/mcp"}}}'
            />
            {error && (
              <p role="alert" className="ds-field__error">
                {error}
              </p>
            )}
            <div className="page__footer">
              <Button
                type="submit"
                block
                disabled={busy || !draft.trim()}
                action="install-mcp-submit"
              >
                <Text value={msg('mcp.install_connect', '安装并连接')} />
              </Button>
            </div>
          </form>
        </Modal>
      )}
      {!model.local && (
        <MCPTokensPanel
          model={model}
          navigate={navigate}
          header={header}
          route={{ path: '/advanced/mcp' }}
        />
      )}
    </div>
  );
}

export function LocalOverview({ model, navigate }: PageProps) {
  const snapshot = useAtomValue(model.current);
  const device = useAtomValue(model.resources.device.view);
  const mirror = useAtomValue(model.resources.mirroring.view);
  const config = useAtomValue(model.resources.snapshot.view);
  const [restored, setRestored] = useState(false);
  const status = device.data?.agent_status || {};
  const provider = snapshot.config?.model?.provider;
  useEffect(() => {
    if (mirror.stale || !mirror.data?.supported) setRestored(false);
  }, [mirror.stale, mirror.data?.supported]);
  return (
    <div className="ds-screen">
      <div className="basic-intro">
        <Text value={msg('local.title', '本机 Agent')} className="page__title" />
        <Text
          value={msg(
            'local.intro',
            '管理模型、对话和运行偏好。设置会保存到本机，并自动应用到正在运行的 Agent。',
          )}
          className="page__description"
        />
      </div>
      <ResourceError model={model} name="snapshot" />
      {snapshot.snapshot_error && (
        <Button onClick={() => model.resources.snapshot.refresh()}>
          <Text value={msg('ui.retry', '重试')} />
        </Button>
      )}
      <Group>
        <Row
          label={msg('advanced.agent_status', 'Agent 状态')}
          loading={isInitialLoading(device)}
          value={
            device.stale
              ? msg('status.state.unknown', '未知')
              : status.process_running && status.port_reachable
                ? msg('advanced.agent_running', '运行中')
                : msg('local.offline', '无法连接')
          }
          description={
            status.url ||
            (device.stale
              ? undefined
              : msg('local.offline_help', '可以保存配置；Agent 启动后会加载它。'))
          }
        />
        <Row
          label={msg('local.target', '当前目标设备')}
          value={status.device_type || '—'}
          loading={isInitialLoading(device)}
        />
      </Group>
      {mirror.data?.supported && (
        <Group>
          <Row
            label={msg('local.mirror_title', 'iPhone 镜像')}
            value={restored ? msg('local.mirror_restored', '会话已恢复，并已获取镜像画面。') : null}
          />
          <ResourceError model={model} name="mirroring" />
          <Button
            disabled={mirror.busy || mirror.stale}
            onClick={async () => {
              setRestored(false);
              try {
                const result = await model.resources.mirroring.mutate(
                  () =>
                    model.send<MirroringData>('/api/device/iphone-mirroring/reconnect', {
                      method: 'POST',
                      ...json({}),
                    }),
                  () => null,
                );
                if (!result.ok || !result.restored || !result.screenshot_available)
                  throw new Error(result.code || 'capture_failed');
                setRestored(true);
              } catch (error) {
                report(error);
              }
            }}
          >
            <Text value={msg('local.mirror_reconnect', '恢复 iPhone 镜像连接')} />
          </Button>
        </Group>
      )}
      {snapshot.config_valid === false && (
        <p role="alert">{snapshot.config_errors?.map((error) => error.message).join('\n')}</p>
      )}
      <Group>
        <Row
          label={msg('local.main_model', '当前主模型')}
          loading={!snapshot.config && isInitialLoading(config)}
          value={snapshot.config?.model?.model || '—'}
          description={provider}
          chevron
          onPress={() => navigate('/model/main')}
        />
        <Row
          label="API Key"
          loading={!snapshot.config && isInitialLoading(config)}
          value={
            !snapshot.config
              ? msg('status.state.unknown', '未知')
              : snapshot.config.model_providers?.[provider || '']?.has_api_key
                ? msg('local.key_ready', '已配置')
                : msg('local.key_missing', '未配置')
          }
          chevron
          onPress={() => navigate('/model/providers')}
        />
        <Row
          label={msg('local.config_file', '配置文件')}
          description={snapshot.paths?.agent_config}
        />
      </Group>
      {status.url && (
        <a
          className="local-agent-link"
          data-action="open-agent"
          href={status.url}
          target="_blank"
          rel="noreferrer"
        >
          <Text value={msg('local.open_agent', '打开 Agent 界面')} />
        </a>
      )}
      {model.container && status.terminal_url && (
        <a
          className="local-agent-link"
          data-action="open-terminal"
          href={status.terminal_url}
          target="_blank"
          rel="noreferrer"
        >
          <Text value={msg('local.open_terminal', '打开容器终端')} />
        </a>
      )}
      <div className="local-shortcuts">
        <Group>
          {(model.container ? CONTAINER_ITEMS : LOCAL_ITEMS)
            .filter((item) => item.route !== '/')
            .map((item) => (
              <Row key={item.id} label={item.label} chevron onPress={() => navigate(item.route)} />
            ))}
        </Group>
      </div>
    </div>
  );
}
