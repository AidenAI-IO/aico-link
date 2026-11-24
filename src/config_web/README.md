# Config Web frontend

The device, native host and Launcher container profiles share a React application
written in strict TypeScript. Jotai owns confirmed device state; forms own their unsaved drafts.
The Go Config Web server and Device Management API remain the backend.

## Development

Run commands from the repository root unless indicated otherwise. The pinned
Docker build requires Docker and rsync, with no host Node installation:

```bash
bash scripts/build-config-web.sh
```

Serve `src/config_web/dist` with `agent config-web --web-root` (add
`--local-agent` for the native host profile or `--container-agent` for Launcher
containers). Debian packaging, the development sandbox,
and `scripts/local_agent.py` build and use these static assets automatically.
The device does not need Node.js. Do not deploy `web/` directly: it contains
TypeScript source and bare package imports.

For a development server with fast refresh, install Node.js 22.23.2 or newer:

```bash
cd src/config_web
npm ci --ignore-scripts
AICO_CONFIG_WEB_URL=http://192.168.42.1 npm run dev
```

Vite proxies `/api` and `/login` to that origin. Open `/` for the device profile
or `/local.html` for the native profile, or `/container.html` for the Launcher
container profile. Override the API origin when developing
against a native Agent's Config Web port.

```bash
npm run typecheck
npm run lint
npm run format
npm test
npm run build
```

`npm run check` combines type checking, ESLint, Prettier, Vitest and a production
build. The required repository validation is `make check`, using the pinned
Docker test image. Its Web suite installs the lockfile in a temporary directory
and runs the frontend checks plus the existing API, editor and design tests.
Generated design tokens are excluded from Prettier; regenerate them with
`node scripts/gen_config_web_tokens.mjs` from the repository root.

## State ownership

- `web/react/state.ts` creates one Jotai store per application. Navigation and
  pages subscribe to the same resources and derived device snapshot.
- `resource.ts` owns request deduplication, completion-based polling, deadlines,
  cancellation, mutation exclusion and response ordering. A save or a newer
  observation invalidates older requests. Identical observations keep their
  data identity. Failed reads retain confirmed values and expose stale/error
  metadata; Wi-Fi summaries become unknown until recovery.
- Wi-Fi observations continue while a connection task runs, so association
  changes are visible before the task finishes. Its final result invalidates
  older reads. Other resources pause reads during their mutations.
- Wi-Fi polls every second; storage every three seconds; OTA and configuration
  application every two seconds; device/firmware status every ten seconds.
  USB Internet, LAN/password and installed MCP servers poll every five seconds
  on their routes. Native overview also observes iPhone mirroring support.
  Hidden tabs pause polling. Return and browser network recovery refresh the
  current subscriptions immediately.
- Configuration save events publish persisted config immediately. Application
  status continues polling in both profiles. A changed runtime revision
  refreshes the aggregate snapshot, including edits made outside this tab.
- React owns the shell, navigation, Wi-Fi, storage status, OTA, USB Internet,
  LAN/password, installed MCP servers and native overview. Keyed rows preserve
  controls and focus. Wi-Fi dialogs use a portal, trap focus and retain entered
  credentials while status changes. A removed list control is refocused only
  when focus belonged to that list.
- `editor.tsx` is the compatibility boundary for the existing model/provider,
  voice, TOML and backup editors. Each editor mounts once per route with a cloned
  confirmed snapshot; observations do not rebuild it or erase drafts. Existing
  validation, save planning, backup transfers and native save actions are
  retained. These editor internals remain JavaScript while new application code
  is TypeScript. Initial snapshot failures show a retry action; the raw TOML
  editor remains accessible as an independent recovery route.

The router preserves real URLs and the native WebView bridge. CSS, icons,
translations and API transport are shared with the retained editors. The build
emits all three profile documents and the separate LLM log viewer. PC profiles
poll only their available APIs. The container profile retains software voice,
memory retention/reset and support-log export while excluding board controls;
its overview follows published Agent and optional terminal URLs from status.
