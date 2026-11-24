import { useEffect, useRef, useState } from 'react';
import { useAtomValue } from 'jotai';
import type { PageProps, TextSpec } from '../types';
import { businessReleaseVersion, parseOtaLog, shortVersion } from '../lib/ota';
import { waitForReboot } from '../lib/apply';
import { postToHost } from '../lib/host';
import { confirmSheet } from '../components/overlays';
import { msg, resolve } from '../lib/i18n';
import { toast } from '../components/overlays';
import { Button, Group, ResourceError, Row, Text } from '../components/ui';

const stages: Record<string, TextSpec> = {
  check: msg('ota.stage_check', '正在检查更新'),
  download: msg('ota.stage_download', '正在下载'),
  install: msg('ota.stage_install', '正在安装'),
  'package-check': msg('ota.stage_package_check', '正在检查业务包更新'),
  'package-install': msg('ota.stage_package_install', '正在升级业务包'),
  health: msg('ota.stage_health', '正在确认新固件启动状态'),
  reboot: msg('ota.stage_reboot', '即将重启'),
};
const health: Record<string, TextSpec> = {
  success: msg('ota.health_success', '正常'),
  pending: msg('ota.health_pending', '待确认'),
  failed: msg('ota.health_failed', '失败'),
};
const results: Record<string, TextSpec> = {
  running: msg('ota.pill_running', '更新中'),
  rebooting: msg('ota.pill_rebooting', '重启中'),
  failed: msg('ota.pill_failed', '更新失败'),
  completed: msg('ota.pill_completed', '更新完成'),
  up_to_date: msg('ota.pill_latest', '已是最新'),
};
const localTime = (value?: string) => {
  const date = value ? new Date(value) : null;
  return date && !Number.isNaN(date.getTime()) ? date.toLocaleString() : '';
};

export function OTAPage({ model, navigate, logOnly = false }: PageProps & { logOnly?: boolean }) {
  const { data, stale, busy } = useAtomValue(model.resources.ota.atom);
  const snapshot = useAtomValue(model.current);
  const firmware = snapshot.firmware || {};
  const pre = useRef<HTMLPreElement>(null);
  const stick = useRef(true);
  const mounted = useRef(true);
  const watching = useRef(false);
  const oldLog = useRef<string | null>(null);
  const attemptedReboot = useRef<string | null>(null);
  const [rebooting, setRebooting] = useState(false);
  const log = data?.ota_log?.log || data?.ota_health_log?.log || '';
  const parsed = parseOtaLog(data?.ota_log?.log || '');
  const progress =
    data?.hybrid_update_phase === 'waiting-health'
      ? { ...parsed, state: 'running', stage: 'health', percent: null }
      : parsed;
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);
  useEffect(() => {
    if (data?.ota_update_running) watching.current = true;
    if (
      !watching.current ||
      stale ||
      data?.hybrid_update_phase === 'waiting-health' ||
      !['rebooting', 'updated'].includes(progress.state) ||
      oldLog.current === log ||
      attemptedReboot.current === log
    )
      return;
    attemptedReboot.current = log;
    setRebooting(true);
    postToHost({ type: 'aico_config_restart', apply: 'reboot', phase: 'started' });
    waitForReboot()
      .then(() => {
        if (!mounted.current) return;
        postToHost({ type: 'aico_config_restart', apply: 'reboot', phase: 'finished' });
        window.location.reload();
      })
      .catch((error: Error) => {
        if (!mounted.current) return;
        setRebooting(false);
        postToHost({ type: 'aico_config_restart', apply: 'reboot', phase: 'failed' });
        toast(error.message);
      });
  }, [data, progress.state, stale, log]);
  useEffect(() => {
    if (pre.current && stick.current) pre.current.scrollTop = pre.current.scrollHeight;
  }, [log, logOnly]);
  async function start() {
    if (
      !(await confirmSheet({
        title: msg('ota.confirm_title', '检查并安装更新？'),
        body: msg(
          'ota.confirm_body',
          '检查 GitHub 固件和 aico-business 业务包更新。有新固件时先安装并重启，启动确认后继续检查业务包；仅业务包更新时会短暂重启相关服务。',
        ),
        confirmLabel: msg('ota.update', '检查并安装更新'),
        action: 'confirm-ota',
      }))
    )
      return;
    try {
      oldLog.current = log;
      await model.resources.ota.mutate(
        () => model.send('/api/ota/updates', { method: 'POST' }),
        () => ({ ...data, ota_update_running: true }),
      );
      watching.current = true;
    } catch (error) {
      toast(error instanceof Error ? error.message : String(error));
    }
  }
  if (logOnly)
    return (
      <div className="ds-screen ds-screen--sticky">
        <ResourceError model={model} name="ota" />
        <div className="ds-group ds-group--fill">
          <div className="ds-log-frame">
            <pre
              ref={pre}
              className="ds-log"
              data-action="ota-log"
              onScroll={(event) => {
                const node = event.currentTarget;
                stick.current = node.scrollTop + node.clientHeight >= node.scrollHeight - 16;
              }}
            >
              {log || resolve(msg('ota.no_log', '还没有更新记录'))}
            </pre>
          </div>
        </div>
      </div>
    );
  const running = data?.ota_update_running || busy || rebooting;
  const shown = rebooting ? 'rebooting' : running ? 'running' : progress.state;
  const versionRow = (label: TextSpec, value?: string) => (
    <Row
      label={label}
      value={shortVersion(value) || '—'}
      description={value && value !== shortVersion(value) ? value : undefined}
    />
  );
  return (
    <div className="ds-screen">
      <Group caption={msg('ota.current_firmware', '当前固件')}>
        {versionRow(
          msg('ota.row_firmware_version', '固件版本'),
          firmware.current_version || firmware.version,
        )}
        <Row
          label="aico-business"
          value={businessReleaseVersion(firmware.business_version) || '—'}
        />
        {results[shown] && (
          <p role="status">
            <Text value={results[shown]} />
          </p>
        )}
        <ResourceError model={model} name="ota" />
        <ResourceError model={model} name="device" />
        {running ? (
          <div role="status" className="ds-progress">
            <div className="ds-progress__head">
              <Text
                value={
                  rebooting
                    ? msg('ota.rebooting', '更新已安装，设备正在重启…')
                    : stages[progress.stage || 'check']
                }
                className="ds-progress__label"
              />
              {progress.percent != null && <span>{progress.percent}%</span>}
            </div>
            <div className="ds-meter">
              <span
                className={`ds-meter__fill ${progress.percent == null ? 'ds-meter__fill--indeterminate' : ''}`}
                style={progress.percent != null ? { width: `${progress.percent}%` } : undefined}
              />
            </div>
            {progress.item && (
              <p className="ds-progress__detail">
                {progress.item} · {progress.done} / {progress.total}
              </p>
            )}
          </div>
        ) : progress.state === 'failed' ? (
          <p role="alert" className="ds-field__error">
            {progress.error}
          </p>
        ) : progress.state === 'completed' ? (
          <Text
            value={msg('ota.completed', '业务包升级完成；如运行配置要求重启，请稍后重启设备。')}
          />
        ) : progress.state === 'up_to_date' ? (
          <p>{localTime(progress.at)}</p>
        ) : null}
        <div className="ds-card__actions">
          <Button block action="start-ota" disabled={stale || running} onClick={start}>
            <Text value={msg('ota.update', '检查并安装更新')} />
          </Button>
        </div>
      </Group>
      <Group caption={msg('ota.versions', '版本信息')}>
        {(firmware.current_build_time || firmware.build_time) && (
          <Row
            label={msg('ota.build_time', '构建时间')}
            value={localTime(firmware.current_build_time || firmware.build_time)}
          />
        )}
        {versionRow('Boot', firmware.components?.boot)}
        {versionRow('RootFS', firmware.components?.rootfs)}
        <Row
          label={msg('ota.row_slot', '运行分区')}
          value={firmware.running_slot?.toUpperCase() || '—'}
        />
        <Row
          label={msg('ota.health', '启动状态')}
          value={health[firmware.health_status || ''] || firmware.health_status || '—'}
          tone={firmware.health_status === 'failed' ? 'danger' : undefined}
          description={firmware.health_error}
        />
        {firmware.previous_version &&
          versionRow(msg('ota.row_previous_version', '上一版本'), firmware.previous_version)}
      </Group>
      <Group>
        <Row
          label={msg('ota.log', '更新日志')}
          action="open-ota-log"
          chevron
          onPress={() => navigate('/firmware/log')}
        />
      </Group>
    </div>
  );
}
