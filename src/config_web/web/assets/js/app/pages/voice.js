/**
 * Voice routes.
 *
 *   /voice                                   mode switch, then the providers the mode uses
 *   /voice/providers?kind=<kind>             configured records, then unconfigured types
 *   /voice/providers/edit?kind=&name=|type=  a record's connection settings
 *   /voice/model?kind=<kind>                 the record's model
 *   /voice/extra?kind=<kind>                 everything else for that kind
 *
 * `kind` is `realtime`, `stt` or `tts`. Each is a provider-record pattern like
 * the main model's: `<ref>.provider` names a record in `<records>.<name>`, and
 * the record carries the type and its settings.
 *
 * Which fields a provider type has comes from the Agent's schema
 * (`visibleWhen` on `stt_providers.type` and friends), never from a list here.
 * The page only decides where a field is shown: connection fields (keys,
 * endpoints, account identifiers) on the provider page, `model` on its own
 * page, and the rest under "Extra", following the classic page's grouping.
 *
 * Switching mode saves `agent.input_mode` alone when the Agent accepts it. When
 * the target mode's providers are not configured yet, the Agent rejects that
 * (see #701), so the page shows the target mode unsaved (`?mode=`) and carries
 * the mode into the first provider selection that makes it valid.
 */

import {adoptSaved, ensureSnapshot, t, voiceMode} from '../data.js';
import {customValueField, inlineValueRow} from '../fields.js';
import {createSaver} from '../saver.js';
import {fieldHelp, fieldLabel, loadSchema, matches, optionLabel, optionsFor, placeholderFor, rangeOptions, sectionFields} from '../schema.js';
import {providerMark} from '../../ui/brand.js';
import {button} from '../../ui/button.js';
import {choiceSheet} from '../../ui/choice-sheet.js';
import {confirmSheet} from '../../ui/confirm.js';
import {swipeRow} from '../../ui/swipe.js';
import {toast} from '../../ui/toast.js';
import {el, replace} from '../../ui/dom.js';
import {icon} from '../../ui/icon.js';
import {group, row, screen} from '../../ui/list.js';
import {setChecked, switchControl} from '../../ui/switch.js';
import {msg, resolve, text} from '../../ui/text.js';

export const KINDS = {
  realtime: {records: 'voice_model_providers', ref: 'voice_model', mode: 'realtime', prefs: ['use_backend_agent'],
    title: msg('voice.realtime', 'Realtime')},
  stt: {records: 'stt_providers', ref: 'stt', mode: 'stt', prefs: ['language'],
    title: msg('voice.stt', 'STT（语音转文字）')},
  tts: {records: 'tts_providers', ref: 'tts', mode: 'stt', prefs: ['speed'],
    title: msg('voice.tts', 'TTS（文字转语音）')},
};

/** Fields shown on the provider page: how to reach and authenticate a service. */
export const CONNECTION_KEYS = new Set([
  'api_key', 'secret_id', 'secret_key', 'app_id', 'base_url', 'endpoint', 'auth_mode',
  'project_id', 'location', 'workspace_id', 'upstream_provider', 'agent_id', 'region',
]);

/**
 * Display names for record types whose schema option carries no label (the
 * STT and TTS enums are bare values). Brand names are not translated.
 */
const TYPE_NAMES = {
  'openai-whisper': 'OpenAI Whisper',
  'tencent-asr': msg('voice.type_tencent_asr', '腾讯云 ASR'),
  openrouter: 'OpenRouter',
  'qwen-asr': msg('voice.type_qwen_asr', '通义千问 ASR'),
  'google-cloud': 'Google Cloud',
  alicloud: msg('voice.type_alicloud', '阿里云'),
  'fish-audio': 'Fish Audio',
  minimax: 'MiniMax',
  'minimax-cn': msg('voice.type_minimax_cn', 'MiniMax（中国）'),
  volcengine: msg('model.type_volcengine', '火山引擎'),
};

function typeName(type) {
  const name = TYPE_NAMES[type];
  return name ? resolve(name) : undefined;
}

/**
 * The mode the switch showed on the previous render. Switching re-renders the
 * page, so the thumb starts from here and slides to the new side instead of
 * appearing there.
 */
let lastShownMode = null;

const MODES = [
  {value: 'realtime', label: msg('voice.mode_realtime', 'Realtime')},
  {value: 'stt', label: msg('voice.mode_classic', 'Classic')},
];

const config = snapshot => (snapshot && snapshot.config) || {};
const recordsOf = (snapshot, kind) => config(snapshot)[KINDS[kind].records] || {};
const refOf = (snapshot, kind) => config(snapshot)[KINDS[kind].ref] || {};

function currentName(snapshot, kind) {
  const name = refOf(snapshot, kind).provider || '';
  return Object.prototype.hasOwnProperty.call(recordsOf(snapshot, kind), name) ? name : '';
}

/** The mode the Agent runs; see `voiceMode` in data.js. */
export const effectiveMode = voiceMode;

function kindFromQuery(context) {
  const kind = context.query && context.query.kind;
  return KINDS[kind] ? kind : null;
}

function typeLabel(schema, kind, type) {
  const field = sectionFields(schema, KINDS[kind].records).find(entry => entry.key === 'type');
  const option = field && (field.enum || []).find(entry => entry.value === type);
  return option ? optionLabel(KINDS[kind].records, 'type', option, typeName(type)) : typeName(type) || type;
}

/** "Qwen" for a record named after its type, "qwen-2 (Qwen)" otherwise. */
function recordLabel(schema, kind, name, record) {
  const label = resolve(typeLabel(schema, kind, record.type));
  return name === record.type ? label : `${name} (${label})`;
}

/** Map of dotted paths to values, for `visibleWhen` and placeholders. */
function recordContext(kind, record, mode) {
  const ctx = {'agent.input_mode': mode};
  for (const [key, value] of Object.entries(record || {})) ctx[`${KINDS[kind].records}.${key}`] = value;
  return ctx;
}

function visibleRecordFields(schema, kind, record, mode) {
  const ctx = recordContext(kind, record, mode);
  return sectionFields(schema, KINDS[kind].records).filter(field => field.key !== 'type' && matches(field.visibleWhen, ctx));
}

function uniqueName(base, records) {
  const clean = String(base || 'provider').replace(/[^A-Za-z0-9_-]+/g, '-').replace(/^-+|-+$/g, '') || 'provider';
  if (!Object.prototype.hasOwnProperty.call(records, clean)) return clean;
  for (let i = 2; i < 1000; i += 1) {
    if (!Object.prototype.hasOwnProperty.call(records, `${clean}-${i}`)) return `${clean}-${i}`;
  }
  return clean;
}

async function load(context) {
  const [snapshot, schema] = await Promise.all([
    ensureSnapshot(context),
    loadSchema().catch(() => ({sections: []})),
  ]);
  return {snapshot, schema};
}

const modeQuery = mode => (mode ? `&mode=${encodeURIComponent(mode)}` : '');

/**
 * Point `kind` at a record. A pending mode switch rides along when the new
 * reference makes it valid; if it still does not, the reference saves alone.
 */
async function useRecord(saver, kind, name, pendingMode) {
  const patch = {[KINDS[kind].ref]: {provider: name}};
  if (pendingMode && pendingMode === KINDS[kind].mode) {
    const outcome = await saver.change(`${KINDS[kind].ref}.provider`, {...patch, agent: {input_mode: pendingMode}}, {quietInvalid: true});
    if (outcome !== 'invalid') return outcome;
  }
  return saver.change(`${KINDS[kind].ref}.provider`, patch);
}

/* ------------------------------------------------------------------ home --- */

export async function voicePage(context) {
  const {snapshot, schema} = await load(context);
  const body = el('div');
  const saver = createSaver(context, {title: msg('ui.nav_voice', '语音'), back: '/', root: body, onSaved: adoptSaved(snapshot)});
  const saved = effectiveMode(snapshot);
  const requested = context.query && context.query.mode;
  const shown = MODES.some(entry => entry.value === requested) ? requested : saved || 'realtime';
  const pending = shown !== saved ? shown : '';

  async function switchMode(mode) {
    if (mode === shown) return;
    if (mode === saved) {
      context.navigate('/voice', {replace: true});
      return;
    }
    const outcome = await saver.change('agent.input_mode', {agent: {input_mode: mode}}, {quietInvalid: true});
    context.navigate(outcome === 'saved' || outcome === 'staged' ? '/voice' : `/voice?mode=${mode}`, {replace: true});
  }

  // Every item carries its check, faded out unless active, so the switch can
  // cross-fade instead of inserting and removing the glyph.
  const thumb = el('span', {class: 'ds-segmented__thumb', attrs: {'aria-hidden': 'true'}});
  const items = MODES.map(entry => el('button', {
    class: 'ds-segmented__item',
    attrs: {type: 'button', role: 'tab'},
    data: {action: `voice-mode-${entry.value}`},
    on: {click: () => {
      if (entry.value === lastShownMode) return;
      // Answer the tap at once; saving the mode can take a round trip. The
      // page then re-renders with this mode as its starting point, so neither
      // the thumb nor the content animates a second time.
      select(entry.value);
      lastShownMode = entry.value;
      replace(content, [modeContent(entry.value, true)]);
      replace(note, []);
      switchMode(entry.value);
    }},
  }, [
    icon('check', {class: 'ds-segmented__check', size: 16}),
    text(entry.label, 'ds-segmented__label'),
  ]));
  const tabs = el('div', {class: 'ds-segmented', attrs: {role: 'tablist'}}, [thumb, ...items]);
  function select(mode) {
    const index = Math.max(0, MODES.findIndex(entry => entry.value === mode));
    tabs.style.setProperty('--segmented-index', String(index));
    items.forEach((item, i) => {
      item.classList.toggle('ds-segmented__item--active', i === index);
      item.querySelector('.ds-segmented__check').classList.toggle('ds-segmented__check--visible', i === index);
      item.setAttribute('aria-selected', i === index ? 'true' : 'false');
    });
  }
  const changed = lastShownMode !== null && lastShownMode !== shown;
  select(changed ? lastShownMode : shown);
  if (changed) {
    // Let the old position paint once, then slide to the new one.
    requestAnimationFrame(() => requestAnimationFrame(() => select(shown)));
  }
  lastShownMode = shown;

  function kindGroup(kind, caption, mode) {
    const name = currentName(snapshot, kind);
    const record = name ? recordsOf(snapshot, kind)[name] : null;
    const notSet = msg('voice.not_configured', '未配置');
    const modelField = record && visibleRecordFields(schema, kind, record, mode).find(field => field.key === 'model');
    const modelValue = record && (record.model || placeholderFor(modelField || {}, recordContext(kind, record, mode)));
    const q = `kind=${kind}${modeQuery(mode !== saved ? mode : '')}`;
    return group({
      caption,
      rows: [
        row({
          label: msg('voice.provider', 'Provider'),
          accessory: el('span', {class: 'ds-row__value-with-mark'}, [
            record ? providerMark(record.type) : null,
            text(record ? recordLabel(schema, kind, name, record) : notSet, record ? 'ds-row__value' : 'ds-row__value ds-row__value--muted'),
          ]),
          chevron: true,
          action: `open-${kind}-providers`,
          onPress: () => context.navigate(`/voice/providers?${q}`),
        }),
        row({
          label: msg('voice.model', '模型'),
          value: !record ? notSet : modelField ? (modelValue || msg('voice.default', '默认')) : msg('voice.model_fixed', '由提供商决定'),
          tone: record ? null : 'muted',
          chevron: Boolean(modelField),
          action: `open-${kind}-model`,
          onPress: modelField ? () => context.navigate(`/voice/model?${q}`) : undefined,
        }),
        row({
          label: msg('voice.extra', 'Extra'),
          chevron: true,
          action: `open-${kind}-extra`,
          onPress: () => context.navigate(`/voice/extra?${q}`),
        }),
      ],
    });
  }

  function modeContent(mode, animate) {
    return el('div', {class: animate ? 'ds-fade-in' : null}, mode === 'realtime'
      ? [kindGroup('realtime', undefined, mode)]
      : [kindGroup('stt', KINDS.stt.title, mode), kindGroup('tts', KINDS.tts.title, mode)]);
  }

  // Filled once the save answers: whether the switch is pending is not known
  // at the tap, and a note that appears late reads better than one that flashes.
  const note = el('div', {}, pending
    ? [text(saved
      ? msg('voice.mode_pending', '配置好以下提供商后才会切换到该模式')
      : msg('voice.mode_off', '语音尚未启用，配置好以下提供商后即可使用'), 'page__note')]
    : []);
  const content = el('div', {}, [modeContent(shown, changed)]);
  replace(body, [tabs, note, content]);
  saver.refreshChrome();
  return screen([body]);
}

/* ------------------------------------------------------------- providers --- */

/**
 * Confirm, then delete a configured voice provider record. Shared by the
 * list's swipe action and the edit page's delete row (for a mouse).
 */
async function deleteRecord(context, saver, {schema, snapshot, kind, mode, name}) {
  const confirmed = await confirmSheet({
    title: t('model.delete_title', {name: recordLabel(schema, kind, name, recordsOf(snapshot, kind)[name]), defaultValue: '删除 {{name}} 的配置？'}),
    body: msg('model.delete_body', '已保存的 API Key 等连接信息会一并删除，之后需要重新配置。'),
    confirmLabel: msg('action.delete', '删除'),
    danger: true,
    action: 'confirm-delete-provider',
  });
  if (!confirmed) return;
  const outcome = await saver.change(`${KINDS[kind].records}.${name}`, {[KINDS[kind].records]: {[name]: null}});
  if (outcome === 'saved' || outcome === 'staged') context.navigate(`/voice/providers?kind=${kind}${modeQuery(mode)}`, {replace: true});
}

export async function voiceProvidersPage(context) {
  const kind = kindFromQuery(context);
  if (!kind) {
    context.navigate('/voice', {replace: true});
    return screen([]);
  }
  const {snapshot, schema} = await load(context);
  const mode = (context.query && context.query.mode) || '';
  const body = el('div');
  const saver = createSaver(context, {title: msg('voice.providers', '提供商'), back: `/voice${mode ? `?mode=${mode}` : ''}`, root: body});

  const records = recordsOf(snapshot, kind);
  const names = Object.keys(records).sort();
  const current = currentName(snapshot, kind);
  const configured = new Set(names.map(name => records[name].type));
  const typeField = sectionFields(schema, KINDS[kind].records).find(field => field.key === 'type') || {};
  const edit = query => context.navigate(`/voice/providers/edit?kind=${kind}&${query}${modeQuery(mode)}`);

  replace(body, [
    names.length
      ? group({
          caption: msg('model.configured', '已配置'),
          captionMarker: 'success',
          rows: names.map(name => swipeRow(row({
            label: recordLabel(schema, kind, name, records[name]),
            leading: providerMark(records[name].type),
            action: `edit-${kind}-provider-${name}`,
            onPress: () => edit(`name=${encodeURIComponent(name)}`),
            accessory: name === current ? icon('check', {class: 'ds-choice-row__check', size: 22}) : null,
            chevron: true,
          }), {
            action: `delete-${kind}-provider-${name}`,
            onDelete: () => deleteRecord(context, saver, {schema, snapshot, kind, mode, name}),
            lockedLabel: name === current ? msg('model.in_use', '使用中') : null,
            onLocked: () => toast(msg('model.in_use_hint', '正在使用的配置不能删除，请先切换到其他提供商。')),
          })),
        })
      : null,
    group({
      caption: msg('model.unconfigured', '未配置'),
      captionMarker: 'muted',
      rows: (typeField.enum || []).filter(option => !configured.has(option.value)).map(option => row({
        label: optionLabel(KINDS[kind].records, 'type', option, typeName(option.value)),
        leading: providerMark(option.value),
        chevron: true,
        extraClass: 'ds-row--muted',
        action: `configure-${kind}-provider-${option.value}`,
        onPress: () => edit(`type=${encodeURIComponent(option.value)}`),
      })),
    }),
  ]);
  saver.refreshChrome();
  return screen([body]);
}

/** One connection field as a form control; returns `{node, read}`. */
function connectionControl(section, field, record, ctx, onSelect, typed) {
  const label = fieldLabel(section, field);
  if (field.widget === 'select' && field.enum && field.enum.length) {
    const options = optionsFor(field, record.type);
    const current = record[field.key] ?? field.default ?? '';
    const selected = options.find(option => String(option.value) === String(current));
    return {
      node: el('div', {class: 'ds-form-card'}, [
        text(label, 'ds-form-card__title'),
        row({
          label: selected ? optionLabel(section, field.key, selected) : msg('voice.choose', '请选择'),
          chevron: true,
          extraClass: 'ds-row--outlined',
          action: `choose-${field.key}`,
          onPress: () => choiceSheet({
            options: options.map(option => ({value: option.value, label: optionLabel(section, field.key, option)})),
            selected: current,
            onSelect: value => onSelect(field.key, value),
          }),
        }),
      ]),
      read: () => current,
    };
  }
  const secret = Boolean(field.secret);
  const input = el('input', {
    class: secret ? 'ds-input ds-input--with-toggle' : 'ds-input',
    attrs: {
      type: secret ? 'password' : 'text',
      value: secret ? (typed[field.key] || '') : (record[field.key] ?? ''),
      autocomplete: 'off', autocapitalize: 'none', spellcheck: 'false',
      placeholder: secret && record[`has_${field.key}`]
        ? resolve(msg('model.api_key_saved', '已保存，留空保持不变'))
        : placeholderFor(field, ctx),
      'aria-label': resolve(label),
    },
    data: {action: `edit-${field.key}`},
  });
  const control = [input];
  if (secret) {
    const toggle = el('button', {class: 'ds-input__toggle', attrs: {type: 'button', 'aria-pressed': 'false'}, data: {action: `toggle-${field.key}`}});
    const paint = () => {
      const revealed = input.type === 'text';
      replace(toggle, [icon(revealed ? 'eyeOff' : 'eye', {size: 18})]);
      toggle.setAttribute('aria-pressed', revealed ? 'true' : 'false');
      toggle.setAttribute('aria-label', resolve(revealed ? msg('model.hide_key', '隐藏密钥') : msg('model.show_key', '显示密钥')));
    };
    toggle.addEventListener('click', () => {
      input.type = input.type === 'password' ? 'text' : 'password';
      paint();
      input.focus();
    });
    paint();
    control.push(toggle);
  }
  return {
    node: el('div', {class: 'ds-form-card'}, [
      text(label, 'ds-form-card__title'),
      secret ? el('div', {class: 'ds-input-wrap'}, control) : input,
    ]),
    read: () => input.value.trim(),
    input,
    secret,
  };
}

export async function voiceProviderEditPage(context) {
  const kind = kindFromQuery(context);
  if (!kind) {
    context.navigate('/voice', {replace: true});
    return screen([]);
  }
  const {snapshot, schema} = await load(context);
  const query = context.query || {};
  const mode = query.mode || '';
  const records = recordsOf(snapshot, kind);
  const existingName = query.name && records[query.name] ? query.name : '';
  const typeField = sectionFields(schema, KINDS[kind].records).find(field => field.key === 'type') || {};
  const type = existingName ? records[existingName].type : query.type;
  if (!existingName && !(typeField.enum || []).some(option => option.value === type)) {
    context.navigate(`/voice/providers?kind=${kind}${modeQuery(mode)}`, {replace: true});
    return screen([]);
  }
  // Unsaved edits to select fields live here; text fields are read at save.
  const draft = {...(existingName ? records[existingName] : {}), type};
  // Secrets typed before a re-render; they never enter `draft`, which mirrors
  // the saved record and so must not carry a plaintext key.
  const typed = {};
  const body = el('div');
  const back = `/voice/providers?kind=${kind}${modeQuery(mode)}`;
  let controls = [];
  // Something to save: a field was typed in or an option picked, or a new
  // provider that needs no connection details at all.
  let touched = false;
  let submitting = false;
  const saver = createSaver(context, {
    title: existingName ? msg('model.edit_provider_title', '编辑提供商') : typeLabel(schema, kind, type),
    back,
    root: body,
    onSaved: adoptSaved(snapshot),
    form: {ready: () => !submitting && (touched || (!existingName && controls.length === 0)), submit},
  });
  const current = currentName(snapshot, kind);
  const useIt = existingName && existingName !== current
    ? button({
        label: msg('voice.use_provider', '设为当前提供商'),
        variant: 'secondary',
        block: true,
        action: 'use-provider',
        onPress: async () => {
          useIt.disabled = true;
          const outcome = await useRecord(saver, kind, existingName, mode);
          // Keep a pending mode: when it still did not validate, the home page
          // must go on showing it; when it did, the page sees it as saved.
          if (outcome === 'saved' || outcome === 'staged') context.navigate(`/voice${mode ? `?mode=${mode}` : ''}`, {replace: true});
          else useIt.disabled = false;
        },
      })
    : null;

  async function submit() {
    if (submitting) return;
    const record = {type};
    for (const control of controls) {
      const value = control.read();
      // A blank secret keeps the saved one; a blank plain field clears it.
      if (control.secret && !value) continue;
      if (value !== '' || existingName) record[control.key] = value;
    }
    const name = existingName || uniqueName(type, records);
    submitting = true;
    saver.refreshChrome();
    const outcome = await saver.change(`${KINDS[kind].records}.${name}`, {[KINDS[kind].records]: {[name]: record}});
    submitting = false;
    if (outcome !== 'saved' && outcome !== 'staged') {
      saver.refreshChrome();
      return;
    }
    // The first provider of a kind becomes its current one, so a fresh setup
    // is usable without a second step.
    if (!existingName && !current) await useRecord(saver, kind, name, mode);
    context.navigate(back, {replace: true});
  }

  function render() {
    const ctx = recordContext(kind, draft, mode || effectiveMode(snapshot));
    const fields = visibleRecordFields(schema, kind, draft, mode || effectiveMode(snapshot))
      .filter(field => CONNECTION_KEYS.has(field.key));
    controls = fields.map(field => {
      const control = connectionControl(KINDS[kind].records, field, draft, ctx, (key, value) => {
        // Keep what was typed so far; choosing an option re-renders the form.
        for (const other of controls) {
          if (!other.input) continue;
          if (other.secret) typed[other.key] = other.read();
          else draft[other.key] = other.read();
        }
        draft[key] = value;
        touched = true;
        render();
      }, typed);
      control.key = field.key;
      if (control.input) {
        control.input.addEventListener('input', () => {
          if (touched) return;
          touched = true;
          saver.refreshChrome();
        });
      }
      return control;
    });
    replace(body, [
      group({
        joined: true,
        rows: [
          el('div', {class: 'page__intro page__intro--in-card'}, [
            providerMark(type, {size: 'tile'}),
            text(existingName ? recordLabel(schema, kind, existingName, draft) : typeLabel(schema, kind, type), 'page__title'),
          ]),
          ...controls.map(control => control.node),
          fields.length ? null : text(msg('voice.no_connection_fields', '该提供商无需填写连接信息'), 'ds-form-card__help ds-form-card'),
        ],
      }),
      useIt ? el('div', {class: 'page__footer page__footer--stack'}, [useIt]) : null,
      // The configuration in use cannot be deleted, so it gets no delete row.
      existingName && existingName !== current ? group({rows: [row({
        label: msg('model.delete_config', '删除配置'),
        labelTone: 'danger',
        extraClass: 'ds-row--centered',
        action: 'delete-provider',
        onPress: () => deleteRecord(context, saver, {schema, snapshot, kind, mode, name: existingName}),
      })]}) : null,
    ]);
    saver.refreshChrome();
  }

  render();
  return screen([body]);
}

/* ----------------------------------------------------------------- model --- */

export async function voiceModelPage(context) {
  const kind = kindFromQuery(context);
  const {snapshot, schema} = await load(context);
  const name = kind ? currentName(snapshot, kind) : '';
  if (!kind || !name) {
    context.navigate('/voice', {replace: true});
    return screen([]);
  }
  const mode = (context.query && context.query.mode) || '';
  const records = KINDS[kind].records;
  const record = recordsOf(snapshot, kind)[name];
  const ctx = recordContext(kind, record, mode || effectiveMode(snapshot));
  const field = visibleRecordFields(schema, kind, record, mode || effectiveMode(snapshot)).find(entry => entry.key === 'model');
  const body = el('div');
  const saver = createSaver(context, {title: msg('voice.model', '模型'), back: `/voice${mode ? `?mode=${mode}` : ''}`, root: body, onSaved: adoptSaved(snapshot)});
  if (!field) {
    context.navigate('/voice', {replace: true});
    return screen([]);
  }
  const current = record.model || '';
  const fallback = placeholderFor(field, ctx);
  const options = optionsFor(field, record.type);
  const selectOnly = Boolean(field.selectWhen) && matches(field.selectWhen, ctx);

  async function choose(value) {
    if (value === current) {
      context.navigate('/voice', {replace: true});
      return;
    }
    const outcome = await saver.change(`${records}.${name}.model`, {[records]: {[name]: {model: value}}});
    if (outcome === 'saved' || outcome === 'staged') context.navigate(`/voice${mode ? `?mode=${mode}` : ''}`, {replace: true});
  }

  const check = active => (active ? icon('check', {class: 'ds-choice-row__check', size: 22}) : null);
  const rows = options.map(option => row({
    label: optionLabel(records, 'model', option),
    description: option.label ? String(option.value) : null,
    leading: providerMark(record.type),
    action: `choose-model-${option.value}`,
    onPress: () => choose(String(option.value)),
    accessory: check(String(option.value) === current),
  }));
  if (!selectOnly && current && !options.some(option => String(option.value) === current)) {
    rows.unshift(row({label: current, leading: providerMark(record.type), accessory: check(true)}));
  }
  if (!selectOnly && fallback && !options.some(option => String(option.value) === fallback)) {
    // An empty model uses the provider default, which the schema names.
    rows.push(row({
      label: msg('voice.default', '默认'),
      description: fallback,
      leading: providerMark(record.type),
      action: 'choose-model-default',
      onPress: () => choose(''),
      accessory: check(!current),
    }));
  }

  const custom = selectOnly ? null : group({rows: [customValueField({
    title: msg('model.custom_model', '其他模型'),
    placeholder: msg('model.custom_model_placeholder', '输入列表中没有的模型名称'),
    action: 'edit-custom-model',
    onUse: choose,
  })]});

  replace(body, [rows.length ? group({rows}) : null, custom]);
  saver.refreshChrome();
  return screen([body]);
}

/* ----------------------------------------------------------------- extra --- */

/** A schema field as a settings row that saves on commit. */
function settingRow(section, field, value, ctx, type, saver, patchFor) {
  const label = fieldLabel(section, field);
  const description = fieldHelp(section, field);
  const key = `${section}.${field.key}`;
  if (field.widget === 'boolean') {
    const toggle = switchControl({
      checked: value == null ? Boolean(field.default) : Boolean(value),
      label,
      onChange: async next => {
        const outcome = await saver.change(key, patchFor(next));
        if (outcome === 'invalid' || outcome === 'failed') setChecked(toggle, !next);
      },
    });
    return row({label, description, accessory: toggle});
  }
  if (field.widget === 'select') {
    const options = field.enum && field.enum.length ? optionsFor(field, type) : rangeOptions(field);
    const current = value ?? field.default ?? '';
    const selected = options.find(option => String(option.value) === String(current));
    return row({
      label,
      description,
      value: selected ? (field.enum ? optionLabel(section, field.key, selected) : selected.label) : String(current),
      action: `open-${field.key}`,
      onPress: () => choiceSheet({
        options: options.map(option => ({value: option.value, label: field.enum ? optionLabel(section, field.key, option) : option.label})),
        selected: current,
        onSelect: next => saver.change(key, patchFor(next)),
      }),
    });
  }
  const numeric = field.widget === 'number';
  return inlineValueRow({
    key,
    label,
    description,
    // A number box is too narrow for the schema's "Default: 0.5" hints; the
    // help line already names the default.
    placeholder: numeric ? msg('voice.default', '默认') : (placeholderFor(field, ctx) || null),
    inputmode: numeric ? 'decimal' : 'text',
    wide: !numeric,
    saved: () => (value == null ? '' : String(value)),
    parse: numeric
      ? textValue => (textValue === '' ? (field.nullable ? null : 0) : (/^-?\d*\.?\d+$/.test(textValue) ? Number(textValue) : undefined))
      : textValue => textValue,
    invalid: msg('voice.number_invalid', '请输入数字'),
    patch: patchFor,
    saver,
  });
}

export async function voiceExtraPage(context) {
  const kind = kindFromQuery(context);
  if (!kind) {
    context.navigate('/voice', {replace: true});
    return screen([]);
  }
  const {snapshot, schema} = await load(context);
  const mode = (context.query && context.query.mode) || '';
  const shownMode = mode || effectiveMode(snapshot) || KINDS[kind].mode;
  const {records, ref, prefs} = KINDS[kind];
  const body = el('div');
  const saver = createSaver(context, {title: msg('voice.extra', 'Extra'), back: `/voice${mode ? `?mode=${mode}` : ''}`, root: body, onSaved: adoptSaved(snapshot)});
  const name = currentName(snapshot, kind);
  const record = name ? recordsOf(snapshot, kind)[name] : null;

  // Preferences that hold whichever provider is selected (`stt.language`, ...).
  const prefContext = {'agent.input_mode': shownMode};
  const prefRows = sectionFields(schema, ref)
    .filter(field => prefs.includes(field.key) && matches(field.visibleWhen, prefContext))
    .map(field => settingRow(ref, field, refOf(snapshot, kind)[field.key], prefContext, '', saver,
      value => ({[ref]: {[field.key]: value}})));

  // The selected record's own settings other than connection and model.
  const recordRows = {basic: [], advanced: []};
  if (record) {
    const ctx = recordContext(kind, record, shownMode);
    for (const field of visibleRecordFields(schema, kind, record, shownMode)) {
      if (field.key === 'model' || CONNECTION_KEYS.has(field.key) || field.secret) continue;
      const node = settingRow(records, field, record[field.key], ctx, record.type, saver,
        value => ({[records]: {[name]: {[field.key]: value}}}));
      recordRows[field.advanced ? 'advanced' : 'basic'].push(node);
    }
  }

  const sections = [
    prefRows.length || recordRows.basic.length ? group({rows: [...prefRows, ...recordRows.basic]}) : null,
    recordRows.advanced.length ? group({caption: msg('voice.advanced', '高级'), rows: recordRows.advanced}) : null,
  ].filter(Boolean);
  replace(body, sections.length ? sections : [group({rows: [row({label: msg('voice.no_extra', '当前提供商没有其他设置')})]})]);
  saver.refreshChrome();
  return screen([body]);
}
