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
The local Agent launcher reuses an existing `dist/index.html`; pass
`--rebuild-web` after frontend changes to rebuild it. Docker and rsync are
required for the first build or an explicit rebuild, but not for reusing assets.
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
  UI subscriptions exclude observation timestamps; Wi-Fi ordering still uses
  every raw observation. The app shell subscribes only to the locale it renders.
- Initial reads use accessible shimmer placeholders instead of assuming an
  offline, disabled or empty state. Confirmed content stays mounted during
  background refreshes; failed reads leave loading and show status errors.
  Reduced-motion preferences disable the shimmer animation.
- Wi-Fi observations continue while a connection task runs, so association
  changes are visible before the task finishes. Its final result invalidates
  older reads. Other resources pause reads during their mutations.
- Wi-Fi polls every second; storage every three seconds; OTA and configuration
  application every two seconds; firmware every ten seconds. Agent process
  status polls every ten seconds on the overview, Agent log and container voice
  routes that need it.
  USB Internet, LAN/password and installed MCP servers poll every five seconds
  on their routes. Native overview also observes iPhone mirroring support.
  Hidden tabs pause polling. Return and browser network recovery refresh the
  current subscriptions immediately.
  Background OTA polls omit logs (`?logs=0`); only firmware and OTA log routes
  poll the complete log response, with progress and flags from one observation.
- Configuration save events publish persisted config immediately. Application
  status continues polling in both profiles. A changed persisted-file revision
  refreshes configuration independently of the Agent's runtime revision,
  including external edits while the Agent is offline. Failed reconciliations
  retry on later observations, including a failed canonical read after an
  optimistic save has already published the same revision. Saves reread canonical validation state. The initial
  application observation joins the pending read instead of cancelling it;
  revision mismatches reconcile an edit that raced the first read.
  All profiles hide the application banner once the saved configuration is
  confirmed applied. Pending, unapplied, failed, unavailable, and reboot-required
  states remain visible; successful saves use the existing transient feedback.
- Configuration reads use `GET /api/config`; firmware uses
  `GET /api/device/firmware`. Neither summary waits for the aggregate snapshot
  or Agent process probes. Both endpoints share the canonical backend loaders.
- React owns every route, including provider forms, voice, password and token
  dialogs, full backup and restore, raw TOML recovery, and the LLM log viewer.
  Keyed rows preserve controls and focus. Dialogs use portals and trap focus.
  A removed list control is refocused only when focus belonged to that list.
- `src/hooks/settings.tsx` plans saves against the backend, saves live changes
  immediately, and confirms staged changes before persisting and restarting.
  Per-field sequences reject obsolete results. Forms retain their own drafts
  across observations; blank secrets keep the saved credentials.
- Schema-driven fields derive types, options and provider visibility from the
  backend. Schema is generated in process and shared across editor mounts within
  each app/profile, including concurrent reads and error retries.
  Initial snapshot failures expose retry; the raw TOML editor remains
  an independent recovery route and enables saving only after a successful read.
- Voice tabs keep a separate selection in the application's Jotai store. Browsing
  either mode, entering subpages, and returning do not save or apply a mode.
  The selection survives navigation and background observations, and a full
  reload restores the saved device mode. For a previewed mode with configured
  providers, the header's Apply action validates and saves that mode without
  editing its credentials. Incomplete modes keep the action disabled. Explicit
  provider selection or saving the selected provider's credentials can apply the previewed mode after
  canonical validation succeeds. This behavior is shared by board and PC profiles.
- OpenRouter Decisions model suggestions use an inline list inside the settings
  card, with model names and IDs, keyboard navigation, and free-text entry.
  Catalog failures leave custom model entry available.
- Confirmed model IDs join the main, decision and voice model choices. A shared
  Jotai history retains previous choices across navigation and browser reloads
  using local browser storage, scoped by profile, provider record and endpoint.
  Drafts and rejected saves are excluded. Catalog entries keep their names and
  order, and duplicate IDs appear only once. Unavailable storage falls back to
  in-memory history without blocking configuration saves.
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
