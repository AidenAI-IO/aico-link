/**
 * Runtime environment detection.
 *
 * The same document is served to a standalone browser and to the WebView embedded
 * in the companion app, and the two need different chrome: a standalone browser
 * has no app around it, so the frame carries the Aico Link wordmark, while the
 * embedded case must not repeat branding the native screen already shows.
 *
 * Detection is deliberately multi-signal, because the most obvious signal is not
 * a reliable one:
 *
 *   - `window.ReactNativeWebView` is injected by react-native-webview only when
 *     the host passes an `onMessage` handler (`messagingEnabled` is derived from
 *     it, and both the iOS `postMessageScript` and the Android
 *     `addJavascriptInterface` call are conditional on it). The companion app
 *     passes `onMessage` for its terminal screen but not for settings, so this
 *     signal is evidence, not a guarantee.
 *   - The iOS history shim message handler is registered unconditionally by that
 *     same library and is not removed when messaging is disabled, which makes it
 *     the signal that actually fires on iOS settings.
 *   - An explicit opt-in parameter and a host user-agent token are the two
 *     deterministic options a host can adopt without depending on library
 *     internals.
 *
 * Detection is import-safe and has no side effects; call
 * `applyEnvironmentClass` once at boot when a stylesheet needs the flag.
 */

/** Name of the always-registered react-native-webview iOS history shim handler. */
const IOS_HISTORY_SHIM = 'ReactNativeHistoryShim';
/** The bridge object's own script-message-handler name. */
const IOS_BRIDGE_HANDLER = 'ReactNativeWebView';
/** Android WebView puts this token in its user agent; Chrome does not. */
const ANDROID_WEBVIEW_TOKEN = /;\s*wv\)/;

/**
 * True when the document is running inside the companion app's WebView.
 *
 * @param {Window} [win] - injectable for tests.
 */
export function isEmbedded(win = typeof window === 'undefined' ? null : window) {
  if (!win) return false;

  // 1. The bridge object, present when the host enabled messaging.
  if (win.ReactNativeWebView) return true;

  // 2. Generic WKWebView message handlers. `ReactNativeHistoryShim` is
  //    registered unconditionally by react-native-webview, so this covers the
  //    case where the host passed no `onMessage` and signal 1 is absent.
  const handlers = win.webkit && win.webkit.messageHandlers;
  if (handlers && (handlers[IOS_HISTORY_SHIM] || handlers[IOS_BRIDGE_HANDLER])) return true;

  // 3. An explicit opt-in the host controls.
  try {
    if (new URLSearchParams(win.location.search).get('webview') === 'true') return true;
  } catch (error) {
    // An unusable query string is not evidence of embedding.
    console.warn('[settings] could not read the query string:', error && error.message);
  }

  const agent = (win.navigator && win.navigator.userAgent) || '';
  // 4. The host's own user-agent token.
  if (/AicoApp/i.test(agent)) return true;
  // 5. Android marks its WebView in the user agent; Chrome and Safari do not.
  return ANDROID_WEBVIEW_TOKEN.test(agent);
}

/**
 * Tag the document with the resolved environment.
 *
 * @param {HTMLElement} [root] - element to tag, defaults to `<html>`.
 * @returns {'is-embedded'|'is-standalone'}
 */
export function applyEnvironmentClass(root = document.documentElement) {
  const embedded = isEmbedded();
  root.classList.add(embedded ? 'is-embedded' : 'is-standalone');
  root.classList.remove(embedded ? 'is-standalone' : 'is-embedded');
  return embedded ? 'is-embedded' : 'is-standalone';
}
