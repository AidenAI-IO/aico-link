from __future__ import annotations
import json
import urllib.error
import urllib.request
from typing import Any
from runner.agent_client import (
    AgentClient,
    AgentRequestError,
    AgentSemanticError,
    AgentTimeoutError,
)
from runner.environment_endpoint import EnvironmentEndpoint
from runner.suite import SETUP_KEYS, SuiteValidationError, validate_assert_memory_setup


class ResetError(RuntimeError):
    pass


class SetupAssertionError(ResetError):
    """A setup assertion observed a benchmark capability mismatch."""


STALE_ADB_OWNER_LEASE_STATES = {"expired", "abandoned"}


def _environment_endpoint(environment_url: str) -> EnvironmentEndpoint:
    try:
        return EnvironmentEndpoint(environment_url)
    except ValueError as exc:
        raise ResetError(str(exc)) from exc


def _environment_headers(task_id: str | None = None) -> dict[str, str]:
    headers = {"Content-Type": "application/json"}
    task_id = str(task_id or "").strip()
    if task_id:
        headers["benchmark-task-id"] = task_id
    return headers


def _post_environment(
    endpoint: str,
    *,
    timeout: int,
    headers: dict[str, str],
    action: str,
    payload: dict[str, Any] | None = None,
) -> dict[str, Any]:
    req = urllib.request.Request(
        endpoint,
        data=json.dumps(payload or {}, ensure_ascii=False).encode("utf-8"),
        headers=headers,
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body = resp.read()
    except urllib.error.HTTPError as e:
        try:
            body = e.read()
        except Exception:
            body = b""
        raise ResetError(f"{action} endpoint failed HTTP {e.code}: {body[:200]!r}") from e
    except urllib.error.URLError as e:
        raise ResetError(f"{action} endpoint request failed: {e}") from e
    except TimeoutError as e:
        raise ResetError(f"{action} endpoint timed out: {e}") from e
    try:
        payload = json.loads(body.decode("utf-8")) if body else {}
    except (UnicodeDecodeError, json.JSONDecodeError) as e:
        raise ResetError(f"{action} endpoint returned invalid JSON: {body[:200]!r}") from e
    if isinstance(payload, dict) and payload.get("ok") is False:
        raise ResetError(f"{action} endpoint failed: {payload.get('error') or payload}")
    return payload if isinstance(payload, dict) else {}


def call_environment_setup(
    environment_url: str,
    timeout: int = 30,
    task_id: str | None = None,
    app_ids: list[str] | None = None,
    foreground_app_id: str | None = None,
) -> dict[str, Any]:
    return _post_environment(
        _environment_endpoint(environment_url).setup,
        timeout=timeout,
        headers=_environment_headers(task_id),
        action="setup",
        payload={
            "app_ids": list(app_ids or []),
            **({"foreground_app_id": foreground_app_id} if foreground_app_id else {}),
        },
    )


def call_environment_release(environment_url: str, timeout: int = 30, task_id: str | None = None) -> dict[str, Any]:
    return _post_environment(
        _environment_endpoint(environment_url).release,
        timeout=timeout,
        headers=_environment_headers(task_id),
        action="release",
    )


def clear_stale_adb_android_owner(environment_url: str, timeout: float = 2.0) -> str:
    """Release a leftover ADB Android bridge owner before a fresh benchmark run."""
    req = urllib.request.Request(_environment_endpoint(environment_url).health, method="GET")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body = resp.read()
    except urllib.error.URLError:
        return ""
    except TimeoutError:
        return ""
    try:
        payload = json.loads(body.decode("utf-8")) if body else {}
    except (UnicodeDecodeError, json.JSONDecodeError):
        return ""
    if not isinstance(payload, dict):
        return ""
    data = payload.get("data") if isinstance(payload.get("data"), dict) else payload
    if not isinstance(data, dict):
        return ""
    if str(data.get("bridge_type") or "").strip().lower() != "adb_android":
        return ""
    active_task_id = str(data.get("active_task_id") or "").strip()
    if not active_task_id:
        return ""
    lease_state = str(data.get("active_task_lease_state") or "").strip().lower()
    if lease_state not in STALE_ADB_OWNER_LEASE_STATES:
        return ""
    try:
        call_environment_release(environment_url, timeout=max(1, int(timeout)), task_id=active_task_id)
    except ResetError:
        return ""
    return active_task_id


def per_task_setup(
    client: AgentClient,
    setup: dict[str, Any] | list[dict[str, Any]] | None,
    *,
    prompt_prefix: str = "",
    consolidation_expectation: Any = None,
) -> dict[str, Any] | None:
    if setup is None:
        return
    if isinstance(setup, list):
        if not setup:
            raise ResetError("setup sequence must contain at least one setup")
        expectation_indexes = [
            index
            for index, item in enumerate(setup)
            if isinstance(item, dict)
            and item.get("type") == "seed_episode"
            and item.get("consolidation_expectation") is not None
        ]
        expectation_index = expectation_indexes[0] if expectation_indexes else None
        if consolidation_expectation is not None and expectation_index is None:
            seed_episode_indexes = [
                index
                for index, item in enumerate(setup)
                if isinstance(item, dict) and item.get("type") == "seed_episode"
            ]
            if len(seed_episode_indexes) != 1:
                raise ResetError(
                    "task-level consolidation_expectation requires exactly one seed_episode setup"
                )
            expectation_index = seed_episode_indexes[0]
        result: dict[str, Any] | None = None
        for index, item in enumerate(setup):
            if not isinstance(item, dict):
                raise ResetError(f"setup[{index}] must be an object")
            try:
                item_result = per_task_setup(
                    client,
                    item,
                    prompt_prefix=prompt_prefix,
                    consolidation_expectation=(
                        consolidation_expectation
                        if index == expectation_index
                        else None
                    ),
                )
                if item_result is not None and (
                    result is None or index == expectation_index
                ):
                    result = item_result
            except SetupAssertionError as e:
                raise SetupAssertionError(f"setup[{index}] failed: {e}") from e
            except ResetError as e:
                raise ResetError(f"setup[{index}] failed: {e}") from e
        return result
    if not isinstance(setup, dict):
        raise ResetError(f"setup must be an object or an array: {setup!r}")
    setup_type = setup.get("type")
    if not isinstance(setup_type, str):
        raise ResetError(f"setup type must be a string: {setup!r}")
    allowed_keys = SETUP_KEYS.get(setup_type)
    if allowed_keys is None:
        raise ResetError(f"unsupported setup form: {setup!r}")
    unknown_keys = sorted(set(setup) - allowed_keys)
    if unknown_keys:
        raise ResetError(f"unsupported {setup_type} setup keys: {', '.join(unknown_keys)}")
    if setup_type == "agent_prompt":
        _per_task_setup_agent_prompt(client, setup, prompt_prefix=prompt_prefix)
        return
    if setup_type == "seed_memory":
        _per_task_setup_seed_memory(client, setup)
        return
    if setup_type == "seed_episode":
        if (
            (setup.get("consolidation_expectation") is not None or consolidation_expectation is not None)
            and setup.get("consolidate", False) is not True
        ):
            raise ResetError("seed_episode consolidation_expectation requires consolidate=true")
        return _per_task_setup_seed_episode(
            client,
            setup,
            consolidation_expectation=consolidation_expectation,
        )
    if setup_type == "seed_session_chunk":
        _per_task_setup_seed_session_chunk(client, setup)
        return
    if setup_type == "seed_notification":
        _per_task_setup_seed_notification(client, setup)
        return
    if setup_type == "assert_memory":
        _per_task_setup_assert_memory(client, setup)
        return
    raise ResetError(f"unsupported setup form: {setup!r}")


def _per_task_setup_agent_prompt(client: AgentClient, setup: dict[str, Any], *, prompt_prefix: str = "") -> None:
    prompt = setup.get("prompt")
    if not prompt:
        raise ResetError(f"agent_prompt setup missing prompt: {setup!r}")
    prefix = str(prompt_prefix or "").strip()
    if prefix:
        prompt = f"{prefix}\n\n{prompt}"
    try:
        timeout = int(setup.get("timeout_sec", 90))
    except (ValueError, TypeError) as e:
        raise ResetError(f"invalid timeout_sec: {setup.get('timeout_sec')!r}") from e
    try:
        result = client.chat(prompt, timeout_sec=timeout)
    except AgentTimeoutError as e:
        raise ResetError(f"setup agent_prompt timed out: {e}") from e
    except AgentRequestError as e:
        raise ResetError(f"setup agent_prompt failed: {e}") from e
    expected_response = setup.get("expected_response")
    if expected_response is not None:
        if not isinstance(expected_response, str) or not expected_response:
            raise ResetError(f"expected_response must be a non-empty string: {expected_response!r}")
        actual_response = str(getattr(result, "response", "") or "").strip()
        if actual_response != expected_response:
            raise ResetError(
                f"setup agent_prompt response mismatch: expected {expected_response!r}, got {actual_response!r}"
            )
    clear_history_after = setup.get("clear_history_after", True)
    if not isinstance(clear_history_after, bool):
        raise ResetError(f"clear_history_after must be boolean: {clear_history_after!r}")
    if clear_history_after:
        try:
            client.clear_history()
        except AgentRequestError as e:
            raise ResetError(f"setup agent_prompt clear_history failed: {e}") from e


def _per_task_setup_seed_memory(client: AgentClient, setup: dict[str, Any]) -> None:
    memories = setup.get("memories")
    if not isinstance(memories, list) or not memories:
        raise ResetError(f"seed_memory setup requires non-empty 'memories' list: {setup!r}")
    try:
        timeout = int(setup.get("timeout_sec", 30))
    except (ValueError, TypeError) as e:
        raise ResetError(f"invalid timeout_sec: {setup.get('timeout_sec')!r}") from e
    for index, memory in enumerate(memories):
        if not isinstance(memory, dict):
            raise ResetError(f"seed_memory memories[{index}] must be a dict, got {type(memory).__name__}")
        memory_id = memory.get("id")
        if not isinstance(memory_id, str) or not memory_id.strip():
            raise ResetError(f"seed_memory memories[{index}] missing required 'id'")
        if not isinstance(memory.get("content"), str) or not memory["content"].strip():
            raise ResetError(f"seed_memory memories[{index}] (id={memory_id!r}) missing required 'content'")
        try:
            client.seed_memory(memory, timeout=timeout)
        except AgentTimeoutError as e:
            raise ResetError(f"seed_memory timed out at index {index} (id={memory_id!r}): {e}") from e
        except AgentRequestError as e:
            raise ResetError(f"seed_memory failed at index {index} (id={memory_id!r}): {e}") from e
    if setup.get("clear_history_after"):
        try:
            client.clear_history()
        except AgentRequestError as e:
            raise ResetError(f"seed_memory clear_history failed: {e}") from e


def _per_task_setup_seed_session_chunk(client: AgentClient, setup: dict[str, Any]) -> None:
    session_id = setup.get("session_id")
    summary = setup.get("summary")
    messages = setup.get("messages")
    if not isinstance(session_id, str) or not session_id.strip():
        raise ResetError(f"seed_session_chunk setup missing session_id: {setup!r}")
    if not isinstance(summary, str) or not summary.strip():
        raise ResetError(f"seed_session_chunk setup missing summary: {setup!r}")
    if not isinstance(messages, list) or not messages:
        raise ResetError(f"seed_session_chunk setup requires non-empty messages list: {setup!r}")
    for index, message in enumerate(messages):
        if not isinstance(message, dict):
            raise ResetError(f"seed_session_chunk messages[{index}] must be a dict")
        if not isinstance(message.get("role"), str) or not message["role"].strip():
            raise ResetError(f"seed_session_chunk messages[{index}] missing role")
        if not isinstance(message.get("content"), str) or not message["content"].strip():
            raise ResetError(f"seed_session_chunk messages[{index}] missing content")
    try:
        timeout = int(setup.get("timeout_sec", 30))
    except (ValueError, TypeError) as e:
        raise ResetError(f"invalid timeout_sec: {setup.get('timeout_sec')!r}") from e
    chunk = {
        "session_id": session_id,
        "summary": summary,
        "messages": messages,
    }
    try:
        client.seed_session_chunk(chunk, timeout=timeout)
    except AgentTimeoutError as e:
        raise ResetError(f"seed_session_chunk timed out for {session_id!r}: {e}") from e
    except AgentRequestError as e:
        raise ResetError(f"seed_session_chunk failed for {session_id!r}: {e}") from e
    if setup.get("clear_history_after", False):
        try:
            client.clear_history()
        except (AgentTimeoutError, AgentRequestError) as e:
            raise ResetError(f"seed_session_chunk clear_history failed: {e}") from e

    # Check availability independently of the Agent's query selection. A setup
    # failure must not count as an Agent recall failure.
    try:
        recalled = client.invoke_tool("recall_session_chunks", {"limit": 100}, timeout=timeout)
    except (AgentTimeoutError, AgentRequestError) as e:
        raise ResetError(f"seed_session_chunk verification failed: {e}") from e
    if recalled.is_error:
        raise ResetError("seed_session_chunk verification returned a tool error")
    try:
        payload = json.loads(recalled.output)
    except (TypeError, json.JSONDecodeError) as e:
        raise ResetError(f"seed_session_chunk verification returned invalid JSON: {e}") from e
    results = payload.get("results") if isinstance(payload, dict) else None
    if not isinstance(results, list) or not any(
        isinstance(item, dict)
        and item.get("session_id") == session_id.strip()
        and item.get("summary") == summary.strip()
        for item in results
    ):
        raise ResetError(f"seed_session_chunk verification could not recall the seeded summary for {session_id!r}")


def _per_task_setup_seed_episode(
    client: AgentClient,
    setup: dict[str, Any],
    *,
    consolidation_expectation: Any = None,
) -> dict[str, Any]:
    episode = setup.get("episode")
    if not isinstance(episode, dict):
        raise ResetError(f"seed_episode setup requires an 'episode' object: {setup!r}")
    episode_id = episode.get("id")
    if not isinstance(episode_id, str) or not episode_id.strip():
        raise ResetError("seed_episode episode missing required 'id'")
    if not isinstance(episode.get("user_goal"), str) or not episode["user_goal"].strip():
        raise ResetError(f"seed_episode episode {episode_id!r} missing required 'user_goal'")
    consolidate = setup.get("consolidate", False)
    if not isinstance(consolidate, bool):
        raise ResetError(f"seed_episode consolidate must be boolean: {consolidate!r}")
    try:
        timeout = int(setup.get("timeout_sec", 90 if consolidate else 30))
    except (ValueError, TypeError) as e:
        raise ResetError(f"invalid timeout_sec: {setup.get('timeout_sec')!r}") from e
    try:
        client.seed_episode(episode, timeout=timeout)
    except AgentTimeoutError as e:
        raise ResetError(f"seed_episode timed out for {episode_id!r}: {e}") from e
    except AgentRequestError as e:
        raise ResetError(f"seed_episode failed for {episode_id!r}: {e}") from e
    if not consolidate:
        return {"type": "seed_episode", "episode_id": episode_id, "consolidated": False}
    try:
        result = client.process_episode_memory(episode_id, timeout=timeout)
    except AgentTimeoutError as e:
        raise ResetError(f"episode memory consolidation timed out for {episode_id!r}: {e}") from e
    except AgentRequestError as e:
        raise ResetError(f"episode memory consolidation failed for {episode_id!r}: {e}") from e
    expectation = setup.get("consolidation_expectation")
    if consolidation_expectation is not None:
        expectation = vars(consolidation_expectation)

    def fail(message: str) -> None:
        error = ResetError(message)
        error.consolidation = result
        raise error

    status = str(result.get("status") or "").strip().lower()
    expected_status = (expectation or {}).get("expected_status") if isinstance(expectation, dict) else None
    if expected_status is not None and status != expected_status:
        fail(f"episode memory consolidation for {episode_id!r} status mismatch: expected {expected_status!r}, got {status or 'missing'!r}")
    if status == "ignored" and expected_status != "ignored":
        fail(f"episode memory consolidation for {episode_id!r} was ignored by the worker")
    if status == "ignored":
        return {"type": "seed_episode", "episode_id": episode_id, "consolidated": True, "consolidation": result}
    if status != "done":
        fail(f"episode memory consolidation for {episode_id!r} did not reach a terminal status: {status or 'missing'}")
    _validate_consolidation_result(episode_id, result, expectation)
    _validate_consolidated_memory_content(client, episode_id, result, expectation, timeout)
    return {"type": "seed_episode", "episode_id": episode_id, "consolidated": True, "consolidation": result}


def _validate_consolidation_result(
    episode_id: str,
    result: dict[str, Any],
    expectation: dict[str, Any] | None,
) -> None:
    def fail(message: str) -> None:
        error = ResetError(message)
        error.consolidation = result
        raise error

    memory_ids = result.get("memory_ids")
    if not isinstance(memory_ids, list) or not all(isinstance(item, str) and item.strip() for item in memory_ids):
        fail(f"episode memory consolidation for {episode_id!r} returned invalid memory_ids")
    memory_count = len(memory_ids)
    if expectation is None:
        if not memory_count:
            fail(f"episode memory consolidation for {episode_id!r} produced no device memory")
        return
    if not isinstance(expectation, dict):
        fail("seed_episode consolidation_expectation must be an object")
    expected_goal = expectation.get("goal_result")
    assessment = result.get("assessment")
    if expected_goal is not None:
        actual_goal = assessment.get("goal_result") if isinstance(assessment, dict) else None
        if actual_goal != expected_goal:
            fail(f"episode memory consolidation for {episode_id!r} goal_result mismatch: expected {expected_goal!r}, got {actual_goal or 'missing'!r}")
    if expectation.get("required_assessment_evidence"):
        refs = assessment.get("evidence_refs") if isinstance(assessment, dict) else None
        if not isinstance(refs, list) or not refs:
            fail(f"episode memory consolidation for {episode_id!r} returned no assessment evidence_refs")
    min_memory_ids = int(expectation.get("min_memory_ids", 0))
    max_memory_ids = expectation.get("max_memory_ids")
    max_memory_ids = None if max_memory_ids is None else int(max_memory_ids)
    if memory_count < min_memory_ids:
        fail(f"episode memory consolidation for {episode_id!r} produced {memory_count} device memories, expected at least {min_memory_ids}")
    if max_memory_ids is not None and memory_count > max_memory_ids:
        fail(f"episode memory consolidation for {episode_id!r} produced {memory_count} device memories, expected at most {max_memory_ids}")
    positive_contract = any(expectation.get(field) for field in ("required_memory_substrings", "required_memory_types", "required_memory_scope"))
    if memory_count == 0 and (positive_contract or not expectation.get("allow_empty_memory", False)):
        fail(f"episode memory consolidation for {episode_id!r} produced no device memory")


def _validate_consolidated_memory_content(
    client: AgentClient,
    episode_id: str,
    result: dict[str, Any],
    expectation: dict[str, Any] | None,
    timeout: int,
) -> None:
    if not isinstance(expectation, dict):
        return
    forbidden = expectation.get("forbidden_memory_substrings", [])
    required_substrings = expectation.get("required_memory_substrings", [])
    required_types = expectation.get("required_memory_types", [])
    required_scope = expectation.get("required_memory_scope", {})
    if not any((forbidden, required_substrings, required_types, required_scope)):
        return
    memory_ids = result.get("memory_ids")
    if not isinstance(memory_ids, list) or not memory_ids:
        if required_substrings or required_types or required_scope:
            error = ResetError(f"episode memory consolidation for {episode_id!r} produced no device memory for the required content/type/scope contract")
            error.consolidation = result
            raise error
        return

    def fail(message: str) -> None:
        error = ResetError(message)
        error.consolidation = result
        raise error

    generated: list[dict[str, Any]] = []
    for memory_id in memory_ids:
        try:
            recalled = client.invoke_tool("recall_device_memory", {"terms": [memory_id], "limit": 5}, timeout=timeout)
        except (AgentTimeoutError, AgentRequestError) as exc:
            fail(f"episode memory consolidation for {episode_id!r} could not inspect memory {memory_id!r}: {exc}")
        if recalled.is_error:
            fail(f"episode memory consolidation for {episode_id!r} could not inspect memory {memory_id!r}")
        try:
            payload = json.loads(recalled.output)
        except (TypeError, json.JSONDecodeError) as exc:
            fail(f"episode memory consolidation for {episode_id!r} returned invalid recall output for memory {memory_id!r}: {exc}")
        matches = payload.get("results") if isinstance(payload, dict) else None
        exact = next((item for item in matches or [] if isinstance(item, dict) and item.get("id") == memory_id), None)
        if exact is None:
            fail(f"episode memory consolidation for {episode_id!r} could not recall generated memory {memory_id!r}")
        generated.append(exact)
    serialized = json.dumps(generated, ensure_ascii=False).casefold()
    leaked = [value for value in forbidden if value.casefold() in serialized]
    if leaked:
        fail(f"episode memory consolidation for {episode_id!r} persisted forbidden value(s): {', '.join(leaked)}")
    required_type_set = {value.strip().casefold() for value in required_types}
    def matches_required(item: dict[str, Any]) -> bool:
        if required_type_set and str(item.get("type") or "").strip().casefold() not in required_type_set:
            return False
        serialized_item = json.dumps(item, ensure_ascii=False).casefold()
        if any(value.casefold() not in serialized_item for value in required_substrings):
            return False
        applicability = item.get("applicability")
        if required_scope and (not isinstance(applicability, dict) or any(str(applicability.get(key) or "") != value for key, value in required_scope.items())):
            return False
        return True
    if (required_substrings or required_types or required_scope) and not any(matches_required(item) for item in generated):
        fail(f"episode memory consolidation for {episode_id!r} produced no single memory matching required type/content/scope")


def _per_task_setup_seed_notification(client: AgentClient, setup: dict[str, Any]) -> None:
    events = setup.get("events")
    if not isinstance(events, list) or not events or len(events) > 100:
        raise ResetError("seed_notification setup requires 1 to 100 events")
    for index, event in enumerate(events):
        if not isinstance(event, dict):
            raise ResetError(f"seed_notification events[{index}] must be an object")
        if not str(event.get("title") or event.get("message") or "").strip():
            raise ResetError(f"seed_notification events[{index}] requires a title or message")
    consolidate = setup.get("consolidate", False)
    if not isinstance(consolidate, bool):
        raise ResetError(f"seed_notification consolidate must be boolean: {consolidate!r}")
    try:
        timeout = int(setup.get("timeout_sec", 90 if consolidate else 30))
    except (ValueError, TypeError) as e:
        raise ResetError(f"invalid timeout_sec: {setup.get('timeout_sec')!r}") from e
    try:
        seeded = client.seed_notification(events, timeout=timeout)
        if not isinstance(seeded, dict):
            raise ResetError("seed_notification returned an invalid response")
        context_ids = seeded.get("context_ids")
        if not isinstance(context_ids, list) or len(context_ids) != len(events):
            raise ResetError(
                "seed_notification did not persist every fixture event: "
                f"expected {len(events)} context ids, got {context_ids!r}"
            )
        if not consolidate:
            return
    except AgentTimeoutError as e:
        raise ResetError(f"notification benchmark setup timed out: {e}") from e
    except AgentRequestError as e:
        raise ResetError(f"notification benchmark setup failed: {e}") from e
    try:
        result = client.process_notification_memory(timeout=timeout)
    except AgentTimeoutError as e:
        raise ResetError(f"notification memory processing timed out: {e}") from e
    except AgentSemanticError as e:
        raise SetupAssertionError(
            f"notification memory processing failed: {e}"
        ) from e
    except AgentRequestError as e:
        raise ResetError(f"notification memory processing failed: {e}") from e
    expected_cursor = str(context_ids[-1]).strip() if context_ids else ""
    cursor = str(result.get("memory_cursor") or "").strip()
    if expected_cursor and cursor != expected_cursor:
        raise SetupAssertionError(f"notification memory cursor mismatch: expected {expected_cursor!r}, got {cursor or 'missing'}")
    memory_ids = result.get("memory_ids")
    if not isinstance(memory_ids, list):
        raise ResetError("notification memory consolidation returned invalid memory_ids")
    expected_count = setup.get("expected_memory_count")
    if expected_count is not None:
        try:
            expected_count = int(expected_count)
        except (ValueError, TypeError) as e:
            raise ResetError(
                f"invalid expected_memory_count: {setup.get('expected_memory_count')!r}"
            ) from e
        if expected_count < 0 or len(memory_ids) != expected_count:
            raise SetupAssertionError(
                "notification memory count mismatch: "
                f"expected {expected_count}, got {len(memory_ids)}"
            )
    expected_scope = str(setup.get("expected_memory_scope") or "").strip().lower()
    if not expected_scope:
        return
    if expected_scope not in {"temporary", "long_term"}:
        raise ResetError(
            f"invalid expected_memory_scope: {setup.get('expected_memory_scope')!r}"
        )
    query = setup.get("expected_memory_query")
    if query is None:
        query = {}
    if not isinstance(query, dict):
        raise ResetError("seed_notification expected_memory_query must be an object")
    query = dict(query)
    try:
        configured_limit = int(query.get("limit", 0))
    except (ValueError, TypeError) as e:
        raise ResetError("seed_notification expected_memory_query limit must be an integer") from e
    query["limit"] = max(20, len(memory_ids), configured_limit)
    try:
        recalled = client.invoke_tool(
            "recall_memory",
            query,
            timeout=timeout,
        )
    except (AgentTimeoutError, AgentRequestError) as e:
        raise ResetError(f"notification memory scope check failed: {e}") from e
    if recalled.is_error:
        raise ResetError("notification memory scope check returned an error")
    try:
        payload = json.loads(recalled.output)
    except (TypeError, json.JSONDecodeError) as e:
        raise ResetError(f"notification memory scope check returned invalid JSON: {e}") from e
    matches = payload.get("results") if isinstance(payload, dict) else []
    scopes_by_id = {
        item.get("id"): str(item.get("memory_scope") or "").strip().lower()
        for item in matches or []
        if isinstance(item, dict) and item.get("id")
    }
    for memory_id in memory_ids:
        actual_scope = scopes_by_id.get(memory_id, "")
        if actual_scope != expected_scope:
            raise SetupAssertionError(
                f"notification memory {memory_id!r} scope mismatch: "
                f"expected {expected_scope!r}, got {actual_scope or 'missing'}"
            )


def _per_task_setup_assert_memory(client: AgentClient, setup: dict[str, Any]) -> None:
    assert_memory_results(read_memory_results(client, setup), setup)


def read_memory_results(client: AgentClient, setup: dict[str, Any]) -> list[dict[str, Any]]:
    try:
        validate_assert_memory_setup(setup)
    except SuiteValidationError as e:
        raise ResetError(str(e)) from e
    query = setup.get("query") or {"limit": 20}
    if not isinstance(query, dict):
        raise ResetError("assert_memory query must be an object")
    try:
        timeout = int(setup.get("timeout_sec", 30))
    except (ValueError, TypeError) as e:
        raise ResetError(f"invalid timeout_sec: {setup.get('timeout_sec')!r}") from e
    try:
        recalled = client.invoke_tool("recall_memory", query, timeout=timeout)
    except (AgentTimeoutError, AgentRequestError) as e:
        raise ResetError(f"assert_memory recall failed: {e}") from e
    if recalled.is_error:
        raise ResetError("assert_memory recall returned an error")
    try:
        payload = json.loads(recalled.output)
    except (TypeError, json.JSONDecodeError) as e:
        raise ResetError(f"assert_memory recall returned invalid JSON: {e}") from e
    results = payload.get("results") if isinstance(payload, dict) else None
    if not isinstance(results, list) or not all(isinstance(item, dict) for item in results):
        raise ResetError("assert_memory recall returned invalid results")
    return results


def assert_memory_results(results: list[dict[str, Any]], setup: dict[str, Any]) -> None:
    expected_count = setup.get("expected_count")
    if expected_count is not None:
        try:
            expected_count = int(expected_count)
        except (ValueError, TypeError) as e:
            raise ResetError(f"invalid expected_count: {setup.get('expected_count')!r}") from e
        if expected_count < 0 or len(results) != expected_count:
            raise SetupAssertionError(
                f"assert_memory count mismatch: expected {expected_count}, got {len(results)}"
            )

    absent_ids = setup.get("absent_ids") or []
    if not isinstance(absent_ids, list) or not all(
        isinstance(item, str) and item.strip() for item in absent_ids
    ):
        raise ResetError("assert_memory absent_ids must be a list of non-empty strings")
    result_ids = {str(item.get("id") or "").strip() for item in results}
    unexpected = [item for item in absent_ids if item.strip() in result_ids]
    if unexpected:
        raise SetupAssertionError(f"assert_memory found absent id(s): {', '.join(unexpected)}")

    expected = setup.get("expected") or []
    if not isinstance(expected, list) or not all(isinstance(item, dict) for item in expected):
        raise ResetError("assert_memory expected must be a list of objects")
    for index, spec in enumerate(expected):
        if not spec:
            raise ResetError(f"assert_memory expected[{index}] must not be empty")
        if not any(_memory_result_matches(item, spec) for item in results):
            raise SetupAssertionError(
                f"assert_memory expected[{index}] did not match any recalled memory: {spec!r}"
            )


def _memory_result_matches(result: dict[str, Any], spec: dict[str, Any]) -> bool:
    for field in ("id", "memory_scope", "type"):
        if field in spec and str(result.get(field) or "").strip().lower() != str(spec[field]).strip().lower():
            return False
    if "revision" in spec:
        try:
            if int(result.get("revision") or 0) != int(spec["revision"]):
                return False
        except (ValueError, TypeError):
            return False
    for field, result_field in (("content_contains", "content"), ("title_contains", "title")):
        if field in spec and str(spec[field]).strip().lower() not in str(result.get(result_field) or "").lower():
            return False
    for field, result_field in (("tags_contains", "tags"), ("entities_contains", "entities")):
        if field not in spec:
            continue
        wanted = spec[field]
        actual = {str(item).strip().lower() for item in result.get(result_field) or []}
        if any(item.strip().lower() not in actual for item in wanted):
            return False
    for field, result_field in (
        ("source_refs_contain", "source_refs"),
        ("evidence_refs_contain", "evidence_refs"),
    ):
        if field not in spec:
            continue
        wanted_refs = spec[field]
        actual_refs = result.get(result_field) or []
        if not isinstance(actual_refs, list):
            return False
        if not all(
            isinstance(item, dict) and _memory_source_ref_matches(actual_refs, item)
            for item in wanted_refs
        ):
            return False
    return True


def _memory_source_ref_matches(
    actual_refs: list[Any], wanted: dict[str, Any]
) -> bool:
    for actual in actual_refs:
        if not isinstance(actual, dict):
            continue
        if "type" in wanted and str(actual.get("type") or "").strip().lower() != str(wanted["type"]).strip().lower():
            continue
        if "id" in wanted and str(actual.get("id") or "").strip() != str(wanted["id"]).strip():
            continue
        event_ids = wanted.get("event_ids_contains")
        if event_ids is not None:
            actual_event_ids = {str(item).strip() for item in actual.get("event_ids") or []}
            if any(str(item).strip() not in actual_event_ids for item in event_ids):
                continue
        return True
    return False
