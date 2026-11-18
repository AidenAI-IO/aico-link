/**
 * Settings home.
 *
 * On a phone this is the whole screen: the settings list, exactly as the design
 * draws it. On the desktop layout the same list is already visible as the left
 * navigation rail, so this copy is hidden and the right-hand panel stays empty
 * until an entry is chosen.
 */

import {settingsList} from '../settings-list.js';
import {el, replace} from '../../ui/dom.js';

/**
 * @param {{snapshot: object|null, navigate: Function}} context
 * @returns {Node}
 */
export function homePage(context) {
  const host = el('div', {}, [settingsList(context.snapshot || null, context.navigate)]);
  const status = snapshot => JSON.stringify([snapshot?.wifi_status?.connected, snapshot?.wifi_status?.state === 'UNKNOWN']);
  let state = status(context.snapshot);
  context.onWiFiChange?.(snapshot => {
    if (state === status(snapshot)) return;
    state = status(snapshot);
    replace(host, [settingsList(snapshot, context.navigate)]);
  });
  return host;
}
