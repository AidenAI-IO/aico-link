---
sidebar_position: 4
---

# Device CLI and navigation graph

The backend agent operates the target screen using `device-cli` in a foreground
`shell` call. The CLI forwards to the existing [Tools HTTP API](tools-http-api.md);
it does not implement another device driver. The backend agent no longer receives
the legacy direct UI tools. The HTTP API and the standalone realtime voice path
still expose their existing device tools. Set `AICO_LEGACY_DEVICE_TOOLS=1` before
starting the daemon to opt out of CLI mode and UTG learning.

```sh
device-cli tap 500 650
device-cli swipe 500 800 500 300
device-cli type 'Hello' && device-cli enter
```

Coordinates are normalized to 0–1000 on both axes. After a foreground shell has
actually invoked the scoped CLI, the shell result is followed by one screenshot
sheet containing up to the last five captured frames, in order from left to
right, then top to bottom. Each panel marks only its own operation, including
steps dispatched by UTG replay. There is no footer; a `!` next to a marker
indicates a failed operation. Normalize new coordinates within the last panel,
not across the entire sheet. A foreground shell can dispatch up to 64 operations.
An unchanged display is not an error. A CLI failure exits nonzero, so `&&` stops
the chain. Without an active shell scope, the executable accepts `--url` to
target a daemon directly (default
`http://127.0.0.1:8080`); use the HTTP screenshot endpoint to retrieve the image
outside the agent. UTG CLI commands are available only with an active agent turn.

## Debugging learned navigation

Open **Coordinate Debug → Device navigation graph** (`/utg-debug`). Refresh to
see committed states, directed raw-action edges, and a representative screenshot
for each state. The outlined state is the last learned state, not a live match.
The graph file (`device-utg.json` in the agent data directory) contains device
screenshots. Treat it as sensitive and do not share or commit it.

**Reset graph** clears the saved states and transitions after confirmation. It
leaves the chat history and device data unchanged. In-progress and queued
learning from before the reset is discarded; new turns can learn a fresh graph.

- `utg-status` identifies the live screen and lists reachable keys.
- `utg-goto KEY` plans a directed shortest path. It can fall back to a known Home
  anchor. It verifies source and destination at each edge. If destination
  verification fails, it reuses the landing screenshot to check all known
  states reachable from the edge's source, including the source itself and
  recorded outcomes of quarantined outgoing edges. If it identifies a
  different state, it replans without the failed edge for this command.
- `utg-step EDGE_ID` runs one edge from its verified source.
- `utg-tag KEY LABEL` queues a display-label update for the end of the turn.

UTG recognition uses the existing [decision model](configuration.md#model_settingsdecision)
(e.g. TypeSafe Jev). Enable it in **Config Web → Models → Decision model** and
supply credentials for the selected provider. The decision model receives the
live screenshot and representative images from the graph. It compares the app,
page, navigation layout and modal layer, ignoring clocks and other dynamic
content. Initial discovery considers up to four candidate states; replay checks
the expected destination of each edge. Relocation batches references four at
a time and requires a unique confident match across batches. New reference
images are resized to 480 pixels high when the source is large enough; older
smaller references are refreshed as those states are observed again. If the
model is unavailable, the state cannot be identified, or no route remains
from the confirmed state (including the Home fallback), use visual navigation.
There is no pixel-matching fallback. UTG requires a confidence threshold of at least 0.8.
Learning still happens after the turn even when the decision model is disabled.

During a turn the agent records CLI observations, without invoking a graph
classification model. After the loop returns, a background request reuses the
conversation prefix and asks for semantic frame labels with tools disabled.
Only validated step indices and recorded raw parameters can add edges. When
replay confidently rejects an expected destination and the background classifier
identifies a different landing state on that same frame, the update quarantines
the failed edge. It retains the edge ID on disk for audit and deduplication, but
excludes it from planning and `utg-step`. An uncertain match or service error
alone does not quarantine an edge. The current command can replan before
that update; later turns use the corrected routes once the update commits. Graph
writes are atomic; the next turn waits briefly for the latest update, then uses
last committed graph if the update is still busy. Background work is bounded
and a saturated queue can skip learning without interrupting device control.
Cache reuse depends on the model provider's prompt caching; no server-side KV
cache hit is assumed. The classifier may mislabel visually ambiguous screens;
replay deliberately does not borrow edges from unverified lookalike hubs.
