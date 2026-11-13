/**
 * Application shell.
 *
 * The settings list is the navigation. On a phone it is the whole screen at `/`,
 * because the native screen hosting this WebView supplies the back affordance;
 * above the desktop breakpoint the same list becomes the left rail and stays in
 * place while the right-hand panel shows the chosen sub-page. At `/` on desktop
 * the panel is deliberately empty, which is what makes the two-column reading
 * work: the left column is the list, the right column is the detail.
 *
 * Both copies of the list are rendered from `settings-list.js`; CSS decides
 * which one is visible, so resizing never needs a re-render.
 */

import {applyServerLocale, initI18n, loadSnapshot} from './data.js';
import {createHeader} from './header.js';
import {ALL_ITEMS} from './nav-items.js';
import {createRouter} from './router.js';
import {localApplicationStatus} from './pages/local.js';
import {localRoutes, routes} from './routes.js';
import {settingsList} from './settings-list.js';
import {el, replace} from '../ui/dom.js';
import {applyEnvironmentClass, isEmbedded} from '../ui/environment.js';

/** The route one level up: `/basic/language` → `/basic`, `/wifi` → `/`. */
export function parentPath(path) {
  const trimmed = path.replace(/\/+$/, '');
  const cut = trimmed.lastIndexOf('/');
  return cut > 0 ? trimmed.slice(0, cut) : '/';
}

/** Default bar for a route; a page refines it through `context.header`. */
export function defaultHeader(path, navigate) {
  if (path === '/') return null;
  const item = ALL_ITEMS.find(entry => entry.route === path.replace(/\/+$/, ''));
  return {title: item ? item.label : null, onBack: () => navigate(parentPath(path))};
}

/**
 * Mount the settings application.
 *
 * @param {string} [rootId] - id of the host element.
 * @returns {Promise<object>} the router, for tests and debugging.
 */
export async function boot(rootId = 'app', options = {}) {
  const root = document.getElementById(rootId);
  if (!root) throw new Error(`settings shell: #${rootId} not found`);

  applyEnvironmentClass();
  // The embedded case is wrapped by a native screen that already shows the
  // product chrome and the page's back/title/save, so no top bar is built.
  const header = createHeader({embedded: isEmbedded(), localAgent: options.localAgent});
  /** Bumped per navigation, so a page that settles late cannot repaint the bar. */
  let generation = 0;

  const view = el('div', {class: 'app__scroll', data: {region: 'content'}});
  const railBody = el('div', {class: 'app__nav-body'});
  const navHost = el('div', {class: 'app__nav'}, [railBody]);

  let navigate = () => {};
  const renderRail = (snapshot, currentPath) =>
    replace(railBody, [settingsList(snapshot, navigate, currentPath, options.localAgent)]);

  const router = createRouter({
    routes: options.localAgent ? localRoutes : routes,
    render: async context => {
      navigate = context.navigate;
      const current = ++generation;
      /*
       * The bar keeps showing the previous page until the new one is mounted and
       * then switches together with the content. Applying the route default up
       * front blanked the title for the whole snapshot fetch on routes whose
       * title only the page knows, such as `/basic/language`.
       */
      let pending = defaultHeader(context.path, context.navigate);
      let mounted = false;
      const setHeader = spec => {
        if (current !== generation) return;
        if (mounted) header.set(spec);
        else pending = spec;
      };
      // One snapshot per navigation feeds the rail and the page, so the status
      // accessories in the two copies cannot disagree.
      const snapshot = await loadSnapshot();
      // The device's configured language wins over the browser's stored choice.
      applyServerLocale(snapshot);

      view.className = context.name === 'home' ? 'app__scroll route-home' : 'app__scroll';
      const content = await context.page({...context, snapshot, localAgent: options.localAgent, header: setHeader});
      if (current !== generation) return;
      replace(view, [content]);
      mounted = true;
      header.set(pending);
      renderRail(snapshot, context.path);

      // Re-run the catalogue over the freshly rendered subtree so a language
      // change applies without every page having to re-render itself.
      initI18n();
    },
  });

  navigate = router.navigate;
  /*
   * The bar sits in the frame rather than inside the navigation rail: the rail is
   * hidden below the desktop breakpoint, and with the bar in the frame both
   * columns start at the same height.
   */
  root.appendChild(
    el('div', {class: 'app'}, [
      header.element,
      el('div', {class: 'app__body'}, [navHost, el('div', {class: 'app__panel'}, [options.localAgent ? localApplicationStatus() : null, view])]),
    ]),
  );

  await router.start();
  return router;
}
