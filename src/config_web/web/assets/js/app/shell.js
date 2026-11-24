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

import { applyServerLocale, initI18n, loadSnapshot } from './data.js';
import { createHeader } from './header.js';
import { ALL_ITEMS } from './nav-items.js';
import { createRouter } from './router.js';
import { localApplicationStatus } from './pages/local.js';
import { containerRoutes, localRoutes, routes } from './routes.js';
import { settingsList, updateWiFiStatus } from './settings-list.js';
import { watchWiFi } from './wifi-status.js';
import { el, replace } from '../ui/dom.js';
import { applyEnvironmentClass, isEmbedded } from '../ui/environment.js';

/** The route one level up: `/basic/language` → `/basic`, `/wifi` → `/`. */
export function parentPath(path) {
  const trimmed = path.replace(/\/+$/, '');
  const cut = trimmed.lastIndexOf('/');
  return cut > 0 ? trimmed.slice(0, cut) : '/';
}

/** Default bar for a route; a page refines it through `context.header`. */
export function defaultHeader(path, navigate) {
  if (path === '/') return null;
  const item = ALL_ITEMS.find((entry) => entry.route === path.replace(/\/+$/, ''));
  return { title: item ? item.label : null, onBack: () => navigate(parentPath(path)) };
}

/** Both timestamps come from the device, before optional enrichment or delivery. */
function olderWiFi(value, reference) {
  const time = value?.wifi_status?.observed_at_unix_us;
  const referenceTime = reference?.wifi_status?.observed_at_unix_us;
  return Number.isSafeInteger(time) && Number.isSafeInteger(referenceTime) && time < referenceTime;
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
  const header = createHeader({ embedded: isEmbedded(), localAgent: options.localAgent });
  /** Bumped per navigation, so a page that settles late cannot repaint the bar. */
  let generation = 0;
  let liveWiFi = null;
  let updateWiFi = () => {};
  let refreshWiFi = () => {};
  let wifiRevision = 0;
  let wifiTaskRevision = 0;
  let liveWiFiSource = 'poll';

  const view = el('div', { class: 'app__scroll', data: { region: 'content' } });
  const railBody = el('div', { class: 'app__nav-body' });
  const navHost = el('div', { class: 'app__nav' }, [railBody]);

  let navigate = () => {};
  const renderRail = (snapshot, currentPath) =>
    replace(railBody, [
      settingsList(snapshot, navigate, currentPath, options.localAgent, options.containerAgent),
    ]);

  const router = createRouter({
    routes: options.containerAgent ? containerRoutes : options.localAgent ? localRoutes : routes,
    render: async (context) => {
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
      const setHeader = (spec) => {
        if (current !== generation) return;
        if (mounted) header.set(spec);
        else pending = spec;
      };
      // One snapshot per navigation feeds the rail and the page, so the status
      // accessories in the two copies cannot disagree.
      const taskRevisionAtRead = wifiTaskRevision;
      let snapshot = await loadSnapshot();
      if (current !== generation) return;
      if (
        snapshot?.wifi_status &&
        taskRevisionAtRead === wifiTaskRevision &&
        !olderWiFi(snapshot, liveWiFi)
      ) {
        // A navigation read wins over retained polling data, but a task that
        // finished after the read started, or a later radio observation, wins.
        // Invalidate in-flight polls too; the next one starts after this read.
        liveWiFi = {
          wifi_status: snapshot.wifi_status,
          ...(snapshot.wifi ? { wifi: snapshot.wifi } : {}),
        };
        liveWiFiSource = 'snapshot';
        refreshWiFi();
      } else if (liveWiFi) snapshot = { ...snapshot, ...liveWiFi };
      const snapshotRevision = wifiRevision;
      const snapshotTaskRevision = wifiTaskRevision;
      const wifiListeners = [];
      // The device's configured language wins over the browser's stored choice.
      applyServerLocale(snapshot);

      view.className = context.name === 'home' ? 'app__scroll route-home' : 'app__scroll';
      const content = await context.page({
        ...context,
        snapshot,
        localAgent: options.localAgent,
        containerAgent: options.containerAgent,
        header: setHeader,
        onWiFiChange: (listener) => wifiListeners.push(listener),
      });
      if (current !== generation) return;
      updateWiFi = (value, { source = 'poll' } = {}) => {
        if (source === 'poll' && olderWiFi(value, snapshot)) return;
        snapshot = { ...snapshot, ...value };
        updateWiFiStatus(railBody, snapshot);
        wifiListeners.forEach((listener) => listener(snapshot));
      };
      // A status read can finish while an asynchronous page is being built.
      if (liveWiFi && wifiRevision > snapshotRevision) {
        updateWiFi(liveWiFi, {
          source: wifiTaskRevision > snapshotTaskRevision ? 'task' : liveWiFiSource,
        });
      }
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
    el('div', { class: 'app' }, [
      header.element,
      el('div', { class: 'app__body' }, [
        navHost,
        el('div', { class: 'app__panel' }, [
          options.localAgent ? localApplicationStatus() : null,
          view,
        ]),
      ]),
    ]),
  );

  await router.start();
  if (!options.localAgent) {
    const stopWiFi = watchWiFi(
      (value, { source }) => {
        if (source === 'poll' && olderWiFi(value, liveWiFi)) return;
        updateWiFi(value, { source });
      },
      {
        isActive: () => root.isConnected,
        onObservation: (value, { source }) => {
          if (source === 'poll' && olderWiFi(value, liveWiFi)) return;
          wifiRevision += 1;
          if (source === 'task') wifiTaskRevision += 1;
          liveWiFi = { ...liveWiFi, ...value };
          liveWiFiSource = source;
        },
      },
    );
    refreshWiFi = stopWiFi.refresh;
  }
  return router;
}
