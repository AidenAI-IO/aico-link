import { useState } from 'react';
import { useAtomValue } from 'jotai';
import type { JsonValue, PageProps } from '../types';
import { useSettings, section, usePageHeader } from '../hooks/settings';
import { useSchema } from '../hooks/query';
import type { Field } from '../lib/schema';
import { sectionFields } from '../lib/schema';
import { applyLocale, msg, resolve } from '../lib/i18n';
import { apiError, json } from '../api/client';
import { ConfigField, QueryError, ValueField } from '../components/fields';
import { Button, Group, Row, Text } from '../components/ui';
import { confirmSheet, toast } from '../components/overlays';
const label = (key: string, fallback: string) => msg(key, fallback);
export function BasicPage(props: PageProps) {
  const saver = useSettings(props, msg('ui.nav_basic', '基础设置'));
  const config = saver.config;
  return (
    <div className="ds-screen">
      <Group>
        <Row
          label={msg('basic.language_title', '语言与时区')}
          chevron
          onPress={() => props.navigate('/basic/language')}
        />
      </Group>
      <Group>
        {[
          {
            section: 'device',
            field: {
              key: 'device_type',
              label: 'Device type',
              widget: 'select',
              enum: ['iOS', 'Android', 'macOS', 'windows', 'linux'].map((value) => ({ value })),
            },
          },
          {
            section: 'hid',
            field: {
              key: 'keyboard_layout',
              label: 'Keyboard layout',
              widget: 'select',
              enum: ['qwerty', 'azerty', 'qwertz'].map((value) => ({ value })),
            },
          },
          {
            section: 'agent',
            field: {
              key: 'tap_wakeup_enabled',
              label: 'Tap to wake',
              widget: 'boolean',
              default: true,
            },
          },
        ].map(({ section: name, field }) => (
          <ConfigField
            key={field.key}
            section={name}
            field={field}
            value={section(config, name)[field.key]}
            save={(value) =>
              saver.change(`${name}.${field.key}`, { [name]: { [field.key]: value } })
            }
          />
        ))}
      </Group>
    </div>
  );
}
export function LanguagePage(props: PageProps) {
  const saver = useSettings(
    props,
    msg('basic.language_title', '语言与时区'),
    props.model.local ? '/' : '/basic',
  );
  const schema = useSchema(props.model);
  const field = sectionFields(schema.data, 'agent').find((field) => field.key === 'timezone');
  async function choose(locale: string) {
    try {
      const result = await props.model.send<import('../types').ApplicationData>(
        '/api/config/locale',
        { method: 'PUT', ...json({ locale }) },
      );
      props.model.publishSave(result);
      applyLocale(result.locale || locale, true);
    } catch (caught) {
      const error = apiError(caught);
      if (error.persisted) {
        props.model.publishSave(error);
        applyLocale(locale, true);
      } else toast(error.message);
    }
  }
  return (
    <div className="ds-screen">
      <Group>
        {[
          { value: 'en-US', label: 'English' },
          { value: 'zh-CN', label: '简体中文' },
        ].map((option) => (
          <Row
            key={option.value}
            label={option.label}
            value={saver.config.agent?.locale === option.value ? '✓' : ''}
            onPress={() => void choose(option.value)}
          />
        ))}
      </Group>
      <QueryError {...schema} />
      {field ? (
        <Group>
          <ConfigField
            section="agent"
            field={field}
            value={saver.config.agent?.timezone || 'UTC'}
            save={(value) => saver.change('agent.timezone', { agent: { timezone: value } })}
          />
        </Group>
      ) : (
        <ValueField
          label={msg('basic.timezone', '时区')}
          saved={saver.config.agent?.timezone || 'UTC'}
          onCommit={(value) => saver.change('agent.timezone', { agent: { timezone: value } })}
        />
      )}
    </div>
  );
}
export function ConversationPage(props: PageProps) {
  const saver = useSettings(props, msg('ui.nav_conversation', '对话与工具'));
  const schema = useSchema(props.model);
  const values = section(saver.config, 'agent');
  const fields: Field[] = [
    { key: 'max_iterations', widget: 'number', label: 'Maximum tool iterations' },
    { key: 'context_prune_threshold', widget: 'number', label: 'History pruning threshold' },
    { key: 'context_prune_state_limit', widget: 'number', label: 'Screenshot state limit' },
    {
      key: 'context_compaction_threshold',
      widget: 'number',
      label: 'Context compaction threshold',
    },
    { key: 'ui_grounding_enabled', widget: 'boolean', label: 'UI Grounding', default: true },
  ].map(
    (fallback) =>
      sectionFields(schema.data, 'agent').find((field) => field.key === fallback.key) || fallback,
  );
  return (
    <div className="ds-screen">
      <Group>
        <Row
          label={msg('conversation.prompt', '自定义提示词')}
          chevron
          onPress={() => props.navigate('/conversation/prompt')}
        />
        {fields.map((field) => (
          <ConfigField
            key={field.key}
            section="agent"
            field={field}
            value={values[field.key]}
            save={(value) => saver.change(`agent.${field.key}`, { agent: { [field.key]: value } })}
          />
        ))}
      </Group>
      <Group>
        <Row
          label={msg('conversation.tools', '工具设置')}
          chevron
          onPress={() => props.navigate('/conversation/tools')}
        />
        <Row
          label={msg('conversation.reset', '重置当前对话')}
          action="reset-conversation"
          onPress={async () => {
            if (
              await confirmSheet({
                title: msg('conversation.reset', '重置当前对话'),
                body: msg(
                  'conversation.reset_confirm',
                  '确定重置当前对话吗？对话历史会被清空，记忆会保留。',
                ),
                danger: true,
              })
            ) {
              try {
                await props.model.send('/api/conversation/reset', { method: 'POST' });
                toast(msg('conversation.reset_done', '当前对话已重置'));
              } catch (error) {
                toast(apiError(error).message);
              }
            }
          }}
        />
      </Group>
    </div>
  );
}
export function PromptPage(props: PageProps) {
  const saver = useSettings(props, msg('conversation.prompt', '自定义提示词'), undefined, false);
  const [draft, setDraft] = useState<string | null>(null),
    [busy, setBusy] = useState(false);
  const saved = String(saver.config.agent?.prompt || ''),
    text = draft ?? saved,
    dirty = text !== saved;
  async function submit() {
    if (!dirty || busy) return;
    const submitted = text;
    setBusy(true);
    try {
      if ((await saver.apply('agent.prompt', { agent: { prompt: submitted } })) === 'saved')
        setDraft((value) => (value === submitted ? null : value));
    } finally {
      setBusy(false);
    }
  }
  usePageHeader(props, msg('conversation.prompt', '自定义提示词'), dirty && !busy, submit);
  return (
    <Group>
      <form
        onSubmit={(event) => {
          event.preventDefault();
          void submit();
        }}
      >
        <label className="ds-field">
          <Text value={msg('conversation.prompt_extra', '附加指令')} />
          <textarea
            className="ds-textarea"
            data-action="edit-prompt"
            rows={10}
            value={text}
            disabled={busy}
            onChange={(event) => setDraft(event.target.value)}
          />
          <Text
            value={msg(
              'conversation.prompt_extra_help',
              '追加在内置主指令之后，适合填写设备、项目或环境专用要求',
            )}
          />
        </label>
        <Button type="submit" disabled={!dirty || busy}>
          <Text value={msg('action.save', '保存')} />
        </Button>
      </form>
    </Group>
  );
}
export const SEARCH_PROVIDERS = [
  { value: 'duckduckgo', label: 'DuckDuckGo', needsKey: false },
  { value: 'brave', label: 'Brave', needsKey: true },
  { value: 'tavily', label: 'Tavily', needsKey: true },
];
export function SearchPage(props: PageProps) {
  const saver = useSettings(props, msg('websearch.title', 'Web 搜索'), '/conversation');
  const search = section(saver.config, 'search');
  const pending = props.route.query?.provider;
  const provider =
    SEARCH_PROVIDERS.find((option) => option.value === (pending || search.provider)) ||
    SEARCH_PROVIDERS[0];
  const choose = async (value: string) => {
    const result = await saver.apply(
      'search.provider',
      { search: { provider: value } },
      { quietInvalid: true },
    );
    if (result === 'invalid')
      props.navigate(`/conversation/tools?provider=${encodeURIComponent(value)}`, {
        replace: true,
      });
    else if (result === 'saved') props.navigate('/conversation/tools', { replace: true });
  };
  if (props.route.path.endsWith('/provider'))
    return (
      <Group>
        {SEARCH_PROVIDERS.map((option) => (
          <Row
            key={option.value}
            label={option.label}
            value={search.provider === option.value ? '✓' : ''}
            action={`choose-search-${option.value}`}
            onPress={() => void choose(option.value)}
          />
        ))}
      </Group>
    );
  return (
    <div className="ds-screen">
      <Group>
        <Row
          label={msg('websearch.provider', '服务商')}
          value={provider.label}
          chevron
          onPress={() => props.navigate('/conversation/tools/provider')}
        />
        {provider.needsKey && (
          <ValueField
            key={provider.value}
            label={msg('websearch.api_key', '搜索 API 密钥')}
            saved=""
            secret
            placeholder={resolve(
              msg(
                !pending && search.has_api_key
                  ? 'websearch.api_key_saved'
                  : 'websearch.api_key_placeholder',
                '请输入',
              ),
            )}
            onCommit={async (value) => {
              const result = await saver.change('search', {
                search: { provider: provider.value, api_key: value },
              });
              if (result === 'saved' && pending)
                props.navigate('/conversation/tools', { replace: true });
              return result;
            }}
          />
        )}
      </Group>
    </div>
  );
}
export function MemoryPage(props: PageProps) {
  const saver = useSettings(props, msg('ui.nav_memory', '记忆'));
  const fields = [
    {
      name: 'quick_capture',
      field: {
        key: 'screen_memory_ttl',
        widget: 'select',
        enum: ['7d', '30d', '90d', '180d', '365d', 'forever'].map((value) => ({ value })),
        default: '90d',
      },
    },
    {
      name: 'voice_notifications',
      field: {
        key: 'retention_days',
        widget: 'number',
        enum: [3, 7, 14, 30, 60, 90].map((value) => ({ value: String(value) })),
        selectWhen: { all: [] },
      },
    },
  ];
  return (
    <div className="ds-screen">
      <Group>
        {!props.model.container && (
          <ConfigField
            section="quick_capture"
            field={{ key: 'enabled', widget: 'boolean', default: true }}
            value={section(saver.config, 'quick_capture').enabled}
            save={(value) =>
              saver.change('quick_capture.enabled', { quick_capture: { enabled: value } })
            }
          />
        )}
        {fields.map(({ name, field }) => (
          <ConfigField
            key={name}
            section={name}
            field={field}
            value={section(saver.config, name)[field.key]}
            save={(value) =>
              saver.change(`${name}.${field.key}`, { [name]: { [field.key]: value } })
            }
          />
        ))}
      </Group>
      <Group>
        <Row
          label={msg('memory.reset', '重置对话与记忆')}
          action="reset-memory"
          onPress={async () => {
            if (
              await confirmSheet({
                title: msg('memory.reset_confirm_title', '确认重置'),
                body: msg(
                  'memory.reset_confirm_body',
                  '此操作将删除所有对话历史和持久化记忆，并重启 Agent。此操作不可撤销。',
                ),
                danger: true,
              })
            ) {
              try {
                await props.model.send('/api/memory/reset', { method: 'POST' });
                toast(msg('memory.reset_done', '已重置，Agent 正在重启'));
              } catch (error) {
                toast(apiError(error).message);
              }
            }
          }}
        />
      </Group>
    </div>
  );
}
export function RequireConfig({
  props,
  children,
}: {
  props: PageProps;
  children: React.ReactNode;
}) {
  const resource = useAtomValue(props.model.resources.snapshot.atom);
  if (!resource.data)
    return (
      <div role="status">
        {resource.error || resolve(label('status.loading', '正在读取设备配置…'))}
        {resource.error && (
          <Button onClick={() => props.model.resources.snapshot.refresh()}>
            <Text value={msg('ui.retry', '重试')} />
          </Button>
        )}
      </div>
    );
  return children;
}
export type SettingValue = JsonValue;
export { usePageHeader };
