/**
 * Model routes.
 *
 *   /model                              entries: the main model, the optional decision model
 *   /model/main                         provider, model, parameters, context limits, test
 *   /model/providers                    configured provider records, then unconfigured types
 *   /model/providers/edit?name=<record> edit a configured record's credentials
 *   /model/providers/edit?type=<type>   configure a new record of a type
 *   /model/providers/edit?type=custom   "Others": pick the interface type, key and Base URL
 *   /model/models                       the selected provider's model catalogue
 *   /model/decision                     the optional decision model: key, test, confidence
 *
 * The main model references a provider *record* by name (`model.provider`); a
 * record carries the type and credentials (`model_providers.<name>`). Several
 * records can stay configured, so switching is a one-line change.
 *
 * Every field here is applied live, so values save as they are committed. The
 * one explicit button is the credential form's 保存, because a key is entered
 * as a whole rather than edited value by value.
 *
 * The advanced model fields (conversation API, provider compaction, raw HTTP
 * logging, reasoning budget) are not part of this design; they remain on the
 * classic page.
 *
 * The decision model (`decision`) is a separate, optional classifier for fixed
 * workflow checks, so it has its own page rather than a group under the main
 * model's parameters. Its Base URL and model keep their defaults here;
 * they are edited in the TOML file when they ever need to change.
 */

import {fetchSnapshot, request, t} from '../data.js';
import {getActiveLocale} from '../../config/i18n.js';
import {customValueField, inlineValueRow, parse} from '../fields.js';
import {createSaver} from '../saver.js';
import {providerMark} from '../../ui/brand.js';
import {button} from '../../ui/button.js';
import {choiceSheet} from '../../ui/choice-sheet.js';
import {confirmSheet} from '../../ui/confirm.js';
import {swipeRow} from '../../ui/swipe.js';
import {el, replace} from '../../ui/dom.js';
import {icon} from '../../ui/icon.js';
import {group, row, screen} from '../../ui/list.js';
import {msg, resolve, text} from '../../ui/text.js';
import {toast} from '../../ui/toast.js';

/**
 * Provider types offered by the Agent, in its registry order
 * (`modelProviderDefinitions` in src/agent/internal/agent/model_provider_registry.go).
 * Brand names are not translated.
 */
export const PROVIDER_TYPES = [
  {type: 'openai', label: 'OpenAI'},
  {type: 'anthropic', label: 'Anthropic'},
  {type: 'openrouter', label: 'OpenRouter'},
  {type: 'kimi', label: msg('model.type_kimi', 'Kimi（国际）')},
  {type: 'kimi-cn', label: msg('model.type_kimi_cn', 'Kimi（中国）')},
  {type: 'volcengine', label: msg('model.type_volcengine', '火山引擎')},
  {type: 'deepseek', label: 'DeepSeek'},
  {type: 'ollama', label: 'Ollama'},
  {type: 'gemini', label: 'Gemini'},
];

/** Types that accept a custom Base URL (`allowsCustomBaseURL` in the registry). */
export const CUSTOM_BASE_URL_TYPES = ['openai', 'anthropic', 'ollama', 'gemini'];

/**
 * Interface types offered under "其他", which is the entry for third-party relay
 * endpoints. Relays speak the OpenAI or the Anthropic protocol. Ollama's Base
 * URL points at a local server and Gemini's at Google's Interactions API, which
 * relays do not implement; both stay editable from their own entries.
 */
export const RELAY_TYPES = [
  {type: 'openai', label: msg('model.relay_openai', 'OpenAI 兼容')},
  {type: 'anthropic', label: msg('model.relay_anthropic', 'Anthropic 兼容')},
];

function relayLabel(type) {
  const entry = RELAY_TYPES.find(item => item.type === type);
  return entry ? entry.label : typeInfo(type).label;
}

/** Ollama runs locally and needs no key; every other type does. */
const KEYLESS_TYPES = ['ollama'];

const EFFORT_LABELS = {
  '': msg('model.effort_auto', '自动（默认）'),
  none: msg('model.effort_none', '关闭'),
  minimal: msg('model.effort_minimal', '最低'),
  low: msg('model.effort_low', '低'),
  medium: msg('model.effort_medium', '中'),
  high: msg('model.effort_high', '高'),
  xhigh: msg('model.effort_xhigh', '超高'),
  max: msg('model.effort_max', '最高'),
};

function typeInfo(type) {
  return PROVIDER_TYPES.find(entry => entry.type === type) || {type, label: type || '—'};
}

/** "OpenAI" for a record named after its type, "apibest (OpenAI)" otherwise. */
export function recordLabel(name, record) {
  const type = (record && record.type) || '';
  const label = resolve(typeInfo(type).label);
  return !type || name === type ? label : `${name} (${label})`;
}

/** Host part of a Base URL, for a record's secondary line. */
function hostOf(url) {
  try {
    return new URL(url).host;
  } catch (_error) {
    return '';
  }
}

/**
 * A record name for a new provider, following the classic page: the type, or
 * for an OpenAI-compatible endpoint the registrable part of its host, with a
 * numeric suffix when taken.
 */
export function newRecordName(record, records) {
  let base = record.type;
  if (record.type === 'openai' && record.base_url) {
    const labels = hostOf(record.base_url).replace(/:\d+$/, '').split('.').filter(Boolean);
    if (labels.length > 1) {
      let end = labels.length - 1;
      if (end > 1 && ['com', 'co', 'net', 'org', 'gov', 'edu', 'ac'].includes(labels[end - 1])) end -= 1;
      base = labels[end - 1];
    } else if (labels.length === 1) {
      base = labels[0];
    }
  }
  base = String(base || 'provider').replace(/[^A-Za-z0-9_-]+/g, '-').replace(/^-+|-+$/g, '') || 'provider';
  if (!Object.prototype.hasOwnProperty.call(records, base)) return base;
  for (let i = 2; i < 1000; i += 1) {
    if (!Object.prototype.hasOwnProperty.call(records, `${base}-${i}`)) return `${base}-${i}`;
  }
  return base;
}

async function ensureSnapshot(context) {
  return context.snapshot || fetchSnapshot().catch(() => ({config: {}}));
}

const modelConfig = snapshot => (snapshot && snapshot.config && snapshot.config.model) || {};
const records = snapshot => (snapshot && snapshot.config && snapshot.config.model_providers) || {};
const currentRecord = snapshot => records(snapshot)[modelConfig(snapshot).provider] || null;
const decisionConfig = snapshot => (snapshot && snapshot.config && snapshot.config.decision) || {};

/** Decision model providers, as `decision.provider` accepts them. */
export const DECISION_PROVIDERS = [
  {value: 'typesafe', label: msg('model.decision_provider_typesafe', 'TypeSafe Jev'),
    help: msg('model.decision_provider_typesafe_help', '在 typesafe.ai 获取 API Key')},
  {value: 'openai', label: msg('model.decision_provider_openai', 'OpenAI Luna'),
    help: msg('model.decision_provider_openai_help', '使用 OpenAI 的 Decisions API；它不能定位点击位置，这类判断仍交给主模型')},
];

function decisionProvider(value) {
  return DECISION_PROVIDERS.find(entry => entry.value === value) || DECISION_PROVIDERS[0];
}

function adoptSaved(snapshot) {
  return payload => {
    if (payload && payload.config) snapshot.config = payload.config;
  };
}

/** The catalogue for a provider type; one model, when given, adds its `spec`. */
async function catalogue(type, model) {
  if (!type) return {models: []};
  const query = new URLSearchParams({provider: type, locale: getActiveLocale() || 'zh-CN'});
  if (model) query.set('model', model);
  try {
    return await request(`/api/models?${query}`);
  } catch (error) {
    console.warn('[model] catalogue unavailable:', error && error.message);
    return {models: []};
  }
}

/** Reasoning choices for a model, or null when it cannot reason at all. */
function effortChoices(spec) {
  const reasoning = spec && spec.reasoning;
  if (reasoning && reasoning.supported === false) return null;
  const efforts = reasoning && Array.isArray(reasoning.efforts) && reasoning.efforts.length
    ? reasoning.efforts
    : ['low', 'medium', 'high'];
  return ['', ...efforts.filter(effort => EFFORT_LABELS[effort])];
}

/**
 * The patch that points the main model at `model` on `provider`, clearing a
 * reasoning effort the new model does not accept rather than letting the save
 * be rejected for it.
 */
async function selectionPatch(snapshot, providerName, type, model) {
  const patch = {model: {provider: providerName, model}};
  const effort = modelConfig(snapshot).reasoning_effort || '';
  if (effort) {
    const spec = (await catalogue(type, model)).spec;
    const choices = effortChoices(spec);
    if (!choices || !choices.includes(effort)) patch.model.reasoning_effort = '';
  }
  return patch;
}

/**
 * Point the main model at a configured record. The current model is kept when
 * the new provider lists it; otherwise the provider's recommended model is
 * used, so the pair is always usable.
 */
async function selectProvider(snapshot, saver, name, record) {
  const models = (await catalogue(record.type)).models || [];
  const current = modelConfig(snapshot).model;
  const keep = models.some(entry => entry.id === current);
  const model = keep ? current : ((models.find(entry => entry.recommended) || models[0] || {}).id || current);
  return saver.change('model.provider', await selectionPatch(snapshot, name, record.type, model));
}

/* ------------------------------------------------------------------ home --- */

/** The models the Agent uses: the main model and the optional decision model. */
export async function modelPage(context) {
  const snapshot = await ensureSnapshot(context);
  const providerName = modelConfig(snapshot).provider || '';
  const record = currentRecord(snapshot);
  const modelName = modelConfig(snapshot).model || '';
  const mainSummary = providerName
    ? [recordLabel(providerName, record), modelName].filter(Boolean).join(' · ')
    : resolve(msg('model.not_set', '未设置'));
  const decisionOn = Boolean(decisionConfig(snapshot).has_api_key);
  return screen([
    group({
      rows: [
        row({
          label: msg('model.main', '主模型'),
          accessory: el('span', {class: 'ds-row__value-with-mark'}, [
            record ? providerMark(record.type) : null,
            text(mainSummary, 'ds-row__value'),
          ]),
          chevron: true,
          action: 'open-main-model',
          onPress: () => context.navigate('/model/main'),
        }),
      ],
    }),
    group({
      caption: msg('model.section_auxiliary', '辅助模型（可选）'),
      rows: [
        row({
          label: msg('model.decision', '决策模型'),
          value: decisionOn ? msg('model.decision_on', '已开启') : msg('model.decision_off', '未开启'),
          tone: decisionOn ? 'success' : undefined,
          chevron: true,
          action: 'open-decision-model',
          onPress: () => context.navigate('/model/decision'),
        }),
      ],
    }),
  ]);
}

/* ------------------------------------------------------------- main model --- */

export async function mainModelPage(context) {
  const snapshot = await ensureSnapshot(context);
  const body = el('div');
  const saver = createSaver(context, {
    title: msg('model.main', '主模型'),
    back: '/model',
    root: body,
    onSaved: payload => {
      adoptSaved(snapshot)(payload);
      render();
    },
  });
  const model = () => modelConfig(snapshot);
  const type = () => (currentRecord(snapshot) || {}).type || '';
  let spec = (await catalogue(type(), model().model)).spec || null;

  const numberRow = (key, label, description, parser, invalid, options = {}) => inlineValueRow({
    key: `model.${key}`,
    label,
    description,
    parse: parser,
    invalid,
    saved: () => (model()[key] == null ? '' : String(model()[key])),
    patch: value => ({model: {[key]: value}}),
    saver,
    ...options,
  });

  function effortRow() {
    const choices = effortChoices(spec);
    const current = model().reasoning_effort || '';
    if (!choices) {
      return row({label: msg('model.reasoning_effort', '推理强度'), value: msg('model.effort_unsupported', '不支持')});
    }
    return row({
      label: msg('model.reasoning_effort', '推理强度'),
      value: EFFORT_LABELS[current] || current,
      action: 'open-reasoning-effort',
      onPress: () => choiceSheet({
        options: choices.map(value => ({value, label: EFFORT_LABELS[value]})),
        selected: current,
        onSelect: value => saver.change('model.reasoning_effort', {model: {reasoning_effort: value}}),
      }),
    });
  }

  async function testConnection(action) {
    action.disabled = true;
    try {
      const payload = await request('/api/config/test', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({section: 'model', values: model()}),
      });
      const failed = (payload.results || []).find(result => !result.passed);
      toast(payload.ok
        ? msg('model.test_passed', '连接成功')
        : `${resolve(msg('model.test_failed', '连接失败'))}${failed && failed.detail ? `：${failed.detail}` : ''}`,
        {durationMs: payload.ok ? undefined : 5000});
    } catch (error) {
      toast(error && error.message ? error.message : resolve(msg('model.test_failed', '连接失败')), {durationMs: 5000});
    } finally {
      action.disabled = false;
    }
  }

  function render() {
    const providerName = model().provider || '';
    const record = currentRecord(snapshot);
    const testButton = el('button', {
      class: 'ds-card-action',
      attrs: {type: 'button'},
      data: {action: 'test-model-connection'},
      on: {click: () => testConnection(testButton)},
    }, [text(msg('model.test', '测试连接'))]);
    replace(body, [
      group({
        rows: [
          row({
            label: msg('model.provider', '主模型提供商'),
            accessory: el('span', {class: 'ds-row__value-with-mark'}, [
              record ? providerMark(record.type) : null,
              text(providerName ? recordLabel(providerName, record) : msg('model.not_set', '未设置'), 'ds-row__value'),
            ]),
            chevron: true,
            action: 'open-model-providers',
            onPress: () => context.navigate('/model/providers'),
          }),
        ],
      }),
      group({
        caption: msg('model.section_model', '模型配置'),
        rows: [
          row({
            label: msg('model.model', '模型'),
            value: model().model || msg('model.not_set', '未设置'),
            chevron: true,
            action: 'open-models',
            onPress: () => context.navigate('/model/models'),
          }),
        ],
      }),
      group({
        caption: msg('model.section_parameters', '参数设置'),
        rows: [
          numberRow('temperature', msg('model.temperature', '温度'), msg('model.temperature_help', '越高回答越发散；留空表示使用模型默认值'),
            parse.optionalRange(0, 2), msg('model.temperature_invalid', '请输入 0 到 2 之间的数，或留空'),
            {placeholder: msg('model.auto', '自动')}),
          effortRow(),
          numberRow('max_response_tokens', msg('model.max_response_tokens', '最大响应 Token 数'), null,
            parse.integer(0), msg('model.integer_invalid', '请输入 0 或正整数'), {inputmode: 'numeric'}),
        ],
      }),
      group({
        caption: msg('model.section_context', '上下文限制'),
        rows: [
          numberRow('context_window', msg('model.context_window', '上下文窗口（Token）'), msg('model.auto_help', '0 表示自动，使用提供商公布的数值'),
            parse.integer(0), msg('model.integer_invalid', '请输入 0 或正整数'), {inputmode: 'numeric'}),
          numberRow('model_max_output_tokens', msg('model.max_output', '提供商输出上限（Token）'), msg('model.auto_help', '0 表示自动，使用提供商公布的数值'),
            parse.integer(0), msg('model.integer_invalid', '请输入 0 或正整数'), {inputmode: 'numeric'}),
        ],
      }),
      el('div', {class: 'ds-group'}, [testButton]),
    ]);
    saver.refreshChrome();
  }

  render();
  return screen([body]);
}

/* ------------------------------------------------------------- providers --- */

/**
 * Confirm, then delete a configured provider record (a null merge patch).
 * Shared by the list's swipe action and the edit page's delete row, which is
 * there for a mouse, where nobody drags a row sideways.
 */
async function deleteProvider(context, saver, name, record) {
  const confirmed = await confirmSheet({
    title: t('model.delete_title', {name: recordLabel(name, record), defaultValue: '删除 {{name}} 的配置？'}),
    body: msg('model.delete_body', '已保存的 API Key 等连接信息会一并删除，之后需要重新配置。'),
    confirmLabel: msg('action.delete', '删除'),
    danger: true,
    action: 'confirm-delete-provider',
  });
  if (!confirmed) return;
  const outcome = await saver.change(`model_providers.${name}`, {model_providers: {[name]: null}});
  if (outcome === 'saved' || outcome === 'staged') context.navigate('/model/providers', {replace: true});
}

export async function providersPage(context) {
  const snapshot = await ensureSnapshot(context);
  const body = el('div');
  const saver = createSaver(context, {
    title: msg('model.providers', '提供商'),
    back: '/model/main',
    root: body,
    onSaved: adoptSaved(snapshot),
  });

  const all = records(snapshot);
  const names = Object.keys(all).sort();
  const configuredTypes = new Set(names.map(name => all[name].type));
  const current = modelConfig(snapshot).provider;

  // A configured provider opens its credentials, as the design draws it; the
  // check marks the one the main model uses, and switching happens there.
  // Swiping left deletes its configuration, except the one in use.
  const configuredRows = names.map(name => {
    const record = all[name];
    return swipeRow(row({
      label: recordLabel(name, record),
      description: record.base_url ? hostOf(record.base_url) : null,
      leading: providerMark(record.type),
      action: `edit-provider-${name}`,
      onPress: () => context.navigate(`/model/providers/edit?name=${encodeURIComponent(name)}`),
      accessory: name === current ? icon('check', {class: 'ds-choice-row__check', size: 22}) : null,
      chevron: true,
    }), {
      action: `delete-provider-${name}`,
      onDelete: () => deleteProvider(context, saver, name, record),
      lockedLabel: name === current ? msg('model.in_use', '使用中') : null,
      onLocked: () => toast(msg('model.in_use_hint', '正在使用的配置不能删除，请先切换到其他提供商。')),
    });
  });

  const unconfiguredRows = PROVIDER_TYPES.filter(entry => !configuredTypes.has(entry.type)).map(entry => row({
    label: entry.label,
    leading: providerMark(entry.type),
    chevron: true,
    extraClass: 'ds-row--muted',
    action: `configure-provider-${entry.type}`,
    onPress: () => context.navigate(`/model/providers/edit?type=${encodeURIComponent(entry.type)}`),
  }));
  unconfiguredRows.push(row({
    label: msg('model.others', '其他'),
    labelTone: 'accent',
    leading: providerMark('custom'),
    action: 'configure-provider-custom',
    onPress: () => context.navigate('/model/providers/edit?type=custom'),
  }));

  replace(body, [
    names.length ? group({caption: msg('model.configured', '已配置'), captionMarker: 'success', rows: configuredRows}) : null,
    group({caption: msg('model.unconfigured', '未配置'), captionMarker: 'muted', rows: unconfiguredRows}),
  ]);
  saver.refreshChrome();
  return screen([body], 'model-providers');
}

/* ----------------------------------------------------------- credentials --- */

function secretField(label, placeholder) {
  const input = el('input', {
    class: 'ds-input ds-input--with-toggle',
    attrs: {type: 'password', autocomplete: 'off', autocapitalize: 'none', spellcheck: 'false', placeholder: resolve(placeholder), 'aria-label': resolve(label)},
    data: {action: 'edit-provider-api-key'},
  });
  const toggle = el('button', {
    class: 'ds-input__toggle',
    attrs: {type: 'button', 'aria-label': resolve(msg('model.show_key', '显示密钥')), 'aria-pressed': 'false'},
    data: {action: 'toggle-api-key'},
  });
  // The glyph shows what a tap does next: an open eye reveals, a struck eye hides.
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
  return {input, node: el('div', {class: 'ds-form-card'}, [
    text(label, 'ds-form-card__title'),
    el('div', {class: 'ds-input-wrap'}, [input, toggle]),
  ])};
}

function textField(label, value, placeholder, action) {
  const input = el('input', {
    class: 'ds-input',
    attrs: {type: 'url', value: value || '', autocomplete: 'off', autocapitalize: 'none', spellcheck: 'false', inputmode: 'url', placeholder: resolve(placeholder), 'aria-label': resolve(label)},
    data: {action},
  });
  return {input, node: el('div', {class: 'ds-form-card'}, [text(label, 'ds-form-card__title'), input])};
}

/**
 * Configure a provider record: an existing one (`?name=`), a new one of a type
 * (`?type=`), or a custom endpoint (`?type=custom`) whose interface type is
 * chosen here.
 */
export async function providerEditPage(context) {
  const snapshot = await ensureSnapshot(context);
  const query = context.query || {};
  const existingName = query.name && records(snapshot)[query.name] ? query.name : '';
  const existing = existingName ? records(snapshot)[existingName] : null;
  const custom = !existing && query.type === 'custom';
  let type = existing ? existing.type : custom ? RELAY_TYPES[0].type : query.type;
  if (!existing && !custom && !PROVIDER_TYPES.some(entry => entry.type === type)) {
    context.navigate('/model/providers', {replace: true});
    return screen([]);
  }

  const body = el('div');
  const title = existing ? msg('model.edit_provider_title', '编辑提供商') : custom ? msg('model.others', '其他') : typeInfo(type).label;
  let submitting = false;
  // Saved from the top bar (or the app's native bar) once there is something
  // complete to save.
  const saver = createSaver(context, {
    title,
    back: '/model/providers',
    root: body,
    onSaved: adoptSaved(snapshot),
    form: {ready: () => !submitting && formReady(), submit},
  });

  const apiKey = secretField(msg('model.api_key', 'API Key'), existing && existing.has_api_key
    ? msg('model.api_key_saved', '已保存，留空保持不变')
    : KEYLESS_TYPES.includes(type) ? msg('model.api_key_optional', '可选') : msg('model.api_key_placeholder', '请输入 API Key'));
  const baseURL = textField(msg('model.base_url', 'Base URL'), existing && existing.base_url,
    custom ? msg('model.base_url_placeholder', 'https://example.com/v1') : msg('model.base_url_optional', '可选，留空使用官方地址'), 'edit-provider-base-url');

  // Switching the main model to this record lives here, because tapping a
  // configured provider in the list opens this page.
  const isCurrent = existingName && existingName === modelConfig(snapshot).provider;
  const useAsMain = existing && !isCurrent
    ? button({
        label: msg('model.use_provider', '设为主模型提供商'),
        variant: 'secondary',
        block: true,
        action: 'use-provider',
        onPress: async () => {
          useAsMain.disabled = true;
          const outcome = await selectProvider(snapshot, saver, existingName, existing);
          if (outcome === 'saved' || outcome === 'staged') context.navigate('/model/main', {replace: true});
          else useAsMain.disabled = false;
        },
      })
    : null;
  function formReady() {
    const key = apiKey.input.value.trim();
    const url = baseURL.input.value.trim();
    const changed = key !== '' || url !== ((existing && existing.base_url) || '');
    const complete = (key !== '' || KEYLESS_TYPES.includes(type) || (existing && existing.has_api_key)) && (!custom || url !== '');
    return changed && complete;
  }
  const refreshSave = () => saver.refreshChrome();
  apiKey.input.addEventListener('input', refreshSave);
  baseURL.input.addEventListener('input', refreshSave);

  async function submit() {
    if (submitting || !formReady()) return;
    const record = {type};
    const key = apiKey.input.value.trim();
    const url = baseURL.input.value.trim();
    if (key) record.api_key = key;
    if (CUSTOM_BASE_URL_TYPES.includes(type)) record.base_url = url;
    const name = existingName || newRecordName(record, records(snapshot));
    submitting = true;
    refreshSave();
    const outcome = await saver.change(`model_providers.${name}`, {model_providers: {[name]: record}});
    submitting = false;
    if (outcome === 'saved' || outcome === 'staged') context.navigate('/model/providers', {replace: true});
    else refreshSave();
  }

  function render() {
    const typeRow = custom
      ? el('div', {class: 'ds-form-card'}, [
        text(msg('model.interface_type', '接口类型'), 'ds-form-card__title'),
        row({
          label: relayLabel(type),
          leading: providerMark(type),
          chevron: true,
          extraClass: 'ds-row--outlined',
          action: 'open-interface-type',
          onPress: () => choiceSheet({
            layout: 'cards',
            options: RELAY_TYPES.map(entry => ({value: entry.type, label: entry.label, leading: () => providerMark(entry.type)})),
            selected: type,
            onSelect: value => {
              type = value;
              render();
            },
          }),
        }),
      ])
      : null;
    replace(body, [
      group({
        joined: true,
        rows: [
          el('div', {class: 'page__intro page__intro--in-card'}, [
            providerMark(custom ? 'custom' : type, {size: 'tile'}),
            text(custom ? msg('model.others', '其他') : existing ? recordLabel(existingName, existing) : typeInfo(type).label, 'page__title'),
          ]),
          typeRow,
          apiKey.node,
          CUSTOM_BASE_URL_TYPES.includes(type) ? baseURL.node : null,
        ],
      }),
      useAsMain ? el('div', {class: 'page__footer page__footer--stack'}, [useAsMain]) : null,
      // The configuration in use cannot be deleted, so it gets no delete row.
      existing && !isCurrent ? group({rows: [row({
        label: msg('model.delete_config', '删除配置'),
        labelTone: 'danger',
        extraClass: 'ds-row--centered',
        action: 'delete-provider',
        onPress: () => deleteProvider(context, saver, existingName, existing),
      })]}) : null,
    ]);
    refreshSave();
  }

  render();
  return screen([body]);
}

/* ---------------------------------------------------------------- models --- */

export async function modelsPage(context) {
  const snapshot = await ensureSnapshot(context);
  const body = el('div');
  const saver = createSaver(context, {title: msg('model.model', '模型'), back: '/model/main', root: body, onSaved: adoptSaved(snapshot)});
  const providerName = modelConfig(snapshot).provider;
  const record = currentRecord(snapshot);
  const type = record ? record.type : '';
  const models = (await catalogue(type)).models || [];
  const current = modelConfig(snapshot).model || '';

  async function choose(model) {
    if (model === current) {
      context.navigate('/model/main', {replace: true});
      return;
    }
    const outcome = await saver.change('model.model', await selectionPatch(snapshot, providerName, type, model));
    if (outcome === 'saved' || outcome === 'staged') context.navigate('/model/main', {replace: true});
  }

  const listed = models.map(entry => entry.id);
  const rows = models.map(entry => row({
    label: entry.id,
    description: entry.description || null,
    leading: providerMark(type),
    action: `choose-model-${entry.id}`,
    onPress: () => choose(entry.id),
    accessory: entry.id === current ? icon('check', {class: 'ds-choice-row__check', size: 22}) : null,
  }));
  // A model the catalogue does not list (common behind a custom Base URL) is
  // still shown as selected, and any other name can be entered below.
  if (current && !listed.includes(current)) {
    rows.unshift(row({label: current, leading: providerMark(type), accessory: icon('check', {class: 'ds-choice-row__check', size: 22})}));
  }

  replace(body, [
    rows.length ? group({rows}) : null,
    group({rows: [customValueField({
      title: msg('model.custom_model', '其他模型'),
      placeholder: msg('model.custom_model_placeholder', '输入列表中没有的模型名称'),
      action: 'edit-custom-model',
      onUse: choose,
    })]}),
  ]);
  saver.refreshChrome();
  return screen([body]);
}

/* -------------------------------------------------------------- decision --- */

/**
 * The optional decision model. The key is write-only like the Web search key:
 * the Agent reports `has_api_key`, and an empty field keeps the saved key. The
 * key is the only switch: saving one turns the decision model on, removing it
 * turns it off.
 */
export async function decisionPage(context) {
  const snapshot = await ensureSnapshot(context);
  const body = el('div');
  const saver = createSaver(context, {
    title: msg('model.decision', '决策模型'),
    back: '/model',
    root: body,
    onSaved: payload => {
      adoptSaved(snapshot)(payload);
      render();
    },
  });
  const decision = () => decisionConfig(snapshot);

  function apiKeyField() {
    const input = el('input', {
      class: 'ds-input',
      attrs: {
        type: 'password',
        autocomplete: 'off',
        autocapitalize: 'none',
        spellcheck: 'false',
        placeholder: resolve(decision().has_api_key
          ? msg('model.api_key_saved', '已保存，留空保持不变')
          : msg('model.api_key_placeholder', '请输入 API Key')),
        'aria-label': resolve(msg('model.decision_api_key', '决策模型 API Key')),
      },
      data: {action: 'edit-decision-api-key'},
    });
    input.addEventListener('keydown', event => {
      if (event.key === 'Enter') input.blur();
    });
    input.addEventListener('change', async () => {
      const key = input.value.trim();
      if (!key) return;
      if (await saver.change('decision.api_key', {decision: {api_key: key}}) === 'saved') input.value = '';
    });
    return el('div', {class: 'ds-form-card'}, [
      text(msg('model.decision_api_key', '决策模型 API Key'), 'ds-form-card__title'),
      text(decisionProvider(decision().provider).help, 'ds-form-card__help'),
      input,
    ]);
  }

  /**
   * The key belongs to one provider, so switching clears it, which turns the
   * model off; the Base URL and model return to the new provider's defaults.
   */
  async function chooseProvider(value) {
    const current = decision();
    if (value === decisionProvider(current.provider).value) return;
    const patch = {decision: {provider: value, base_url: null, model: null}};
    if (current.has_api_key) {
      const confirmed = await confirmSheet({
        title: msg('model.decision_switch_title', '切换决策模型提供商？'),
        body: msg('model.decision_switch_body', '已保存的 API Key 会被清除，决策模型随之关闭；填写新提供商的 API Key 后自动开启。'),
        confirmLabel: msg('model.decision_switch_confirm', '切换'),
        action: 'confirm-decision-provider',
      });
      if (!confirmed) return;
      patch.decision.api_key = '';
    }
    await saver.change('decision.provider', patch);
  }

  async function removeKey() {
    const confirmed = await confirmSheet({
      title: msg('model.decision_remove_key_title', '移除决策模型 API Key？'),
      body: msg('model.decision_remove_key_body', '移除后决策模型关闭，所有判断交给主模型。'),
      confirmLabel: msg('model.decision_remove_key_confirm', '移除'),
      danger: true,
      action: 'confirm-remove-decision-key',
    });
    if (confirmed) await saver.change('decision.api_key', {decision: {api_key: ''}});
  }

  /** Sends one small question with the saved settings and reports the reply. */
  async function testConnection(action) {
    action.disabled = true;
    try {
      const payload = await request('/api/config/test', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify({section: 'decision', values: decision()}),
      });
      const result = (payload.results || [])[0];
      toast(payload.ok
        ? msg('model.test_passed', '连接成功')
        : `${resolve(msg('model.test_failed', '连接失败'))}${result && result.detail ? `：${result.detail}` : ''}`,
        {durationMs: payload.ok ? undefined : 5000});
    } catch (error) {
      toast(error && error.message ? error.message : resolve(msg('model.test_failed', '连接失败')), {durationMs: 5000});
    } finally {
      action.disabled = false;
    }
  }

  function render() {
    const provider = decisionProvider(decision().provider);
    const hasKey = Boolean(decision().has_api_key);
    const testButton = el('button', {
      class: 'ds-card-action',
      attrs: {type: 'button'},
      data: {action: 'test-decision-connection'},
      on: {click: () => testConnection(testButton)},
    }, [text(msg('model.test', '测试连接'))]);
    replace(body, [
      group({rows: [row({
        label: msg('model.decision_provider', '提供商'),
        value: provider.label,
        chevron: true,
        action: 'open-decision-provider',
        onPress: () => choiceSheet({
          options: DECISION_PROVIDERS.map(entry => ({value: entry.value, label: entry.label})),
          selected: provider.value,
          onSelect: chooseProvider,
        }),
      })]}),
      group({
        rows: [apiKeyField()],
        note: msg('model.decision_key_help', '填写 API Key 后即开启：固定流程里的判断（如输入法状态、App 是否打开）同时问决策模型和主模型，决策模型有把握时直接采用，否则用主模型的结果。'),
      }),
      hasKey ? el('div', {class: 'ds-group'}, [testButton]) : null,
      hasKey ? group({rows: [row({
        label: msg('model.decision_remove_key', '移除 API Key'),
        labelTone: 'danger',
        action: 'remove-decision-key',
        onPress: removeKey,
      })]}) : null,
      group({
        rows: [inlineValueRow({
          key: 'decision.min_confidence',
          label: msg('model.decision_min_confidence', '最低置信度'),
          description: msg('model.decision_min_confidence_help', '低于该值的判断交回主模型；留空使用默认值 0.8'),
          parse: parse.optionalRange(0, 1),
          invalid: msg('model.decision_min_confidence_invalid', '请输入 0 到 1 之间的数，或留空'),
          saved: () => (decision().min_confidence == null ? '' : String(decision().min_confidence)),
          patch: value => ({decision: {min_confidence: value}}),
          saver,
        })],
      }),
    ]);
    saver.refreshChrome();
  }

  render();
  return screen([body]);
}

export {EFFORT_LABELS, effortChoices};
