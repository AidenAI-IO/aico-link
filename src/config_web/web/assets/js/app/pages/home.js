/**
 * Settings home.
 *
 * On a phone this is the whole screen: the settings list, exactly as the design
 * draws it. On the desktop layout the same list is already visible as the left
 * navigation rail, so this copy is hidden and the right-hand panel stays empty
 * until an entry is chosen.
 */

import { settingsList, updateWiFiStatus } from '../settings-list.js';
import { el } from '../../ui/dom.js';

/**
 * @param {{snapshot: object|null, navigate: Function}} context
 * @returns {Node}
 */
export function homePage(context) {
  const host = el('div', {}, [settingsList(context.snapshot || null, context.navigate)]);
  context.onWiFiChange?.((snapshot) => updateWiFiStatus(host, snapshot));
  return host;
}
