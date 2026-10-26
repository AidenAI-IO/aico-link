/**
 * The settings entries, in the order the design lists them.
 *
 * One definition drives both surfaces: the mobile home screen renders it as
 * inset cards, and the desktop navigation rail renders the same entries as a
 * side list, so the two can never disagree about what settings exist.
 *
 * `status` marks the entries that carry a live value on the right-hand side.
 */

import {msg} from '../ui/text.js';

/** Entries shown in the first card group, per the design. */
export const PRIMARY_ITEMS = [
  {id: 'wifi', label: 'Wi-Fi', route: '/wifi', status: 'wifi'},
  {id: 'basic', label: msg('ui.nav_basic', '基础设置'), route: '/basic'},
  {id: 'conversation', label: msg('ui.nav_conversation', '对话与工具'), route: '/conversation'},
  {id: 'model', label: msg('ui.nav_model', '模型设置'), route: '/model'},
  {id: 'voice', label: msg('ui.nav_voice', '语音'), route: '/voice', status: 'voice'},
];

/** Entries shown in the second card group, per the design. */
export const SECONDARY_ITEMS = [
  {id: 'memory', label: msg('ui.nav_memory', '记忆'), route: '/memory'},
  {id: 'storage', label: msg('ui.nav_storage', '存储与备份'), route: '/storage', status: 'storage'},
  {id: 'advanced', label: msg('ui.nav_advanced', '高级设置'), route: '/advanced'},
  {id: 'firmware', label: msg('ui.nav_firmware', '固件更新'), route: '/firmware', status: 'firmware'},
];

/** Every entry, in presentation order. */
export const ALL_ITEMS = [...PRIMARY_ITEMS, ...SECONDARY_ITEMS];
