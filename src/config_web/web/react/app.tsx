import type { DeviceState } from './state';
import type { HeaderSpec, Navigate, PageProps, Route, TextSpec } from './types';
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';
import { useAtomValue } from 'jotai';
import { routes, localRoutes, containerRoutes } from '../assets/js/app/routes.js';
import { createRouter } from '../assets/js/app/router.js';
import { ALL_ITEMS } from '../assets/js/app/nav-items.js';
import { applyEnvironmentClass, isEmbedded } from '../assets/js/ui/environment.js';
import { applyServerLocale, initI18n } from '../assets/js/app/data.js';
import { msg, resolve } from '../assets/js/ui/text.js';
import { ApplicationStatus, announcePage } from './application';
import { ReadyEditor } from './editor';
import { Navigation } from './navigation';
import { AdvancedPage, LocalOverview, MCPPage, StoragePage } from './pages';
import { OTAPage } from './ota';
import { WiFiDetail, WiFiPage } from './wifi';
import { SecurityPage } from './security';
import { EditorNode, Icon, Text } from './ui';

const titles: Record<string, TextSpec> = {
  '/advanced/lan': msg('lan.title', '局域网访问'),
  '/advanced/password': msg('lan.password', '设备密码'),
  '/advanced/mcp': 'MCP',
  '/firmware/log': msg('ota.log', '更新日志'),
};
function routeHeader(route: Route, navigate: Navigate): HeaderSpec {
  const title =
    route.params?.ssid ||
    (route.name === 'local-home' ? msg('local.overview', '运行概览') : undefined) ||
    titles[route.path] ||
    ALL_ITEMS.find((item) => item.route === route.path)?.label;
  const parent = route.path.slice(0, route.path.lastIndexOf('/')) || '/';
  return { title, onBack: route.path === '/' ? null : () => navigate(parent) };
}

function Content(props: PageProps) {
  const { model, route, navigate } = props;
  if (route.path === '/')
    return model.local ? (
      <LocalOverview {...props} />
    ) : (
      <Navigation model={model} navigate={navigate} path="/" />
    );
  if (route.pattern === '/wifi/:ssid') return <WiFiDetail {...props} />;
  if (route.path === '/wifi') return <WiFiPage {...props} />;
  if (route.path === '/storage') return <StoragePage {...props} />;
  if (route.path === '/advanced') return <AdvancedPage {...props} />;
  if (route.path === '/advanced/lan' || route.path === '/advanced/password')
    return <SecurityPage {...props} password={route.path.endsWith('/password')} />;
  if (route.path === '/advanced/mcp') return <MCPPage {...props} />;
  if (route.path.startsWith('/firmware'))
    return <OTAPage {...props} logOnly={route.path.endsWith('/log')} />;
  return <ReadyEditor {...props} />;
}

export function App({ model }: { model: DeviceState }) {
  const [route, setRoute] = useState<Route | null>(null);
  const [header, setHeader] = useState<HeaderSpec>({});
  const router = useRef<ReturnType<typeof createRouter> | null>(null);
  const snapshot = useAtomValue(model.current);
  useAtomValue(model.locale);
  const navigate = useCallback(
    (path: string, options?: { replace?: boolean; silent?: boolean }) =>
      router.current?.navigate(path, options),
    [],
  );
  const locale = snapshot.config?.agent?.locale;
  useEffect(() => {
    applyEnvironmentClass();
    const onLocale = () => model.store.set(model.locale, (value) => value + 1);
    const onSave = (event: Event) =>
      model.publishSave((event as CustomEvent<import('./types').ApplicationData>).detail);
    const onWiFi = (event: Event) => model.publishWiFi((event as CustomEvent).detail);
    const onVisibility = () => model.setVisible(!document.hidden);
    const onOnline = () => model.setVisible(!document.hidden);
    document.addEventListener('aico:locale-changed', onLocale);
    document.addEventListener('visibilitychange', onVisibility);
    window.addEventListener('aico:config-saved', onSave);
    window.addEventListener('aico:wifi-changed', onWiFi);
    window.addEventListener('online', onOnline);
    router.current = createRouter({
      routes: model.container ? containerRoutes : model.local ? localRoutes : routes,
      render: (value) => {
        const next = { ...value, path: (value as Route).path.replace(/\/+$/, '') || '/' } as Route;
        setRoute(next);
        setHeader(routeHeader(next, navigate));
        model.activate(next.path);
      },
    });
    void router.current.start();
    model.setVisible(!document.hidden);
    void model.start();
    return () => {
      router.current?.stop();
      model.dispose();
      document.removeEventListener('aico:locale-changed', onLocale);
      document.removeEventListener('visibilitychange', onVisibility);
      window.removeEventListener('aico:config-saved', onSave);
      window.removeEventListener('aico:wifi-changed', onWiFi);
      window.removeEventListener('online', onOnline);
    };
  }, [model, navigate]);
  useEffect(() => {
    if (locale) applyServerLocale({ config: { agent: { locale } } });
    else initI18n();
  }, [locale]);
  useLayoutEffect(() => {
    if (route) announcePage(resolve(routeHeader(route, navigate).title));
  }, [route, navigate]);
  const setPageHeader = useCallback((value: HeaderSpec) => setHeader(value), []);
  const home = route?.path === '/' && !model.local;
  return (
    <div className="app">
      {!isEmbedded() && (
        <div className={`app__topbar ${header.onBack ? 'app__topbar--sub' : ''}`}>
          <div className="app__brand">
            <a
              className="app__brand-link"
              href="https://aico.link"
              target="_blank"
              rel="noreferrer"
            >
              <span className="app__wordmark" style={{ fontSize: 22 }}>
                Aico Link
              </span>
            </a>
            <Text
              value={model.local ? msg('local.title', '本机 Agent') : msg('ui.app_title', '配置')}
              className="app__brand-title"
            />
          </div>
          <div className="app__topbar-nav">
            <div className="app__topbar-row">
              {header.onBack ? (
                <button
                  type="button"
                  className="app__back"
                  onClick={header.onBack}
                  aria-label={resolve(msg('action.back', '返回'))}
                >
                  <Icon name="chevron" className="app__back-icon" />
                </button>
              ) : (
                <span />
              )}
              <Text value={header.title} className="app__topbar-heading" />
              <div className="app__topbar-action">
                <EditorNode node={header.action} />
              </div>
            </div>
          </div>
        </div>
      )}
      <div className="app__body">
        <div className="app__nav">
          <div className="app__nav-body">
            <Navigation model={model} navigate={navigate} path={route?.path || '/'} />
          </div>
        </div>
        <div className="app__panel">
          <ApplicationStatus model={model} />
          <div className={`app__scroll ${home ? 'route-home' : ''}`} data-region="content">
            {route && (
              <Content
                key={`${route.path}?${new URLSearchParams(route.query)}`}
                route={route}
                model={model}
                navigate={navigate}
                header={setPageHeader}
              />
            )}
          </div>
        </div>
      </div>
    </div>
  );
}
