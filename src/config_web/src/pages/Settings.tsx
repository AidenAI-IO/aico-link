import { useEffect, useState } from 'react';
import { useAtomValue } from 'jotai';
import type { JsonValue, PageProps } from '../types';
import { useSettings, section, usePageHeader } from '../hooks/settings';
import { useSchema } from '../hooks/query';
import { sectionFields } from '../lib/schema';
import { applyLocale, msg, resolve, t } from '../lib/i18n';
import { apiError, json } from '../api/client';
import { ConfigField, QueryError, ValueField } from '../components/fields';
import { ChoiceRow, Dropdown } from '../components/choices';
import { IntroTile, SelectedCheck } from '../components/marks';
import { Button, Group, Icon, Row, Switch, Text } from '../components/ui';
import { Loading } from '../components/Loading';
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
          value={config.agent?.locale === 'en-US' ? 'English' : '简体中文'}
          chevron
          onPress={() => props.navigate('/basic/language')}
        />
      </Group>
      <Group>
        <ChoiceRow
          label={msg('basic.device_type', '设备类型')}
          options={[
            { value: 'iOS', label: 'iOS', icon: 'apple' },
            { value: 'Android', label: 'Android', icon: 'android' },
            { value: 'macOS', label: 'macOS', icon: 'monitor' },
            { value: 'windows', label: 'Windows', icon: 'windows' },
            { value: 'linux', label: 'Linux', icon: 'terminal' },
          ]}
          selected={String(section(config, 'device').device_type || 'iOS')}
          layout="cards"
          action="open-device-type"
          onSelect={(value) =>
            saver.change('device.device_type', { device: { device_type: value } })
          }
        />
        <ChoiceRow
          label={msg('basic.keyboard_layout', '键盘布局')}
          options={['qwerty', 'azerty', 'qwertz'].map((value) => ({
            value,
            label: value.toUpperCase(),
          }))}
          selected={String(section(config, 'hid').keyboard_layout || 'qwerty')}
          layout="cards"
          action="open-keyboard-layout"
          onSelect={(value) =>
            saver.change('hid.keyboard_layout', { hid: { keyboard_layout: value } })
          }
        />
      </Group>
      <Group>
        <Row
          label={msg('config.fields.agent.tap_wakeup_enabled.label', '轻触唤醒')}
          description={msg(
            'config.fields.agent.tap_wakeup_enabled.help',
            '开启后，MCU 轻触按键可唤醒 Aico。关闭后不影响 ASR 等其它唤醒方式。',
          )}
        >
          <ConfigToggle
            saver={saver}
            name="agent"
            fieldKey="tap_wakeup_enabled"
            value={config.agent?.tap_wakeup_enabled !== false}
            label={msg('config.fields.agent.tap_wakeup_enabled.label', '轻触唤醒')}
          />
        </Row>
      </Group>
    </div>
  );
}
function ConfigToggle({
  saver,
  name,
  fieldKey,
  value,
  label,
}: {
  saver: ReturnType<typeof useSettings>;
  name: string;
  fieldKey: string;
  value: boolean;
  label: import('../types').TextSpec;
}) {
  const [draft, setDraft] = useState<boolean | null>(null),
    [busy, setBusy] = useState(false);
  useEffect(() => {
    setDraft((current) => (current === value ? null : current));
  }, [value]);
  return (
    <Switch
      label={label}
      checked={draft ?? value}
      disabled={busy}
      onChange={async (next) => {
        setDraft(next);
        setBusy(true);
        try {
          const result = await saver.change(`${name}.${fieldKey}`, {
            [name]: { [fieldKey]: next },
          });
          if (result !== 'staged') setDraft(null);
        } catch (error) {
          setDraft(null);
          toast(apiError(error).message);
        } finally {
          setBusy(false);
        }
      }}
    />
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
  const timezone = String(saver.config.agent?.timezone || 'UTC');
  const zones = field?.enum?.length
    ? field.enum.map((option) => option.value)
    : [
        'Africa/Johannesburg',
        'America/Chicago',
        'America/Denver',
        'America/Los_Angeles',
        'America/New_York',
        'America/Sao_Paulo',
        'America/Toronto',
        'America/Vancouver',
        'Asia/Bangkok',
        'Asia/Dubai',
        'Asia/Hong_Kong',
        'Asia/Jakarta',
        'Asia/Kolkata',
        'Asia/Kuala_Lumpur',
        'Asia/Seoul',
        'Asia/Shanghai',
        'Asia/Singapore',
        'Asia/Taipei',
        'Asia/Tokyo',
        'Australia/Sydney',
        'Europe/Amsterdam',
        'Europe/Berlin',
        'Europe/London',
        'Europe/Moscow',
        'Europe/Paris',
        'Pacific/Auckland',
        'UTC',
      ];
  return (
    <div className="ds-screen">
      <Group>
        <div className="basic-intro">
          <IntroTile kind="language" />
          <Text value={msg('basic.language_title', '语言与时区')} className="page__title" />
          <Text
            value={
              props.model.local
                ? msg('local.language_help', '设置 Agent 和配置界面的语言，以及任务使用的时区。')
                : msg(
                    'basic.language_description',
                    '连接后，Aico 可实现消息推送、信息捕捉，还能学习你的操作方式，借助自动化能力简化重复任务。',
                  )
            }
            className="page__description"
          />
        </div>
        {[
          { value: 'en-US', label: 'English' },
          { value: 'zh-CN', label: '简体中文' },
        ].map((option) => (
          <Row
            key={option.value}
            label={option.label}
            className="ds-row--plain"
            onPress={() => void choose(option.value)}
          >
            {saver.config.agent?.locale === option.value && <SelectedCheck />}
          </Row>
        ))}
      </Group>
      <QueryError {...schema} />
      <Dropdown
        label={msg('basic.timezone', '时区')}
        options={zones.map((value) => ({
          value,
          label:
            (
              { 'Asia/Shanghai': 'Shanghai', 'Asia/Hong_Kong': 'Hong Kong' } as Record<
                string,
                string
              >
            )[value] || value.replaceAll('_', ' '),
        }))}
        selected={timezone}
        action="open-timezone"
        onSelect={(value) => saver.change('agent.timezone', { agent: { timezone: value } })}
      />
    </div>
  );
}

export function ConversationPage(props: PageProps) {
  const saver = useSettings(props, msg('ui.nav_conversation', '对话与工具'));
  const values = section(saver.config, 'agent');
  const limits = [
    {
      key: 'max_iterations',
      label: msg('conversation.max_iterations', '最大工具调用轮数'),
      help: msg(
        'conversation.max_iterations_help',
        '单个任务中模型与工具的最大循环次数，-1 表示不限',
      ),
    },
    {
      key: 'context_prune_threshold',
      label: msg('conversation.prune_threshold', '历史消息裁剪阈值'),
      help: msg(
        'conversation.prune_threshold_help',
        '上下文占用达到该比例时清理旧状态并压缩较早的工具结果，0 表示自动（0.5）',
      ),
    },
    {
      key: 'context_prune_state_limit',
      label: msg('conversation.prune_state_limit', '截图状态上限'),
      help: msg(
        'conversation.prune_state_limit_help',
        '零散的状态超过该数量时清理旧状态，只保留当前这一轮的最新状态，0 表示自动（6）',
      ),
    },
    {
      key: 'context_compaction_threshold',
      label: msg('conversation.compaction_threshold', '上下文压缩阈值'),
      help: msg(
        'conversation.compaction_threshold_help',
        '上下文占用达到此比例时总结较早的对话，0 表示自动（0.8）',
      ),
    },
  ];
  return (
    <div className="ds-screen">
      <Group>
        <Row
          label={msg('conversation.prompt', '自定义提示词')}
          value={
            values.prompt
              ? msg('conversation.prompt_custom', '已设置')
              : msg('conversation.prompt_default', '默认')
          }
          chevron
          onPress={() => props.navigate('/conversation/prompt')}
        />
        {limits.map((field) => (
          <ValueField
            key={field.key}
            inline
            label={field.label}
            description={field.help}
            saved={values[field.key]}
            inputMode={field.key.includes('threshold') ? 'decimal' : 'numeric'}
            parse={(text) => {
              const value = Number(text);
              return text !== '' &&
                Number.isFinite(value) &&
                (field.key.includes('threshold')
                  ? value >= 0 && value < 1
                  : Number.isInteger(value) && value >= (field.key === 'max_iterations' ? -1 : 0))
                ? value
                : undefined;
            }}
            onCommit={(value) =>
              saver.change(`agent.${field.key}`, { agent: { [field.key]: value } })
            }
          />
        ))}
      </Group>
      <Group
        footnote={msg(
          'conversation.ui_grounding_help',
          '帮助 Aico 更准确地点击屏幕上的按钮和输入框。',
        )}
      >
        <Row label={msg('conversation.ui_grounding', '自动标注可点击元素')}>
          <ConfigToggle
            saver={saver}
            name="agent"
            fieldKey="ui_grounding_enabled"
            value={values.ui_grounding_enabled !== false}
            label={msg('conversation.ui_grounding', '自动标注可点击元素')}
          />
        </Row>
      </Group>
      <Group>
        <Row
          label={msg('conversation.tools', '工具设置')}
          leading={<Icon name="gearBadge" className="ds-row__badge" />}
          chevron
          onPress={() => props.navigate('/conversation/tools')}
        />
        <Row
          label={msg('conversation.reset', '重置当前对话')}
          labelTone="danger"
          leading={<Icon name="alertBadge" className="ds-row__badge" />}
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
        <label className="ds-form-card">
          <Text
            value={msg('conversation.prompt_extra', '附加指令')}
            className="ds-form-card__title"
          />
          <Text
            value={msg(
              'conversation.prompt_extra_help',
              '追加在内置主指令之后，适合填写设备、项目或环境专用要求',
            )}
            className="ds-form-card__help"
          />
          <textarea
            className="ds-textarea"
            placeholder={resolve(
              msg('conversation.prompt_placeholder', '例如：回答保持简洁；操作前先确认当前页面'),
            )}
            data-action="edit-prompt"
            rows={6}
            value={text}
            disabled={busy}
            onChange={(event) => setDraft(event.target.value)}
          />
        </label>
      </form>
    </Group>
  );
}
export const SEARCH_PROVIDERS = [
  { value: 'duckduckgo', label: 'DuckDuckGo', needsKey: false },
  { value: 'brave', label: 'Brave', needsKey: true },
  { value: 'tavily', label: 'Tavily', needsKey: true },
];
function SearchMark({ provider }: { provider: string }) {
  return (
    <img
      className="ds-row__logo"
      src={`/assets/icons/search/${provider}.png`}
      alt=""
      width={32}
      height={32}
    />
  );
}
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
            leading={<SearchMark provider={option.value} />}
            action={`choose-search-${option.value}`}
            onPress={() => void choose(option.value)}
          >
            {search.provider === option.value && <SelectedCheck />}
          </Row>
        ))}
      </Group>
    );
  return (
    <div className="ds-screen">
      <Group joined>
        <div className="page__intro page__intro--in-card">
          <IntroTile kind="search" />
          <Text value={msg('websearch.title', 'Web 搜索')} className="page__title" />
          <Text
            value={msg(
              'websearch.description',
              '连接后，Aico 可实现消息推送、信息捕捉，还能学习你的操作方式，借助自动化能力简化重复任务。',
            )}
            className="page__description"
          />
        </div>
        <div className="ds-form-card">
          <Text value={msg('websearch.provider', '服务商')} className="ds-form-card__title" />
          <Row
            label={provider.label}
            leading={<SearchMark provider={provider.value} />}
            chevron
            className="ds-row--outlined"
            action="open-search-provider"
            onPress={() => props.navigate('/conversation/tools/provider')}
          />
        </div>
        {provider.needsKey && (
          <ValueField
            key={provider.value}
            label={msg('websearch.api_key', '搜索 API 密钥')}
            saved=""
            secret
            description={
              pending
                ? msg('websearch.api_key_required', '填写 API 密钥后才会切换到该服务商')
                : undefined
            }
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
  const ttl = String(section(saver.config, 'quick_capture').screen_memory_ttl || '90d');
  const retention = section(saver.config, 'voice_notifications').retention_days;
  return (
    <div className="ds-screen">
      {!props.model.container && (
        <Group
          footnote={msg(
            'memory.screen_enabled_help',
            '开启后，按下设备上的截屏按键会保存当前屏幕内容，供 Aico 之后回忆。',
          )}
        >
          <Row label={msg('memory.screen_enabled', '屏幕记忆')}>
            <ConfigToggle
              saver={saver}
              name="quick_capture"
              fieldKey="enabled"
              value={section(saver.config, 'quick_capture').enabled !== false}
              label={msg('memory.screen_enabled', '屏幕记忆')}
            />
          </Row>
        </Group>
      )}
      <Group>
        <ChoiceRow
          label={msg('memory.screen_ttl', '屏幕记忆保留期')}
          selected={ttl}
          options={[...new Set([ttl, '7d', '30d', '90d', '180d', '365d', 'forever'])].map(
            (value) => ({
              value,
              label:
                value === 'forever'
                  ? msg('memory.forever', '永久')
                  : /^\d+d$/.test(value)
                    ? t('memory.days', {
                        defaultValue: '{{n}} 天',
                        n: Number(value.slice(0, -1)),
                      })
                    : value,
            }),
          )}
          onSelect={(value) =>
            saver.change('quick_capture.screen_memory_ttl', {
              quick_capture: { screen_memory_ttl: value },
            })
          }
        />
        <ChoiceRow
          label={msg('memory.notification_days', '通知记忆保留天数')}
          selected={String(retention ?? '')}
          options={[
            ...new Set([
              ...(typeof retention === 'number' ? [retention] : []),
              3,
              7,
              14,
              30,
              60,
              90,
            ]),
          ].map((value) => ({
            value: String(value),
            label: t('memory.days', { defaultValue: '{{n}} 天', n: value }),
          }))}
          onSelect={(value) =>
            saver.change('voice_notifications.retention_days', {
              voice_notifications: { retention_days: Number(value) },
            })
          }
        />
      </Group>
      <Group
        footnote={msg(
          'memory.reset_help',
          '删除对话历史和所有持久化记忆，然后重启 Agent 并开始新的对话。',
        )}
      >
        <Row
          labelTone="danger"
          leading={<Icon name="alertBadge" className="ds-row__badge" />}
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
  const resource = useAtomValue(props.model.resources.snapshot.view);
  if (!resource.data && !resource.error)
    return (
      <div className="ds-screen">
        <Group>
          <Loading block />
        </Group>
      </div>
    );
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
