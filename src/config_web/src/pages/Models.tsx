export const CONNECTION_KEYS = new Set([
  'api_key',
  'secret_id',
  'secret_key',
  'app_id',
  'base_url',
  'endpoint',
  'auth_mode',
  'project_id',
  'location',
  'workspace_id',
  'upstream_provider',
  'agent_id',
  'region',
]);
import { useState } from 'react';
import type { ReactNode } from 'react';
import { SwipeRow } from '../components/SwipeRow';
import type { ConfigPatch, JsonObject, PageProps, ProviderRecord, VoiceMode } from '../types';
import { useSettings, section, usePageHeader } from '../hooks/settings';
import { useQuery, useSchema } from '../hooks/query';
import { matches, sectionFields, fieldLabel, optionsFor, placeholderFor } from '../lib/schema';
import type { Field } from '../lib/schema';
import { apiError, json } from '../api/client';
import { getActiveLocale, msg, resolve } from '../lib/i18n';
import { ChoiceRow, CustomValue } from '../components/choices';
import { ProviderMark, SelectedCheck } from '../components/marks';
import { Button, Group, Row, Text } from '../components/ui';
import { ConfigField, FieldInput, QueryError, ValueField } from '../components/fields';
import { confirmSheet, toast } from '../components/overlays';
export const PROVIDER_TYPES = [
  'openai',
  'anthropic',
  'openrouter',
  'kimi',
  'kimi-cn',
  'volcengine',
  'deepseek',
  'ollama',
  'gemini',
];
export const providerLabel = (type: string) =>
  (
    ({
      openai: 'OpenAI',
      anthropic: 'Anthropic',
      openrouter: 'OpenRouter',
      deepseek: 'DeepSeek',
      ollama: 'Ollama',
      gemini: 'Gemini',
      'openai-whisper': 'OpenAI Whisper',
      'google-cloud': 'Google Cloud',
      'fish-audio': 'Fish Audio',
      minimax: 'MiniMax',
    }) as Record<string, string>
  )[type] ||
  resolve(
    msg(
      type === 'custom'
        ? 'model.others'
        : `${['tencent-asr', 'qwen-asr', 'alicloud', 'minimax-cn'].includes(type) ? 'voice' : 'model'}.type_${type.replaceAll('-', '_')}`,
      type,
    ),
  );
export function recordLabel(name: string, record?: ProviderRecord) {
  const label = providerLabel(record?.type || '');
  return name.toLowerCase() === (record?.type || '').toLowerCase() ? label : `${name} (${label})`;
}
export const DECISION_PROVIDERS = ['typesafe', 'openai', 'gemini', 'openrouter'];
export interface ModelSpec {
  reasoning?: { supported?: boolean; efforts?: string[] };
}
export interface ModelCatalogue {
  models: { id: string; name?: string; recommended?: boolean }[];
  spec?: ModelSpec;
}
export function effortChoices(spec?: ModelSpec) {
  return spec?.reasoning?.supported === false
    ? null
    : [
        '',
        ...(spec?.reasoning?.efforts?.length ? spec.reasoning.efforts : ['low', 'medium', 'high']),
      ];
}
export function newRecordName(
  record: { type: string; base_url?: unknown },
  records: Record<string, unknown>,
) {
  let base = record.type;
  if (record.type === 'openai' && record.base_url) {
    try {
      const parts = new URL(String(record.base_url)).hostname.split('.').filter(Boolean);
      let end = parts.length - 1;
      if (end > 1 && ['com', 'co', 'net', 'org', 'gov', 'edu', 'ac'].includes(parts[end - 1]))
        end--;
      base = parts[end - 1] || parts[0] || base;
    } catch {
      /* The server validates endpoint URLs. */
    }
  }
  base = base.replace(/[^A-Za-z0-9_-]/g, '-') || 'provider';
  let name = base;
  let index = 2;
  while (Object.hasOwn(records, name)) name = `${base}-${index++}`;
  return name;
}
export const recordsOf = (
  config: import('../types').Configuration,
  name: string,
): Record<string, ProviderRecord> => section(config, name) as Record<string, ProviderRecord>;
export async function selectModel(
  props: PageProps,
  provider: string,
  type: string,
  model: string,
  effort: unknown,
): Promise<ConfigPatch> {
  const patch: ConfigPatch = { model: { provider, model } };
  if (effort) {
    const catalog = await props.model
      .send<ModelCatalogue>(`/api/models?${new URLSearchParams({ provider: type, model })}`)
      .catch(() => ({ models: [] }) as ModelCatalogue);
    if (!effortChoices(catalog.spec)?.includes(String(effort)))
      patch.model = { provider, model, reasoning_effort: '' };
  }
  return patch;
}
export function ModelsPage(props: PageProps) {
  const saver = useSettings(props, msg('ui.nav_model', '模型设置'));
  const name = saver.config.model?.provider || '',
    record = saver.config.model_providers?.[name];
  const decision = section(saver.config, 'decision');
  const on = !!(decision.enabled && decision.has_api_key);
  return (
    <div className="ds-screen">
      <Group>
        <Row
          label={msg('model.main', '主模型')}
          chevron
          onPress={() => props.navigate('/model/main')}
        >
          <span className="ds-row__value-with-mark">
            {record && <ProviderMark type={record.type} />}
            <Text
              value={
                name
                  ? [recordLabel(name, record), saver.config.model?.model]
                      .filter(Boolean)
                      .join(' · ')
                  : msg('model.not_set', '未设置')
              }
              className="ds-row__value"
            />
          </span>
        </Row>
      </Group>
      <Group caption={msg('model.section_auxiliary', '辅助模型（可选）')}>
        <Row
          label={msg('model.decision', '决策模型')}
          value={on ? msg('model.decision_on', '已开启') : msg('model.decision_off', '未开启')}
          tone={on ? 'success' : undefined}
          chevron
          onPress={() => props.navigate('/model/decision')}
        />
      </Group>
    </div>
  );
}

export function MainModelPage(props: PageProps) {
  const saver = useSettings(props, msg('model.main', '主模型'), '/model');
  const model = section(saver.config, 'model');
  const type = saver.config.model_providers?.[String(model.provider)]?.type || '';
  const catalogue = useQuery(
    (signal) =>
      props.model.send<ModelCatalogue>(
        `/api/models?${new URLSearchParams({ provider: type, model: String(model.model || ''), locale: getActiveLocale() })}`,
        { signal },
      ),
    [props.model, type, model.model],
  );
  const efforts = effortChoices(catalogue.data?.spec);
  const record = saver.config.model_providers?.[String(model.provider || '')];
  const numberField = (
    key: string,
    label: import('../types').TextSpec,
    help: import('../types').TextSpec,
    nullable = false,
  ) => (
    <ValueField
      key={key}
      inline
      label={label}
      description={help}
      saved={model[key]}
      placeholder={nullable ? resolve(msg('model.auto', '自动')) : ''}
      inputMode={nullable ? 'decimal' : 'numeric'}
      parse={(text) =>
        text === '' && nullable
          ? null
          : text !== '' &&
              Number.isFinite(Number(text)) &&
              Number(text) >= 0 &&
              (nullable ? Number(text) <= 2 : Number.isInteger(Number(text)))
            ? Number(text)
            : undefined
      }
      onCommit={(value) => saver.change(`model.${key}`, { model: { [key]: value } })}
    />
  );
  return (
    <div className="ds-screen">
      <Group>
        <Row
          label={msg('model.provider', '主模型提供商')}
          chevron
          onPress={() => props.navigate('/model/providers')}
        >
          <span className="ds-row__value-with-mark">
            {record && <ProviderMark type={record.type} />}
            <Text
              value={
                model.provider
                  ? recordLabel(String(model.provider), record)
                  : msg('model.not_set', '未设置')
              }
              className="ds-row__value"
            />
          </span>
        </Row>
      </Group>
      <Group caption={msg('model.section_model', '模型配置')}>
        <Row
          label={msg('model.model', '模型')}
          value={String(model.model || resolve(msg('model.not_set', '未设置')))}
          chevron
          onPress={() => props.navigate('/model/models')}
        />
      </Group>
      <Group caption={msg('model.section_parameters', '参数设置')}>
        {numberField(
          'temperature',
          msg('model.temperature', '温度'),
          msg('model.temperature_help', '越高回答越发散；留空表示使用模型默认值'),
          true,
        )}
        {efforts ? (
          <ChoiceRow
            label={msg('model.reasoning_effort', '推理强度')}
            selected={String(model.reasoning_effort || '')}
            options={efforts.map((value) => ({
              value,
              label: msg(
                value ? `model.effort_${value}` : 'model.effort_auto',
                value || '自动（默认）',
              ),
            }))}
            onSelect={(value) =>
              saver.change('model.reasoning_effort', { model: { reasoning_effort: value } })
            }
          />
        ) : (
          <Row
            label={msg('model.reasoning_effort', '推理强度')}
            value={msg('model.effort_unsupported', '不支持')}
          />
        )}
        {numberField(
          'max_response_tokens',
          msg('model.max_response_tokens', '最大响应 Token 数'),
          msg('model.max_response_tokens_help', '0 表示不设上限，使用提供商默认值'),
        )}
      </Group>
      <Group caption={msg('model.section_context', '上下文限制')}>
        {numberField(
          'context_window',
          msg('model.context_window', '上下文窗口（Token）'),
          msg('model.auto_help', '0 表示自动，使用提供商公布的数值'),
        )}
        {numberField(
          'model_max_output_tokens',
          msg('model.max_output', '提供商输出上限（Token）'),
          msg('model.auto_help', '0 表示自动，使用提供商公布的数值'),
        )}
      </Group>
      <ConnectionTest props={props} section="model" values={model} />
    </div>
  );
}
export function ConnectionTest({
  props,
  section: sectionName,
  values,
}: {
  props: PageProps;
  section: string;
  values: Record<string, unknown>;
}) {
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState('');
  return (
    <div className="ds-group">
      <Button
        className="ds-card-action"
        action={`test-${sectionName}-connection`}
        disabled={busy}
        onClick={async () => {
          setBusy(true);
          try {
            const response = await props.model.send<{
              ok: boolean;
              results?: { passed: boolean; detail?: string }[];
            }>('/api/config/test', { method: 'POST', ...json({ section: sectionName, values }) });
            setResult(
              response.ok
                ? resolve(msg('model.test_passed', '连接成功'))
                : response.results?.find((entry) => !entry.passed)?.detail ||
                    resolve(msg('model.test_failed', '连接失败')),
            );
          } catch (error) {
            setResult(apiError(error).message);
          } finally {
            setBusy(false);
          }
        }}
      >
        <Text value={msg('model.test', '测试连接')} />
      </Button>
      {result && <p role="status">{result}</p>}
    </div>
  );
}
export function ProviderForm({
  props,
  recordSection,
  type,
  existingName,
  mode,
  back,
  onSaved,
  actions,
}: {
  props: PageProps;
  recordSection: string;
  type: string;
  existingName?: string;
  mode?: VoiceMode;
  back: string;
  onSaved?: (name: string) => Promise<void>;
  actions?: ReactNode;
}) {
  const saver = useSettings(props, msg('model.edit_provider_title', '编辑提供商'), back, false);
  const records = recordsOf(saver.config, recordSection),
    existing = records[existingName || ''];
  const [draft, setDraft] = useState<JsonObject>({ type });
  const [busy, setBusy] = useState(false);
  const [providerType, setProviderType] = useState(type === 'custom' ? 'openai' : type);
  const schema = useSchema(props.model);
  const values = { ...existing, ...draft, type: providerType };
  const ctx = Object.fromEntries(
    Object.entries(values).map(([key, value]) => [`${recordSection}.${key}`, value]),
  );
  ctx['agent.input_mode'] = mode || saver.config.agent?.input_mode || '';
  const fields = sectionFields(schema.data, recordSection).filter(
    (field) =>
      field.key !== 'type' &&
      field.key !== 'model' &&
      (recordSection === 'model_providers' || CONNECTION_KEYS.has(field.key)) &&
      matches(field.visibleWhen, ctx),
  );
  const fallback: Field[] = [{ key: 'api_key', secret: true }, { key: 'base_url' }];
  const shown = fields.length ? fields : recordSection === 'model_providers' ? fallback : [];
  const touched =
    providerType !== (type === 'custom' ? 'openai' : type) ||
    Object.keys(draft).length > 1 ||
    (!existing && shown.length === 0 && !!schema.data);
  async function submit() {
    if (busy || !schema.data) return;
    const record: JsonObject = { type: providerType };
    for (const field of shown) {
      const value = draft[field.key];
      if (value === undefined || (field.secret && value === '')) continue;
      record[field.key] = value;
    }
    const name =
      existingName || newRecordName({ type: providerType, base_url: record.base_url }, records);
    setBusy(true);
    try {
      const outcome = await saver.apply(`${recordSection}.${name}`, {
        [recordSection]: { [name]: record },
      });
      if (outcome === 'saved') {
        await onSaved?.(name);
        props.navigate(back, { replace: true });
      }
    } finally {
      setBusy(false);
    }
  }
  usePageHeader(
    props,
    existing ? msg('model.edit_provider_title', '编辑提供商') : providerLabel(type),
    touched && !busy,
    submit,
    'live',
    back,
  );
  return (
    <div className="ds-screen">
      <QueryError {...schema} />
      <form
        onSubmit={(event) => {
          event.preventDefault();
          void submit();
        }}
      >
        <Group joined>
          <div className="page__intro page__intro--in-card">
            <ProviderMark type={type} tile />
            <Text
              value={existing ? recordLabel(existingName || '', existing) : providerLabel(type)}
              className="page__title"
            />
          </div>
          {type === 'custom' && (
            <div className="ds-form-card">
              <Text
                value={msg('model.interface_type', '接口类型')}
                className="ds-form-card__title"
              />
              <ChoiceRow
                label={msg('model.interface_type', '接口类型')}
                leading={<ProviderMark type={providerType} />}
                layout="cards"
                selected={providerType}
                options={['openai', 'anthropic'].map((value) => ({
                  value,
                  label: msg(`model.relay_${value}`, `${providerLabel(value)} 兼容`),
                  leading: <ProviderMark type={value} />,
                }))}
                outlined
                chevron
                className="ds-row--outlined"
                onSelect={setProviderType}
                disabled={busy}
              />
            </div>
          )}
          {shown.map((field) => {
            const value = draft[field.key] ?? (field.secret ? '' : existing?.[field.key]) ?? '';
            const options = optionsFor(field, providerType);
            return field.widget === 'select' ||
              (field.selectWhen && matches(field.selectWhen, ctx)) ? (
              <div className="ds-form-card" key={field.key}>
                <Text value={fieldLabel(recordSection, field)} className="ds-form-card__title" />
                <ChoiceRow
                  label={fieldLabel(recordSection, field)}
                  options={options.map((option) => ({
                    value: String(option.value),
                    label: option.label || String(option.value),
                  }))}
                  selected={String(value)}
                  disabled={busy}
                  outlined
                  chevron
                  className="ds-row--outlined"
                  onSelect={(value) =>
                    setDraft((previous) => ({ ...previous, [field.key]: value }))
                  }
                />
              </div>
            ) : (
              <FieldInput
                key={field.key}
                label={
                  recordSection === 'model_providers' && field.key === 'api_key'
                    ? msg('model.api_key', 'API Key')
                    : recordSection === 'model_providers' && field.key === 'base_url'
                      ? msg('model.base_url', 'Base URL')
                      : fieldLabel(recordSection, field)
                }
                secret={field.secret}
                value={String(value)}
                disabled={busy}
                placeholder={
                  field.secret && existing?.[`has_${field.key}`]
                    ? resolve(msg('model.api_key_saved', '已保存，留空保持不变'))
                    : recordSection === 'model_providers' && field.key === 'api_key'
                      ? resolve(
                          providerType === 'ollama'
                            ? msg('model.api_key_optional', '可选')
                            : msg('model.api_key_placeholder', '请输入 API Key'),
                        )
                      : recordSection === 'model_providers' && field.key === 'base_url'
                        ? resolve(
                            type === 'custom'
                              ? msg('model.base_url_placeholder', 'https://example.com/v1')
                              : msg('model.base_url_optional', '可选，留空使用官方地址'),
                          )
                        : placeholderFor(field, ctx)
                }
                onChange={(value) =>
                  setDraft((previous) => ({
                    ...previous,
                    [field.key]: field.widget === 'number' ? Number(value) : value,
                  }))
                }
              />
            );
          })}
        </Group>
      </form>
      {actions}
    </div>
  );
}
export function ProvidersPage(props: PageProps) {
  const saver = useSettings(props, msg('model.provider', '主模型提供商'), '/model/main');
  const records = saver.config.model_providers || {};
  return (
    <div className="ds-screen">
      {Object.keys(records).length > 0 && (
        <Group caption={msg('model.configured', '已配置')} captionMarker="success">
          {Object.entries(records)
            .sort(([a], [b]) => a.localeCompare(b))
            .map(([name, record]) => (
              <SwipeRow
                key={name}
                action={`delete-provider-${name}`}
                lockedLabel={
                  saver.config.model?.provider === name ? msg('model.in_use', '使用中') : undefined
                }
                onLocked={() =>
                  toast(msg('model.in_use_hint', '正在使用的配置不能删除，请先切换到其他提供商。'))
                }
                onDelete={() => void deleteProvider(saver, 'model_providers', name)}
              >
                <Row
                  label={recordLabel(name, record)}
                  leading={<ProviderMark type={record.type} />}
                  description={hostOf(record.base_url)}
                  chevron
                  action={`edit-provider-${name}`}
                  onPress={() =>
                    props.navigate(`/model/providers/edit?name=${encodeURIComponent(name)}`)
                  }
                >
                  {saver.config.model?.provider === name && <SelectedCheck />}
                </Row>
              </SwipeRow>
            ))}
        </Group>
      )}
      <Group caption={msg('model.unconfigured', '未配置')} captionMarker="muted">
        {PROVIDER_TYPES.filter(
          (type) => !Object.values(records).some((record) => record.type === type),
        ).map((type) => (
          <Row
            key={type}
            label={providerLabel(type)}
            leading={<ProviderMark type={type} />}
            className="ds-row--muted"
            chevron
            onPress={() => props.navigate(`/model/providers/edit?type=${type}`)}
          />
        ))}
        <Row
          label={msg('model.others', '其他')}
          labelTone="accent"
          leading={<ProviderMark type="custom" />}
          onPress={() => props.navigate('/model/providers/edit?type=custom')}
        />
      </Group>
    </div>
  );
}
export function ModelPicker(props: PageProps) {
  const saver = useSettings(props, msg('model.model', '模型'), '/model/main');
  const selected = saver.config.model || {};
  const type = saver.config.model_providers?.[selected.provider || '']?.type || '';
  const catalog = useQuery(
    (signal) =>
      props.model.send<ModelCatalogue>(
        `/api/models?provider=${encodeURIComponent(type)}&locale=${getActiveLocale()}`,
        { signal },
      ),
    [type, props.model],
  );
  async function choose(model: string) {
    const outcome = await saver.apply(
      'model.model',
      await selectModel(props, selected.provider || '', type, model, selected.reasoning_effort),
    );
    if (outcome === 'saved') props.navigate('/model/main');
  }
  return (
    <div className="ds-screen">
      <QueryError {...catalog} />
      <Group>
        {catalog.data?.models.map((model) => (
          <Row
            key={model.id}
            label={model.name || model.id}
            description={model.id}
            onPress={() => void choose(model.id)}
          >
            {selected.model === model.id && <SelectedCheck />}
          </Row>
        ))}
      </Group>
      <Group>
        <CustomValue
          label={msg('model.custom_model', '其他模型')}
          placeholder={msg('model.custom_model_placeholder', '输入模型 ID')}
          onUse={choose}
        />
      </Group>
    </div>
  );
}
export function DecisionPage(props: PageProps) {
  const saver = useSettings(props, msg('model.decision', '决策模型'), '/model');
  const decision = section(saver.config, 'decision');
  const provider = String(decision.provider || 'typesafe');
  const catalog = useQuery(
    (signal) =>
      provider === 'openrouter'
        ? props.model.send<ModelCatalogue>(
            '/api/models?provider=openrouter&output_modalities=decisions&order=top-weekly',
            { signal },
          )
        : Promise.resolve({ models: [] }),
    [props.model, provider],
  );
  async function choose(value: string) {
    if (value === provider) return;
    const patch: JsonObject = { provider: value, base_url: null, model: null };
    if (decision.has_api_key || decision.enabled) {
      if (
        !(await confirmSheet({
          title: msg('model.decision_switch_title', '切换决策模型提供商？'),
          body: msg(
            'model.decision_switch_body',
            '已保存的 API Key 会被清除，决策模型会先关闭，填写新提供商的 API Key 后再开启。',
          ),
        }))
      )
        return;
      patch.api_key = '';
      patch.enabled = false;
    }
    await saver.change('decision.provider', { decision: patch });
  }
  return (
    <div className="ds-screen">
      <Group
        note={msg(
          'model.decision_enabled_help',
          '开启后，固定流程里的判断同时问决策模型和主模型，决策模型有把握时直接采用，否则用主模型的结果。',
        )}
      >
        <ConfigField
          section="decision"
          field={{ key: 'enabled', widget: 'boolean' }}
          value={decision.enabled}
          save={(value) => {
            if (value && !decision.has_api_key) {
              toast(msg('model.decision_key_required', '请先填写 API Key'));
              return Promise.resolve('invalid');
            }
            return saver.change('decision.enabled', { decision: { enabled: value } });
          }}
        />
      </Group>
      <Group>
        <ChoiceRow
          label={msg('model.decision_provider', '提供商')}
          selected={provider}
          options={DECISION_PROVIDERS.map((value) => ({
            value,
            label:
              value === 'openrouter'
                ? 'OpenRouter'
                : msg(`model.decision_provider_${value}`, value),
          }))}
          chevron
          onSelect={choose}
        />
      </Group>
      <Group key={provider}>
        <ValueField
          label={msg('model.decision_api_key', '决策模型 API Key')}
          secret
          description={msg(`model.decision_provider_${provider}_help`, '')}
          saved=""
          placeholder={resolve(
            msg(
              decision.has_api_key ? 'model.api_key_saved' : 'model.api_key_placeholder',
              '请输入 API Key',
            ),
          )}
          onCommit={(value) => saver.change('decision.api_key', { decision: { api_key: value } })}
        />
      </Group>
      {provider === 'openrouter' && (
        <>
          <Group>
            <ValueField
              label={msg('config.fields.decision.model.label', '决策模型')}
              saved={decision.model}
              action="decision-model"
              placeholder="~typesafe/jev-latest"
              description={msg(
                'model.decision_openrouter_model_help',
                '留空默认使用 Jev；输入时显示模型建议，也可填写其他模型 ID',
              )}
              suggestions={catalog.data?.models.map((model) => model.id)}
              onCommit={(value) =>
                saver.change('decision.model', { decision: { model: value || null } })
              }
            />
          </Group>
          <Group>
            <ValueField
              label={msg('config.fields.decision.base_url.label', '决策模型 Base URL')}
              saved={decision.base_url}
              placeholder="https://openrouter.ai/api/alpha"
              description={msg('model.decision_base_url_help', '留空使用官方地址')}
              onCommit={(value) =>
                saver.change('decision.base_url', { decision: { base_url: value || null } })
              }
            />
          </Group>
        </>
      )}
      <Group>
        <ValueField
          inline
          description={msg(
            'model.decision_min_confidence_help',
            '低于该值的判断交回主模型，默认 0.8',
          )}
          label={msg('model.decision_min_confidence', '最低置信度')}
          saved={decision.min_confidence}
          parse={(value) =>
            value === ''
              ? null
              : Number.isFinite(Number(value)) && Number(value) >= 0 && Number(value) <= 1
                ? Number(value)
                : undefined
          }
          onCommit={(value) =>
            saver.change('decision.min_confidence', { decision: { min_confidence: value } })
          }
        />
      </Group>
      {!!decision.has_api_key && (
        <ConnectionTest props={props} section="decision" values={decision} />
      )}
    </div>
  );
}

export async function deleteProvider(
  saver: ReturnType<typeof useSettings>,
  recordSection: string,
  name: string,
) {
  if (
    await confirmSheet({
      title: msg('model.delete_config', '删除配置'),
      body: msg('model.delete_body', '已保存的 API Key 等连接信息会一并删除，之后需要重新配置。'),
      danger: true,
      confirmLabel: msg('action.delete', '删除'),
    })
  )
    await saver.change(`${recordSection}.${name}`, { [recordSection]: { [name]: null } });
}
function hostOf(value: unknown) {
  if (!value) return undefined;
  try {
    return new URL(String(value)).host;
  } catch {
    return String(value);
  }
}
export function ProviderEditPage(props: PageProps) {
  const saver = useSettings(
    props,
    msg('model.edit_provider_title', '编辑提供商'),
    '/model/providers',
    false,
  );
  const existingName = props.route.query?.name;
  const record = saver.config.model_providers?.[existingName || ''];
  const type = record?.type || props.route.query?.type || 'openai';
  async function choose(name: string, record: ProviderRecord) {
    try {
      const catalog = await props.model.send<ModelCatalogue>(
        `/api/models?provider=${encodeURIComponent(record.type)}`,
      );
      const current = saver.config.model?.model || '';
      const selected = catalog.models.some((model) => model.id === current)
        ? current
        : (catalog.models.find((model) => model.recommended) || catalog.models[0])?.id || current;
      const result = await saver.apply(
        'model.provider',
        await selectModel(props, name, record.type, selected, saver.config.model?.reasoning_effort),
      );
      if (result === 'saved') props.navigate('/model/main');
    } catch (error) {
      toast(apiError(error).message);
    }
  }
  return (
    <ProviderForm
      props={props}
      recordSection="model_providers"
      type={type}
      existingName={existingName}
      back="/model/providers"
      actions={
        existingName && record && saver.config.model?.provider !== existingName ? (
          <>
            <div className="page__footer page__footer--stack">
              <Button variant="secondary" block onClick={() => void choose(existingName, record)}>
                <Text value={msg('model.use_provider', '设为主模型提供商')} />
              </Button>
            </div>
            <Group>
              <Row
                label={msg('model.delete_config', '删除配置')}
                labelTone="danger"
                className="ds-row--centered"
                action="delete-provider"
                onPress={async () => {
                  await deleteProvider(saver, 'model_providers', existingName);
                  if (!props.model.store.get(props.model.config)?.model_providers?.[existingName])
                    props.navigate('/model/providers');
                }}
              />
            </Group>
          </>
        ) : undefined
      }
    />
  );
}
