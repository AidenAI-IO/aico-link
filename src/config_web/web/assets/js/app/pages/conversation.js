/**
 * Conversation & tools routes.
 *
 *   /conversation                            prompt entry, loop and context limits, tools, reset
 *   /conversation/prompt                     the supplementary prompt
 *   /conversation/tools                      Web search: provider and API key
 *   /conversation/tools/provider             Web search provider choice
 *
 * Every setting here is applied live by the Agent, so each one saves as soon as
 * it is committed and no page shows a save button; the shared saver still asks
 * the Agent first, so a setting that ever needs a restart is staged instead.
 *
 * The design also draws a "主指令" field that replaces the built-in Agent
 * instruction. The Agent deliberately does not accept that (`Config.Instruction`
 * is not read from the file and `agent.instruction` is a retired request field),
 * so only the supplementary prompt is offered.
 */

import {adoptSaved, ensureSnapshot, request} from '../data.js';
import {inlineValueRow, parse} from '../fields.js';
import {createSaver} from '../saver.js';
import {confirmSheet} from '../../ui/confirm.js';
import {el, replace} from '../../ui/dom.js';
import {group, row, screen} from '../../ui/list.js';
import {icon, searchTile} from '../../ui/icon.js';
import {msg, resolve, text} from '../../ui/text.js';
import {toast} from '../../ui/toast.js';

export const SEARCH_PROVIDERS = [
  {value: 'duckduckgo', label: 'DuckDuckGo', logo: '/assets/icons/search/duckduckgo.png', needsKey: false},
  {value: 'brave', label: 'Brave', logo: '/assets/icons/search/brave.png', needsKey: true},
  {value: 'tavily', label: 'Tavily', logo: '/assets/icons/search/tavily.png', needsKey: true},
];

/**
 * The numeric limits, with the value that means "use the default" spelled
 * out, because the raw numbers (-1, 0) are not self-explanatory. Ranges match
 * `Config.Validate` in src/agent/internal/agent/config.go.
 */
export const LIMIT_FIELDS = [
  {
    key: 'max_iterations',
    label: msg('conversation.max_iterations', '最大工具调用轮数'),
    description: msg('conversation.max_iterations_help', '单个任务中模型与工具的最大循环次数，-1 表示不限'),
    inputmode: 'numeric',
    parse: parse.integer(-1),
    invalid: msg('conversation.max_iterations_invalid', '请输入 -1 或正整数'),
  },
  {
    key: 'context_prune_threshold',
    label: msg('conversation.prune_threshold', '历史消息裁剪阈值'),
    description: msg('conversation.prune_threshold_help', '上下文占用达到该比例时清理旧状态并压缩较早的工具结果，0 表示自动（0.5）'),
    inputmode: 'decimal',
    parse: parse.fraction,
    invalid: msg('conversation.fraction_invalid', '请输入 0，或 0 到 1 之间的小数'),
  },
  {
    key: 'context_prune_state_limit',
    label: msg('conversation.prune_state_limit', '截图状态上限'),
    description: msg('conversation.prune_state_limit_help', '带截图的状态超过该数量时清理旧状态，只保留当前这一轮的最新状态，0 表示自动（6）'),
    inputmode: 'numeric',
    parse: parse.integer(0),
    invalid: msg('conversation.prune_state_limit_invalid', '请输入 0 或正整数'),
  },
  {
    key: 'context_compaction_threshold',
    label: msg('conversation.compaction_threshold', '上下文压缩阈值'),
    description: msg('conversation.compaction_threshold_help', '上下文占用达到该比例时总结较早的对话，0 表示自动（0.8）'),
    inputmode: 'decimal',
    parse: parse.fraction,
    invalid: msg('conversation.fraction_invalid', '请输入 0，或 0 到 1 之间的小数'),
  },
];

function agentValue(snapshot, key) {
  return snapshot?.config?.agent?.[key];
}

function limitRow(field, snapshot, saver) {
  return inlineValueRow({
    ...field,
    key: `agent.${field.key}`,
    saved: () => String(agentValue(snapshot, field.key) ?? ''),
    patch: value => ({agent: {[field.key]: value}}),
    saver,
  });
}

function badge(name) {
  return icon(name, {class: 'ds-row__badge'});
}

async function resetConversation() {
  const confirmed = await confirmSheet({
    title: msg('conversation.reset', '重置当前对话'),
    body: msg('conversation.reset_confirm', '确定重置当前对话吗？对话历史会被清空，记忆会保留。'),
    confirmLabel: msg('conversation.reset_action', '重置'),
    danger: true,
    action: 'confirm-conversation-reset',
  });
  if (!confirmed) return;
  try {
    await request('/api/conversation/reset', {method: 'POST'});
    toast(msg('conversation.reset_done', '当前对话已重置'));
  } catch (error) {
    toast(error && error.message ? error.message : resolve(msg('conversation.reset_failed', '重置失败')));
  }
}

/** Conversation & tools home. */
export async function conversationPage(context) {
  const snapshot = await ensureSnapshot(context);
  const body = el('div');
  const saver = createSaver(context, {
    title: msg('ui.nav_conversation', '对话与工具'),
    back: '/',
    root: body,
    onSaved: adoptSaved(snapshot),
  });
  const prompt = String(agentValue(snapshot, 'prompt') || '').trim();
  replace(body, [
    group({
      rows: [
        row({
          label: msg('conversation.prompt', '自定义提示词'),
          value: prompt ? msg('conversation.prompt_custom', '已设置') : msg('conversation.prompt_default', '默认'),
          chevron: true,
          action: 'open-prompt',
          onPress: () => context.navigate('/conversation/prompt'),
        }),
        ...LIMIT_FIELDS.map(field => limitRow(field, snapshot, saver)),
      ],
    }),
    group({
      rows: [
        row({
          label: msg('conversation.tools', '工具设置'),
          leading: badge('gearBadge'),
          chevron: true,
          action: 'open-tools',
          onPress: () => context.navigate('/conversation/tools'),
        }),
        row({
          label: msg('conversation.reset', '重置当前对话'),
          labelTone: 'danger',
          leading: badge('alertBadge'),
          action: 'reset-conversation',
          onPress: resetConversation,
        }),
      ],
    }),
  ]);
  saver.refreshChrome();
  return screen([body]);
}

/** The supplementary prompt, appended after the built-in instruction. */
export async function promptPage(context) {
  const snapshot = await ensureSnapshot(context);
  const body = el('div');
  const saver = createSaver(context, {
    title: msg('conversation.prompt', '自定义提示词'),
    back: '/conversation',
    root: body,
    onSaved: adoptSaved(snapshot),
  });
  const saved = () => String(agentValue(snapshot, 'prompt') || '');
  const textarea = el('textarea', {
    class: 'ds-textarea',
    attrs: {
      rows: '6',
      placeholder: resolve(msg('conversation.prompt_placeholder', '例如：回答保持简洁；操作前先确认当前页面')),
      'aria-label': resolve(msg('conversation.prompt_extra', '附加指令')),
    },
    data: {action: 'edit-prompt'},
  });
  textarea.value = saved();
  // Saved when the field loses focus, which on a phone is when the keyboard closes.
  textarea.addEventListener('change', async () => {
    if (textarea.value === saved()) return;
    const outcome = await saver.change('agent.prompt', {agent: {prompt: textarea.value}});
    if (outcome === 'invalid' || outcome === 'failed') textarea.value = saved();
  });
  replace(body, [
    group({
      rows: [
        el('div', {class: 'ds-form-card'}, [
          text(msg('conversation.prompt_extra', '附加指令'), 'ds-form-card__title'),
          text(msg('conversation.prompt_extra_help', '追加在内置主指令之后，适合填写设备、项目或环境专用要求'), 'ds-form-card__help'),
          textarea,
        ]),
      ],
    }),
  ]);
  saver.refreshChrome();
  return screen([body]);
}

function providerLogo(provider) {
  return el('img', {class: 'ds-row__logo', attrs: {src: provider.logo, alt: '', width: '32', height: '32'}});
}

function providerFor(value) {
  return SEARCH_PROVIDERS.find(provider => provider.value === value) || SEARCH_PROVIDERS[0];
}

/**
 * Web search: provider and API key. It is the only configurable tool today, so
 * "工具设置" opens it directly, as the design draws it.
 *
 * Brave and Tavily cannot be saved without a key, so choosing one of them when
 * no key is stored lands here with `?provider=` set: the choice is shown but
 * not saved, and entering the key saves both together.
 */
export async function websearchPage(context) {
  const snapshot = await ensureSnapshot(context);
  const body = el('div');
  const saver = createSaver(context, {
    title: msg('websearch.title', 'Web 搜索'),
    back: '/conversation',
    root: body,
    onSaved: adoptSaved(snapshot),
  });
  const search = () => snapshot?.config?.search || {};
  const pendingValue = context.query && context.query.provider;
  const pending = pendingValue && pendingValue !== search().provider ? providerFor(pendingValue) : null;

  function render() {
    const provider = pending || providerFor(search().provider);
    const rows = [
      el('div', {class: 'page__intro page__intro--in-card'}, [
        searchTile(),
        text(msg('websearch.title', 'Web 搜索'), 'page__title'),
        text(
          msg('websearch.description', '连接后，Aico 可实现消息推送、信息捕捉，还能学习你的操作方式，借助自动化能力简化重复任务。'),
          'page__description',
        ),
      ]),
      el('div', {class: 'ds-form-card'}, [
        text(msg('websearch.provider', '服务商'), 'ds-form-card__title'),
        row({
          label: provider.label,
          leading: providerLogo(provider),
          chevron: true,
          extraClass: 'ds-row--outlined',
          action: 'open-search-provider',
          onPress: () => context.navigate('/conversation/tools/provider'),
        }),
      ]),
    ];
    if (provider.needsKey) rows.push(apiKeyField(provider));
    replace(body, [group({rows, joined: true})]);
    saver.refreshChrome();
  }

  function apiKeyField(provider) {
    const hasKey = !pending && search().has_api_key;
    const input = el('input', {
      class: 'ds-input',
      attrs: {
        type: 'password',
        autocomplete: 'off',
        autocapitalize: 'none',
        spellcheck: 'false',
        placeholder: resolve(hasKey
          ? msg('websearch.api_key_saved', '已保存，留空保持不变')
          : msg('websearch.api_key_placeholder', '请输入')),
        'aria-label': resolve(msg('websearch.api_key', '搜索 API 密钥')),
      },
      data: {action: 'edit-search-api-key'},
    });
    input.addEventListener('keydown', event => {
      if (event.key === 'Enter') input.blur();
    });
    input.addEventListener('change', async () => {
      const key = input.value.trim();
      if (!key) return;
      const patch = {search: {api_key: key}};
      if (pending) patch.search.provider = provider.value;
      const outcome = await saver.change('search', patch);
      if (outcome === 'saved') {
        input.value = '';
        if (pending) context.navigate('/conversation/tools', {replace: true});
        else render();
      }
    });
    return el('div', {class: 'ds-form-card'}, [
      text(msg('websearch.api_key', '搜索 API 密钥'), 'ds-form-card__title'),
      pending ? text(msg('websearch.api_key_required', '填写 API 密钥后才会切换到该服务商'), 'ds-form-card__help') : null,
      input,
    ]);
  }

  render();
  return screen([body]);
}

/** Provider choice. A provider that can be saved as is saves immediately. */
export async function searchProviderPage(context) {
  const snapshot = await ensureSnapshot(context);
  const body = el('div');
  const saver = createSaver(context, {
    title: msg('websearch.provider', '服务商'),
    back: '/conversation/tools',
    root: body,
    onSaved: adoptSaved(snapshot),
  });
  const current = providerFor(snapshot?.config?.search?.provider).value;

  async function choose(provider) {
    if (provider.value === current) {
      context.navigate('/conversation/tools', {replace: true});
      return;
    }
    const outcome = await saver.change('search.provider', {search: {provider: provider.value}}, {quietInvalid: provider.needsKey});
    if (outcome === 'saved' || outcome === 'staged') {
      context.navigate('/conversation/tools', {replace: true});
    } else if (outcome === 'invalid' && provider.needsKey) {
      // Rejected before anything was written: it needs a key first.
      context.navigate(`/conversation/tools?provider=${encodeURIComponent(provider.value)}`, {replace: true});
    }
  }

  replace(body, [
    group({
      rows: SEARCH_PROVIDERS.map(provider => row({
        label: provider.label,
        leading: providerLogo(provider),
        action: `choose-search-${provider.value}`,
        onPress: () => choose(provider),
        accessory: provider.value === current ? icon('check', {class: 'ds-choice-row__check', size: 22}) : null,
      })),
    }),
  ]);
  saver.refreshChrome();
  return screen([body]);
}
