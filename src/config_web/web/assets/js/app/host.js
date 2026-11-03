/**
 * Messages to the companion app hosting this page in a WebView.
 *
 * The app draws the navigation bar natively, so a page reports what that bar
 * should show — title, whether there is something to save, and what saving
 * will need — and the app sends `aico:save-request` back when its save
 * button is tapped. A standalone browser has no bridge and ignores all of it.
 *
 * Message types:
 *   - `aico_config_save_state` {title, dirty, apply, action_label}; `title`
 *     and `action_label` arrive translated into the device's language
 *   - `aico_config_restart` {apply, phase: 'started' | 'finished' | 'failed'}
 */

/**
 * @param {object} message
 * @param {Window} [win] - injectable for tests.
 */
export function postToHost(message, win = typeof window === 'undefined' ? null : window) {
  try {
    if (!win?.ReactNativeWebView) return false;
    win.ReactNativeWebView.postMessage(JSON.stringify(message));
    return true;
  } catch (error) {
    // A host that rejects the message must not break the page.
    console.warn('[settings] host message failed:', error && error.message);
    return false;
  }
}
