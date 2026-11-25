import { useEffect, useRef, useState } from 'react';
import { atom, useAtomValue } from 'jotai';
import type { DeviceState } from '../state/device';
import type { ApplicationData } from '../types';
import { createBackupEngine, errorText, formatBytes } from '../lib/backup';
import type { BackupView } from '../lib/backup';
import { isEmbedded } from '../lib/environment';
import { postToHost } from '../lib/host';
import { downloadBlob } from '../lib/download';
import { apiError } from '../api/client';
import { msg, t } from '../lib/i18n';
import { Button, Group, Row, Text } from '../components/ui';
import { confirmSheet, toast } from '../components/overlays';
export const FILESYSTEMS = [
  { value: 'fat32', label: msg('storage.fs.fat32', 'FAT32（电脑 / 手机可读取）') },
  {
    value: 'exfat',
    label: msg('storage.fs.exfat', 'exFAT（适合大容量卡和文件，电脑 / 手机可读取）'),
  },
  { value: 'ext4', label: msg('storage.fs.ext4', 'ext4（适合设备本地使用）') },
];
const sessions = new WeakMap<DeviceState, ReturnType<typeof makeSession>>();
function makeSession(model: DeviceState) {
  const state = atom<BackupView>({
    kind: null,
    running: false,
    stalled: false,
    phase: '',
    done: 0,
    total: 0,
    message: '',
    error: false,
    finished: false,
    rebootRequired: false,
  });
  const engine = createBackupEngine((value) => model.store.set(state, value));
  return { state, engine };
}
function sessionFor(model: DeviceState) {
  let result = sessions.get(model);
  if (!result) {
    result = makeSession(model);
    sessions.set(model, result);
  }
  return result;
}
export function BackupPanel({ model }: { model: DeviceState }) {
  const { state, engine } = sessionFor(model);
  const backup = useAtomValue(state, { store: model.store });
  const [blocked, setBlocked] = useState(''),
    [busy, setBusy] = useState(false);
  const file = useRef<HTMLInputElement>(null),
    toml = useRef<HTMLInputElement>(null);
  useEffect(() => {
    let mounted = true;
    engine
      .capabilities()
      .then(
        () => {
          if (mounted) setBlocked('');
        },
        (error) => {
          if (mounted) setBlocked(errorText(error));
        },
      )
      .finally(() => void engine.attach().catch(() => false));
    return () => {
      mounted = false;
    };
  }, [engine]);
  async function restore(chosen: File) {
    try {
      const created = await engine.chooseRestore(chosen);
      if (
        await confirmSheet({
          title: msg('storage.full_restore_confirm_title', '恢复这个备份？'),
          body: t('storage.full_restore_confirm_body', {
            name: chosen.name,
            created: created || '—',
          }),
          confirmLabel: msg('storage.restore_backup', '恢复备份'),
          danger: true,
        })
      )
        await engine.startRestore({ confirmIdentity: false });
    } catch (error) {
      toast(errorText(error));
    }
  }
  async function exportConfig() {
    setBusy(true);
    try {
      const response = await fetch('/api/config/backup', { cache: 'no-store' });
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      const filename =
        response.headers.get('Content-Disposition')?.match(/filename="([^"]+)"/i)?.[1] ||
        'aico-config.toml';
      if (isEmbedded())
        postToHost({
          type: 'aico_file_export',
          filename,
          mime: 'application/toml',
          text: await response.text(),
        });
      else downloadBlob(await response.blob(), filename);
    } catch (error) {
      toast(apiError(error).message);
    } finally {
      setBusy(false);
    }
  }
  async function importConfig(file: File) {
    if (!/\.toml$/i.test(file.name)) {
      toast(msg('storage.backup_file_required', '请选择 TOML 配置备份。'));
      return;
    }
    if (
      !(await confirmSheet({
        title: msg('storage.import_confirm_title', '导入配置备份？'),
        body: t('storage.backup_import_confirm', { name: file.name }),
      }))
    )
      return;
    setBusy(true);
    try {
      const result = await model.send<ApplicationData>('/api/config/backup', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/toml; charset=utf-8' },
        body: file,
      });
      model.publishSave(result);
      toast(msg('storage.backup_imported', '配置备份已导入。'));
    } catch (caught) {
      const error = apiError(caught);
      if (error.persisted) model.publishSave(error);
      toast(
        error.persisted
          ? msg('storage.backup_imported_not_applied', '配置备份已保存，但 Agent 未能应用。')
          : error.message,
      );
    } finally {
      setBusy(false);
    }
  }
  return (
    <>
      <Group caption={msg('storage.backup_caption', '备份与还原')} joined>
        <p className="ds-group__intro">
          <Text
            value={msg(
              'storage.full_backup_intro',
              '自动备份全部可用数据，包括 SD 用户文件、Python 环境和诊断日志。',
            )}
          />
        </p>
        {blocked && <p role="alert">{blocked}</p>}
        {isEmbedded() && (
          <p>
            <Text value={msg('backup.browser_required', '请在浏览器中创建完整备份')} />
          </p>
        )}
        <div className="ds-button-pair">
          <Button
            variant="accent-outline"
            block
            className="ds-btn ds-btn--accent-outline ds-btn--block ds-button-pair__item"
            action="create-backup"
            disabled={backup.running || !!blocked || isEmbedded()}
            onClick={async () => {
              if (
                await confirmSheet({
                  title: msg('storage.full_backup_confirm_title', '创建完整备份？'),
                  body: msg(
                    'backup.create_intro',
                    '自动备份全部可用数据，包括 SD 用户文件、Python 环境和诊断日志。',
                  ),
                })
              )
                await engine.createBackup();
            }}
          >
            <Text value={msg('storage.create_backup', '创建备份')} />
          </Button>
          <Button
            variant="accent"
            block
            className="ds-btn ds-btn--accent ds-btn--block ds-button-pair__item"
            action="restore-backup"
            disabled={backup.running || !!blocked}
            onClick={() => file.current?.click()}
          >
            <Text value={msg('storage.restore_backup', '恢复备份')} />
          </Button>
        </div>
        <input
          ref={file}
          type="file"
          hidden
          accept=".aico-backup,application/vnd.aico.backup,application/octet-stream"
          onChange={(event) => {
            const chosen = event.target.files?.[0];
            event.target.value = '';
            if (chosen) void restore(chosen);
          }}
        />
        {backup.running && (
          <div role="status">
            <p>{backup.phase}</p>
            {backup.total > 0 && (
              <>
                <progress value={backup.done} max={backup.total} />
                <p>
                  {formatBytes(backup.done)} / {formatBytes(backup.total)}
                </p>
              </>
            )}
            <Button
              action="cancel-backup"
              variant="quiet"
              onClick={() => engine.cancel().catch((error) => toast(errorText(error)))}
            >
              <Text value={msg('action.cancel', '取消')} />
            </Button>
          </div>
        )}
        {backup.message && <p role={backup.error ? 'alert' : 'status'}>{backup.message}</p>}
        {backup.rebootRequired && (
          <Row
            label={msg('action.reboot', '重启')}
            onPress={async () => {
              if (await confirmSheet({ title: msg('action.reboot', '重启'), danger: true }))
                await model.send('/api/device/reboot', { method: 'POST' });
            }}
          />
        )}
      </Group>
      <Group caption={msg('storage.config_backup_caption', '仅 Agent 配置（.toml）')} joined>
        <Text
          value={msg(
            'storage.config_backup_intro',
            '导出当前 Agent 配置，或从经过校验的 TOML 备份恢复配置。',
          )}
          className="ds-group__intro"
        />
        <div className="ds-button-pair">
          <Button
            variant="accent-outline"
            block
            className="ds-btn ds-btn--accent-outline ds-btn--block ds-button-pair__item"
            disabled={busy}
            action="export-backup"
            onClick={() => void exportConfig()}
          >
            <Text value={msg('storage.export', '导出')} />
          </Button>
          <Button
            variant="accent"
            block
            className="ds-btn ds-btn--accent ds-btn--block ds-button-pair__item"
            disabled={busy || backup.running}
            action="import-backup"
            onClick={() => toml.current?.click()}
          >
            <Text value={msg('storage.import', '导入')} />
          </Button>
        </div>
        <input
          ref={toml}
          type="file"
          hidden
          accept=".toml,application/toml,text/plain"
          onChange={(event) => {
            const chosen = event.target.files?.[0];
            event.target.value = '';
            if (chosen) void importConfig(chosen);
          }}
        />
      </Group>
    </>
  );
}
