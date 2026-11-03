---
sidebar_position: 16
---

# Episode Telemetry and Langfuse Integration

After each task completes, Aico asynchronously reports the complete task episode (metadata, event chain, screenshots) to [Langfuse](https://langfuse.com/) as one trace per episode.

Traces are sent over Langfuse's OpenTelemetry (OTLP) endpoint (`POST /api/public/otel/v1/traces`) with the `langfuse.*` attribute conventions. The legacy `/api/public/ingestion` event API is deprecated and stops accepting trace and observation events on 2026-11-16, so it is no longer used.

## Feature Toggle

Configure the `[advanced_settings.runtime.telemetry]` section in `agent.toml`. Disabled by default; when enabled, it does not affect task execution (best-effort async reporting).

```toml
[advanced_settings.runtime.telemetry]
enabled = true
provider = "langfuse"
base_url = "http://langfuse.example.com:3000"
public_key = "pk-lf-..."
secret_key = "sk-lf-..."
upload_screenshots = true
upload_timeout_sec = 30
max_retry = 2
environment = "prod"
tags = ["aico-hardware"]
```

| Field | Description |
| --- | --- |
| `enabled` | Master switch; zero overhead when `false` |
| `base_url` | Langfuse Web address (without path) |
| `public_key` / `secret_key` | Langfuse API keys |
| `upload_screenshots` | Whether to upload `artifacts/step_*.jpeg` screenshots |
| `upload_timeout_sec` | Timeout for single report |
| `max_retry` | Retry count on failure |
| `environment` | Langfuse trace environment tag |
| `tags` | Tags attached to each trace |

Credentials are written directly into the `[advanced_settings.runtime.telemetry]` section of `agent.toml`.

The Langfuse deployment must support OTLP ingestion: self-hosted Langfuse `>= 3.22.0`, or Langfuse Cloud. The agent sends the `x-langfuse-ingestion-version: 4` header so spans land on the observations-first data model in real time.

## Data Flow

```text
Runtime.Run()
  → Agent execution loop (model calls captured with role, timing, usage, cost)
  → EpisodeRecorder records events
  → CommitEpisode persists to disk (episode.yaml + events.jsonl + artifacts/)
  → exportEpisodeBestEffort async maps episode + events + prompt calls to OTLP spans
  → POST /api/public/otel/v1/traces (spans), POST /api/public/scores (outcome)
```

Spans are exported once, complete: Langfuse treats ingested spans as immutable, so no observation is created and then updated. Only chunks that were not accepted are retried, and a partial-success response is reported instead of re-sent, so retries cannot duplicate observations.

### Langfuse Mapping

| Aico Episode | Langfuse |
| --- | --- |
| `TaskEpisode` | Trace (`aico-episode`), one trace per episode |
| Run | Root observation `agent-run` (`agent` type) carrying the user goal and final answer |
| `loop_phase` | `span` `phase/{phase}` |
| Iteration | `span` `agent-iteration` with `iteration` in metadata |
| `tool_call` + `tool_result` | One `tool` observation named after the tool, with the call arguments as input and the result as output |
| Model calls | `generation` observations: `agent-response`, `summarize-context` (context compaction), `llm-response` fallback |
| Memory retrieval | `retriever` `memory/retrieve` |
| STT / voice | `span` `stt/*`, `voice/*`, with model, provider, and latency metadata |
| `Outcome.Success` | Boolean Score `success=1/0` via the scores API |
| `artifacts/*.jpeg` | Media upload, referenced from the tool observation's output |
| `Extra` metrics | Trace metadata + generation model/cost/usage fields |

Typical trace structure:

```text
aico-episode (trace)
└── agent-run (agent)
    ├── session/begin
    ├── memory/retrieve (retriever)
    └── phase/default
        └── agent-iteration
            ├── agent-response (generation)
            ├── screenshot (tool)
            └── verifier
```

Observation names are stable operations, never per-call values: every model invocation of a run produces its own generation (with its own model, tokens, and cost), and repeated steps share one name with run-specific values in metadata.

### Trace Metadata and Tags

Trace-wide context — `langfuse.trace.name`, `langfuse.user.id`, `langfuse.session.id`, `langfuse.trace.tags`, `langfuse.release`, `langfuse.version`, `langfuse.environment`, and `langfuse.trace.metadata.*` — is attached to **every** observation, not only the root. Langfuse v4 filters and aggregates per observation, so context that only sits on the root is unavailable on its children.

In addition to tokens, duration, and model info in `episode.Extra`, the exporter derives execution metrics from the event chain and writes them into trace `metadata`:

| Field | Description |
| --- | --- |
| `tool_call_count` | Total number of tool calls in the episode |
| `iteration_count` | Number of agent iterations (from episode recorder counter or episode.Extra) |

Additional trace `tags`:

| Tag | Condition |
| --- | --- |
| `success` | Task completed successfully |
| `failure` | Task failed |

In Langfuse UI, you can filter tasks by success/failure and tool usage patterns.

Local episodes are still written to `/userdata/agent/memory/episodes/`; Langfuse serves as an additional copy for centralized analysis and dataset management.

## Self-Hosted Langfuse

The project provides a Docker Compose configuration: [`deploy/langfuse/docker-compose.yml`](../../deploy/langfuse/docker-compose.yml)

```bash
cd deploy/langfuse
cp .env.example .env
# Edit .env, set ENCRYPTION_KEY=$(openssl rand -hex 32)
docker compose up -d
```

After startup, visit `http://localhost:3000`, create an Organization / Project, and copy the Public Key and Secret Key to device environment variables. To skip that step, set `LANGFUSE_INIT_ORG_ID`, `LANGFUSE_INIT_PROJECT_ID`, and the project key pair in `.env` before the first start; the stack then provisions the project on boot.

Components: Langfuse Web + Worker, Postgres, ClickHouse, Redis, MinIO (screenshot and event blob storage).

## Verification

1. Start Langfuse (local or remote)
2. Set `telemetry.enabled = true` in device `agent.toml`
3. Execute a task (Web UI or benchmark)
4. Confirm in Langfuse UI → Traces:
   - `aico-episode` trace exists with root observation `agent-run`
   - Tool calls appear as `tool` observations with input and output on the same observation
   - Model calls appear as `generation` observations with model, token usage, and cost
   - Screenshots can be previewed in tool observations
   - Metadata contains `tool_call_count`, `iteration_count`, token stats
   - Tags contain `success` or `failure`
   - Trace contains `userId` (device ID) and `sessionId` (runtime session ID)

To verify the exporter itself against a running Langfuse, run the opt-in end-to-end test. It exports a synthetic episode through the production exporter and reads the resulting trace back through the Langfuse API:

```bash
cd src/agent
AICO_LANGFUSE_LIVE=1 \
LANGFUSE_BASE_URL=http://localhost:3000 \
LANGFUSE_PUBLIC_KEY=pk-lf-... LANGFUSE_SECRET_KEY=sk-lf-... \
go test ./internal/agent/ -run TestLangfuseLiveEpisodeExport -v -count=1
```

## Trace → Dataset → Benchmark Workflow

Langfuse is used to filter high-quality samples from production episodes and convert them into project benchmark suites.

### 1. Filter Traces in Langfuse

- Open **Traces**, filter by tag (`success` / `failure`) or metadata
- Review iteration spans and screenshots to confirm task quality
- Add labels or scores to qualified traces

### 2. Create Dataset

1. Langfuse UI → **Datasets** → New Dataset (e.g., `phone_control_candidates_v2`)
2. From trace detail page → **Add to dataset**
3. Fill in input (user goal `user_goal`) and expected output (`final_answer` or rubric description)
4. Optional: Reference screenshot media as dataset item metadata

### 3. Export and Convert to Benchmark Task

Langfuse supports exporting dataset items (UI or [Public API](https://langfuse.com/docs/api-and-data-platform/features/public-api)).

Convert selected items manually or via script to [`benchmark/suites/`](../../benchmark/suites/) format:

```json
{
  "id": "open_settings_from_prod_001",
  "category": "single_step",
  "description_for_judge": "Agent must open Settings from home screen.",
  "prompt": "Please open system settings.",
  "rubric": [
    {
      "id": "in_settings",
      "check": "Post-screenshot shows the Settings app main page."
    }
  ],
  "hard_assertions": {
    "min_tool_calls": 1,
    "max_tool_calls": 8,
    "must_complete_within_sec": 90
  }
}
```

Field mapping:

**Episode / trace metadata additional fields:**

| Field | Description |
| --- | --- |
| `model` / `model_name` / `model_provider` | LLM used for this run (from `[model_settings.model]` in `agent.toml`) |
| `agent_commit` | Git commit when Agent binary was built (application build task ldflags injection) |
| `agent_build` | Agent build version number (`YYYYMMDD-HHMMSS-<commit>`) |
| `firmware_version` | `current_version` from device OTA state `/userdata/ota/state.json` |
| `session_boundary_decision` / `session_boundary_reason` | Session-boundary classifier output for the run. |
| `session_rotated` | Whether the run archived the previous active session before handling the user turn. |
| `pending_chunks_recalled` | Number of `recall_session_chunks` results whose explicit result `source` is `pending`; `chunk_id` prefixes are ignored. |

**Langfuse trace field mapping:**

| Langfuse Field | Source |
| --- | --- |
| `version` | Agent build version `agent_build`, or `firmware_version` if absent |
| `release` | Git commit `agent_commit`, or `firmware_version` if absent |
| `metadata.model` | LLM model (e.g., `openrouter/google/gemini-3.5-flash`) |
| `metadata` | All above fields + episode metrics |
| `tags` | Configured tags + `model:{provider/model}` |
| `userId` | `device_scope.device_id`, or `extra.user_id` |
| `sessionId` | Runtime session ID, or `extra.session_id` |
| generation `modelParameters` | Invocation parameters like `temperature`, `max_tokens`, tool count, etc. |
| generation `usageDetails` / `costDetails` | Token usage and provider/local estimated cost |
| generation `completionStartTime` | Start time plus the provider's time-to-first-content metric, when reported |
| score `success` | Written for every task, `1` for success, `0` for failure |

**Langfuse Dataset → Benchmark field mapping:**

| Langfuse Dataset Item | Benchmark TaskSpec |
| --- | --- |
| `input` | `prompt` |
| trace metadata `user_goal` | Same or reference for `description_for_judge` |
| expected output / manual annotation | `rubric` checks |
| trace tool call count | Reference for `hard_assertions.min/max_tool_calls` |
| Screenshot artifact | `input_screenshot` (for static perception tasks) |

### 4. Regression Validation

```bash
cd benchmark
uv run python -m runner run --suite suites/phone_control_v1.json --agent-url http://device:8080
```

After new tasks are added to the suite, use the benchmark runner for automated regression; Langfuse continues collecting new production traces, forming a closed loop.

## Troubleshooting

| Symptom | Possible Cause |
| --- | --- |
| Log `[telemetry] export episode failed` | `base_url` unreachable, incorrect credentials, timeout, or a Langfuse version without OTLP ingestion (`< 3.22.0`) |
| Log `langfuse rejected N span(s)` | Langfuse accepted the request but dropped spans; the batch is not retried because the rest was ingested |
| Trace has no screenshots / media not yet uploaded | Agent did not PATCH upload status (fixed); or MinIO presigned URL uses `localhost:9090`, device cannot access; check agent log `[telemetry] screenshot upload failed` |

Screenshot upload complete flow:

1. Agent `POST {base_url}/api/public/media` gets `mediaId` + presigned `uploadUrl`
2. Agent `PUT uploadUrl` direct upload to MinIO
3. Agent `PATCH {base_url}/api/public/media/{mediaId}` writes `uploadHttpStatus=200` (**missing this step shows media not yet uploaded**)
4. Agent `POST {base_url}/api/public/otel/v1/traces` sends the tool observation containing `@@@langfuseMedia:...@@@`

When Agent runs on a device such as Luckfox, the Langfuse `.env` must use a MinIO address reachable from that device:

```bash
LANGFUSE_S3_MEDIA_UPLOAD_ENDPOINT=http://192.168.50.246:9090
```

After modification, restart langfuse-web / langfuse-worker with `docker compose up -d`.
| No trace | `telemetry.enabled=false` or episode not committed (empty `user_goal`) |

Report failures do not affect task execution or local memory plane writes.
