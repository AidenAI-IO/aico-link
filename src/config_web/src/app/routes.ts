import type { RouteDefinition } from './router';
export const routes: RouteDefinition[] = [
  '/',
  '/wifi',
  '/wifi/:ssid',
  '/basic',
  '/basic/language',
  '/conversation',
  '/conversation/prompt',
  '/conversation/tools',
  '/conversation/tools/provider',
  '/model',
  '/model/main',
  '/model/decision',
  '/model/providers',
  '/model/providers/edit',
  '/model/models',
  '/voice',
  '/voice/providers',
  '/voice/providers/edit',
  '/voice/model',
  '/voice/extra',
  '/memory',
  '/storage',
  '/advanced',
  '/advanced/logs',
  '/advanced/logs/agent',
  '/advanced/logs/level',
  '/advanced/config',
  '/advanced/lan',
  '/advanced/password',
  '/advanced/mcp',
  '/firmware',
  '/firmware/log',
].map((path) => ({ path, name: path === '/' ? 'home' : path }));
export const localRoutes = [
  { path: '/', name: 'local-home' },
  ...routes.filter(
    (route) =>
      route.path.startsWith('/model') ||
      route.path.startsWith('/conversation') ||
      route.path === '/basic/language' ||
      route.path === '/advanced/mcp' ||
      route.path.startsWith('/advanced/logs'),
  ),
];
export const containerRoutes = [
  ...localRoutes,
  ...routes.filter((route) => route.path.startsWith('/voice') || route.path === '/memory'),
];
