import { useState } from 'react';
import { useAtom, useAtomValue } from 'jotai';
import type { Configuration, PageProps, VoiceMode } from '../types';
import { useSettings, section } from '../hooks/settings';
import type { Settings } from '../hooks/settings';
import { useQuery, useSchema } from '../hooks/query';
import { matches, optionLabel, placeholderFor, sectionFields } from '../lib/schema';
import { msg, resolve } from '../lib/i18n';
import { apiError } from '../api/client';
import { ConfigField, QueryError } from '../components/fields';
import { ProviderMark, SelectedCheck } from '../components/marks';
import { Button, Group, Icon, Row, Text } from '../components/ui';
import { confirmSheet, toast } from '../components/overlays';
import { CONNECTION_KEYS, ProviderForm, recordsOf, deleteProvider } from './Models';
import { SwipeRow } from '../components/SwipeRow';
import { Loading } from '../components/Loading';
import { isInitialLoading } from '../state/resource';
export const KINDS = {
  realtime: {
    records: 'voice_model_providers',
    ref: 'voice_model',
    mode: 'realtime',
    prefs: ['use_backend_agent'],
  },
  stt: { records: 'stt_providers', ref: 'stt', mode: 'stt', prefs: ['language'] },
  tts: { records: 'tts_providers', ref: 'tts', mode: 'stt', prefs: ['speed'] },
} as const;
export type VoiceKind = keyof typeof KINDS;
const kindOf = (props: PageProps): VoiceKind =>
  props.route.query?.kind === 'tts'
    ? 'tts'
    : props.route.query?.kind === 'realtime'
      ? 'realtime'
      : 'stt';
async function chooseRecord(
  saver: Settings,
  kind: VoiceKind,
  name: string,
  mode: VoiceMode,
  config: Configuration,
) {
  const ref = { provider: name };
  if (
    mode === KINDS[kind].mode &&
    hasModePrerequisites({ ...config, [KINDS[kind].ref]: ref }, mode)
  ) {
    const result = await saver.apply(
      KINDS[kind].ref,
      { [KINDS[kind].ref]: ref, agent: { input_mode: mode } },
      { quietInvalid: true },
    );
    if (result !== 'invalid') return result;
  }
  return saver.apply(KINDS[kind].ref, { [KINDS[kind].ref]: ref });
}
function voiceLabel(
  schema: import('../lib/schema').Schema | null | undefined,
  kind: VoiceKind,
  type: string,
) {
  const field = sectionFields(schema, KINDS[kind].records).find((field) => field.key === 'type');
  const option = field?.enum?.find((option) => option.value === type);
  const names: Record<string, import('../types').TextSpec> = {
    'openai-whisper': 'OpenAI Whisper',
    'tencent-asr': msg('voice.type_tencent_asr', '腾讯云 ASR'),
    'qwen-asr': msg('voice.type_qwen_asr', '通义千问 ASR'),
    'google-cloud': 'Google Cloud',
    alicloud: msg('voice.type_alicloud', '阿里云'),
    'fish-audio': 'Fish Audio',
    minimax: 'MiniMax',
    'minimax-cn': msg('voice.type_minimax_cn', 'MiniMax（中国）'),
    volcengine: msg('model.type_volcengine', '火山引擎'),
  };
  return resolve(
    optionLabel(KINDS[kind].records, 'type', {
      value: type,
      label: option?.label || resolve(names[type]) || type,
    }),
  );
}
function voiceRecordLabel(
  schema: import('../lib/schema').Schema | null | undefined,
  kind: VoiceKind,
  name: string,
  record: import('../types').ProviderRecord,
) {
  const label = voiceLabel(schema, kind, record.type);
  return name === record.type ? label : `${name} (${label})`;
}
function hasModePrerequisites(config: Configuration, mode: string) {
  if (mode === 'realtime') {
    const ref = section(config, 'voice_model');
    const record = recordsOf(config, 'voice_model_providers')[String(ref.provider || '')];
    return ref.has_api_key === true || record?.has_api_key === true;
  }
  return !!section(config, 'stt').provider && !!section(config, 'tts').provider;
}
export function VoicePage(props: PageProps) {
  const saver = useSettings(props, msg('ui.nav_voice', '语音'));
  const snapshot = useAtomValue(props.model.current);
  const schema = useSchema(props.model);
  const [mode, setMode] = useAtom(props.model.voiceMode);
  const saved = saver.config.agent?.input_mode;
  const disabled = props.model.container && snapshot.agent_status?.classic_voice_enabled === false;
  const kinds: VoiceKind[] = mode === 'realtime' ? ['realtime'] : ['stt', 'tts'];
  return (
    <div className="ds-screen">
      <QueryError {...schema} />
      <div
        className="ds-segmented"
        role="tablist"
        style={{ '--segmented-index': mode === 'stt' ? 1 : 0 } as React.CSSProperties}
      >
        <div className="ds-segmented__thumb" aria-hidden="true" />
        {(['realtime', 'stt'] as const).map((value) => (
          <button
            key={value}
            type="button"
            role="tab"
            aria-selected={mode === value}
            tabIndex={mode === value ? 0 : -1}
            className={`ds-segmented__item ${mode === value ? 'ds-segmented__item--active' : ''}`}
            data-action={`voice-mode-${value}`}
            onClick={() => setMode(value)}
            onKeyDown={(event) => {
              const choices = ['realtime', 'stt'] as const;
              const current = choices.indexOf(value);
              const next =
                event.key === 'ArrowRight'
                  ? (current + 1) % choices.length
                  : event.key === 'ArrowLeft'
                    ? (current - 1 + choices.length) % choices.length
                    : event.key === 'Home'
                      ? 0
                      : event.key === 'End'
                        ? choices.length - 1
                        : -1;
              if (next < 0) return;
              event.preventDefault();
              const selected = choices[next];
              setMode(selected);
              event.currentTarget.parentElement
                ?.querySelector<HTMLButtonElement>(`[data-action="voice-mode-${selected}"]`)
                ?.focus();
            }}
          >
            <span
              className={`ds-segmented__check ${mode === value ? 'ds-segmented__check--visible' : ''}`}
            >
              <Icon name="check" size={16} />
            </span>
            <Text
              value={
                value === 'stt'
                  ? msg('voice.mode_classic', '经典模式')
                  : msg('voice.mode_realtime', '实时模式')
              }
              className="ds-segmented__label"
            />
          </button>
        ))}
      </div>
      {mode !== saved && (
        <p className="ds-group__note" role="status">
          <Text
            value={msg(
              'voice.configure_before_enable',
              '此模式尚未启用。切换标签仅预览配置，保存提供商配置后才会应用此模式。',
            )}
          />
        </p>
      )}
      {disabled && (
        <p className="page__note">
          <Text
            value={msg(
              'voice.classic_disabled',
              '当前容器未启用 Classic 语音，Classic 模式仅支持文字对话。你仍可编辑 STT/TTS 配置；使用浏览器语音请选择 Realtime。',
            )}
          />
        </p>
      )}
      {kinds.map((kind) => {
        const reference = section(saver.config, KINDS[kind].ref),
          record = recordsOf(saver.config, KINDS[kind].records)[String(reference.provider || '')];
        const ctx = { [`${KINDS[kind].records}.type`]: record?.type, 'agent.input_mode': mode };
        const modelField = sectionFields(schema.data, KINDS[kind].records).find(
          (field) => field.key === 'model' && matches(field.visibleWhen, ctx),
        );
        return (
          <Group
            key={kind}
            caption={
              kind === 'realtime'
                ? undefined
                : kind === 'stt'
                  ? msg('voice.stt', 'STT（语音转文字）')
                  : msg('voice.tts', 'TTS（文字转语音）')
            }
          >
            <Row
              label={msg('voice.provider', '提供商')}
              action={`open-${kind}-providers`}
              chevron
              onPress={() => props.navigate(`/voice/providers?kind=${kind}`)}
            >
              <span className="ds-row__value-with-mark">
                {record && <ProviderMark type={record.type} />}
                <Text
                  value={
                    record
                      ? voiceRecordLabel(schema.data, kind, String(reference.provider), record)
                      : msg('voice.not_configured', '未配置')
                  }
                  className="ds-row__value"
                />
              </span>
            </Row>
            <Row
              label={msg('voice.model', '模型')}
              value={
                !record
                  ? msg('voice.not_configured', '未配置')
                  : !schema.data
                    ? String(record.model || '—')
                    : modelField
                      ? String(
                          record.model ||
                            placeholderFor(modelField, ctx) ||
                            resolve(msg('voice.default', '默认')),
                        )
                      : msg('voice.model_fixed', '由提供商决定')
              }
              loading={!!record && !record.model && schema.loading}
              chevron={!!record && !!modelField}
              onPress={
                record && modelField ? () => props.navigate(`/voice/model?kind=${kind}`) : undefined
              }
            />
            <Row
              label={msg('voice.extra', 'Extra')}
              chevron
              onPress={() => props.navigate(`/voice/extra?kind=${kind}`)}
            />
          </Group>
        );
      })}
      {mode === 'stt' && !props.model.local && <SpeakerPanel props={props} saver={saver} />}
    </div>
  );
}
export function VoiceProviders(props: PageProps) {
  const kind = kindOf(props),
    spec = KINDS[kind];
  const saver = useSettings(props, msg('voice.providers', '提供商'), '/voice');
  const schema = useSchema(props.model);
  const records = recordsOf(saver.config, spec.records),
    current = String(section(saver.config, spec.ref).provider || '');
  const options =
    sectionFields(schema.data, spec.records).find((field) => field.key === 'type')?.enum || [];
  return (
    <div className="ds-screen">
      <QueryError {...schema} />
      {Object.keys(records).length > 0 && (
        <Group caption={msg('model.configured', '已配置')} captionMarker="success">
          {Object.entries(records)
            .sort(([a], [b]) => a.localeCompare(b))
            .map(([name, record]) => (
              <SwipeRow
                key={name}
                action={`delete-${kind}-provider-${name}`}
                onDelete={() => void deleteProvider(saver, spec.records, name)}
                lockedLabel={current === name ? msg('model.in_use', '使用中') : undefined}
                onLocked={() =>
                  toast(msg('model.in_use_hint', '正在使用的配置不能删除，请先切换到其他提供商。'))
                }
              >
                <Row
                  label={voiceRecordLabel(schema.data, kind, name, record)}
                  leading={<ProviderMark type={record.type} />}
                  chevron
                  onPress={() =>
                    props.navigate(
                      `/voice/providers/edit?kind=${kind}&name=${encodeURIComponent(name)}`,
                    )
                  }
                >
                  {current === name && <SelectedCheck />}
                </Row>
              </SwipeRow>
            ))}
        </Group>
      )}
      <Group caption={msg('model.unconfigured', '未配置')} captionMarker="muted">
        {options
          .filter(
            (option) => !Object.values(records).some((record) => record.type === option.value),
          )
          .map((option) => (
            <Row
              key={option.value}
              label={voiceLabel(schema.data, kind, option.value)}
              leading={<ProviderMark type={option.value} />}
              className="ds-row--muted"
              chevron
              onPress={() =>
                props.navigate(
                  `/voice/providers/edit?kind=${kind}&type=${encodeURIComponent(option.value)}`,
                )
              }
            />
          ))}
      </Group>
    </div>
  );
}
export function VoiceProviderEdit(props: PageProps) {
  const mode = useAtomValue(props.model.voiceMode);
  const saver = useSettings(
    props,
    msg('model.edit_provider_title', '编辑提供商'),
    undefined,
    false,
  );
  const config = saver.config;
  const kind = kindOf(props),
    spec = KINDS[kind];
  const existing = props.route.query?.name;
  const record = recordsOf(config, spec.records)[existing || ''];
  const type = record?.type || props.route.query?.type || '';
  return (
    <ProviderForm
      props={props}
      recordSection={spec.records}
      type={type}
      mode={mode}
      existingName={existing}
      back={`/voice/providers?kind=${kind}`}
      actions={
        existing &&
        record &&
        (section(config, spec.ref).provider !== existing || config.agent?.input_mode !== mode) ? (
          <>
            <div className="page__footer page__footer--stack">
              <Button
                variant="secondary"
                block
                onClick={async () => {
                  if ((await chooseRecord(saver, kind, existing, mode, config)) === 'saved')
                    props.navigate('/voice');
                }}
              >
                <Text value={msg('voice.use_provider', '设为当前提供商')} />
              </Button>
            </div>
            {section(config, spec.ref).provider !== existing && (
              <Group>
                <Row
                  label={msg('model.delete_config', '删除配置')}
                  labelTone="danger"
                  className="ds-row--centered"
                  action="delete-provider"
                  onPress={async () => {
                    await deleteProvider(saver, spec.records, existing);
                    if (
                      !recordsOf(props.model.store.get(props.model.config) || {}, spec.records)[
                        existing
                      ]
                    )
                      props.navigate(`/voice/providers?kind=${kind}`);
                  }}
                />
              </Group>
            )}
          </>
        ) : undefined
      }
      onSaved={async (name) => {
        const current = props.model.store.get(props.model.config) || {};
        const selected = section(current, spec.ref).provider;
        if (!selected || selected === name) await chooseRecord(saver, kind, name, mode, current);
      }}
    />
  );
}

export function VoiceOptions(props: PageProps) {
  const mode = useAtomValue(props.model.voiceMode);
  const kind = kindOf(props),
    spec = KINDS[kind];
  const saver = useSettings(
    props,
    msg(
      props.route.path.endsWith('/model') ? 'model.model' : 'voice.extra',
      props.route.path.endsWith('/model') ? '模型' : 'Extra',
    ),
    '/voice',
  );
  const schema = useSchema(props.model);
  const name = String(section(saver.config, spec.ref).provider || ''),
    record = recordsOf(saver.config, spec.records)[name];
  const ctx = Object.fromEntries(
    Object.entries(record || {}).map(([key, value]) => [`${spec.records}.${key}`, value]),
  );
  ctx['agent.input_mode'] = mode;
  const modelOnly = props.route.path.endsWith('/model');
  const fields = sectionFields(schema.data, spec.records).filter(
    (field) =>
      field.key !== 'type' &&
      matches(field.visibleWhen, ctx) &&
      (modelOnly
        ? field.key === 'model'
        : field.key !== 'model' && !field.secret && !CONNECTION_KEYS.has(field.key)),
  );
  return (
    <div className="ds-screen">
      <QueryError {...schema} />
      {record ? (
        <Group>
          {!modelOnly &&
            sectionFields(schema.data, spec.ref)
              .filter(
                (field) =>
                  spec.prefs.some((key) => key === field.key) && matches(field.visibleWhen, ctx),
              )
              .map((field) => (
                <ConfigField
                  key={field.key}
                  section={spec.ref}
                  field={field}
                  value={section(saver.config, spec.ref)[field.key]}
                  ctx={ctx}
                  type={record.type}
                  save={(value) =>
                    saver.change(`${spec.ref}.${field.key}`, { [spec.ref]: { [field.key]: value } })
                  }
                />
              ))}
          {fields.map((field) => (
            <ConfigField
              key={field.key}
              section={spec.records}
              field={field}
              value={record[field.key]}
              ctx={ctx}
              type={record.type}
              save={(value) =>
                saver.change(`${spec.records}.${name}.${field.key}`, {
                  [spec.records]: { [name]: { [field.key]: value } },
                })
              }
            />
          ))}
        </Group>
      ) : (
        <Row
          label={msg('model.not_set', '未设置')}
          chevron
          onPress={() => props.navigate(`/voice/providers?kind=${kind}`)}
        />
      )}
    </div>
  );
}
function SpeakerPanel({ props, saver }: { props: PageProps; saver: Settings }) {
  const [busy, setBusy] = useState(false);
  const query = useQuery(
    async (signal) => {
      const [status, profiles] = await Promise.all([
        props.model.send<{ gate_enabled?: boolean }>('/api/speaker/status', { signal }),
        props.model.send<{ profiles: { id: string }[] }>('/api/speaker/profiles', { signal }),
      ]);
      return { status, profiles: profiles.profiles || [] };
    },
    [props.model],
    5000,
  );
  async function enroll() {
    if (
      !(await confirmSheet({
        title: msg('voice.speaker_enroll_title', '靠近设备说话'),
        body: msg('voice.speaker_enroll_body', '确认后请用 Aico 麦克风正常说话约 4 秒，内容随意。'),
      }))
    )
      return;
    setBusy(true);
    try {
      await props.model.send('/api/speaker/enroll/capture?id=speaker', { method: 'POST' });
      query.retry();
      toast(msg('voice.speaker_enrolled', '声纹已录入'));
    } catch (error) {
      toast(apiError(error).message);
    } finally {
      setBusy(false);
    }
  }
  const gate = saver.config.agent?.voice_speaker_gate_enabled ?? query.data?.status.gate_enabled;
  return (
    <div data-action="speaker-panel">
      <QueryError {...query} />
      <Group caption={msg('voice.speaker_caption', '声纹')}>
        {typeof gate === 'boolean' ? (
          <ConfigField
            section="agent"
            label={msg('voice.speaker_gate', '声纹校验')}
            description={msg(
              'voice.speaker_gate_help',
              '开启后，Classic 只接受已录入声纹的说话。建议先录入再打开。',
            )}
            field={{
              key: 'voice_speaker_gate_enabled',
              widget: 'boolean',
              label: resolve(msg('voice.speaker_gate', '声纹校验')),
              help: resolve(
                msg(
                  'voice.speaker_gate_help',
                  '开启后，Classic 只接受已录入声纹的说话。建议先录入再打开。',
                ),
              ),
            }}
            value={gate}
            save={(value) =>
              saver.change('agent.voice_speaker_gate_enabled', {
                agent: { voice_speaker_gate_enabled: value },
              })
            }
          />
        ) : (
          <Row
            label={msg('voice.speaker_gate', '声纹校验')}
            value={msg('status.state.unknown', '未知')}
            loading={isInitialLoading(query)}
          />
        )}
        {isInitialLoading(query) && <Loading block />}
        {query.data?.profiles.length === 0 && (
          <Row
            label={msg('voice.speaker_profiles', '当前声纹')}
            value={msg('voice.speaker_empty', '还没有声纹')}
            tone="muted"
          />
        )}
        {query.data?.profiles.map((profile) => (
          <Row
            key={profile.id}
            label={msg('voice.speaker_enrolled_status', '已录入')}
            value={msg('action.delete', '删除')}
            onPress={async () => {
              if (
                await confirmSheet({
                  title: msg('voice.speaker_delete_title', '删除当前声纹？'),
                  danger: true,
                })
              ) {
                try {
                  await props.model.send(
                    `/api/speaker/profiles/${encodeURIComponent(profile.id)}`,
                    { method: 'DELETE' },
                  );
                  query.retry();
                } catch (error) {
                  toast(apiError(error).message);
                }
              }
            }}
          />
        ))}
        <div className="ds-card__actions">
          <Button
            variant="accent"
            block
            action="speaker-enroll"
            disabled={busy}
            onClick={() => void enroll()}
          >
            <Text value={msg('voice.speaker_enroll', '在设备上录入')} />
          </Button>
        </div>
      </Group>
    </div>
  );
}
