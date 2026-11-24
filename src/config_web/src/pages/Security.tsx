import type { DeviceState } from '../state/device';
import type { LANData } from '../types';
import { useState } from 'react';
import { apiError } from '../api/client';
import { FieldInput } from '../components/fields';
import { useAtomValue } from 'jotai';
import { msg, resolve, t } from '../lib/i18n';
import { Modal, toast } from '../components/overlays';
import { Button, Text, Group, ResourceError, Row, Switch } from '../components/ui';

export function SecurityPage({
  model,
  password = false,
}: {
  model: DeviceState;
  password?: boolean;
}) {
  const { data, stale, busy } = useAtomValue(model.resources.lan.atom);
  const [sheet, setSheet] = useState<'set' | 'clear' | null>(null);
  const write = async (body: Record<string, unknown>, clear = false) => {
    try {
      const result = await model.resources.lan.mutate(() =>
        model.send<LANData>(clear ? '/api/security/password' : '/api/security/lan-access', {
          method: clear ? 'DELETE' : 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        }),
      );
      if (result.system_synced === false)
        toast(msg('lan.login_sync_failed', '已保存，SSH 和终端登录将在设备重启后生效'));
    } catch (error) {
      const failure = apiError(error);
      if (failure.error === 'current_password_invalid')
        failure.message = resolve(msg('lan.password_current_invalid', '当前密码不正确'));
      if (failure.error === 'too_many_attempts')
        failure.message = resolve(msg('lan.password_too_many', '尝试次数过多，请稍后再试'));
      throw failure;
    }
  };
  const unavailable = busy || stale;
  if (!password)
    return (
      <div className="ds-screen">
        <Group
          note={
            data?.enabled && !data?.password_set
              ? msg(
                  'lan.no_password_warning',
                  '未设置设备密码：同一局域网内的设备无需登录即可访问。可在“设备密码”中设置。',
                )
              : null
          }
        >
          <Row
            label={msg('lan.title', '局域网访问')}
            description={msg(
              'lan.help',
              '开启后，同一局域网内的设备也能访问设置页和 Agent。MCP 使用单独的令牌，不受这个开关影响。',
            )}
          >
            <Switch
              checked={data?.enabled}
              label={msg('lan.title', '局域网访问')}
              action="toggle-lan-access"
              disabled={unavailable}
              onChange={(enabled) => write({ enabled }).catch((error) => toast(error.message))}
            />
          </Row>
        </Group>
        <ResourceError model={model} name="lan" />
      </div>
    );
  const askPassword = () => setSheet('set');
  return (
    <div className="ds-screen">
      <Group
        note={msg(
          'lan.password_help',
          '设置后，所有设备都要先登录：局域网设备输入这个密码，Aico App 通过 USB 自动完成。它同时是 aico 账号的密码，用于网页终端和 SSH；没有设置时这两者无法登录。修改或清除密码会让所有设备退出登录。',
        )}
      >
        <Row
          label={
            data?.password_set
              ? msg('lan.change_password', '修改密码')
              : msg('lan.set_password', '设置密码')
          }
          value={
            stale
              ? msg('status.state.unknown', '未知')
              : data?.password_set
                ? msg('lan.password_set', '已设置')
                : msg('lan.password_unset', '未设置')
          }
          action="open-lan-password"
          chevron
          onPress={unavailable ? undefined : askPassword}
        />
        {data?.password_set && (
          <Row
            label={msg('lan.clear_password', '清除密码')}
            action="lan-clear-password"
            onPress={
              unavailable
                ? undefined
                : () => {
                    setSheet('clear');
                  }
            }
          />
        )}
      </Group>
      <ResourceError model={model} name="lan" />
      {sheet && (
        <PasswordDialog
          clear={sheet === 'clear'}
          askCurrent={!!data?.password_set}
          minLength={data?.min_length || 6}
          onClose={() => setSheet(null)}
          onSubmit={write}
        />
      )}
      {data?.password_set && !data?.via_usb && (
        <Group>
          <Row
            label={msg('lan.sign_out', '退出登录')}
            action="lan-sign-out"
            onPress={async () => {
              try {
                await model.send('/api/auth/logout', { method: 'POST' });
              } finally {
                window.location.assign('/login');
              }
            }}
          />
        </Group>
      )}
    </div>
  );
}

function PasswordDialog({
  clear,
  askCurrent,
  minLength,
  onClose,
  onSubmit,
}: {
  clear: boolean;
  askCurrent: boolean;
  minLength: number;
  onClose: () => void;
  onSubmit: (body: Record<string, unknown>, clear: boolean) => Promise<void>;
}) {
  const [current, setCurrent] = useState(''),
    [password, setPassword] = useState(''),
    [repeat, setRepeat] = useState(''),
    [error, setError] = useState(''),
    [busy, setBusy] = useState(false);
  async function submit() {
    if (busy) return;
    if (askCurrent && !current) {
      setError(resolve(msg('lan.password_current_required', '请输入当前密码')));
      return;
    }
    if (!clear && [...password].length < minLength) {
      setError(t('lan.password_min_length', { n: minLength }));
      return;
    }
    if (!clear && password !== repeat) {
      setError(resolve(msg('lan.password_mismatch', '两次输入的密码不一致')));
      return;
    }
    setBusy(true);
    setError('');
    try {
      await onSubmit(
        { ...(!clear ? { password } : {}), ...(askCurrent ? { current_password: current } : {}) },
        clear,
      );
      onClose();
    } catch (error) {
      setError(apiError(error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <Modal
      title={clear ? msg('lan.clear_password', '清除密码') : msg('lan.password_title', '设备密码')}
      onClose={onClose}
      busy={busy}
    >
      <form
        onSubmit={(event) => {
          event.preventDefault();
          void submit();
        }}
      >
        {askCurrent && (
          <FieldInput
            label={msg('lan.password_current', '当前密码')}
            value={current}
            onChange={setCurrent}
            secret
            disabled={busy}
          />
        )}{' '}
        {!clear && (
          <>
            <FieldInput
              label={msg('lan.password_placeholder', '输入新密码')}
              value={password}
              onChange={setPassword}
              secret
              disabled={busy}
            />
            <FieldInput
              label={msg('lan.password_confirm', '再次输入密码')}
              value={repeat}
              onChange={setRepeat}
              secret
              disabled={busy}
            />
          </>
        )}
        {error && (
          <p role="alert" className="ds-field__error">
            {error}
          </p>
        )}
        <Button
          type="submit"
          disabled={busy}
          action="lan-password-submit"
          variant={clear ? 'danger' : 'primary'}
        >
          <Text
            value={clear ? msg('lan.clear_password', '清除密码') : msg('action.save', '保存')}
          />
        </Button>
      </form>
    </Modal>
  );
}
