# Diagnosis of benchmark run 42179744713

Evidence: [workflow and artifacts](https://github.com/AidenAI-IO/aico-link/actions/runs/42179744713), commit `f40698ad4`. The analysis uses `results.jsonl`, `manifest.json`, task traces/episodes, pre/post screenshots, setup records, and per-worker daemon logs. No private credentials or raw model prompts are copied here.

## Measurement validity

52 attempts produced 34 passes, 7 eligible failures, 10 judge errors, and one invalid starting state. The displayed 34/41 (82.9%) is conditional on eligibility; it is not a complete evaluation of all 52 attempts. Notification memory has only one eligible attempt out of eight and cannot support a suite-level capability conclusion.

### Judge availability

All ten HTTP 502 failures occurred between 07:18 and 07:21 UTC across notification-memory and ADB basic. The recorded Judge was `qwen3.6-35b` on the configured Qwen proxy, not the workflow's default model. Later visual judging succeeded. This establishes a concentrated Judge-path availability failure, not a malformed rubric response or an Agent failure. The saved error contains only HTTP status, so it does not establish whether the proxy or its upstream model service originated the failure.

The existing Judge retries transient HTTP errors three times, with short backoff. Repeated unavailable results are correctly excluded from capability scores. However, CI previously treated a complete result set containing judge errors as success. The follow-up change fails the case on `judge_errors=N` while preserving results and publication. Agent failures/timeouts remain distinct from unavailable scoring. Do not retry valid negative verdicts to improve the score; recover judge errors by rejudging saved evidence or a new validation run.

### Invalid camera setup

`files_locate_camera_image` recorded a successful `camera_photo` preparation and a new image, but its pre-screenshot still showed Camera. The recipe sent HOME and returned without confirming the launcher. The independent precondition correctly excluded the attempt. The follow-up change resets Home and waits for launcher text before reporting success. Synthetic tests cover a camera that remains foreground and a delayed transition to Home. This guards against premature success; a fresh emulator run is still needed to validate the fix on the original device.

CI also fails cases with explicit `preconditions_passed=false` rather than hiding this invalid measurement behind complete artifacts. Coverage remains necessary: this does not turn every platform skip into an infrastructure failure.

### Other preparation failures

- The device-information recipe failed in all three tasks using it. Its UIAutomator confirmation timed out; agent fallback took 34.8–66.4 seconds per task. Fallback screenshots and task outcomes subsequently passed. The artifact does not contain the UI hierarchy at the recipe failure, so it cannot distinguish navigation failure from an inaccessible/unstable hierarchy. Do not fix this by merely extending the timeout or assuming Back-stack behavior.
- `files_preview_camera_image` could not find the newly captured filename in the Files folder within 30 seconds. Agent fallback made the complete setup take 115.2 seconds, after which the measured task took only 8.2 seconds and passed. The missing recipe-time hierarchy prevents attributing this specifically to folder navigation, media indexing, or filename rendering.

Both need recipe-time UI/focus evidence on the emulator before further changes.

## Eligible failures and slow Agent behavior

| Task | Observed cause | Interpretation |
| --- | --- | --- |
| `screenshot_home` | No screenshot/tool call; request history contains only user and assistant text. | Fails the explicit screenshot-tool contract. Do not grant success based on a plausible description. The artifact does not expose the entire model prompt, so it does not prove the source of the description. |
| `notification_round_trip` | After observing notifications, Agent invoked `quick_action(home)` instead of dismissing the panel back to Settings. | Genuine wrong-action failure; post-screenshot is Home. |
| `return_to_settings_via_recents` | Searched Settings for Clock, navigated repeatedly through recents, and ended on Clock's settings/app-information surface. | Did not complete the requested app-switching journey; final success claim is unsupported. |
| `search_ascii_with_spaces` | `enter_text` returned `ok=true`, but the final search field was empty. Final reply both claimed entry and acknowledged it was not visible. | Outcome failure is supported by the image. Possible text-input verification/transition defect; not evidence that the benchmark should accept the action-output boolean. |
| `contacts_search_absent_name` | Four `enter_text` calls failed probe cleanup verification: the probe character remained visible and cleanup was considered unsafe. A keyboard call also supplied multiple non-modifier keys, which the bridge rejects. Agent handed off. | Agent/tool-stack failure, not a Judge failure. Need intermediate field/keyboard evidence to separate bad focus from cleanup-verifier error. |
| `contacts_edit_benchmark_contact` | Valid initial contact; 45 recorded calls, repeated input/undo/scroll cycles, about 300 seconds executing, no final answer. | Real non-completion. The post-run call-budget assertion rejects it; `max_tool_calls` is not a live stop mechanism. Do not claim the 30-call budget stopped execution. |
| `notification_user_fact_beats_public_info` | Notification consolidation returned HTTP 422: proposal `actions.content` was an object where the Agent parser expected a string. | Evaluated model-processing behavior failure, not environment seeding or Judge transport failure. The exact model output is not retained in the result, so parser-versus-prompt responsibility needs further Agent-side evidence. |

The text-input implementation uses visual `TargetMatched` verification; probe cleanup can refuse unsafe deletion. These paths explain the observed errors but do not prove the visual model's internal decision was correct. Avoid changing scoring or input safety based on a single aggregate result.

## Where the time went

The execution job lasted 56m28s. Four ADB cases share one emulator and are serialized:

| Case | Duration |
| --- | ---: |
| notification-memory (parallel) | 81.0 s |
| ADB basic | 800.7 s |
| ADB text editing | 193.1 s |
| ADB text editing + AidenBridge | 222.7 s |
| ADB built-in apps | 2030.9 s |

Built-in apps account for about 63% of the serialized ADB duration. Its task intervals break down as follows:

- Agent request execution: 1469.3 s (24.5 min).
- Setup: 270.0 s (4.5 min).
- Remaining time inside task intervals: 116.3 s (1.9 min), including screenshot capture, judging, and artifact work. This is not a separately measured Judge total.
- Remaining case time outside those intervals: about 175 s, including worker lifecycle and orchestration.

Inside Agent execution, recorded device-tool durations sum to about 941 s. These are included in, not added to, Agent request time. The largest groups are 33 text-entry calls (393.6 s; mean 11.9 s), 100 gestures (343.2 s; mean 3.4 s), eight app launches (103.8 s), and 28 quick actions (88.7 s). Base/final screenshots and stable-screen waits contribute to each gesture's latency; an example gesture took about 0.7 s for the action plus roughly 2.3 s of observation/waiting.

Contacts edit (320 s), delete (222.3 s), and create (155.9 s) total 11.6 minutes. Delete succeeded but used 28 calls, including repeated focus/input recovery after the contact was already absent. The dominant delay is long action/recovery sequences, not the newly introduced precondition Judge calls: all non-setup/non-Agent task work together is only about 1.9 minutes in this suite.

## Remaining validation before a readiness claim

1. Recover the unscored Judge attempts and inspect notification update/promotion verdicts using persisted-memory evidence.
2. Validate camera-to-Home confirmation on the same emulator; collect UI hierarchy/focus evidence at the device-info and Files recipe failures.
3. Investigate input probe cleanup and false-success evidence in the Agent/tool stack without weakening outcome assertions.
4. Report task-level phase timing/progress live; current case subprocess output is redirected to local files, so Actions only exposes case START/DONE until artifact upload.
5. Keep workflow completion, evaluation coverage, and Agent capability scores separate. Neither green CI nor a conditional pass rate proves all suites are ready.
