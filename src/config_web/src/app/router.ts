import type { Route } from '../types';
export interface RouteDefinition {
  path: string;
  name?: string;
}
export function createRouter({
  routes,
  render,
}: {
  routes: RouteDefinition[];
  render: (route: Route) => void;
}) {
  let active: Route | null = null;
  function match(path: string): Route | null {
    for (const route of routes) {
      const keys: string[] = [];
      const expression = route.path
        .split('/')
        .map((part) =>
          part.startsWith(':')
            ? (keys.push(part.slice(1)), '([^/]+)')
            : part.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'),
        )
        .join('/');
      const found = new RegExp(`^${expression}/?$`).exec(path);
      if (!found) continue;
      try {
        return {
          ...route,
          path,
          pattern: route.path,
          params: Object.fromEntries(
            keys.map((key, index) => [key, decodeURIComponent(found[index + 1])]),
          ),
        };
      } catch {
        return null;
      }
    }
    return null;
  }
  async function navigate(path: string, options: { replace?: boolean; silent?: boolean } = {}) {
    const url = new URL(path, window.location.origin);
    if (!options.silent) {
      if (options.replace) window.history.replaceState({}, '', url.pathname + url.search);
      else if (window.location.pathname + window.location.search !== url.pathname + url.search)
        window.history.pushState({}, '', url.pathname + url.search);
    }
    const route = match(url.pathname);
    active = route ? { ...route, query: Object.fromEntries(url.searchParams) } : match('/');
    if (active) render(active);
    return !!route;
  }
  const onPopState = () => {
    void navigate(window.location.pathname + window.location.search, { silent: true });
  };
  const onClick = (event: MouseEvent) => {
    if (
      event.defaultPrevented ||
      event.button ||
      event.metaKey ||
      event.ctrlKey ||
      event.shiftKey ||
      event.altKey
    )
      return;
    const anchor = event.target instanceof Element ? event.target.closest('a[href]') : null;
    if (
      !(anchor instanceof HTMLAnchorElement) ||
      anchor.hasAttribute('download') ||
      (anchor.target && anchor.target !== '_self')
    )
      return;
    const path = anchor.getAttribute('href');
    if (
      !path?.startsWith('/') ||
      path.startsWith('//') ||
      !match(new URL(path, window.location.origin).pathname)
    )
      return;
    event.preventDefault();
    void navigate(path);
  };
  return {
    navigate,
    current: () => active,
    start: async () => {
      document.addEventListener('click', onClick);
      window.addEventListener('popstate', onPopState);
      await navigate(window.location.pathname + window.location.search, { silent: true });
    },
    stop: () => {
      document.removeEventListener('click', onClick);
      window.removeEventListener('popstate', onPopState);
    },
  };
}
