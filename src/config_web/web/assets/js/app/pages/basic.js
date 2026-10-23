/** Basic settings routes. */

import {fetchSnapshot, request} from '../data.js';
import {postToHost} from '../host.js';
import {saveLabel} from '../apply.js';
import {createSaver} from '../saver.js';
import {applyLocale} from '../../config/i18n.js';
import {el, replace} from '../../ui/dom.js';
import {choiceSheet, localeOptions, optionLabel} from '../../ui/choice-sheet.js';
import {dropdown} from '../../ui/dropdown.js';
import {group, row, screen, separator} from '../../ui/list.js';
import {icon, languageTile} from '../../ui/icon.js';
import {msg, resolve, text} from '../../ui/text.js';
import {toast} from '../../ui/toast.js';

const DEVICE_OPTIONS = [
  {value: 'iOS', label: 'iOS', icon: 'apple'},
  {value: 'Android', label: 'Android', icon: 'android'},
  {value: 'macOS', label: 'macOS', icon: 'monitor'},
  {value: 'windows', label: 'Windows', icon: 'windows'},
  {value: 'linux', label: 'Linux', icon: 'terminal'},
];

const KEYBOARD_OPTIONS = [
  {value: 'qwerty', label: 'QWERTY'},
  {value: 'azerty', label: 'AZERTY'},
  {value: 'qwertz', label: 'QWERTZ'},
];

/**
 * The Agent's supported time zones (`supportedTimezones` in
 * src/agent/internal/agent/timezone.go), used only while the schema is
 * unreachable. The schema's enum is the authority.
 */
const TIMEZONE_FALLBACKS = [
  'Africa/Johannesburg', 'America/Chicago', 'America/Denver', 'America/Los_Angeles',
  'America/New_York', 'America/Sao_Paulo', 'America/Toronto', 'America/Vancouver',
  'Asia/Bangkok', 'Asia/Dubai', 'Asia/Hong_Kong', 'Asia/Jakarta', 'Asia/Kolkata',
  'Asia/Kuala_Lumpur', 'Asia/Seoul', 'Asia/Shanghai', 'Asia/Singapore', 'Asia/Taipei',
  'Asia/Tokyo', 'Australia/Sydney', 'Europe/Amsterdam', 'Europe/Berlin', 'Europe/London',
  'Europe/Moscow', 'Europe/Paris', 'Pacific/Auckland', 'UTC',
];

/** The Agent's own default when `timezone` is unset (`defaultTimezone`). */
const DEFAULT_TIMEZONE = 'UTC';

/** Mirror the top bar to the companion app's native navigation bar. */
function notifyHost(title, dirty) {
  postToHost({type: 'aico_config_save_state', title: resolve(title), dirty, apply: 'live', action_label: resolve(saveLabel('live'))});
}

function configValue(snapshot, section, key, fallback) {
  return snapshot?.config?.[section]?.[key] || fallback;
}

function timezoneLabel(value) {
  const names = {'Asia/Hong_Kong': 'Hong Kong', 'Asia/Shanghai': 'Shanghai', UTC: 'UTC'};
  return names[value] || value.replaceAll('_', ' ');
}

/** Describe the language page to the top bar; its choices apply immediately. */
function setHeader(context, {title, back}) {
  if (context.header) context.header({title, onBack: () => context.navigate(back)});
}

function languageIntro() {
  return el('div', {class: 'basic-intro'}, [
    languageTile(),
    text(msg('basic.language_title', '语言与时区'), 'page__title'),
    text(msg('basic.language_description', '连接后，Aico 可实现消息推送、信息捕捉，还能学习你的操作方式，借助自动化能力简化重复任务。'), 'page__description'),
  ]);
}

async function saveLanguage(snapshot, locale) {
  const payload = await request('/api/config/locale', {
    method: 'PUT', headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({locale}),
  });
  applyLocale(payload.locale || locale, true);
  snapshot.config = snapshot.config || {};
  snapshot.config.agent = {...(snapshot.config.agent || {}), locale: payload.locale || locale};
  toast(msg('basic.language_saved', '语言已保存'));
}

async function saveTimezone(snapshot, timezone) {
  const payload = await request('/api/config', {
    method: 'PATCH', headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({config: {agent: {timezone}}}),
  });
  snapshot.config = payload.config || {...(snapshot.config || {}), agent: {...(snapshot.config?.agent || {}), timezone}};
  toast(msg('basic.timezone_saved', '时区已保存'));
}

/** Language and time zone detail page. Both controls apply immediately. */
export async function languagePage(context) {
  let snapshot = context.snapshot;
  if (!snapshot) snapshot = await fetchSnapshot().catch(() => ({config: {}}));
  const locale = () => configValue(snapshot, 'agent', 'locale', 'zh-CN');
  const timezone = () => configValue(snapshot, 'agent', 'timezone', DEFAULT_TIMEZONE);
  let timezoneOptions = TIMEZONE_FALLBACKS.slice();
  try {
    const schema = await request('/api/config/schema');
    const field = (schema.sections || [])
      .find(section => section.name === 'agent')?.fields?.find(entry => entry.key === 'timezone');
    if (field?.enum?.length) timezoneOptions = field.enum.map(entry => entry.value || entry);
  } catch (_error) {
    // The fallback list keeps the page usable while schema metadata is unavailable.
  }

  const body = el('div');
  const render = () => {
    setHeader(context, {title: msg('basic.language_title', '语言与时区'), back: '/basic'});
    const languageRows = localeOptions.map((option, index) => [
      index ? separator() : null,
      row({
        label: option.label,
        extraClass: 'ds-row--plain',
        action: `choose-locale-${option.value}`,
        onPress: async () => {
          try { await saveLanguage(snapshot, option.value); render(); } catch (error) { toast(error.message || resolve(msg('basic.save_failed', '保存失败'))); }
        },
        accessory: option.value === locale() ? icon('check', {class: 'ds-choice-row__check', size: 24}) : null,
      }),
    ]).flat();
    // A dropdown under its own row, as the design draws it; the grouped card
    // clips overflow, so the dropdown is its own card rather than a row in one.
    const timezoneField = dropdown({
      label: msg('basic.timezone', '时区'),
      options: timezoneOptions.map(value => ({value, label: timezoneLabel(value)})),
      selected: timezone(),
      action: 'open-timezone',
      onSelect: async value => {
        try { await saveTimezone(snapshot, value); render(); } catch (error) { toast(error.message || resolve(msg('basic.save_failed', '保存失败'))); }
      },
    });
    replace(body, [group({rows: [languageIntro(), ...languageRows]}), timezoneField]);
    notifyHost(msg('basic.language_title', '语言与时区'), false);
  };
  render();
  return screen([body]);
}

/**
 * Basic settings home. A device-type change that stays within the same pointer
 * mode saves at once; one that changes the USB gadget (to or from Android, or a
 * keyboard layout) is staged behind "保存并重启".
 */
export async function basicPage(context) {
  let snapshot = context.snapshot;
  if (!snapshot) snapshot = await fetchSnapshot().catch(() => ({config: {}}));
  const saved = () => ({
    deviceType: configValue(snapshot, 'device', 'device_type', 'iOS'),
    keyboardLayout: configValue(snapshot, 'hid', 'keyboard_layout', 'qwerty'),
  });
  // What the rows show: the saved value, or a staged one awaiting a restart.
  const draft = saved();
  const body = el('div');
  const saver = createSaver(context, {
    title: msg('ui.nav_basic', '基础设置'),
    back: '/',
    root: body,
    onSaved: payload => {
      snapshot.config = payload.config || snapshot.config;
      Object.assign(draft, saved());
      render();
    },
  });

  const choose = (key, patchFor) => async value => {
    const previous = draft[key];
    draft[key] = value;
    render();
    const outcome = await saver.change(key, patchFor(value));
    if (outcome === 'invalid' || outcome === 'failed') {
      draft[key] = previous;
      render();
    }
  };

  function render() {
    const language = row({
      label: msg('basic.language_title', '语言与时区'),
      value: optionLabel(localeOptions, configValue(snapshot, 'agent', 'locale', 'zh-CN')),
      chevron: true,
      action: 'open-language-timezone',
      onPress: () => context.navigate('/basic/language'),
    });
    const device = row({
      label: msg('basic.device_type', '设备类型'),
      value: optionLabel(DEVICE_OPTIONS, draft.deviceType),
      action: 'open-device-type',
      onPress: () => choiceSheet({
        layout: 'cards', options: DEVICE_OPTIONS, selected: draft.deviceType,
        onSelect: choose('deviceType', value => ({device: {device_type: value}})),
      }),
    });
    const keyboard = row({
      label: msg('basic.keyboard_layout', '键盘布局'),
      value: draft.keyboardLayout.toUpperCase(),
      action: 'open-keyboard-layout',
      onPress: () => choiceSheet({
        layout: 'cards', options: KEYBOARD_OPTIONS, selected: draft.keyboardLayout,
        onSelect: choose('keyboardLayout', value => ({hid: {keyboard_layout: value}})),
      }),
    });
    replace(body, [group({rows: [language]}), group({rows: [device, keyboard]})]);
    saver.refreshChrome();
  }
  render();
  return screen([body]);
}

export {DEVICE_OPTIONS, KEYBOARD_OPTIONS, TIMEZONE_FALLBACKS, timezoneLabel};
