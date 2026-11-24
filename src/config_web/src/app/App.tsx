import type { DeviceState } from '../state/device';
import type { HeaderSpec, Navigate, PageProps, Route, TextSpec } from '../types';
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';
import { useAtomValue } from 'jotai';
import { routes, localRoutes, containerRoutes } from './routes';
import { createRouter } from './router';
import { ALL_ITEMS } from './navItems';
import { applyEnvironmentClass, isEmbedded } from '../lib/environment';
import { applyServerLocale, initI18n } from '../lib/data';
import { msg, resolve } from '../lib/i18n';
import { ApplicationStatus, announcePage } from '../components/ApplicationStatus';
import {
  BasicPage,
  LanguagePage,
  ConversationPage,
  PromptPage,
  SearchPage,
  MemoryPage,
  RequireConfig,
} from '../pages/Settings';
import {
  ModelsPage,
  MainModelPage,
  ProvidersPage,
  ProviderEditPage,
  ModelPicker,
  DecisionPage,
} from '../pages/Models';
import { VoicePage, VoiceProviders, VoiceProviderEdit, VoiceOptions } from '../pages/Voice';
import { LogsPage, AgentLogsPage, LogLevelPage, RawConfigPage } from '../pages/Advanced';
import { OverlayHost } from '../components/overlays';
import { Navigation } from './Navigation';
import { AdvancedPage, LocalOverview, MCPPage, StoragePage } from '../pages/Device';
import { OTAPage } from '../pages/OTA';
import { WiFiDetail, WiFiPage } from '../pages/WiFi';
import { SecurityPage } from '../pages/Security';
import { Icon, Text } from '../components/ui';

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
  if (route.path === '/advanced/config') return <RawConfigPage {...props} />;
  const pages: Record<string, React.ComponentType<PageProps>> = {
    '/basic': BasicPage,
    '/basic/language': LanguagePage,
    '/conversation': ConversationPage,
    '/conversation/prompt': PromptPage,
    '/conversation/tools': SearchPage,
    '/conversation/tools/provider': SearchPage,
    '/memory': MemoryPage,
    '/model': ModelsPage,
    '/model/main': MainModelPage,
    '/model/providers': ProvidersPage,
    '/model/providers/edit': ProviderEditPage,
    '/model/models': ModelPicker,
    '/model/decision': DecisionPage,
    '/voice': VoicePage,
    '/voice/providers': VoiceProviders,
    '/voice/providers/edit': VoiceProviderEdit,
    '/voice/model': VoiceOptions,
    '/voice/extra': VoiceOptions,
    '/advanced/logs': LogsPage,
    '/advanced/logs/agent': AgentLogsPage,
    '/advanced/logs/level': LogLevelPage,
  };
  const Page = pages[route.path];
  return Page ? (
    <RequireConfig props={props}>
      <Page {...props} />
    </RequireConfig>
  ) : null;
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
      model.publishSave((event as CustomEvent<import('../types').ApplicationData>).detail);
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
      <OverlayHost />
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
              <div className="app__topbar-action">{header.action}</div>
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
