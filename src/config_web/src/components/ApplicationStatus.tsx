import type { DeviceState } from '../state/device';
import type { ApplicationData } from '../types';
import { useAtomValue } from 'jotai';
import { msg } from '../lib/i18n';
import { toast } from './overlays';
import { postToHost } from '../lib/host';
import { confirmRestart, performRestart } from '../lib/apply';
import { Button, Text } from './ui';

export function ApplicationStatus({ model }: { model: DeviceState }) {
  const { data, stale, busy } = useAtomValue(model.resources.application.atom);
  let label = msg(
    'local.apply_unavailable',
    '暂时无法确认配置状态；Agent 启动后会读取已保存的配置',
  );
  if (!stale && data) {
    if (data.state === 'failed' || data.error || data.ok === false)
      label = msg('local.apply_failed', '配置尚未生效，请检查 Agent 日志');
    else if (data.pending || data.state === 'pending')
      label = msg('local.apply_pending', '配置已保存，正在等待 Agent 应用');
    else if (data.reboot_required || data.state === 'reboot_required')
      label = msg('apply.saved_pending_restart', '已保存，重启后生效');
    else if (data.applied || data.state === 'applied') label = msg('local.applied', '配置已生效');
  }
  const failed = !stale && (data?.state === 'failed' || data?.error || data?.ok === false);
  const retry = () =>
    model.resources.application
      .mutate(() =>
        model.send<ApplicationData>('/api/config', {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ config: {} }),
        }),
      )
      .catch((error: Error) => toast(error.message));
  return (
    <div className="local-application" role="status" aria-live="polite">
      <Text value={label} />
      {failed && (
        <>
          <p>{data.error}</p>
          <Button variant="quiet" disabled={busy} onClick={retry}>
            <Text value={msg('config.apply_retry', '重试应用')} />
          </Button>
        </>
      )}
      {!model.local && !stale && data?.reboot_required && (
        <Button
          variant="quiet"
          onClick={async () => {
            if (await confirmRestart('reboot')) await performRestart('reboot');
          }}
        >
          <Text value={msg('config.reboot_action', '重启设备')} />
        </Button>
      )}
    </div>
  );
}

export function announcePage(title: string) {
  postToHost({
    type: 'aico_config_save_state',
    title,
    dirty: false,
    apply: 'live',
    action_label: '',
  });
}
