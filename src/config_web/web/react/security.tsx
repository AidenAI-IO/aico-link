import type { DeviceState } from './state';
import type { LANData } from './types';
import { useEffect, useRef } from 'react';
import { useAtomValue } from 'jotai';
import {
  currentPasswordError,
  openClearPasswordSheet,
  openPasswordSheet,
} from '../assets/js/app/pages/lan-access.js';
import { msg } from '../assets/js/ui/text.js';
import { toast } from '../assets/js/ui/toast.js';
import { Group, ResourceError, Row, Switch } from './ui';

export function SecurityPage({
  model,
  password = false,
}: {
  model: DeviceState;
  password?: boolean;
}) {
  const { data, stale, busy } = useAtomValue(model.resources.lan.atom);
  const sheet = useRef<{ close: () => void } | null>(null);
  useEffect(() => () => sheet.current?.close(), []);
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
      throw currentPasswordError(error);
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
  const askPassword = () => {
    const askCurrent = Boolean(data?.password_set);
    sheet.current = openPasswordSheet({
      minLength: data?.min_length || 6,
      askCurrent,
      submitLabel: askCurrent
        ? msg('lan.change_password', '修改密码')
        : msg('lan.set_password', '设置密码'),
      onSubmit: (value, current) =>
        write({ password: value, ...(askCurrent ? { current_password: current } : {}) }),
    });
  };
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
                    sheet.current = openClearPasswordSheet(Boolean(data?.enabled), (current) =>
                      write({ current_password: current }, true),
                    );
                  }
            }
          />
        )}
      </Group>
      <ResourceError model={model} name="lan" />
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
