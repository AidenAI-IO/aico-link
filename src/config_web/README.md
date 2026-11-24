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
The device does not need Node.js. Deploy the generated `dist/` directory. Application sources live in `src/`;
`public/` contains static icons copied unchanged by Vite.

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
and runs the frontend checks plus the React behavior, protocol, API and design tests.
Generated design tokens are excluded from Prettier; regenerate them with
`node scripts/gen_config_web_tokens.mjs` from the repository root.

## State ownership

- `src/state/device.ts` creates one Jotai store per application. Navigation and
  pages subscribe to the same resources and derived device snapshot.
- `src/state/resource.ts` owns request deduplication, completion-based polling, deadlines,
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
- React owns every route, including provider forms, voice, password and token
  dialogs, full backup and restore, raw TOML recovery, and the LLM log viewer.
  Keyed rows preserve controls and focus. Dialogs use portals and trap focus.
  A removed list control is refocused only when focus belonged to that list.
- `src/hooks/settings.tsx` plans saves against the backend, saves live changes
  immediately, and confirms staged changes before persisting and restarting.
  Per-field sequences reject obsolete results. Forms retain their own drafts
  across observations; blank secrets keep the saved credentials.
- Schema-driven fields derive types, options and provider visibility from the
  backend. Initial snapshot failures expose retry; the raw TOML editor remains
  an independent recovery route and enables saving only after a successful read.
- `src/lib/backup.ts` retains maintenance sessions, stable retry request IDs,
  chunked SHA-256 transfers, restore validation and interrupted-job recovery.
  Its Jotai view survives navigation while a job runs. The LLM viewer streams
  NDJSON, normalizes provider messages and computes bounded line diffs.

The router preserves real URLs and the native WebView bridge. CSS, icons,
translations and typed API transport are shared by the React pages. The build
emits all three profile documents and the separate LLM log viewer. PC profiles
poll only their available APIs. The container profile retains software voice,
memory retention/reset and support-log export while excluding board controls;
its overview follows published Agent and optional terminal URLs from status.
