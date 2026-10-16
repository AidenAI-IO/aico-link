/**
 * Storage & backup route.
 *
 *   - Storage status: eMMC usage from `GET /api/storage/status`.
 *   - microSD: the running mode, the file system to format with, and the two
 *     card actions (`POST /api/storage/format`, `POST /api/storage/eject`).
 *     A running format job is polled until it finishes.
 *   - Backup & restore: the full data backup (memory, sessions, device
 *     identity, SD user files) through the maintenance job API, driven by
 *     `backup-engine.js`; and, separately, the Agent's TOML configuration
 *     alone (`GET/PUT /api/config/backup`).
 *
 * Nothing here is a setting to save, so the page has no save button; each
 * action confirms through the shared bottom sheet and runs at once.
 */

import {createBackupEngine, errorText, formatBytes} from '../backup-engine.js';
import {request, t} from '../data.js';
import {isEmbedded} from '../../ui/environment.js';
import {postToHost} from '../host.js';
import {createSaver} from '../saver.js';
import {button} from '../../ui/button.js';
import {choiceSheet} from '../../ui/choice-sheet.js';
import {confirmSheet} from '../../ui/confirm.js';
import {el, replace} from '../../ui/dom.js';
import {group, row, screen} from '../../ui/list.js';
import {msg, resolve, text} from '../../ui/text.js';
import {toast} from '../../ui/toast.js';

export const FILESYSTEMS = [
  {value: 'fat32', label: msg('storage.fs.fat32', 'FAT32（电脑 / 手机可读取）')},
  {value: 'exfat', label: msg('storage.fs.exfat', 'exFAT（适合大容量卡和文件，电脑 / 手机可读取）')},
  {value: 'ext4', label: msg('storage.fs.ext4', 'ext4（适合设备本地使用）')},
];

const FORMAT_POLL_MS = 3000;

export function gb(bytes) {
  return Number.isFinite(bytes) ? `${(bytes / 1e9).toFixed(1)} GB` : '—';
}

/** `effective_mode` as the classic page names it (storage.js `storageModeName`). */
function modeName(mode) {
  if (mode === 1) return msg('storage.mode.emmc', '仅 eMMC');
  if (mode === 2) return msg('storage.mode.dual', '双存储');
  return msg('storage.mode.auto', '自动');
}

function cardState(status) {
  const card = status.card || {};
  if (card.mounted) return msg('storage.card_state_mounted', 'SD 卡已挂载');
  if (card.present) return msg('storage.card_state_unusable', 'SD 卡不可用');
  return msg('storage.card_state_none', '未检测到 SD 卡');
}

function usageBar(used, total) {
  const share = total > 0 ? Math.min(1, Math.max(0, used / total)) : 0;
  const fill = el('span', {class: 'ds-meter__fill'});
  fill.style.width = `${(share * 100).toFixed(1)}%`;
  return el('div', {class: 'ds-meter', attrs: {role: 'meter', 'aria-valuemin': '0', 'aria-valuemax': '100', 'aria-valuenow': String(Math.round(share * 100))}}, [fill]);
}

/** A caption with a trailing text action, e.g. 存储状态 · 刷新状态. */
function captionWithAction(label, actionLabel, onPress, action) {
  return el('div', {class: 'ds-group__caption ds-group__caption--with-action'}, [
    text(label),
    el('button', {class: 'ds-caption-action', attrs: {type: 'button'}, data: {action}, on: {click: onPress}}, [text(actionLabel)]),
  ]);
}

export async function storagePage(context) {
  const body = el('div');
  const saver = createSaver(context, {title: msg('ui.nav_storage', '存储与备份'), back: '/', root: body});
  let status = {};
  let fs = FILESYSTEMS[0].value;
  let pollTimer = null;

  // ---- full data backup ----
  let backup = {running: false, finished: false, message: '', error: false, phase: '', done: 0, total: 0};
  let backupBlocked = '';
  const engine = createBackupEngine(state => {
    backup = state;
    if (body.isConnected) render();
  });
  // The archive streams through the browser's download manager, which a
  // WebView does not provide; the companion app has no native transfer yet.
  const canDownload = !isEmbedded();
  const guardUnload = event => {
    if (!backup.running) return;
    event.preventDefault();
    event.returnValue = '';
  };
  window.addEventListener('beforeunload', guardUnload);

  async function prepareBackup() {
    try {
      await engine.capabilities();
      backupBlocked = '';
    } catch (error) {
      backupBlocked = error && error.error === 'usb_required' ? t('backup.usb_required') : errorText(error);
    }
    await engine.attach().catch(() => false);
    render();
  }

  async function createBackup() {
    const confirmed = await confirmSheet({
      title: msg('storage.full_backup_confirm_title', '创建完整备份？'),
      body: msg('backup.create_intro', '自动备份全部可用数据，包括 SD 用户文件、Python 环境和诊断日志。'),
      confirmLabel: msg('storage.create_backup', '创建备份'),
      action: 'confirm-create-backup',
    });
    if (confirmed) engine.createBackup();
  }

  const restoreInput = el('input', {attrs: {type: 'file', accept: '.aiden-backup,application/vnd.aiden.backup,application/octet-stream', hidden: 'hidden'}, data: {action: 'restore-file'}});
  restoreInput.addEventListener('change', async () => {
    const chosen = restoreInput.files && restoreInput.files[0];
    restoreInput.value = '';
    if (!chosen) return;
    let created = '';
    try {
      created = await engine.chooseRestore(chosen);
    } catch (error) {
      toast(errorText(error), {durationMs: 5000});
      return;
    }
    const confirmed = await confirmSheet({
      title: msg('storage.full_restore_confirm_title', '恢复这个备份？'),
      body: t('storage.full_restore_confirm_body', {
        name: chosen.name, created: created || '—',
        defaultValue: '{{name}}（创建于 {{created}}）会替换设备上现有的数据和设置，过程中服务会暂停，完成后可能需要重启。请只恢复来自可信来源的备份。',
      }),
      confirmLabel: msg('storage.restore_backup', '恢复备份'),
      danger: true,
      action: 'confirm-restore-backup',
    });
    if (confirmed) engine.startRestore({confirmIdentity: false});
  });

  function fullBackupGroup() {
    const busy = backup.running;
    const create = button({label: msg('storage.create_backup', '创建备份'), variant: 'accent-outline', block: true, action: 'create-backup', onPress: createBackup});
    create.disabled = busy || Boolean(backupBlocked) || !canDownload;
    const restore = button({label: msg('storage.restore_backup', '恢复备份'), variant: 'accent', block: true, action: 'restore-backup', onPress: () => restoreInput.click()});
    restore.disabled = busy || Boolean(backupBlocked);
    let progressNode = null;
    if (busy) {
      const determinate = backup.total > 0;
      const fill = el('span', {class: determinate ? 'ds-meter__fill' : 'ds-meter__fill ds-meter__fill--indeterminate'});
      if (determinate) fill.style.width = `${Math.min(100, (backup.done / backup.total) * 100).toFixed(1)}%`;
      progressNode = el('div', {class: 'ds-progress'}, [
        el('div', {class: 'ds-progress__head'}, [
          text(backup.phase || msg('backup.phase.ready', '准备中'), 'ds-progress__label'),
          el('button', {class: 'ds-caption-action', attrs: {type: 'button'}, data: {action: 'cancel-backup'},
            on: {click: () => engine.cancel().catch(error => toast(errorText(error)))}}, [text(msg('action.cancel', '取消'))]),
        ]),
        el('div', {class: 'ds-meter'}, [fill]),
        determinate ? text(`${formatBytes(backup.done)} / ${formatBytes(backup.total)}`, 'ds-progress__detail') : null,
        backup.stalled
          ? text(backup.message, 'ds-progress__detail ds-progress__detail--danger')
          : text(msg('backup.progress_hint', '操作完成前，请保持页面打开和 USB 连接。'), 'ds-progress__detail'),
      ]);
    }
    let note = null;
    if (backupBlocked) note = text(backupBlocked, 'ds-card__note ds-card__note--danger');
    else if (!canDownload && !busy) note = text(msg('storage.backup_app_hint', '在 App 中可以恢复备份；创建备份需要在电脑浏览器中打开本页。'), 'ds-card__note');
    else if (backup.finished && backup.message) note = text(backup.message, backup.error ? 'ds-card__note ds-card__note--danger' : 'ds-card__note');
    return group({
      caption: msg('storage.backup_caption', '备份与还原'),
      joined: true,
      rows: [
        text(msg('storage.full_backup_intro', '将 Agent 数据和设置（记忆、会话、设备身份等）完整备份到当前电脑或手机，或从 Aiden 备份恢复。'), 'ds-group__intro'),
        progressNode,
        el('div', {class: 'ds-button-pair'}, [
          el('div', {class: 'ds-button-pair__item'}, [create]),
          el('div', {class: 'ds-button-pair__item'}, [restore]),
        ]),
        note,
        restoreInput,
      ],
    });
  }

  async function load(showToast) {
    try {
      status = await request('/api/storage/status');
      if (showToast) toast(msg('storage.refreshed', '存储状态已刷新。'));
    } catch (error) {
      if (showToast) toast(error && error.message ? error.message : resolve(msg('storage.load_failed_short', '加载存储状态失败')));
    }
    render();
    schedulePoll();
  }

  // Poll while a format job runs, and stop once the page is left.
  function schedulePoll() {
    clearTimeout(pollTimer);
    pollTimer = null;
    const running = (status.format_job || {}).status === 'running';
    if (running) {
      pollTimer = setTimeout(() => {
        if (body.isConnected) load(false);
      }, FORMAT_POLL_MS);
    }
  }

  async function format() {
    const chosen = FILESYSTEMS.find(entry => entry.value === fs);
    const confirmed = await confirmSheet({
      title: msg('storage.format_confirm_title', '格式化存储卡？'),
      body: t('storage.format_confirm_body', {fs: fs.toUpperCase(), defaultValue: '此操作将清除 microSD 卡上的所有数据，格式化为 {{fs}}。此操作不可撤销。'}),
      confirmLabel: msg('storage.format_action', '格式化'),
      danger: true,
      action: 'confirm-format',
    });
    if (!confirmed || !chosen) return;
    try {
      await request('/api/storage/format', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({fs, confirm: 'format-sd-card'})});
      toast(msg('storage.format_started', '已开始格式化。'));
    } catch (error) {
      toast(error && error.message ? error.message : resolve(msg('storage.format_start_failed', '启动格式化失败。')));
    }
    load(false);
  }

  async function eject() {
    const confirmed = await confirmSheet({
      title: msg('storage.eject_confirm_title', '安全弹出存储卡？'),
      body: msg('storage.eject_confirm', '同步并卸载 SD 卡，以便安全取出？'),
      confirmLabel: msg('storage.safe_eject', '安全弹出'),
      action: 'confirm-eject',
    });
    if (!confirmed) return;
    try {
      const result = await request('/api/storage/eject', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: '{}'});
      if (result.warning_code === 'storage_flush_failed') {
        toast(msg('storage.ejected_with_warning', '存储卡已弹出，可以取出；部分待写入数据可能未保存。'), {durationMs: 6000});
      } else {
        toast(msg('storage.ejected', '存储卡已弹出，可以安全取出。'));
      }
    } catch (error) {
      toast(error && error.message ? error.message : resolve(msg('storage.eject_failed', '弹出存储卡失败。')));
    }
    load(false);
  }

  async function exportBackup(trigger) {
    trigger.disabled = true;
    try {
      const response = await fetch('/api/config/backup', {cache: 'no-store'});
      if (!response.ok) {
        const payload = await response.json().catch(() => ({}));
        throw new Error(payload.error || `HTTP ${response.status}`);
      }
      const disposition = response.headers.get('Content-Disposition') || '';
      const match = disposition.match(/filename="([^"]+)"/i);
      const filename = match ? match[1] : 'aiden-config.toml';
      if (isEmbedded()) {
        // A WebView cannot save a blob download; the app writes and shares it.
        postToHost({type: 'aiden_file_export', filename, mime: 'application/toml', text: await response.text()});
      } else {
        const url = URL.createObjectURL(await response.blob());
        const link = el('a', {attrs: {href: url, download: filename}});
        document.body.appendChild(link);
        link.click();
        link.remove();
        URL.revokeObjectURL(url);
        toast(msg('storage.backup_exported', '配置备份已导出。'));
      }
    } catch (error) {
      toast(error && error.message ? error.message : resolve(msg('storage.backup_export_failed', '导出配置备份失败。')));
    } finally {
      trigger.disabled = false;
    }
  }

  const fileInput = el('input', {attrs: {type: 'file', accept: '.toml,application/toml,text/plain', hidden: 'hidden'}, data: {action: 'backup-file'}});
  fileInput.addEventListener('change', async () => {
    const file = fileInput.files && fileInput.files[0];
    fileInput.value = '';
    if (!file) return;
    if (!/\.toml$/i.test(file.name)) {
      toast(msg('storage.backup_file_required', '请选择 TOML 配置备份。'));
      return;
    }
    const confirmed = await confirmSheet({
      title: msg('storage.import_confirm_title', '导入配置备份？'),
      body: t('storage.backup_import_confirm', {name: file.name, defaultValue: '确定使用 {{name}} 替换当前 Agent 配置吗？文件会在保存前完成校验。'}),
      confirmLabel: msg('storage.import', '导入'),
      action: 'confirm-import',
    });
    if (!confirmed) return;
    try {
      const payload = await request('/api/config/backup', {method: 'PUT', headers: {'Content-Type': 'application/toml; charset=utf-8'}, body: file});
      toast(payload.pending ? msg('storage.backup_imported_pending', '配置备份已导入，Agent 正在应用。') : msg('storage.backup_imported', '配置备份已导入。'));
    } catch (error) {
      toast(error && error.persisted
        ? msg('storage.backup_imported_not_applied', '配置备份已保存，但 Agent 未能应用。')
        : (error && error.message ? error.message : resolve(msg('storage.backup_import_failed', '导入配置备份失败。'))),
      {durationMs: 5000});
    }
  });

  function render() {
    const internal = status.internal || {};
    const card = status.card || {};
    const job = status.format_job || {};
    const formatting = job.status === 'running';
    const total = internal.total_bytes;
    const free = internal.free_bytes;
    const used = Number.isFinite(total) && Number.isFinite(free) ? total - free : NaN;
    const cardUsable = Boolean(card.present) && !formatting;
    const chosen = FILESYSTEMS.find(entry => entry.value === fs) || FILESYSTEMS[0];

    const formatButton = button({label: msg('storage.format_card', '格式化存储卡'), variant: 'danger-soft', block: true, action: 'format-card', onPress: format});
    formatButton.disabled = !cardUsable;
    const ejectButton = button({label: msg('storage.safe_eject', '安全弹出'), variant: 'accent-outline', block: true, action: 'eject-card', onPress: eject});
    ejectButton.disabled = !card.mounted || formatting;
    const exportButton = button({label: msg('storage.export', '导出'), variant: 'accent-outline', block: true, action: 'export-backup', onPress: () => exportBackup(exportButton)});
    const importButton = button({label: msg('storage.import', '导入'), variant: 'accent', block: true, action: 'import-backup', onPress: () => fileInput.click()});

    replace(body, [
      el('div', {}, [
        captionWithAction(msg('storage.status_caption', '存储状态'), msg('storage.refresh', '刷新状态'), () => load(true), 'refresh-storage'),
        group({
          rows: [
            internal.available
              ? el('div', {class: 'ds-meter-block'}, [
                  usageBar(used, total),
                  el('div', {class: 'ds-meter__legend'}, [
                    text(t('storage.used', {size: gb(used), defaultValue: '已用 {{size}}'})),
                    text(t('storage.remaining', {size: gb(free), defaultValue: '剩余 {{size}}'})),
                  ]),
                ])
              : null,
            row({label: msg('storage.total_space', '总空间'), value: internal.available ? gb(total) : msg('storage.value_unavailable', '不可用')}),
            row({label: msg('storage.available_space', '可用空间'), value: internal.available ? gb(free) : msg('storage.value_unavailable', '不可用')}),
          ],
        }),
      ]),
      group({
        caption: msg('storage.microsd_caption', 'microSD 设置'),
        rows: [
          row({label: msg('storage.running_mode', '运行模式'), value: `${resolve(modeName(status.effective_mode))} — ${resolve(cardState(status))}`}),
          row({
            label: msg('storage.format_as', '格式化为'),
            value: chosen.label,
            action: 'open-format-fs',
            onPress: cardUsable ? () => choiceSheet({
              options: FILESYSTEMS,
              selected: fs,
              onSelect: value => {
                fs = value;
                render();
              },
            }) : undefined,
          }),
          formatting
            ? row({label: t('storage.formatting', {fs: String(job.fs || fs).toUpperCase(), defaultValue: '正在将存储卡格式化为 {{fs}}…请保持存储卡插入。'})})
            : null,
          el('div', {class: 'ds-button-pair'}, [
            el('div', {class: 'ds-button-pair__item'}, [formatButton]),
            el('div', {class: 'ds-button-pair__item'}, [ejectButton]),
          ]),
          status.last_warning
            ? text(msg('storage.last_flush_warning', '此前存储卡同步失败，部分待写入数据可能未保存。'), 'ds-card__note ds-card__note--danger')
            : null,
        ],
      }),
      fullBackupGroup(),
      group({
        caption: msg('storage.config_backup_caption', '仅 Agent 配置（.toml）'),
        rows: [
          text(msg('storage.config_backup_intro', '导出当前 Agent 配置，或从经过校验的 TOML 备份恢复配置。'), 'ds-group__intro'),
          el('div', {class: 'ds-button-pair'}, [
            el('div', {class: 'ds-button-pair__item'}, [exportButton]),
            el('div', {class: 'ds-button-pair__item'}, [importButton]),
          ]),
          fileInput,
        ],
        joined: true,
      }),
    ]);
    saver.refreshChrome();
  }

  render();
  await load(false);
  prepareBackup();
  return screen([body]);
}
