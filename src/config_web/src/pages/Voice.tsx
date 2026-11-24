import { useState } from 'react';
import { useAtomValue } from 'jotai';
import type { PageProps } from '../types';
import { useSettings, section } from '../hooks/settings';
import type { Settings } from '../hooks/settings';
import { useQuery, useSchema } from '../hooks/query';
import { matches, sectionFields } from '../lib/schema';
import { msg } from '../lib/i18n';
import { apiError } from '../api/client';
import { ConfigField, QueryError } from '../components/fields';
import { Button, Group, Row, Text } from '../components/ui';
import { confirmSheet, toast } from '../components/overlays';
import { CONNECTION_KEYS, ProviderForm, recordsOf } from './Models';
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
const suffix = (props: PageProps) =>
  props.route.query?.mode ? `&mode=${encodeURIComponent(props.route.query.mode)}` : '';
async function chooseRecord(saver: Settings, kind: VoiceKind, name: string, mode?: string) {
  const ref = { provider: name };
  if (mode) {
    const result = await saver.apply(
      KINDS[kind].ref,
      { [KINDS[kind].ref]: ref, agent: { input_mode: mode } },
      { quietInvalid: true },
    );
    if (result !== 'invalid') return result;
  }
  return saver.apply(KINDS[kind].ref, { [KINDS[kind].ref]: ref });
}
export function VoicePage(props: PageProps) {
  const saver = useSettings(props, msg('ui.nav_voice', '语音'));
  const snapshot = useAtomValue(props.model.current);
  const saved = saver.config.agent?.input_mode;
  const mode =
    props.route.query?.mode || (['stt', 'realtime'].includes(saved || '') ? saved : 'realtime');
  const disabled = props.model.container && snapshot.agent_status?.classic_voice_enabled === false;
  const kinds: VoiceKind[] = mode === 'realtime' ? ['realtime'] : ['stt', 'tts'];
  async function changeMode(value: string) {
    if (value === saved) {
      props.navigate('/voice', { replace: true });
      return;
    }
    const result = await saver.apply(
      'agent.input_mode',
      { agent: { input_mode: value } },
      { quietInvalid: true },
    );
    if (result === 'saved') props.navigate('/voice', { replace: true });
    else if (result === 'invalid') props.navigate(`/voice?mode=${value}`, { replace: true });
  }
  return (
    <div className="ds-screen">
      <div className="ds-segmented" role="tablist">
        {['realtime', 'stt'].map((value) => (
          <button
            key={value}
            type="button"
            role="tab"
            aria-selected={mode === value}
            className={`ds-segmented__item ${mode === value ? 'ds-segmented__item--active' : ''}`}
            data-action={`voice-mode-${value}`}
            onClick={() => void changeMode(value)}
          >
            {value === 'stt' ? 'Classic' : 'Realtime'}
          </button>
        ))}
      </div>
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
        return (
          <Group key={kind} caption={kind === 'realtime' ? 'Realtime' : kind.toUpperCase()}>
            <Row
              label={msg('model.provider', '提供商')}
              value={String(reference.provider || '—')}
              action={`open-${kind}-providers`}
              chevron
              onPress={() => props.navigate(`/voice/providers?kind=${kind}&mode=${mode}`)}
            />
            <Row
              label={msg('model.model', '模型')}
              value={String(record?.model || '—')}
              chevron
              onPress={() => props.navigate(`/voice/model?kind=${kind}&mode=${mode}`)}
            />
            <Row
              label={msg('voice.extra', 'Extra')}
              chevron
              onPress={() => props.navigate(`/voice/extra?kind=${kind}&mode=${mode}`)}
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
  const saver = useSettings(props, msg('model.provider', '提供商'), '/voice');
  const schema = useSchema(props.model);
  const records = recordsOf(saver.config, spec.records),
    current = String(section(saver.config, spec.ref).provider || '');
  const options =
    sectionFields(schema.data, spec.records).find((field) => field.key === 'type')?.enum || [];
  return (
    <div className="ds-screen">
      <QueryError {...schema} />
      <Group>
        {Object.entries(records).map(([name, record]) => (
          <div key={name}>
            <Row
              label={name}
              value={name === current ? '✓' : record.type}
              chevron
              onPress={() =>
                props.navigate(
                  `/voice/providers/edit?kind=${kind}&name=${encodeURIComponent(name)}${suffix(props)}`,
                )
              }
            />
            <Button
              variant="quiet"
              onClick={async () => {
                if ((await chooseRecord(saver, kind, name, props.route.query?.mode)) === 'saved')
                  props.navigate(
                    `/voice${props.route.query?.mode ? '?mode=' + props.route.query.mode : ''}`,
                  );
              }}
            >
              <Text value={msg('voice.use_provider', '设为当前提供商')} />
            </Button>
            {current !== name && (
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
                    await saver.change(`${spec.records}.${name}`, {
                      [spec.records]: { [name]: null },
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
        {options.map((option) => (
          <Row
            key={option.value}
            label={option.label || option.value}
            chevron
            onPress={() =>
              props.navigate(
                `/voice/providers/edit?kind=${kind}&type=${encodeURIComponent(option.value)}${suffix(props)}`,
              )
            }
          />
        ))}
      </Group>
    </div>
  );
}
export function VoiceProviderEdit(props: PageProps) {
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
      existingName={existing}
      back={`/voice/providers?kind=${kind}${suffix(props)}`}
      onSaved={async (name) => {
        if (!existing && !section(config, spec.ref).provider)
          await chooseRecord(saver, kind, name, props.route.query?.mode);
      }}
    />
  );
}

export function VoiceOptions(props: PageProps) {
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
  ctx['agent.input_mode'] = props.route.query?.mode || saver.config.agent?.input_mode;
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
          onPress={() => props.navigate(`/voice/providers?kind=${kind}${suffix(props)}`)}
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
  return (
    <div data-action="speaker-panel">
      <QueryError {...query} />
      <Group caption={msg('voice.speaker_caption', '声纹')}>
        <ConfigField
          section="agent"
          field={{ key: 'voice_speaker_gate_enabled', widget: 'boolean' }}
          value={saver.config.agent?.voice_speaker_gate_enabled ?? query.data?.status.gate_enabled}
          save={(value) =>
            saver.change('agent.voice_speaker_gate_enabled', {
              agent: { voice_speaker_gate_enabled: value },
            })
          }
        />
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
        <Button action="speaker-enroll" disabled={busy} onClick={() => void enroll()}>
          <Text value={msg('voice.speaker_enroll', '在设备上录入')} />
        </Button>
      </Group>
    </div>
  );
}
