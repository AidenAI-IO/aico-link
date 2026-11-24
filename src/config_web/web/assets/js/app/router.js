/**
 * History-API router.
 *
 * Routes are literal segments plus `:param` placeholders, so `/wifi/:ssid` is a
 * real URL a user can deep-link, reload, or share. The server repeats the route
 * prefix as an SPA fallback so a hard load of any sub-route still returns this
 * document.
 */

/** Compile `/wifi/:ssid` into a matcher. */
function compile(pattern) {
  const keys = [];
  const source = pattern
    .split('/')
    .map((segment) => {
      if (!segment.startsWith(':')) {
        return segment.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
      }
      keys.push(segment.slice(1));
      return '([^/]+)';
    })
    .join('/');
  return { pattern, keys, regex: new RegExp(`^${source}/?$`) };
}

/**
 * @param {object} options
 * @param {Array<{path: string, page: Function}>} options.routes - matched in order.
 * @param {(context: object) => void} options.render - receives `{route, params, query, path, navigate}`.
 */
export function createRouter(options) {
  // Spread the declared route first: `compile()` only produces the matcher, so
  // any metadata on the entry (`name`, `page`) has to be carried over explicitly.
  const compiled = options.routes.map((route) => ({ ...route, ...compile(route.path) }));
  let active = null;
  /** Set while a popstate-driven render runs, to avoid pushing a second entry. */
  let suppressPush = false;

  function match(pathname) {
    for (const route of compiled) {
      const found = route.regex.exec(pathname);
      if (!found) continue;
      const params = {};
      try {
        route.keys.forEach((key, index) => {
          params[key] = decodeURIComponent(found[index + 1]);
        });
      } catch (_malformed) {
        // A hand-edited link such as /wifi/%E0 is not a route; the caller
        // falls back to the settings list instead of leaving the page blank.
        return null;
      }
      return { route, params };
    }
    return null;
  }

  async function render(pathname, query) {
    const found = match(pathname);
    if (!found) return false;
    active = { path: pathname, params: found.params, query };
    await options.render({
      path: pathname,
      params: found.params,
      query,
      name: found.route.name,
      page: found.route.page,
      pattern: found.route.pattern,
      navigate,
    });
    return true;
  }

  /**
   * Navigate to a path.
   *
   * @param {string} path
   * @param {object} [opts]
   * @param {boolean} [opts.replace] - replace the history entry instead of pushing.
   * @param {boolean} [opts.silent] - render without touching history.
   */
  async function navigate(path, opts = {}) {
    const url = new URL(path, window.location.origin);
    if (!opts.silent) {
      if (opts.replace) {
        window.history.replaceState({ path: url.pathname }, '', url.pathname + url.search);
      } else if (url.pathname !== window.location.pathname) {
        window.history.pushState({ path: url.pathname }, '', url.pathname + url.search);
      }
    }
    const query = Object.fromEntries(url.searchParams.entries());
    const handled = await render(url.pathname, query);
    if (!handled) await render('/', {});
    return handled;
  }

  function onClick(event) {
    if (event.defaultPrevented || event.button !== 0) return;
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    const anchor = event.target.closest('a[href]');
    if (!anchor) return;
    // A download (the backup archive, an exported file) or a link meant for
    // another tab is not page navigation; intercepting it would swallow it.
    if (anchor.hasAttribute('download') || (anchor.target && anchor.target !== '_self')) return;
    const href = anchor.getAttribute('href');
    if (!href || !href.startsWith('/')) return;
    event.preventDefault();
    navigate(href);
  }

  function onPopState() {
    suppressPush = true;
    navigate(window.location.pathname + window.location.search, { silent: true }).finally(() => {
      suppressPush = false;
    });
  }

  return {
    /** Attach history and click handling, then render the current location. */
    async start() {
      document.addEventListener('click', onClick);
      window.addEventListener('popstate', onPopState);
      await navigate(window.location.pathname + window.location.search, { silent: true });
    },
    navigate,
    stop() {
      document.removeEventListener('click', onClick);
      window.removeEventListener('popstate', onPopState);
    },
    /** The route currently rendered. */
    current: () => active,
    /** True while a browser back/forward render is in flight. */
    isPopping: () => suppressPush,
  };
}
