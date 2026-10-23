/**
 * Firmware and business updates share the existing /api/ota/updates job.
 * The systemd worker checks GitHub firmware first, then the signed business
 * APT source. Package-only updates can restart Config Web without a reboot.
 */

import {fetchSnapshot, request, t} from '../data.js';
import {waitForReboot} from '../apply.js';
import {postToHost} from '../host.js';
import {parseOtaLog, shortVersion} from '../ota-progress.js';
import {createSaver} from '../saver.js';
import {button} from '../../ui/button.js';
import {confirmSheet} from '../../ui/confirm.js';
import {el, replace} from '../../ui/dom.js';
import {icon} from '../../ui/icon.js';
import {group, row, screen} from '../../ui/list.js';
import {msg, resolve, text} from '../../ui/text.js';
import {toast} from '../../ui/toast.js';

const POLL_MS = 2000;

const STAGE_LABEL = {
  check: msg('ota.stage_check', '正在检查更新'),
  download: msg('ota.stage_download', '正在下载'),
  install: msg('ota.stage_install', '正在安装'),
  'package-check': msg('ota.stage_package_check', '正在检查业务包更新'),
  'package-install': msg('ota.stage_package_install', '正在升级业务包'),
  health: msg('ota.stage_health', '正在确认新固件启动状态'),
  reboot: msg('ota.stage_reboot', '即将重启'),
};

const HEALTH_LABEL = {
  success: msg('ota.health_success', '正常'),
  pending: msg('ota.health_pending', '待确认'),
  failed: msg('ota.health_failed', '失败'),
};

/** `2026-09-22T06:40:44Z` in the viewer's local time, or '' when unset. */
function localTime(value) {
  const date = value ? new Date(value) : null;
  return date && !Number.isNaN(date.getTime())
    ? date.toLocaleString(undefined, {year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit'})
    : '';
}


async function readStatus() {
  const [ota, snapshot] = await Promise.all([
    request('/api/ota/status'),
    fetchSnapshot().catch(() => ({})),
  ]);
  return {ota, firmware: (snapshot && snapshot.firmware) || {}};
}

export async function otaPage(context) {
  const body = el('div');
  const saver = createSaver(context, {title: msg('ui.nav_firmware', '固件更新'), back: '/', root: body});
  let status = await readStatus().catch(() => ({ota: {}, firmware: {}}));
  let timer = null;
  let rebooting = false;

  const progress = () => {
    const parsed = parseOtaLog((status.ota.ota_log || {}).log);
    // Config Web is available before the boot health service finishes. Avoid
    // following the previous boot's reboot log again during that interval.
    if (status.ota.hybrid_update_phase === 'waiting-health') return {...parsed, state: 'running', stage: 'health', percent: null};
    return parsed;
  };
  // The backend's lock flag is the truth; the log only supplies detail, and a
  // run that died without writing its exit line must not look alive forever.
  const running = () => Boolean(status.ota.ota_update_running);
  // A reboot is followed only for a run this page watched: an old log ending
  // in "reboot requested" is history, not an instruction to wait.
  let watching = running();

  function schedule() {
    clearTimeout(timer);
    timer = null;
    if (!running() || rebooting) return;
    timer = setTimeout(async () => {
      if (!body.isConnected) return;
      try { status = await readStatus(); } catch (_error) {
        // aiden-business stops and restores Config Web during unpack/configure.
        // Keep polling through that temporary loss of the status endpoint.
      }
      render();
      schedule();
    }, POLL_MS);
  }

  async function followReboot() {
    if (rebooting) return;
    rebooting = true;
    render();
    postToHost({type: 'aiden_config_restart', apply: 'reboot', phase: 'started'});
    try {
      await waitForReboot();
      postToHost({type: 'aiden_config_restart', apply: 'reboot', phase: 'finished'});
      window.location.reload();
    } catch (error) {
      rebooting = false;
      postToHost({type: 'aiden_config_restart', apply: 'reboot', phase: 'failed'});
      toast(error && error.message ? error.message : resolve(msg('apply.restart_timeout', '等待重启超时，请检查设备。')), {durationMs: 5000});
      render();
    }
  }

  async function start() {
    const confirmed = await confirmSheet({
      title: msg('ota.confirm_title', '检查并安装更新？'),
      body: msg('ota.confirm_body', '检查 GitHub 固件和 aiden-business 业务包更新。有新固件时先安装并重启，启动确认后继续检查业务包；仅业务包更新时会短暂重启相关服务。'),
      confirmLabel: msg('ota.update', '检查并安装更新'),
      action: 'confirm-ota',
    });
    if (!confirmed) return;
    try {
      await request('/api/ota/updates', {method: 'POST'});
    } catch (error) {
      toast(error && error.message ? error.message : resolve(msg('ota.start_failed', '无法开始更新')), {durationMs: 5000});
      return;
    }
    // The updater appends to the log; give it a moment to write its first line.
    watching = true;
    status = await readStatus().catch(() => status);
    status.ota.ota_update_running = true;
    render();
    schedule();
  }

  function statusPill(state) {
    const map = {
      running: [msg('ota.pill_running', '更新中'), 'accent'],
      rebooting: [msg('ota.pill_rebooting', '重启中'), 'accent'],
      failed: [msg('ota.pill_failed', '更新失败'), 'danger'],
      completed: [msg('ota.pill_completed', '更新完成'), 'success'],
      up_to_date: [msg('ota.pill_latest', '已是最新'), 'success'],
    };
    const entry = map[state];
    return entry ? text(entry[0], `ds-pill ds-pill--${entry[1]}`) : null;
  }

  function progressBlock(p) {
    if (rebooting || p.state === 'rebooting') {
      return el('div', {class: 'ds-progress'}, [
        el('div', {class: 'ds-progress__head'}, [text(msg('ota.rebooting', '更新已安装，设备正在重启…'), 'ds-progress__label')]),
        text(msg('apply.rebooting_body', '连接恢复后页面会自动刷新。'), 'ds-progress__detail'),
      ]);
    }
    const fill = el('span', {class: p.percent == null ? 'ds-meter__fill ds-meter__fill--indeterminate' : 'ds-meter__fill'});
    if (p.percent != null) fill.style.width = `${p.percent}%`;
    const detail = p.item && p.total ? `${p.item} · ${p.done} / ${p.total}` : '';
    return el('div', {class: 'ds-progress'}, [
      el('div', {class: 'ds-progress__head'}, [
        text(STAGE_LABEL[p.stage] || STAGE_LABEL.check, 'ds-progress__label'),
        p.percent != null ? text(`${p.percent}%`, 'ds-progress__percent') : null,
      ]),
      el('div', {class: 'ds-meter'}, [fill]),
      detail ? text(detail, 'ds-progress__detail') : null,
    ]);
  }

  function render() {
    const fw = status.firmware || {};
    const p = progress();
    const busy = running() || rebooting;
    // Show progress for the live run only; a finished run's last line is not progress.
    const live = busy && p.state !== 'failed' && p.state !== 'up_to_date' && p.state !== 'completed';
    const shown = rebooting ? 'rebooting' : busy ? 'running' : p.state;
    const version = fw.current_version || fw.version || '';

    const update = button({label: msg('ota.update', '检查并安装更新'), block: true, action: 'start-ota', onPress: start});
    update.disabled = busy;

    let result = null;
    if (!busy && p.state === 'failed') {
      result = text(t('ota.last_failed', {error: p.error || '—', defaultValue: '上次更新失败：{{error}}'}), 'ds-card__note ds-card__note--danger');
    } else if (!busy && p.state === 'completed') {
      result = text(msg('ota.completed', '业务包升级完成；如运行配置要求重启，请稍后重启设备。'), 'ds-card__note');
    } else if (!busy && p.state === 'up_to_date' && p.at) {
      result = text(t('ota.checked_at', {time: localTime(p.at), defaultValue: '{{time}} 检查过，已是最新版本'}), 'ds-card__note');
    }

    const components = fw.components || {};
    // The short date form fits the row; the full identifier sits underneath.
    const versionRow = (label, value) => row({
      label,
      value: shortVersion(value) || '—',
      description: value && value !== shortVersion(value) ? value : null,
      descriptionTone: 'code',
    });
    const versionRows = [
      versionRow(msg('ota.row_firmware_version', '固件版本'), version),
      fw.current_build_time || fw.build_time
        ? row({label: msg('ota.build_time', '构建时间'), value: localTime(fw.current_build_time || fw.build_time)})
        : null,
      versionRow('Boot', components.boot),
      versionRow('RootFS', components.rootfs),
      fw.running_slot ? row({label: msg('ota.row_slot', '运行分区'), value: String(fw.running_slot).toUpperCase()}) : null,
      fw.health_status
        ? row({label: msg('ota.health', '启动状态'), value: HEALTH_LABEL[fw.health_status] || fw.health_status,
            tone: fw.health_status === 'failed' ? 'danger' : null})
        : null,
      fw.previous_version ? versionRow(msg('ota.row_previous_version', '上一版本'), fw.previous_version) : null,
    ];

    replace(body, [
      group({
        joined: true,
        rows: [
          el('div', {class: 'ds-product'}, [
            el('span', {class: 'ds-product__tile', attrs: {'aria-hidden': 'true'}}, [icon('chip', {size: 22})]),
            el('div', {class: 'ds-product__body'}, [
              text(msg('ota.current_firmware', '当前固件'), 'ds-product__name'),
              text(shortVersion(version) || '—', 'ds-product__version'),
            ]),
            statusPill(shown),
          ]),
          live ? progressBlock(p) : null,
          result,
          el('div', {class: 'ds-card__actions'}, [update]),
        ],
      }),
      group({caption: msg('ota.versions', '版本信息'), rows: versionRows}),
      group({rows: [row({
        label: msg('ota.log', '更新日志'),
        chevron: true,
        action: 'open-ota-log',
        onPress: () => context.navigate('/firmware/log'),
      })]}),
    ]);
    saver.refreshChrome();
    if (watching && !rebooting && (p.state === 'rebooting' || p.state === 'updated')) followReboot();
  }

  render();
  schedule();
  return screen([body]);
}

/** The updater's raw log, newest at the bottom, refreshed while a run is live. */
export async function otaLogPage(context) {
  const body = el('div', {class: 'page__fill'});
  const saver = createSaver(context, {title: msg('ota.log', '更新日志'), back: '/firmware', root: body});
  const pre = el('pre', {class: 'ds-log', data: {action: 'ota-log'}});
  let timer = null;

  async function load() {
    const status = await request('/api/ota/status').catch(() => ({}));
    const log = ((status.ota_log || {}).log || (status.ota_health_log || {}).log || '').trim();
    const stick = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 16;
    pre.textContent = log || resolve(msg('ota.no_log', '还没有更新记录'));
    if (stick) pre.scrollTop = pre.scrollHeight;
    clearTimeout(timer);
    if (status.ota_update_running && body.isConnected) timer = setTimeout(load, POLL_MS);
  }

  replace(body, [el('div', {class: 'ds-group ds-group--fill'}, [el('div', {class: 'ds-log-frame'}, [pre])])]);
  saver.refreshChrome();
  await load();
  pre.scrollTop = pre.scrollHeight;
  return screen([body], 'ds-screen--sticky');
}
