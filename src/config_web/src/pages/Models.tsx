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
import { useAtomValue } from 'jotai';
import { useState } from 'react';
import type { ConfigPatch, JsonObject, PageProps, ProviderRecord } from '../types';
import { useSettings, section, usePageHeader } from '../hooks/settings';
import { useQuery, useSchema } from '../hooks/query';
import { matches, sectionFields, fieldLabel, optionsFor, placeholderFor } from '../lib/schema';
import type { Field } from '../lib/schema';
import { apiError, json } from '../api/client';
import { getActiveLocale, msg, resolve } from '../lib/i18n';
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
  return (
    <Group>
      <Row
        label={msg('model.main', '主模型')}
        value={saver.config.model?.model || '—'}
        chevron
        onPress={() => props.navigate('/model/main')}
      />
      <Row
        label={msg('model.decision', '决策模型')}
        chevron
        onPress={() => props.navigate('/model/decision')}
      />
    </Group>
  );
}
export function MainModelPage(props: PageProps) {
  const saver = useSettings(props, msg('model.main', '主模型'), '/model');
  const schema = useSchema(props.model);
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
  const fields = [
    'temperature',
    'reasoning_effort',
    'max_response_tokens',
    'context_window',
    'model_max_output_tokens',
  ].map(
    (key) =>
      sectionFields(schema.data, 'model').find((field) => field.key === key) || {
        key,
        widget: key === 'reasoning_effort' ? 'select' : 'number',
        nullable: key === 'temperature',
      },
  );
  return (
    <div className="ds-screen">
      <Group>
        <Row
          label={msg('model.provider', '主模型提供商')}
          value={String(model.provider || '—')}
          chevron
          onPress={() => props.navigate('/model/providers')}
        />
        <Row
          label={msg('model.model', '模型')}
          value={String(model.model || '—')}
          chevron
          onPress={() => props.navigate('/model/models')}
        />
      </Group>
      <Group>
        {fields.map((field) =>
          field.key === 'reasoning_effort' && !efforts ? (
            <Row
              key={field.key}
              label={msg('model.reasoning_effort', '推理强度')}
              value={msg('model.effort_unsupported', '不支持')}
            />
          ) : (
            <ConfigField
              key={field.key}
              section="model"
              field={
                field.key === 'reasoning_effort'
                  ? {
                      ...field,
                      enum: efforts?.map((value) => ({
                        value,
                        label: value || resolve(msg('model.effort_auto', '自动（默认）')),
                      })),
                    }
                  : field
              }
              value={model[field.key]}
              ctx={{ 'model.model': model.model, 'model.provider.__type': type }}
              type={type}
              save={(value) =>
                saver.change(`model.${field.key}`, { model: { [field.key]: value } })
              }
            />
          ),
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
    <div className="ds-field">
      <Button
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
  back,
  onSaved,
}: {
  props: PageProps;
  recordSection: string;
  type: string;
  existingName?: string;
  back: string;
  onSaved?: (name: string) => Promise<void>;
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
  ctx['agent.input_mode'] = props.route.query?.mode || saver.config.agent?.input_mode || '';
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
    providerType !== type ||
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
    msg('model.edit_provider_title', '编辑提供商'),
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
        <Group>
          {type === 'custom' && (
            <label className="ds-field">
              <Text value={msg('model.interface_type', '接口类型')} />
              <select
                className="ds-input"
                value={providerType}
                onChange={(event) => setProviderType(event.target.value)}
              >
                {['openai', 'anthropic'].map((value) => (
                  <option key={value}>{value}</option>
                ))}
              </select>
            </label>
          )}
          {shown.map((field) => {
            const value = draft[field.key] ?? (field.secret ? '' : existing?.[field.key]) ?? '';
            const options = optionsFor(field, providerType);
            return field.widget === 'select' ||
              (field.selectWhen && matches(field.selectWhen, ctx)) ? (
              <label className="ds-field" key={field.key}>
                <Text value={fieldLabel(recordSection, field)} />
                <select
                  className="ds-input"
                  value={String(value)}
                  onChange={(event) =>
                    setDraft((previous) => ({ ...previous, [field.key]: event.target.value }))
                  }
                >
                  {!options.some((option) => option.value === String(value)) && (
                    <option value={String(value)}>{String(value) || '—'}</option>
                  )}
                  {options.map((option) => (
                    <option key={option.value} value={option.value}>
                      {option.label || option.value}
                    </option>
                  ))}
                </select>
              </label>
            ) : (
              <FieldInput
                key={field.key}
                label={fieldLabel(recordSection, field)}
                secret={field.secret}
                value={String(value)}
                disabled={busy}
                placeholder={
                  field.secret && existing?.[`has_${field.key}`]
                    ? resolve(msg('model.api_key_saved', '已保存，留空保持不变'))
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
        <Button type="submit" disabled={!touched || busy}>
          <Text value={msg('action.save', '保存')} />
        </Button>
      </form>
    </div>
  );
}
export function ProvidersPage(props: PageProps) {
  const saver = useSettings(props, msg('model.provider', '主模型提供商'), '/model/main');
  const records = saver.config.model_providers || {};

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
    <div className="ds-screen">
      <Group>
        {Object.entries(records).map(([name, record]) => (
          <div key={name}>
            <Row
              label={name}
              value={saver.config.model?.provider === name ? '✓' : record.type}
              chevron
              onPress={() =>
                props.navigate(`/model/providers/edit?name=${encodeURIComponent(name)}`)
              }
            />
            <Button variant="quiet" onClick={() => void choose(name, record)}>
              <Text value={msg('model.use_provider', '设为主模型提供商')} />
            </Button>
            {saver.config.model?.provider !== name && (
              <Button
                variant="danger-soft"
                onClick={async () => {
                  if (
                    await confirmSheet({
                      title: msg('model.delete_config', '删除配置'),
                      body: name,
                      danger: true,
                    })
                  )
                    await saver.change(`model_providers.${name}`, {
                      model_providers: { [name]: null },
                    });
                }}
              >
                <Text value={msg('action.delete', '删除')} />
              </Button>
            )}
          </div>
        ))}
      </Group>
      <Group>
        {[...PROVIDER_TYPES, 'custom'].map((type) => (
          <Row
            key={type}
            label={type === 'custom' ? msg('model.others', '其他') : type}
            chevron
            onPress={() => props.navigate(`/model/providers/edit?type=${type}`)}
          />
        ))}
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
  const [custom, setCustom] = useState('');
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
            value={selected.model === model.id ? '✓' : ''}
            onPress={() => void choose(model.id)}
          />
        ))}
      </Group>
      <form
        onSubmit={(event) => {
          event.preventDefault();
          void choose(custom.trim());
        }}
      >
        <FieldInput label={msg('model.model', '模型')} value={custom} onChange={setCustom} />
        <Button type="submit" disabled={!custom.trim()}>
          <Text value={msg('fields.use', '使用')} />
        </Button>
      </form>
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
      <Group>
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
        <label className="ds-field">
          <Text value={msg('model.decision_provider', '提供商')} />
          <select
            className="ds-input"
            value={provider}
            onChange={(event) => void choose(event.target.value)}
          >
            {DECISION_PROVIDERS.map((value) => (
              <option key={value}>{value}</option>
            ))}
          </select>
        </label>
      </Group>
      <Group key={provider}>
        <ValueField
          label={msg('model.decision_api_key', '决策模型 API Key')}
          secret
          saved=""
          placeholder={resolve(
            msg(
              decision.has_api_key ? 'model.api_key_saved' : 'model.api_key_placeholder',
              '请输入 API Key',
            ),
          )}
          onCommit={(value) => saver.change('decision.api_key', { decision: { api_key: value } })}
        />
        {provider === 'openrouter' && (
          <>
            <ValueField
              label={msg('config.fields.decision.model.label', '决策模型')}
              saved={decision.model}
              action="decision-model"
              suggestions={catalog.data?.models.map((model) => model.id)}
              onCommit={(value) =>
                saver.change('decision.model', { decision: { model: value || null } })
              }
            />
            <ValueField
              label={msg('config.fields.decision.base_url.label', '决策模型 Base URL')}
              saved={decision.base_url}
              onCommit={(value) =>
                saver.change('decision.base_url', { decision: { base_url: value || null } })
              }
            />
          </>
        )}
        <ValueField
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

export function ProviderEditPage(props: PageProps) {
  const config = useAtomValue(props.model.config) || {};
  const existingName = props.route.query?.name;
  const type = existingName
    ? config.model_providers?.[existingName]?.type
    : props.route.query?.type;
  return (
    <ProviderForm
      props={props}
      recordSection="model_providers"
      type={type || 'openai'}
      existingName={existingName}
      back="/model/providers"
    />
  );
}
