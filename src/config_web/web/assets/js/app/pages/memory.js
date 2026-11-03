/**
 * Memory route: how long captured memories are kept, and the full reset.
 *
 * Both retention settings apply live, so they save as soon as one is chosen
 * and the page has no save button. The reset clears conversation history and
 * persisted memory and restarts the Agent (`POST /api/memory/reset`); it is
 * confirmed first because it cannot be undone.
 */

import {adoptSaved, ensureSnapshot, request, t} from '../data.js';
import {createSaver} from '../saver.js';
import {choiceSheet} from '../../ui/choice-sheet.js';
import {confirmSheet} from '../../ui/confirm.js';
import {el, replace} from '../../ui/dom.js';
import {icon} from '../../ui/icon.js';
import {group, row, screen} from '../../ui/list.js';
import {msg, resolve, text} from '../../ui/text.js';
import {setChecked, switchControl} from '../../ui/switch.js';
import {toast} from '../../ui/toast.js';

/**
 * Screen Memory retention presets. The Agent stores a duration such as `90d`
 * or `forever` (`quick_capture.screen_memory_ttl`); a saved value outside the
 * presets is still listed so it can be seen and kept.
 */
export const SCREEN_TTL_PRESETS = ['7d', '30d', '90d', '180d', '365d', 'forever'];
/** Notification memory retention presets, in days (`voice_notifications.retention_days`). */
export const NOTIFICATION_DAY_PRESETS = [3, 7, 14, 30, 60, 90];

// `t` interpolates `{{n}}` itself; a resolved spec would already have blanked it.
function daysLabel(days) {
  return t('memory.days', {n: days, defaultValue: '{{n}} 天'});
}

/** `90d` -> "90 天", `forever` -> "永久"; anything else is shown as stored. */
export function ttlLabel(value) {
  const text = String(value || '').trim().toLowerCase();
  if (text === 'forever') return resolve(msg('memory.forever', '永久'));
  const days = /^(\d+)d$/.exec(text);
  return days ? daysLabel(Number(days[1])) : String(value || '');
}

function withCurrent(presets, current) {
  return current !== undefined && current !== '' && !presets.includes(current) ? [current, ...presets] : presets;
}

/** A card whose footnote follows it directly. */
function groupNoted(props) {
  const card = group(props);
  card.classList.add('ds-group--noted');
  return card;
}

export async function memoryPage(context) {
  const snapshot = await ensureSnapshot(context);
  const body = el('div');
  const saver = createSaver(context, {
    title: msg('ui.nav_memory', '记忆'),
    back: '/',
    root: body,
    onSaved: adoptSaved(snapshot, () => render()),
  });
  const config = () => snapshot.config || {};

  async function reset() {
    const confirmed = await confirmSheet({
      title: msg('memory.reset_confirm_title', '确认重置'),
      body: msg('memory.reset_confirm_body', '此操作将删除所有对话历史和持久化记忆，并重启 Agent。此操作不可撤销。'),
      confirmLabel: msg('memory.reset', '重置对话与记忆'),
      danger: true,
      action: 'confirm-memory-reset',
    });
    if (!confirmed) return;
    try {
      await request('/api/memory/reset', {method: 'POST'});
      toast(msg('memory.reset_done', '已重置，Agent 正在重启'));
    } catch (error) {
      toast(error && error.message ? error.message : resolve(msg('memory.reset_failed', '重置失败')));
    }
  }

  function render() {
    const ttl = (config().quick_capture || {}).screen_memory_ttl || '90d';
    // Off stops the capture button from saving Screen Memory; the retention
    // row stays, since existing entries still expire on that schedule.
    const captureOn = (config().quick_capture || {}).enabled !== false;
    const captureToggle = switchControl({
      checked: captureOn,
      label: msg('memory.screen_enabled', '屏幕记忆'),
      onChange: async next => {
        const outcome = await saver.change('quick_capture.enabled', {quick_capture: {enabled: next}});
        if (outcome === 'invalid' || outcome === 'failed') setChecked(captureToggle, !next);
      },
    });
    const days = (config().voice_notifications || {}).retention_days;
    replace(body, [
      el('div', {}, [
        groupNoted({rows: [row({label: msg('memory.screen_enabled', '屏幕记忆'), accessory: captureToggle})]}),
        text(msg('memory.screen_enabled_help', '开启后，按下设备上的截屏按键会保存当前屏幕内容，供 Aico 之后回忆。'), 'page__note page__note--below'),
      ]),
      group({
        rows: [
          row({
            label: msg('memory.screen_ttl', '屏幕记忆保留期'),
            value: ttlLabel(ttl),
            action: 'open-screen-memory-ttl',
            onPress: () => choiceSheet({
              options: withCurrent(SCREEN_TTL_PRESETS, ttl).map(value => ({value, label: ttlLabel(value)})),
              selected: ttl,
              onSelect: value => saver.change('quick_capture.screen_memory_ttl', {quick_capture: {screen_memory_ttl: value}}),
            }),
          }),
          row({
            label: msg('memory.notification_days', '通知记忆保留天数'),
            value: days == null ? '' : daysLabel(days),
            action: 'open-notification-retention',
            onPress: () => choiceSheet({
              options: withCurrent(NOTIFICATION_DAY_PRESETS, days).map(value => ({value, label: daysLabel(value)})),
              selected: days,
              onSelect: value => saver.change('voice_notifications.retention_days', {voice_notifications: {retention_days: value}}),
            }),
          }),
        ],
      }),
      el('div', {}, [
        groupNoted({
          rows: [row({
            label: msg('memory.reset', '重置对话与记忆'),
            labelTone: 'danger',
            leading: icon('alertBadge', {class: 'ds-row__badge'}),
            action: 'reset-memory',
            onPress: reset,
          })],
        }),
        text(msg('memory.reset_help', '删除对话历史和所有持久化记忆，然后重启 Agent 并开始新的对话。'), 'page__note page__note--below'),
      ]),
    ]);
    saver.refreshChrome();
  }

  render();
  return screen([body]);
}
