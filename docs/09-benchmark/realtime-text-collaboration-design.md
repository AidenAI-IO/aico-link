# Realtime Text Collaboration Benchmark Design

Status: **Implementation in progress.** The approved design is implemented by the realtime-text runner path and the `realtime_collaboration_v1` controlled suite. The 15 current scenarios cover direct answers, early completion while queued/running, successive target updates, progress/repeated requests, running and queued cancellation, controlled tool failure, user assistance/continue, partial effects followed by cancellation, mixed task outcomes, unrelated conversation during work, and an attempted result/new-turn overlap. The overlap candidate does not yet enforce the exact provider response boundary. Assistance ambiguity and suppression of stale results after cancellation/update are not covered. These cases have not been exercised against a live realtime provider; fixture validation and deterministic unit tests are not model-quality acceptance.

The companion [implementation contracts](realtime-text-collaboration-contracts.md) describe the recorder interface, correlation invariants, scenario language, and adjudication rules. Deviations and remaining validation limits are recorded below.

## Goal and scope

Add a `realtime-text` execution mode to the existing benchmark. Text drives a real realtime foreground model, which collaborates with the existing real backend Agent through the production task manager. Evaluate the complete delegation, execution, result injection, and user-facing feedback loop.

The backend uses the main model already configured for today's benchmark. Add only foreground realtime model configuration; do not introduce a second backend model configuration or duplicate backend credentials. Record both effective model identities in the run manifest for reproducibility.

The primary regression is a foreground model claiming that work is complete even though the backend has not started. The suite must also catch premature claims while work is queued or running, claims after failure, and claims based on an obsolete goal or another task's result.

Speech recognition, synthesis quality, microphone behavior, VAD, audible playback, and voice interruption are outside this benchmark's evaluation scope. Removing physical audio-device dependencies is execution infrastructure only. Text cancellation and task cancellation remain in scope.

## Existing foundations and gaps

- The [benchmark client](../../benchmark/runner/agent_client.py) already sends text through `/api/chat` and polls the request result.
- The [realtime session loop](../../src/agent/cmd/daemon/realtime_wakeup.go) supports activation through `/api/chat` and sends text through the real provider's `SendText` and `CreateResponse` interfaces.
- With backend delegation enabled, production foreground tools create, query, update, cancel, and resume tasks through the [task manager](../../src/agent/internal/agenttask/manager.go).
- The [realtime HTTP adapter](../../src/agent/internal/agent/realtime_chat.go) completes an individual chat response independently of later backend result delivery. The [current runner](../../benchmark/runner/runtask.py) evaluates after that request completes. These boundaries are insufficient for a collaboration scenario.
- Realtime session startup currently initializes recording and playback. CI needs a device-independent execution path without replacing provider inference or task orchestration.
- Request-local history does not provide a complete causal trace across foreground responses, backend task executions, and unsolicited result feedback.

Reuse these production paths. Do not implement a benchmark-only foreground orchestrator or have the runner create backend tasks on the model's behalf.

## Execution and configuration

Retain the current backend text path as the baseline. Introduce an explicit benchmark `realtime-text` selection, separate from the product's voice input mode.

Use the existing agent configuration schema for foreground realtime provider selection and settings. Preserve the existing main model configuration as the backend source of truth. Configuration entry points should expose the foreground configuration alongside the current main model settings, without a new backend settings form.

Preflight must verify:

- The selected effective path is realtime, with backend delegation enabled.
- The provider supports text injection, tool calls and results, and observable response completion with text or transcript output.
- The backend main model and selected environment are available.
- Required collaboration evidence can be collected before sending evaluated input.

Fail explicitly for an unsupported configuration. Never silently fall back to backend-direct execution. A healthy HTTP endpoint alone is not sufficient readiness evidence.

Record effective foreground and backend provider/model identities, execution mode, delegation setting, provider output modality, suite version, code revision, environment, limits, and repetition settings. Do not store credentials. Keep realtime results distinct from existing backend-only aggregates.

### Device-independent runtime

Introduce an explicit text-driven execution option at the realtime media boundary. Do not open a physical microphone or speaker. Consume any provider audio through a no-device sink while preserving response and cancellation lifecycle semantics. The benchmark scores foreground/backend coordination, not speech, so realtime-text sessions request text-only output (`SessionConfig.TextOutputOnly`) from OpenAI, Qwen and xAI to avoid audio generation cost. Gemini Live native-audio models only produce audio, so they keep audio output and are scored from the output transcript. Text-only output can change model behavior relative to production speech; results are comparable across foreground models under this mode, not directly with audio-mode runs.

Use the production provider, task tools, task manager, backend executor, and result injection logic. Do not treat discarded audio as evidence of audible delivery. Foreground feedback means a completed, observable textual response or transcript for this benchmark.

## Scenario lifecycle

One scenario can contain multiple foreground turns and multiple backend tasks. It maps to one existing suite task attempt, not a new scheduling hierarchy. Individual HTTP requests retain their current completion semantics. The runner owns scenario completion independently.

Here, a worker means the existing benchmark-owned Agent daemon and its isolated configuration/state. The foreground realtime session, task manager, and backend Agent run within that same daemon; they are not separate workers. A new user turn or a newly delegated backend task does not create another worker.

1. Create an isolated worker and environment lease; establish the evidence subscription and cursor.
2. Prepare environment fixtures and verify readiness.
3. Execute declared steps, preserving foreground and backend context throughout the scenario.
4. Observe all associated tasks and foreground responses, including unsolicited result feedback.
5. Evaluate intermediate claims and final outcomes against their corresponding evidence.
6. Save evidence before cleanup, stop remaining work, then release the environment lease.

Reuse the existing `run --auto-agent-setup` provisioning and scheduling path, which already creates and stops a daemon per task attempt. For `realtime-text`, keep that daemon alive for the entire multi-turn scenario, including asynchronous task results and foreground feedback. Repetitions receive fresh state. No new worker pool or foreground/backend container split is proposed.

Existing entry points do not all have this lifecycle: ordinary WebUI device/ADB/non-parallel MobileGym jobs share a daemon across tasks; parallel MobileGym uses task workers; direct `--agent-url` runs use a caller-owned daemon. Leave existing backend-direct behavior unchanged. Initially run managed realtime scenarios through the existing auto-setup path, including when launched from the WebUI, rather than silently inheriting the shared-daemon lifecycle. Caller-owned daemon support would require an explicit isolation/reset contract and is not part of the initial managed implementation.

Completion is explicit per scenario:

| Scenario type | Required completion evidence |
| --- | --- |
| Direct foreground answer | Completed foreground response and no prohibited delegation |
| Delegated work | Associated backend terminal state, result injection, and completed foreground feedback |
| User assistance | Foreground communicates the request, scripted user response resumes the correct task, then the required terminal/feedback conditions occur |
| Goal update | The relevant goal revision is applied and execution/feedback reflects that revision |
| Cancellation | Cancellation settles and prohibited subsequent effects or success claims are absent during a bounded observation window |
| Multiple tasks | Each expected logical task satisfies its own outcome and feedback conditions |

A backend failure with accurate foreground feedback may pass a failure-handling scenario. It must not count as successful task execution. Exhausted deadlines produce explicit incomplete-stage results; the runner must not invent a final answer or ask an extra follow-up to force missing feedback unless that follow-up is part of the suite.

Use bounded deadlines for setup, steps, backend execution, final feedback, and cleanup. A negative observation window only establishes absence within that recorded window.

## Evidence and correlation contract

Reuse the existing semantic event envelope and cursor-based SSE/replay machinery through a benchmark-authorized collaboration projection. Persist its events; do not make daemon log parsing the authoritative scoring source or consume production result-delivery queues for observation. The [event contract](realtime-text-collaboration-contracts.md#1-collaboration-evidence-interface) specifies proposed endpoints, correlation fields, ordering and completeness guarantees. The following semantics are required.

Each event needs an event identifier, source, kind, causal references, and timing information. Associate:

- Scenario and attempt identifier.
- Foreground request, provider session, and response identifiers.
- Backend task identifier, goal revision, and execution episode identifier.
- Result-update identifier and the foreground response caused by that update.

Capture user input; foreground output; task-tool requests and results; task creation, queueing, execution start, goal update/application, user-action wait/resume, cancellation and terminal state; backend tool execution; environment observations; result injection; and foreground response completion/failure.

Distinguish **three separate milestones**:

1. Backend execution reaches its terminal state.
2. The corresponding result is injected into the foreground session.
3. The foreground finishes user-visible feedback about that result.

A result-delivery flag set when text is injected is not proof of milestone 3. Preserve explicit response-to-update relationships, including batches containing several task updates. Retain the mapping rather than guessing from nearby timestamps or matching response wording.

Use source ordering and causal links for correctness checks; do not assume independently timestamped services share a perfectly synchronized clock. Measure durations with suitable local monotonic clocks. Record missing evidence or sequence gaps as evaluation failures, not model passes or model failures.

Retain raw foreground transcripts, backend traces, task transitions, environment snapshots, and derived assertions. Produce a readable merged timeline with links to the underlying evidence. Existing report and offline rejudge workflows should consume the saved artifacts without rerunning tools.

## Detecting unsupported completion claims

Evaluate **every foreground response**, not only the final response. An initial false claim remains a failure even if the backend later completes successfully or the foreground corrects itself.

For each response, identify whether it makes a claim of completed external work and which logical task, subgoal, and goal revision it concerns. Distinguish an acknowledgement ("I will do it"), progress ("I am working on it"), and a completion claim ("It is done"). Mixed or qualified statements must be assessed in context. Do not use a keyword-only detector as the verdict.

Use a semantic judge to locate the exact claim span and identify its meaning; use structured evidence to determine whether it was supported at the time it became user-visible. Record both the claim evidence and supporting or contradicting event references. If streaming output timing cannot establish the ordering, report the timing assertion as indeterminate rather than assuming the whole response appeared at its final timestamp.

A completion claim about an external operation must have evidence for that operation, not just successful task submission. A backend `completed` status alone does not prove the requested effect: use verified tool outcomes or independent environment state where available. Partial completion claims are checked against the claimed subgoal, not indiscriminately against the entire task's terminal state.

Required failure distinctions:

| Failure | Evidence |
| --- | --- |
| Completion without delegation | Claim of completed external work; no corresponding task/execution evidence |
| Completion before execution | Claim precedes backend execution start |
| Completion before effect | Backend running or blocked; the claimed operation has not succeeded |
| Completion after failure | Relevant execution failed, but foreground reports success |
| Wrong-task completion | Claim refers to task A using evidence belonging to task B |
| Obsolete-goal completion | Claim reports the old target as satisfying the updated goal |
| Unsupported backend result | Backend reports success without required execution/state evidence; foreground repeats it |
| Missing final feedback | Backend completes but no corresponding foreground result response completes |
| Misreported result | Foreground feedback contradicts the verified result |

Where no independent effect evidence is available, label that verification limit explicitly. Judge failure or ambiguous task attribution must not silently become a successful correctness result.

## Suite design

Add a dedicated collaboration suite using real foreground and backend models with a controlled mock environment. Keep task prompts natural and user-oriented: do not tell the foreground which orchestration tool to call. Ground required actions in environment facts that the model cannot legitimately know without execution, such as a per-attempt hidden value or state change.

Represent single-turn cases using existing prompt support where possible. Add optional scenario steps for multi-turn cases: send text, await a structured event, release a fixture gate, and assert state. Use logical aliases bound to observed task IDs rather than hardcoded IDs. Validation should reject ambiguous bindings and unsupported step combinations before inference begins.

Gates belong at observable tool/environment boundaries. They control when a tool can return or an effect can occur; they do not force the foreground to delegate. If the model never creates the necessary task, the runner records that outcome rather than creating one itself. To establish a true pre-start queue case, occupy the backend with another scenario task and verify the target remains queued. Do not describe a blocked tool as a backend that has not started.

### Required coverage matrix

| Case | Controlled setup and interaction | Required checks |
| --- | --- | --- |
| Direct answer | Simple conversation requiring no external work | Useful answer; no unnecessary backend task |
| Mandatory delegation | Request a hidden environment fact or external state change | Task created, backend executes, verified result reaches user |
| No-delegation false success | External task with effect initially absent | Any completed-work claim without task/effect evidence fails; missing delegation is recorded |
| Queued task false success | Keep task A executing, then request B | B is queued; foreground must not claim B is complete before B starts or achieves its effect |
| Running task false success | Hold a tool before it performs its effect | No claim that the held effect is complete; final feedback follows verified execution |
| Submission is not completion | Tool acknowledges an asynchronous operation before the effect is available | Foreground does not equate accepted/submitted with completed |
| Backend failure | Return a controlled business/tool failure | No false success; accurate failure and next-step communication |
| Result relay | Backend reads a per-attempt hidden result | Foreground relays the observed value accurately, without runner prompting |
| Progress query | Ask for status while work is blocked | References existing work; no duplicate execution or completion claim |
| Repeated request | Rephrase the same pending request | Reuses relevant task; does not duplicate the external action |
| Goal replacement | Change target while original task is executing | Correct task/revision updated; retained constraints survive; obsolete goal is not reported as success |
| Multiple goal changes | Apply successive revisions at controlled boundaries | Latest applied goal governs subsequent action and feedback; intermediate results are attributed correctly |
| Cancel queued work | Cancel B while A occupies the backend | B never executes; no late success feedback for B |
| Cancel running work | Cancel before a gated effect | Cancellation settles; no effect occurs after acknowledged cancellation; no false success |
| Partial completion then cancel | Permit one reversible effect, block the next, then cancel | Reports what already happened; does not promise rollback unless verified |
| User assistance | Backend requests a specific user action; script later confirms it | Foreground explains the need; correct task resumes once; no premature completion |
| Assistance ambiguity | Two tasks need distinct actions; user response names one | Only the identified task resumes; ambiguous confirmation prompts clarification |
| Conversation during work | Send an unrelated question while a backend task is running | Foreground responds; task continues; result is eventually reported |
| Multiple independent tasks | Request A and B with distinguishable results | Separate attribution; both outcomes delivered; no lost or duplicate effects |
| Mixed outcomes | A succeeds and B fails | No blanket success; reports each outcome accurately |
| Result and new-turn overlap | Send new user input at a controlled result-delivery boundary | No dropped, duplicated, or misattributed feedback |
| Old-result suppression | Update/cancel work while its earlier result is pending delivery | Stale result is not presented as completion of the current goal |

Ordering cases must respect the production scheduler. Do not assume parallel backend execution or force out-of-order completion if the task manager is serial. Use observable delivery boundaries to test interleaving and batching.

For every negative case, retain a positive counterpart: a valid completion claim after verified effect must pass. Use small parameterized fixtures for state combinations instead of near-duplicate suites.

After the controlled suite is stable, run a compatible subset against a simulator or real device. Existing suites that require direct foreground image attachments, backend-specific setup prompts, or backend-only history need explicit compatibility review; do not switch all suites automatically.

## Scoring and reporting

Report separate dimensions:

- Verified task/effect success.
- Delegation and task-control correctness.
- Unsupported completion claim rate, including a dedicated before-backend-start category.
- Result injection and final feedback coverage.
- Result fidelity and task attribution.
- Duplicate external effects and obsolete-goal actions.
- Foreground first-feedback, delegation, backend execution, result-injection, and final-feedback latency.

Define denominators in the report: completion-claim rates use attributable claims; missing-feedback rates use results requiring delivery; scenario pass rate includes all required assertions. Publish ambiguous/unscorable counts alongside scores. Do not hide early false claims behind final task success or a single averaged quality score.

Structural ordering, task identity, revisions, and environment effects use deterministic assertions. Language interpretation uses a judge with explicit evidence references. Missing evidence and provider/environment failures receive separate classifications. A critical false completion fails the relevant scenario even when final environment state is correct.

A report should answer: what did the user ask, what did the foreground claim, what had the backend actually done at that point, what changed in the environment, and where did the result-delivery chain stop?

For model comparisons, hold the existing backend main model and environment fixed and vary only the foreground realtime model. Reuse compatible tasks against the backend-direct baseline to distinguish execution failures from additional coordination failures. Keep audio costs, when incurred by the provider, visible rather than claiming these are text-only billing measurements.

## Implementation sequence after approval

1. **Runtime and observability:** foreground configuration, capability checks, no-device text execution, structured correlation and result-response evidence. Preserve current backend configuration and HTTP completion semantics.
2. **Runner and artifacts:** scenario lifecycle, bounded event waits, isolation/cleanup, evidence persistence, completion rules, and the merged report timeline.
3. **Initial critical suite:** direct answer, mandatory delegation, no-delegation/queued/running false success, verified success, controlled failure, and unsolicited result relay.
4. **Complex collaboration suite:** progress/repeated requests, goal changes, cancellation, user assistance, multiple tasks, and result/input overlaps from the coverage matrix.
5. **Operational integration:** foreground selection in existing configuration entry points, manual/scheduled CI, comparable reports, and a small device smoke subset.

The initial slice is not acceptance of the full feature. Acceptance requires the complex collaboration coverage, particularly changes of goal and multiple tasks.

## Validation and acceptance

Extend existing meaningful tests for event correlation, scenario completion, causal ordering, goal revisions, no-device startup, isolation, and cleanup. Use synthetic events to prove that a chat acknowledgement does not finish a delegated scenario, that a missing final response times out at the right stage, and that an early false claim cannot be repaired by later task success. Avoid source-text and static suite-count tests.

Validate claim scoring with labeled examples covering acknowledgements, tentative/progress statements, partial completion, explicit false completion, corrections, and ambiguous task references. Judge-unavailable behavior must preserve an unscored result. Exercise actual providers in separately invoked benchmark runs; deterministic tests cannot establish model collaboration quality.

Acceptance requires:

- Only foreground realtime configuration is added; the existing main model remains the backend.
- Both agents and task management use production execution paths.
- A complete timeline separates backend terminal state, result injection, and user-visible feedback.
- Premature completion before backend start is detected even if the task succeeds later.
- Goal updates and multiple tasks are attributed and evaluated correctly.
- No physical audio hardware is required, and no speech-quality claims are made.
- Reports expose missing evidence and incomplete delivery explicitly.
- Timed-out scenarios cannot leave tasks operating an environment after its lease is released.
- Existing backend-direct benchmark behavior remains compatible.

Repository validation should follow the pinned Docker workflow described in [the testing guide](../01-getting-started/testing.md). Actual model evaluations require configured credentials supplied through the existing secret/configuration mechanisms, never committed into fixtures or reports.

## Current implementation and operational limits

From `benchmark/`, `uv run --locked python -m runner run --execution-mode realtime-text --auto-agent-setup --suite suites/realtime_collaboration_v1.json` keeps one isolated daemon alive for each scenario attempt. The daemon uses the configured realtime provider and production task manager; its main model remains the backend. The controlled `phone_data_v1` fixture intercepts only clipboard and notification tools and is protected by a per-run control token. Ordinary benchmark runs remain `backend-text` by default.

Foreground provider selection is supplied with `--realtime-provider`, `--realtime-model`, and `AICO_BENCHMARK_REALTIME_API_KEY`, or configured in the existing Agent TOML. Optional CLI flags `--realtime-endpoint`, `--realtime-protocol`, `--realtime-upstream-provider`, and `--realtime-auth-mode` populate the selected foreground provider record. Leave model unset to use the provider's default; providers requiring an explicit model still reject incomplete configuration. Other provider-specific settings belong in the existing TOML/provider record rather than backend configuration. The realtime path must fail preflight instead of falling back to backend-direct execution. The WebUI's legacy shared-daemon device and MobileGym paths are not an isolation contract for this mode; run the managed CLI/CI path.

For an existing TOML that already configures realtime with backend delegation enabled, use `--agent-config /path/to/agent.toml` and leave foreground provider/model flags unset. Otherwise retain the current backend configuration and add only the foreground selection:

```bash
cd benchmark
# Supply AICO_BENCHMARK_REALTIME_API_KEY through your secret manager/environment.
uv run --locked python -m runner run \
  --suite suites/realtime_collaboration_v1.json \
  --execution-mode realtime-text \
  --auto-agent-setup \
  --realtime-provider openai \
  --realtime-model YOUR_REALTIME_MODEL
```

Backend and judge credentials use the existing benchmark configuration. `--no-judge` records evidence but cannot pass semantic completion-claim or feedback checks. CI selection uses the existing workflow's suite input, with `BENCHMARK_REALTIME_PROVIDER`, `BENCHMARK_REALTIME_MODEL`, and the `BENCHMARK_REALTIME_API_KEY` secret; the catalog excludes this case from automatic runnable selection. WebUI execution is explicitly rejected until that entry point has its own configuration and isolation contract.

### Optional CI foreground routing

These GitHub Actions **Variables** are forwarded through the runtime environment into the existing realtime provider record. Empty values are omitted, leaving the provider's defaults intact.

| CI Variable | CLI flag | TOML field |
| --- | --- | --- |
| `BENCHMARK_REALTIME_ENDPOINT` | `--realtime-endpoint` | `endpoint` |
| `BENCHMARK_REALTIME_PROTOCOL` | `--realtime-protocol` | `realtime_protocol` |
| `BENCHMARK_REALTIME_UPSTREAM_PROVIDER` | `--realtime-upstream-provider` | `upstream_provider` |
| `BENCHMARK_REALTIME_AUTH_MODE` | `--realtime-auth-mode` | `auth_mode` |

For an OpenAI-compatible legacy gateway, set `BENCHMARK_REALTIME_PROVIDER=openai`, `BENCHMARK_REALTIME_MODEL=gpt-realtime-2`, `BENCHMARK_REALTIME_ENDPOINT=wss://api.mixroute.ai/v1/realtime`, and `BENCHMARK_REALTIME_PROTOCOL=legacy`. Store its key only in Secret `BENCHMARK_REALTIME_API_KEY`. Endpoint Variables must not contain credentials. Leave upstream/auth fields empty for this example. Clear provider-specific Variables when changing providers; these fields are explicit overrides, not inferred from the endpoint. Gemini API-key authentication can leave auth mode empty or set `api_key`; Speko requires its supported upstream selection and an explicit model.

When supplying a realtime-configured `--agent-config`, leave all foreground CLI/environment overrides unset: mixing both configuration sources is rejected rather than silently replacing routing or backend settings. Backend model and judge configuration remain unchanged.

Events are persisted in `collaboration-events.jsonl`; the captured scenario, fixture, evaluation, summary and merged timeline are stored alongside the normal attempt artifacts. Offline rejudging re-evaluates saved evidence and does not execute tools. A failed or incomplete recorder is unscored; an early unsupported completion claim remains a failure after later success.

Validation completed to date includes focused Python tests and host Go tests for `agenttask`, `stream`, and the realtime provider event bridge. The pinned Docker-backed repository check could not run in the current environment because its Docker daemon was unavailable. No live realtime provider or device evaluation has been run. Provider-specific text/tool behavior, full scenario quality, the remaining coverage matrix, and real-provider cost accounting still require operational validation.
