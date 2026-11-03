/**
 * The standalone browser's top bar.
 *
 * One bar serves every route. At `/` it carries only the wordmark; on a sub-page
 * it also carries the page's navigation: a back button, the page title, and an
 * optional trailing action such as a save button. On a phone-width browser the
 * sub-page navigation replaces the wordmark, the way a native navigation bar
 * replaces the app title; above the desktop breakpoint the wordmark stays over
 * the rail and the navigation sits over the panel.
 *
 * The embedded case renders no bar at all: the companion app's native screen
 * already draws the back button and title, and receives the save state through
 * the WebView bridge instead. Pages therefore describe their navigation through
 * `set()` without caring which environment they run in.
 */

import {el, replace} from '../ui/dom.js';
import {icon} from '../ui/icon.js';
import {aicoLinkLogo} from '../ui/logo.js';
import {msg, resolve, text} from '../ui/text.js';

/**
 * @typedef {object} HeaderSpec
 * @property {import('../ui/text.js').TextSpec} [title]
 * @property {() => void} [onBack] - omitted on the settings home.
 * @property {Node|null} [action] - trailing control, e.g. a save button.
 */

/**
 * Build the bar.
 *
 * @param {object} [options]
 * @param {boolean} [options.embedded] - render nothing, and make `set()` inert.
 * @returns {{element: HTMLElement|null, set: (spec: HeaderSpec|null) => void}}
 */
export function createHeader(options = {}) {
  if (options.embedded) return {element: null, set: () => {}};

  const nav = el('div', {class: 'app__topbar-nav'});
  const element = el('div', {class: 'app__topbar'}, [
    el('div', {class: 'app__brand'}, [
      el('a', {
        class: 'app__brand-link',
        attrs: {href: 'https://aico.link', target: '_blank', rel: 'noopener noreferrer', 'aria-label': 'Aico Link — aico.link'},
      }, [aicoLinkLogo({width: 116})]),
      text({key: 'ui.app_title', fallback: '配置'}, 'app__brand-title'),
    ]),
    nav,
  ]);

  function set(spec) {
    const sub = Boolean(spec && spec.onBack);
    element.classList.toggle('app__topbar--sub', sub);
    if (!spec) {
      replace(nav, []);
      return;
    }
    replace(nav, [
      el('div', {class: 'app__topbar-row'}, [
        spec.onBack
          ? el(
              'button',
              {
                class: 'app__back',
                attrs: {type: 'button', 'aria-label': resolve(msg('action.back', '返回'))},
                data: {action: 'back-settings'},
                on: {click: spec.onBack},
              },
              [icon('chevron', {class: 'app__back-icon'})],
            )
          : el('span'),
        spec.title ? text(spec.title, 'app__topbar-heading') : el('span'),
        el('div', {class: 'app__topbar-action'}, spec.action ? [spec.action] : []),
      ]),
    ]);
  }

  return {element, set};
}
