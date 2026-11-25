import { useState } from 'react';
import { useAtomValue } from 'jotai';
import { Loading } from '../components/Loading';
import { isInitialLoading } from '../state/resource';
import type { PageProps } from '../types';
import { useSettings } from '../hooks/settings';
import { useQuery } from '../hooks/query';
import { apiError, json } from '../api/client';
import { msg, resolve } from '../lib/i18n';
import { Button, Group, Row, Text } from '../components/ui';
import { ConfigField, FieldInput, QueryError } from '../components/fields';
import { Modal, confirmSheet, toast } from '../components/overlays';
interface Token {
  id: string;
  name: string;
  created_at: string;
}
export function wifiEndpoint(
  location: Pick<Location, 'hostname'> = window.location,
  wifiAddress = '',
) {
  return `http://${wifiAddress || location.hostname}:8080/mcp`;
}
export function mcpClientSnippets(endpoint: string, secret: string) {
  return {
    claudeCode: `claude mcp add --transport http aico ${endpoint} --header "Authorization: Bearer ${secret}"`,
    json: JSON.stringify(
      {
        mcpServers: {
          aico: { type: 'http', url: endpoint, headers: { Authorization: `Bearer ${secret}` } },
        },
      },
      null,
      1,
    ),
  };
}
function Code({ value }: { value: string }) {
  return (
    <div className="ds-code">
      <pre className="ds-code__text">{value}</pre>
      <Button
        variant="quiet"
        onClick={() =>
          navigator.clipboard
            .writeText(value)
            .catch(() => toast(msg('mcp.copy_failed', '无法复制，请手动选择')))
        }
      >
        <Text value={msg('mcp.copy', '复制')} />
      </Button>
    </div>
  );
}
export function MCPTokensPanel(props: PageProps) {
  const saver = useSettings(props, 'MCP', props.model.local ? '/' : '/advanced');
  const snapshot = useAtomValue(props.model.current);
  const config = useAtomValue(props.model.resources.snapshot.view);
  const query = useQuery(
    (signal) =>
      props.model.send<{ tokens: Token[] }>('/api/security/mcp-tokens', {
        signal,
        cache: 'no-store',
      }),
    [props.model],
    5000,
  );
  const [dialog, setDialog] = useState<'name' | 'secret' | null>(null),
    [name, setName] = useState(''),
    [secret, setSecret] = useState(''),
    [busy, setBusy] = useState(false),
    [error, setError] = useState('');
  const snippets = mcpClientSnippets(
    wifiEndpoint(window.location, snapshot.wifi_status?.ip_address),
    secret,
  );
  async function create() {
    if (busy || !name.trim()) return;
    setBusy(true);
    try {
      const result = await props.model.send<{ secret: string }>('/api/security/mcp-tokens', {
        method: 'POST',
        ...json({ name: name.trim() }),
      });
      setSecret(result.secret);
      setDialog('secret');
      setName('');
      query.retry();
    } catch (error) {
      setError(apiError(error).message);
    } finally {
      setBusy(false);
    }
  }
  const close = () => {
    setDialog(null);
    setSecret('');
    setError('');
  };
  return (
    <>
      <Group>
        {snapshot.config ? (
          <ConfigField
            section="agent"
            label={msg('advanced.mcp_enabled', 'MCP')}
            description={msg(
              'advanced.mcp_enabled_help',
              '让电脑上的 AI 助手（如 Claude Code、Cursor）通过 Aico Link 操作手机，例如截屏、点击和输入文字。',
            )}
            field={{ key: 'mcp_enabled', widget: 'boolean' }}
            value={saver.config.agent?.mcp_enabled !== false}
            save={(value) => saver.change('agent.mcp_enabled', { agent: { mcp_enabled: value } })}
          />
        ) : (
          <Row
            label="MCP"
            value={msg('status.state.unknown', '未知')}
            loading={isInitialLoading(config)}
          />
        )}
      </Group>
      {saver.config.agent?.mcp_enabled !== false && (
        <Group
          note={msg(
            'mcp.help',
            '电脑和 Aico Link 连同一个 Wi-Fi 就能用，不需要开启局域网访问。令牌只能用于 MCP，不会过期，修改设备密码后依然有效；不再使用时在这里撤销。',
          )}
        >
          <QueryError {...query} />
          {isInitialLoading(query) && <Loading block />}
          {query.data?.tokens.map((token) => (
            <Row
              key={token.id}
              label={token.name}
              description={new Date(token.created_at).toLocaleString()}
              value={msg('mcp.revoke', '撤销')}
              tone="danger"
              onPress={
                busy
                  ? undefined
                  : async () => {
                      if (
                        !(await confirmSheet({
                          title: msg('mcp.revoke_title', '撤销这个令牌？'),
                          body: msg(
                            'mcp.revoke_body',
                            '用这个令牌的 MCP 客户端会立即断开，且无法恢复。',
                          ),
                          danger: true,
                        }))
                      )
                        return;
                      setBusy(true);
                      try {
                        await props.model.send(
                          `/api/security/mcp-tokens/${encodeURIComponent(token.id)}`,
                          { method: 'DELETE' },
                        );
                        query.retry();
                      } catch (error) {
                        toast(apiError(error).message);
                      } finally {
                        setBusy(false);
                      }
                    }
              }
            />
          ))}
          <Row
            label={msg('mcp.create', '创建令牌')}
            labelTone="accent"
            action="create-mcp-token"
            onPress={busy ? undefined : () => setDialog('name')}
          />
        </Group>
      )}
      {dialog && (
        <Modal
          key={dialog}
          title={
            dialog === 'name'
              ? msg('mcp.new_title', '新建令牌')
              : msg('mcp.created_title', '令牌已创建')
          }
          onClose={close}
          busy={busy}
        >
          {dialog === 'name' ? (
            <form
              onSubmit={(event) => {
                event.preventDefault();
                void create();
              }}
            >
              <FieldInput
                label={msg('mcp.name', '名称')}
                value={name}
                onChange={(value) => setName(value.slice(0, 64))}
                disabled={busy}
              />
              {error && <p role="alert">{error}</p>}
              <Button type="submit" action="mcp-token-submit" disabled={busy || !name.trim()}>
                <Text value={msg('mcp.create', '创建令牌')} />
              </Button>
            </form>
          ) : (
            <>
              <p>
                <Text
                  value={msg(
                    'mcp.created_help',
                    '令牌只显示这一次，请现在复制保存。把下面任一配置粘贴到 MCP 客户端即可。',
                  )}
                />
              </p>
              <Code value={secret} />
              <Code value={snippets.claudeCode} />
              <Code value={snippets.json} />
              <p>
                {resolve(
                  msg(
                    'mcp.direct_hint',
                    '用数据线直连 Aico Link 时，把地址里的 IP 换成 192.168.42.1。',
                  ),
                )}
              </p>
            </>
          )}
        </Modal>
      )}
    </>
  );
}
