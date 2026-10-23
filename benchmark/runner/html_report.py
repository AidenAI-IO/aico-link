"""Generate a self-contained HTML benchmark report with drawer UI."""
from __future__ import annotations
import base64
import html as html_mod
import json
from pathlib import Path
from typing import Any

from runner.agent_client import AgentClient
from runner.report import task_error_details


def _esc(s: str) -> str:
    return html_mod.escape(str(s)) if s else ""


def _image_data_uri(path: Path) -> str:
    try:
        data = path.read_bytes()
    except OSError:
        return ""
    if not data:
        return ""
    if data.startswith(b"\x89PNG\r\n\x1a\n"):
        mime = "image/png"
    elif data.startswith(b"\xff\xd8"):
        mime = "image/jpeg"
    else:
        mime = "image/jpeg"
    encoded = base64.b64encode(data).decode("ascii")
    return f"data:{mime};base64,{encoded}"


def _safe_json_loads(text: Any) -> Any:
    if not isinstance(text, str):
        return None
    try:
        return json.loads(text) if text else None
    except json.JSONDecodeError:
        return None


def _compact_tool_result(raw: str) -> str:
    content = _safe_json_loads(raw)
    if isinstance(content, dict):
        content = dict(content)
        if content.get("data"):
            size = len(str(content.get("data") or ""))
            content["data"] = f"<base64 image data, {size} chars>"
        return json.dumps(content, ensure_ascii=False, indent=2)
    return str(raw or "")


def _safe_child_path(root: Path, *parts: str) -> Path | None:
    try:
        root_resolved = root.resolve()
        candidate = root.joinpath(*parts).resolve()
        candidate.relative_to(root_resolved)
    except (OSError, ValueError):
        return None
    return candidate


def _fmt_time_ms(ms: float | int | str | None) -> str:
    """Format milliseconds as a human-readable time string."""
    if not ms:
        return "0ms"
    try:
        ms_num = float(ms)
    except (TypeError, ValueError, OverflowError):
        # If conversion fails, escape and return the value with ms suffix
        return _esc(str(ms)) + "ms"

    # Check for non-finite values (NaN, Infinity, -Infinity)
    import math
    if not math.isfinite(ms_num):
        return "n/a"

    if ms_num < 1000:
        return f"{int(ms_num)}ms"
    sec = ms_num / 1000
    if sec < 60:
        return f"{sec:.1f}s"
    min_part = int(sec // 60)
    sec_part = sec % 60
    return f"{min_part}m{sec_part:.1f}s"


def _safe_int(value: Any, default: int = 0) -> int:
    try:
        return int(value)
    except (TypeError, ValueError):
        return default


def _read_excerpt(path: Path, max_chars: int = 6000) -> str:
    try:
        return path.read_text("utf-8", errors="replace")[:max_chars]
    except OSError:
        return ""


def _task_artifact_dirs(run_dir: Path, task_id: str) -> list[Path]:
    task_dir = _safe_child_path(run_dir / "tasks", task_id)
    if task_dir is None or not task_dir.is_dir():
        return []
    dirs = [task_dir]
    for path in sorted(task_dir.glob("attempt_*")):
        safe = _safe_child_path(run_dir, str(path.relative_to(run_dir)))
        if safe is not None and safe.is_dir():
            dirs.append(safe)
    return dirs


def _result_artifact_dir(run_dir: Path, row: dict[str, Any], task_dir: Path | None) -> Path | None:
    raw = row.get("artifact_dir")
    if isinstance(raw, str) and raw.strip():
        candidate = Path(raw.strip())
        if not candidate.is_absolute():
            candidate = run_dir / candidate
        try:
            resolved = candidate.resolve()
            resolved.relative_to(run_dir.resolve())
            if resolved.is_dir():
                return resolved
        except (OSError, ValueError):
            pass
    if task_dir is not None:
        try:
            attempt = int(row.get("attempt") or 1)
        except (TypeError, ValueError):
            attempt = 1
        if attempt > 1:
            attempt_dir = task_dir / f"attempt_{attempt}"
            if attempt_dir.is_dir():
                return attempt_dir
        if task_dir.is_dir():
            return task_dir
    return None


FAIL_STATUSES = {"failed", "timeout", "judge_error"}


def _task_artifact_refs(run_dir: Path, task_id: str, artifact_dir: Path | None = None) -> str:
    artifact_refs: list[str] = []
    artifact_dirs = [artifact_dir] if artifact_dir is not None else _task_artifact_dirs(run_dir, task_id)
    for artifact_dir in artifact_dirs:
        for name in ("trace.json", "history.json", "judge.json"):
            path = artifact_dir / name
            if path.exists():
                artifact_refs.append(str(path.relative_to(run_dir)))
        for path in sorted(artifact_dir.glob("**/*")):
            if path.suffix.lower() in {".jpg", ".jpeg", ".png", ".webp"}:
                artifact_refs.append(str(path.relative_to(run_dir)))
    return "\n".join(dict.fromkeys(artifact_refs))


def _task_error_log(
    run_dir: Path,
    task_id: str,
    status: str,
    errors: list[list[Any]],
    hard_assertion_failures: list[dict[str, str]],
    artifact_dir: Path | None = None,
) -> str:
    if status not in FAIL_STATUSES and not errors and not hard_assertion_failures:
        return ""
    parts: list[str] = []
    if hard_assertion_failures:
        lines = []
        for item in hard_assertion_failures:
            label = item.get("label") or item.get("id") or "hard_assertion"
            requirement = item.get("requirement") or ""
            actual = item.get("actual") or ""
            lines.append(f"- {label}\n  Requirement: {requirement}\n  Actual: {actual}")
        parts.append("### Hard assertion failures\n" + "\n".join(lines))
    if errors:
        lines = [f"- {item[0]}: {item[1]}" for item in errors]
        parts.append("### Runtime errors\n" + "\n".join(lines))
    return "\n\n".join(part for part in parts if part.strip())


def _analysis_html(run_dir: Path) -> str:
    md = run_dir / "llm_analysis.md"
    err = run_dir / "llm_analysis_error.txt"
    if md.exists():
        text = md.read_text("utf-8")[:20000]
        return f'<section class="analysis"><h2>LLM 分析</h2><pre>{_esc(text)}</pre></section>'
    if err.exists():
        text = err.read_text("utf-8")[:4000]
        return f'<section class="analysis warning"><h2>LLM 分析</h2><pre>{_esc(text)}</pre></section>'
    return ""


def analysis_html_for_run_dir(run_dir: Path) -> str:
    return _analysis_html(run_dir)


def _capability_metrics_html(run_dir: Path) -> str:
    """Render the P0 capability metrics when the run has a metrics artifact."""
    metrics_path = run_dir / "metrics.json"
    try:
        payload = json.loads(metrics_path.read_text("utf-8"))
    except (OSError, json.JSONDecodeError):
        return ""
    aggregate = payload.get("aggregate") if isinstance(payload, dict) else None
    if not isinstance(aggregate, dict):
        return ""

    k = aggregate.get("metrics_k") or payload.get("metrics_k") or 1

    def rate(metric_name: str) -> tuple[str, str]:
        metric = aggregate.get(metric_name)
        if not isinstance(metric, dict) or metric.get("value") is None:
            return "n/a", "coverage n/a"
        value = float(metric["value"]) * 100
        value_text = f"{value:.1f}%"
        eligible = metric.get("eligible_tasks")
        total = metric.get("total_tasks")
        coverage = (
            f"coverage {eligible}/{total} tasks"
            if eligible is not None and total is not None
            else "coverage n/a"
        )
        return value_text, coverage

    def fmt_num(value: Any) -> str:
        """Format numbers - use integers for counts, decimals for rates."""
        if value is None:
            return "n/a"
        if isinstance(value, float):
            # If the value is a whole number (count), show as integer
            if value == int(value):
                return str(int(value))
            # Otherwise show with appropriate decimal places
            return f"{value:.1f}" if value >= 10 else f"{value:.2f}"
        return str(value)

    def fmt_count(value: Any) -> str:
        """Format count metrics as integers (always round, never show decimals)."""
        if value is None:
            return "n/a"
        return str(int(round(value)))

    def fmt_time(stat_dict: dict[str, Any] | None) -> str:
        """Format time from milliseconds using human-readable format."""
        if not stat_dict or stat_dict.get("p50") is None:
            return "n/a"
        ms = stat_dict['p50']
        return _fmt_time_ms(ms)

    pass_at_1, pass_at_1_detail = rate("pass_at_1")
    pass_at_k, pass_at_k_detail = rate("pass_at_k")
    pass_pow_k, pass_pow_k_detail = rate("pass_pow_k")
    best = aggregate.get("oracle_best_score_at_k")
    if isinstance(best, dict) and best.get("value") is not None:
        best_value = f"{float(best['value']):.2f}"
        best_detail = f"{best.get('eligible_tasks', 0)} eligible tasks"
    else:
        best_value = "n/a"
        best_detail = "diagnostic only"

    # Attempt success rate
    attempt_rate = aggregate.get("attempt_success_rate", {})
    if isinstance(attempt_rate, dict) and attempt_rate.get("value") is not None:
        attempt_value = f"{float(attempt_rate['value']) * 100:.1f}%"
        attempt_detail = f"{attempt_rate.get('successes', 0)}/{attempt_rate.get('eligible_attempts', 0)} eligible attempts"
    else:
        attempt_value = "n/a"
        attempt_detail = "n/a"

    # Cost to first success
    cost_to_first = aggregate.get("cost_to_first_success", {})
    tokens_p50 = (cost_to_first.get("total_tokens") or {}).get("p50")
    wall_p50 = (cost_to_first.get("task_wall_ms") or {}).get("p50")
    cost_p50 = (cost_to_first.get("cost_usd") or {}).get("p50")

    tokens_str = fmt_num(tokens_p50) if tokens_p50 else "n/a"
    wall_str = f"{wall_p50 / 1000:.2f}s" if wall_p50 else "n/a"
    cost_str = f"${cost_p50:.4f}" if cost_p50 else "n/a"

    # Efficiency metrics (counts should be integers)
    tool_calls_p50 = fmt_count((aggregate.get("tool_calls") or {}).get("p50"))
    llm_calls_p50 = fmt_count((aggregate.get("llm_calls") or {}).get("p50"))
    task_wall_p50 = fmt_time(aggregate.get("task_wall_ms"))
    llm_time_p50 = fmt_time(aggregate.get("llm_time_ms"))
    device_exec_p50 = fmt_time(aggregate.get("device_execution_ms"))
    screenshot_p50 = fmt_time(aggregate.get("screenshot_capture_ms"))

    input_tokens = fmt_num((aggregate.get("input_tokens") or {}).get("sum"))
    output_tokens = fmt_num((aggregate.get("output_tokens") or {}).get("sum"))

    # Failure analysis
    failure_classes = aggregate.get("failure_classes", {})
    failure_stages = aggregate.get("failure_stages", {})
    failure_coverage = aggregate.get("failure_stage_coverage", {})

    failure_classes_html = ""
    if failure_classes:
        rows = "".join(f"<tr><td>{_esc(k)}</td><td>{_esc(str(v))}</td></tr>"
                      for k, v in sorted(failure_classes.items()))
        failure_classes_html = f"""
        <div class="metric-section">
          <h3>Failure Classes</h3>
          <p class="metric-desc">Categorization of failed attempts by root cause</p>
          <table class="metric-table">
            <thead><tr><th>Class</th><th>Count</th></tr></thead>
            <tbody>{rows}</tbody>
          </table>
        </div>
        """

    failure_stages_html = ""
    if failure_stages:
        classified = failure_coverage.get("classified", 0)
        eligible = failure_coverage.get("eligible_failures", 0)
        rows = "".join(f"<tr><td>{_esc(k)}</td><td>{_esc(str(v))}</td></tr>"
                      for k, v in sorted(failure_stages.items()))
        failure_stages_html = f"""
        <div class="metric-section">
          <h3>First Failure Stages</h3>
          <p class="metric-desc">Where agent failures occurred in the task execution flow (classified: {classified}/{eligible})</p>
          <table class="metric-table">
            <thead><tr><th>Stage</th><th>Count</th></tr></thead>
            <tbody>{rows}</tbody>
          </table>
        </div>
        """

    # Per-task table
    per_task_data = aggregate.get("per_task", {})
    if per_task_data:
        per_task_rows = []
        for task_id, task_metrics in sorted(per_task_data.items(), key=lambda x: x[1].get("pass_rate", 0)):
            pass_rate = task_metrics.get("pass_rate", 0.0)
            passed = task_metrics.get("passed", 0)
            failed = task_metrics.get("failed", 0)
            eligible = task_metrics.get("eligible_attempts", 0)
            first_passed = task_metrics.get("first_attempt_passed")
            best_score = task_metrics.get("best_quality_score")
            avg_score = task_metrics.get("avg_quality_score")
            avg_wall = task_metrics.get("avg_wall_ms")

            pass_rate_str = f"{pass_rate * 100:.1f}%"
            status_str = f"{passed}/{eligible}"
            first_icon = "✓" if first_passed is True else ("✗" if first_passed is False else "-")
            score_str = f"{avg_score:.2f}" if avg_score is not None else "n/a"
            best_score_str = f"{best_score:.2f}" if best_score is not None else "n/a"
            wall_str = _fmt_time_ms(avg_wall) if avg_wall is not None else "n/a"

            status_class = "passed" if pass_rate == 1.0 else ("failed" if pass_rate == 0.0 else "partial")

            per_task_rows.append(f"""
              <tr class="task-row-{status_class}">
                <td class="task-id">{_esc(task_id)}</td>
                <td class="text-center">{_esc(status_str)}</td>
                <td class="text-center">{_esc(pass_rate_str)}</td>
                <td class="text-center">{first_icon}</td>
                <td class="text-right">{_esc(score_str)}</td>
                <td class="text-right">{_esc(best_score_str)}</td>
                <td class="text-right">{_esc(wall_str)}</td>
              </tr>
            """)

        per_task_section = f"""
<section class="per-task-results">
  <h2>Per-Task Results</h2>
  <p class="section-desc">Individual task performance across k attempts (sorted by pass rate, lowest first). Quality Score = rubric pass count / total rubric items (0-1 scale, higher is better).</p>
  <div class="table-wrap">
    <table class="per-task-table">
      <thead>
        <tr>
          <th>Task ID</th>
          <th class="text-center" title="Number of passed attempts / total eligible attempts">Passed/Total</th>
          <th class="text-center" title="Percentage of attempts that passed">Pass Rate</th>
          <th class="text-center" title="✓ = first attempt passed, ✗ = first attempt failed">1st Attempt</th>
          <th class="text-right" title="Average quality score across all attempts (0-1 scale)">Avg Quality</th>
          <th class="text-right" title="Best quality score among all attempts (0-1 scale)">Best Quality</th>
          <th class="text-right" title="Average wall-clock time per attempt">Avg Latency</th>
        </tr>
      </thead>
      <tbody>
        {''.join(per_task_rows)}
      </tbody>
    </table>
  </div>
</section>
        """
    else:
        per_task_section = ""

    return f"""
<section class="capability-metrics">
  <div class="capability-head">
    <div>
      <h2>Capability Metrics</h2>
      <p>Fixed-k evaluation (k={_esc(str(k))}) — measures task success rates with Wilson 95% confidence intervals</p>
    </div>
    <span class="capability-note">Oracle best score@k is diagnostic; it is not executable best-of-k selection.</span>
  </div>
  <div class="capability-grid">
    <article class="capability-card">
      <div class="label">Pass@1</div>
      <div class="value">{_esc(pass_at_1)}</div>
      <div class="detail">{_esc(pass_at_1_detail)}</div>
      <div class="metric-help">Success rate of first attempt per task</div>
    </article>
    <article class="capability-card">
      <div class="label">Pass@k</div>
      <div class="value">{_esc(pass_at_k)}</div>
      <div class="detail">{_esc(pass_at_k_detail)}</div>
      <div class="metric-help">Prob. of success in k attempts (unbiased)</div>
    </article>
    <article class="capability-card">
      <div class="label">Pass^k</div>
      <div class="value">{_esc(pass_pow_k)}</div>
      <div class="detail">{_esc(pass_pow_k_detail)}</div>
      <div class="metric-help">Empirical k-attempt success rate</div>
    </article>
    <article class="capability-card">
      <div class="label">Oracle best score@k</div>
      <div class="value">{_esc(best_value)}</div>
      <div class="detail">{_esc(best_detail)}</div>
      <div class="metric-help">Best rubric score across k attempts</div>
    </article>
    <article class="capability-card">
      <div class="label">Attempt Success Rate</div>
      <div class="value">{_esc(attempt_value)}</div>
      <div class="detail">{_esc(attempt_detail)}</div>
      <div class="metric-help">Success rate across all eligible attempts</div>
    </article>
  </div>
</section>

<section class="efficiency-metrics">
  <h2>Efficiency Metrics</h2>
  <p class="section-desc">Performance and cost statistics (p50 = median across eligible attempts)</p>

  <div class="efficiency-grid">
    <div class="efficiency-group">
      <h3>Call Counts</h3>
      <div class="metric-row"><span class="metric-label">Tool calls (p50)</span><span class="metric-value">{_esc(tool_calls_p50)} calls</span></div>
      <div class="metric-row"><span class="metric-label">LLM calls (p50)</span><span class="metric-value">{_esc(llm_calls_p50)} calls</span></div>
    </div>

    <div class="efficiency-group">
      <h3>Timing</h3>
      <div class="metric-row"><span class="metric-label">Task wall time (p50)</span><span class="metric-value">{_esc(task_wall_p50)}</span></div>
      <div class="metric-row"><span class="metric-label">LLM time (p50)</span><span class="metric-value">{_esc(llm_time_p50)}</span></div>
      <div class="metric-row"><span class="metric-label">Device exec time (p50)</span><span class="metric-value">{_esc(device_exec_p50)}</span></div>
      <div class="metric-row"><span class="metric-label">Screenshot capture time (p50)</span><span class="metric-value">{_esc(screenshot_p50)}</span></div>
    </div>

    <div class="efficiency-group">
      <h3>Tokens</h3>
      <div class="metric-row"><span class="metric-label">Input tokens (total)</span><span class="metric-value">{_esc(input_tokens)}</span></div>
      <div class="metric-row"><span class="metric-label">Output tokens (total)</span><span class="metric-value">{_esc(output_tokens)}</span></div>
    </div>

    <div class="efficiency-group">
      <h3>Cost to First Success</h3>
      <div class="metric-row"><span class="metric-label">Tokens (p50)</span><span class="metric-value">{_esc(tokens_str)}</span></div>
      <div class="metric-row"><span class="metric-label">Wall time (p50)</span><span class="metric-value">{_esc(wall_str)}</span></div>
      <div class="metric-row"><span class="metric-label">Cost (p50)</span><span class="metric-value">{_esc(cost_str)}</span></div>
      <div class="metric-help">Average cost to achieve first success per task</div>
    </div>
  </div>
</section>

{per_task_section}

<section class="failure-analysis">
  <h2>Failure Analysis</h2>
  <p class="section-desc">Breakdown of failures by classification and execution stage</p>
  <div class="failure-grid">
    {failure_classes_html}
    {failure_stages_html}
  </div>
</section>
"""


def _full_trace_payload(task_dir: Path | None, history: list[dict[str, Any]]) -> dict[str, Any]:
    events: list[dict[str, str]] = []
    tool_step = 0
    last_tool = ""
    for index, msg in enumerate(history, start=1):
        mtype = str(msg.get("type") or "")
        if mtype == "tool_call":
            tool_step += 1
            last_tool = str(msg.get("tool_name") or "")
            tool_input = _safe_json_loads(msg.get("tool_input", "")) or msg.get("tool_input", "")
            detail = json.dumps(tool_input, ensure_ascii=False, indent=2) if not isinstance(tool_input, str) else tool_input
            events.append({
                "kind": "tool-call",
                "title": f"Tool call #{tool_step}: {last_tool}",
                "detail": detail,
                "image": "",
            })
        elif mtype == "tool_result":
            raw = str(msg.get("content") or "")
            events.append({
                "kind": "tool-result",
                "title": f"Tool result: {msg.get('tool_name') or last_tool}",
                "detail": _compact_tool_result(raw),
                "image": "",
            })
        elif mtype == "assistant":
            events.append({
                "kind": "assistant",
                "title": "Assistant",
                "detail": str(msg.get("content") or ""),
                "image": "",
            })
        elif mtype == "user":
            events.append({
                "kind": "user",
                "title": "User",
                "detail": str(msg.get("content") or ""),
                "image": "",
            })
        else:
            events.append({
                "kind": mtype or "message",
                "title": f"Message #{index}: {mtype or 'unknown'}",
                "detail": json.dumps(msg, ensure_ascii=False, indent=2),
                "image": "",
            })
    return {
        "pre_screenshot": _image_data_uri(task_dir / "pre.jpg") if task_dir is not None else "",
        "post_screenshot": _image_data_uri(task_dir / "post.jpg") if task_dir is not None else "",
        "events": events,
    }


def generate_report_html(run_dir: Path) -> str:
    run_dir = run_dir.resolve()
    manifest = json.loads((run_dir / "manifest.json").read_text("utf-8"))
    results_path = run_dir / "results.jsonl"
    results: list[dict[str, Any]] = []
    if results_path.exists():
        for line in results_path.read_text("utf-8").strip().splitlines():
            results.append(json.loads(line))

    run_id = manifest.get("run_id", run_dir.name)
    suite_path = manifest.get("suite_path", "")
    suite_name = suite_path.rsplit("/", 1)[-1] if suite_path else "unknown"
    totals = manifest.get("totals", {})
    passed = totals.get("passed", 0)
    total = totals.get("tasks", len(results))
    failed = totals.get("failed", 0)
    skipped = totals.get("skipped", 0)
    timeout = totals.get("timeout", 0)
    judge_error = totals.get("judge_error", 0)
    # completed = everything that produced a result (the whole run is finished by report time)
    completed = total
    agent_url = manifest.get("agent_url", "")
    started = manifest.get("started_at", "")[:19]
    finished = manifest.get("finished_at", "")[:19]
    pass_rate = f"{passed/total*100:.1f}%" if total else "0%"
    # Progress bar segment widths (% of total)
    def _pct(n: int) -> float:
        return (n / total * 100) if total else 0.0
    pass_pct = f"{_pct(passed):.4f}"
    fail_pct = f"{_pct(failed + timeout + judge_error):.4f}"
    skip_pct = f"{_pct(skipped):.4f}"

    tasks_js_items = []
    for r in results:
        tid = r.get("task_id", "")
        fallback_task_dir = _safe_child_path(run_dir / "tasks", tid) if tid else None
        task_dir = _result_artifact_dir(run_dir, r, fallback_task_dir)
        trace_path = task_dir / "trace.json" if task_dir is not None else run_dir / "__missing_trace.json"
        history_path = task_dir / "history.json" if task_dir is not None else run_dir / "__missing_history.json"
        trace_data: dict[str, Any] = {}
        history: list[dict[str, Any]] = []
        prompt = ""
        if trace_path.exists():
            try:
                trace_data = json.loads(trace_path.read_text("utf-8"))
            except Exception:
                pass
        if history_path.exists():
            try:
                loaded_history = json.loads(history_path.read_text("utf-8"))
                history = [msg for msg in loaded_history if isinstance(msg, dict)] if isinstance(loaded_history, list) else []
                for msg in history:
                    if msg.get("type") == "user":
                        prompt = msg.get("content", "")
                        break
            except Exception:
                history = []
                pass

        tool_calls = trace_data.get("tool_calls", [])
        tool_calls_str = ""
        for tc in tool_calls:
            name = tc.get("tool", "")
            inp = json.dumps(tc.get("input", {}), ensure_ascii=False, indent=2)
            tool_calls_str += f"[{name}]\n{inp}\n\n"

        status = r.get("status", "?")
        rubric_items = r.get("rubric", [])
        rubric_spec = r.get("rubric_spec", [])

        rubric_js = []
        if rubric_items:
            for v in rubric_items:
                rubric_js.append([v.get("id",""), v.get("reason",""), v.get("verdict","")])
        elif rubric_spec:
            for s in rubric_spec:
                rubric_js.append([s.get("id",""), s.get("check",""), "\u2014"])

        hard_assertion_failures = [
            {
                "id": str(item.get("id") or ""),
                "label": str(item.get("label") or item.get("id") or ""),
                "requirement": str(item.get("requirement") or ""),
                "actual": str(item.get("actual") or ""),
            }
            for item in r.get("hard_assertion_failures") or []
            if isinstance(item, dict)
        ]

        # Extract errors
        errors = []
        metrics = r.get("metrics") or {}
        errors.extend(
            [label, value] for label, value in task_error_details(metrics)
        )
        artifacts_detail = _task_artifact_refs(run_dir, tid, task_dir)
        error_log_detail = _task_error_log(run_dir, tid, str(status), errors, hard_assertion_failures, task_dir)
        try:
            tool_calls_count = int(metrics.get("tool_calls", 0) or 0)
        except (TypeError, ValueError):
            tool_calls_count = 0

        tasks_js_items.append({
            "id": tid,
            "category": r.get("category", ""),
            "status": status,
            "wall_ms": metrics.get("wall_ms", 0),
            "tool_calls_count": tool_calls_count,
            "screenshots_taken": metrics.get("screenshots_taken", 0),
            "rubric_pass": r.get("rubric_pass_count", 0),
            "rubric_total": r.get("rubric_total", 0),
            "description": r.get("description_for_judge", ""),
            "prompt": prompt,
            "response": trace_data.get("final_response", ""),
            "tool_calls_detail": tool_calls_str.strip(),
            "rubric": rubric_js,
            "hard_assertion_failures": hard_assertion_failures,
            "errors": errors,
            "error_log_detail": error_log_detail,
            "artifacts_detail": artifacts_detail,
            "full_trace": _full_trace_payload(task_dir, history),
            "trace_observations": [
                [
                    obs.get("id", ""),
                    obs.get("reason", obs.get("description", "")),
                    "yes" if obs.get("passed") else "no",
                ]
                for obs in metrics.get("trace_observations") or []
            ],
        })

    tasks_json = json.dumps(tasks_js_items, ensure_ascii=False, indent=2)
    # Escape </script> to prevent XSS script-breakout
    tasks_json = tasks_json.replace("</", r"<\/")

    rows_html = ""
    for i, t in enumerate(tasks_js_items):
        status = t["status"]
        badge_cls = "pass" if status == "passed" else "fail" if status in {"failed", "timeout", "judge_error"} else "skip"
        badge_label = "Pass" if status == "passed" else "Fail" if status == "failed" else status.title()
        rows_html += f"""<tr data-task="{i}">
  <td><span class="task-id">{_esc(t['id'])}</span></td>
  <td>{_esc(t['category'])}</td>
  <td><span class="badge {badge_cls}">{badge_label}</span></td>
  <td class="mono">{t['rubric_pass']}/{t['rubric_total']}</td>
  <td class="mono">{_safe_int(t['tool_calls_count'])}</td>
  <td class="mono">{_fmt_time_ms(t['wall_ms'])}</td>
</tr>\n"""

    return HTML_TEMPLATE.format(
        run_id=run_id, suite=suite_name, passed=passed, total=total,
        failed=failed, skipped=skipped, pass_rate=pass_rate,
        agent_url=agent_url, started=started, finished=finished,
        rows_html=rows_html, tasks_json=tasks_json,
        completed=completed, timeout=timeout, judge_error=judge_error,
        pass_pct=pass_pct, fail_pct=fail_pct, skip_pct=skip_pct,
        capability_metrics_html=_capability_metrics_html(run_dir),
        analysis_html=_analysis_html(run_dir),
    )


def upload_report(client: AgentClient, html: str, run_dir: Path | None = None) -> bool:
    """Upload report HTML to the board via the agent's shell tool."""
    import base64
    try:
        # Use base64 to avoid heredoc delimiter collision
        encoded = base64.b64encode(html.encode("utf-8")).decode("ascii")
        cmd = ("mkdir -p /userdata/agent/benchmark && "
               f"printf '%s' '{encoded}' | base64 -d > /userdata/agent/benchmark/report.html")
        if run_dir is not None:
            run_id = run_dir.name
            manifest = run_dir / "manifest.json"
            manifest_text = manifest.read_text("utf-8")
            json.loads(manifest_text)
            manifest_encoded = base64.b64encode(manifest_text.encode("utf-8")).decode("ascii")
            board_run_dir = f"/userdata/agent/benchmark/runs/{run_id}"
            cmd = (
                f"mkdir -p {board_run_dir} /userdata/agent/benchmark && "
                f"printf '%s' '{encoded}' | base64 -d > {board_run_dir}/report.html && "
                f"printf '%s' '{manifest_encoded}' | base64 -d > {board_run_dir}/manifest.json && "
                f"cp {board_run_dir}/report.html /userdata/agent/benchmark/report.html"
            )
            analysis_parts = []
            for name in ("llm_analysis.md", "llm_analysis.json", "llm_analysis_error.txt"):
                path = run_dir / name
                if not path.exists():
                    continue
                text = path.read_text("utf-8")
                encoded_part = base64.b64encode(text.encode("utf-8")).decode("ascii")
                analysis_parts.append(
                    f"printf '%s' '{encoded_part}' | base64 -d > {board_run_dir}/{name}"
                )
            if analysis_parts:
                cmd += " && " + " && ".join(analysis_parts)
        result = client.invoke_tool("shell", {"command": cmd})
        return not result.is_error
    except Exception:
        return False


HTML_TEMPLATE = """<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Benchmark: {suite} \u2014 {run_id}</title>
<style>
:root {{
  --bg: oklch(99% 0.002 240);
  --surface: oklch(100% 0 0);
  --fg: oklch(18% 0.012 250);
  --muted: oklch(54% 0.012 250);
  --border: oklch(92% 0.005 250);
  --accent: oklch(58% 0.18 255);
  --green: oklch(58% 0.16 145);
  --red: oklch(60% 0.18 28);
  --yellow: oklch(70% 0.14 75);
  --font-body: -apple-system, BlinkMacSystemFont, system-ui, sans-serif;
  --font-mono: ui-monospace, "SF Mono", Menlo, Monaco, Consolas, monospace;
}}
* {{ box-sizing: border-box; margin: 0; padding: 0 }}
html {{ background: var(--bg); color: var(--fg); font-family: var(--font-body); -webkit-font-smoothing: antialiased }}
body {{ min-height: 100vh; background: linear-gradient(to bottom, var(--surface), var(--bg) 360px) }}
.page {{ width: min(1200px, calc(100vw - 48px)); margin: 0 auto; padding: 24px 0 56px }}
.topbar {{ display: flex; align-items: center; justify-content: space-between; padding: 14px 0; border-bottom: 1px solid var(--border); margin-bottom: 24px }}
.brand {{ display: flex; align-items: center; gap: 10px }}
.mark {{ width: 24px; height: 24px; border-radius: 6px; background: var(--fg); position: relative }}
.mark::before,.mark::after {{ content: ""; position: absolute; left: 6px; right: 6px; height: 2px; border-radius: 999px; background: white }}
.mark::before {{ top: 7px }} .mark::after {{ bottom: 7px }}
.brand strong {{ font-size: 14px; letter-spacing: -0.01em }}
.meta-info {{ color: var(--muted); font-size: 12px }}
.summary {{ display: grid; grid-template-columns: repeat(4, 1fr); gap: 12px; margin-bottom: 20px }}
.summary-card {{ border: 1px solid var(--border); border-radius: 14px; background: var(--surface); padding: 14px }}
.summary-card .label {{ color: var(--muted); font-size: 11px; font-weight: 700; letter-spacing: 0.04em; text-transform: uppercase }}
.summary-card .value {{ margin-top: 10px; font-size: 28px; font-weight: 700; letter-spacing: -0.03em }}
.progress {{ border: 1px solid var(--border); border-radius: 14px; background: var(--surface); padding: 14px 16px; margin-bottom: 20px }}
.progress-head {{ display: flex; justify-content: space-between; align-items: baseline; margin-bottom: 10px }}
.progress-head .label {{ color: var(--muted); font-size: 11px; font-weight: 700; letter-spacing: 0.04em; text-transform: uppercase }}
.progress-head .count {{ font-family: var(--font-mono); font-size: 14px; font-weight: 700; font-variant-numeric: tabular-nums }}
.progress-bar {{ display: flex; height: 10px; border-radius: 999px; overflow: hidden; background: color-mix(in oklch, var(--bg) 60%, var(--border)) }}
.progress-bar .seg {{ height: 100% }}
.progress-bar .seg.pass {{ background: var(--green) }}
.progress-bar .seg.fail {{ background: var(--red) }}
.progress-bar .seg.skip {{ background: var(--yellow) }}
.progress-legend {{ display: flex; flex-wrap: wrap; gap: 14px; margin-top: 10px; font-size: 12px; color: var(--muted) }}
.progress-legend span {{ display: inline-flex; align-items: center; gap: 5px }}
.progress-legend i {{ width: 9px; height: 9px; border-radius: 3px; display: inline-block }}
.progress-legend i.pass {{ background: var(--green) }}
.progress-legend i.fail {{ background: var(--red) }}
.progress-legend i.skip {{ background: var(--yellow) }}
.capability-metrics {{ border: 1px solid var(--border); border-radius: 14px; background: var(--surface); padding: 14px 16px; margin-bottom: 20px }}
.capability-head {{ display: flex; justify-content: space-between; align-items: baseline; gap: 16px; margin-bottom: 12px }}
.capability-head h2 {{ font-size: 14px; margin-bottom: 4px }}
.capability-head p,.capability-note {{ color: var(--muted); font-size: 12px }}
.capability-note {{ text-align: right }}
.capability-grid {{ display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 10px }}
.capability-card {{ border: 1px solid var(--border); border-radius: 10px; padding: 11px 12px; background: color-mix(in oklch, var(--bg) 48%, white) }}
.capability-card .label {{ color: var(--muted); font-size: 10px; font-weight: 700; letter-spacing: 0.04em; text-transform: uppercase }}
.capability-card .value {{ margin-top: 7px; font-family: var(--font-mono); font-size: 21px; font-weight: 700; font-variant-numeric: tabular-nums }}
.capability-card .detail {{ margin-top: 5px; color: var(--muted); font-size: 11px }}
.capability-card .metric-help {{ margin-top: 6px; color: var(--muted); font-size: 10px; font-style: italic; border-top: 1px solid color-mix(in oklch, var(--border) 50%, transparent); padding-top: 6px }}
.efficiency-metrics, .failure-analysis {{ border: 1px solid var(--border); border-radius: 14px; background: var(--surface); padding: 14px 16px; margin-bottom: 20px }}
.efficiency-metrics h2, .failure-analysis h2 {{ font-size: 14px; margin-bottom: 4px }}
.section-desc {{ color: var(--muted); font-size: 12px; margin-bottom: 12px }}
.efficiency-grid {{ display: grid; grid-template-columns: repeat(4, 1fr); gap: 14px }}
.efficiency-group {{ border: 1px solid var(--border); border-radius: 10px; padding: 10px 12px; background: color-mix(in oklch, var(--bg) 48%, white) }}
.efficiency-group h3 {{ font-size: 11px; font-weight: 700; color: var(--muted); text-transform: uppercase; letter-spacing: 0.04em; margin-bottom: 8px }}
.metric-row {{ display: flex; justify-content: space-between; align-items: baseline; padding: 4px 0; font-size: 12px }}
.metric-row .metric-label {{ color: var(--muted) }}
.metric-row .metric-value {{ font-family: var(--font-mono); font-weight: 600; font-variant-numeric: tabular-nums }}
.efficiency-group .metric-help {{ margin-top: 6px; color: var(--muted); font-size: 10px; font-style: italic; border-top: 1px solid color-mix(in oklch, var(--border) 50%, transparent); padding-top: 6px }}
.failure-grid {{ display: grid; grid-template-columns: repeat(2, 1fr); gap: 14px }}
.metric-section {{ border: 1px solid var(--border); border-radius: 10px; padding: 10px 12px; background: color-mix(in oklch, var(--bg) 48%, white) }}
.metric-section h3 {{ font-size: 11px; font-weight: 700; color: var(--muted); text-transform: uppercase; letter-spacing: 0.04em; margin-bottom: 4px }}
.metric-desc {{ color: var(--muted); font-size: 10px; margin-bottom: 8px }}
.metric-table {{ width: 100%; font-size: 12px; border-collapse: collapse }}
.metric-table th {{ text-align: left; padding: 4px 8px; border-bottom: 1px solid var(--border); font-weight: 600; color: var(--muted); font-size: 11px }}
.metric-table td {{ padding: 4px 8px; border-bottom: 1px solid color-mix(in oklch, var(--border) 50%, transparent) }}
.metric-table tr:last-child td {{ border-bottom: none }}
.metric-table td:last-child {{ text-align: right; font-family: var(--font-mono); font-variant-numeric: tabular-nums }}
.per-task-results {{ border: 1px solid var(--border); border-radius: 14px; background: var(--surface); padding: 14px 16px; margin-bottom: 20px }}
.per-task-results h2 {{ font-size: 14px; margin-bottom: 4px }}
.per-task-table {{ width: 100%; font-size: 12px; border-collapse: collapse; font-variant-numeric: tabular-nums }}
.per-task-table th, .per-task-table td {{ padding: 8px 10px; border-bottom: 1px solid var(--border) }}
.per-task-table th {{ font-weight: 600; background: color-mix(in oklch, var(--bg) 92%, var(--border)); font-size: 11px; position: sticky; top: 0; z-index: 1 }}
.per-task-table .task-id {{ font-family: var(--font-mono); font-size: 11px; max-width: 400px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap }}
.per-task-table .text-center {{ text-align: center }}
.per-task-table .text-right {{ text-align: right }}
.per-task-table .task-row-passed {{ background: color-mix(in oklch, var(--green) 8%, transparent) }}
.per-task-table .task-row-failed {{ background: color-mix(in oklch, var(--red) 8%, transparent) }}
.per-task-table .task-row-partial {{ background: color-mix(in oklch, var(--yellow) 8%, transparent) }}
.per-task-table tbody tr:hover {{ background: color-mix(in oklch, var(--border) 20%, transparent) }}
.table-wrap {{ max-height: 500px; overflow-y: auto; border: 1px solid var(--border); border-radius: 8px }}
.analysis {{ border: 1px solid var(--border); border-radius: 16px; background: var(--surface); padding: 16px; margin-bottom: 20px }}
.analysis h2 {{ font-size: 14px; margin-bottom: 10px }}
.analysis pre {{ white-space: pre-wrap; font-family: var(--font-mono); font-size: 12px; line-height: 1.5; color: var(--fg) }}
.analysis.warning {{ border-color: color-mix(in oklch, var(--red) 35%, var(--border)) }}
.panel {{ border: 1px solid var(--border); border-radius: 16px; background: var(--surface); overflow: hidden }}
.panel-header {{ display: flex; justify-content: space-between; align-items: center; padding: 14px 16px; border-bottom: 1px solid var(--border) }}
.panel-title {{ font-size: 14px; font-weight: 650 }}
.task-table {{ width: 100%; border-collapse: collapse; font-size: 13px }}
.task-table th {{ padding: 10px 14px; text-align: left; color: var(--muted); font-size: 11px; font-weight: 700; letter-spacing: 0.04em; text-transform: uppercase; background: color-mix(in oklch, var(--bg) 80%, white); border-bottom: 1px solid var(--border); position: sticky; top: 0 }}
.task-table td {{ padding: 12px 14px; border-bottom: 1px solid var(--border) }}
.task-table tr {{ cursor: pointer }}
.task-table tbody tr:hover {{ background: color-mix(in oklch, var(--accent) 4%, white) }}
.task-table tbody tr.active {{ background: color-mix(in oklch, var(--accent) 7%, white) }}
.task-id {{ font-family: var(--font-mono); font-size: 12px; color: var(--muted) }}
.mono {{ font-family: var(--font-mono); font-variant-numeric: tabular-nums }}
.badge {{ display: inline-flex; align-items: center; height: 22px; padding: 0 8px; border-radius: 999px; border: 1px solid var(--border); font-size: 11px; font-weight: 700 }}
.badge.pass {{ color: oklch(42% 0.13 150); border-color: color-mix(in oklch, var(--green) 32%, var(--border)); background: color-mix(in oklch, var(--green) 8%, white) }}
.badge.fail {{ color: oklch(48% 0.16 28); border-color: color-mix(in oklch, var(--red) 28%, var(--border)); background: color-mix(in oklch, var(--red) 7%, white) }}
.badge.skip {{ color: oklch(48% 0.12 75); border-color: color-mix(in oklch, var(--yellow) 34%, var(--border)); background: color-mix(in oklch, var(--yellow) 10%, white) }}
.drawer-backdrop {{ position: fixed; inset: 0; z-index: 40; background: color-mix(in oklch, var(--fg) 18%, transparent); opacity: 0; pointer-events: none; transition: opacity 180ms ease }}
.drawer {{ position: fixed; top: 0; right: 0; bottom: 0; z-index: 50; width: min(720px, 100vw); background: var(--surface); border-left: 1px solid var(--border); box-shadow: -24px 0 80px color-mix(in oklch, var(--fg) 10%, transparent); transform: translateX(100%); transition: transform 220ms ease; display: flex; flex-direction: column }}
body.open .drawer-backdrop {{ opacity: 1; pointer-events: auto }}
body.open .drawer {{ transform: translateX(0) }}
.drawer-top {{ padding: 18px; border-bottom: 1px solid var(--border); background: linear-gradient(to bottom, color-mix(in oklch, var(--bg) 72%, white), var(--surface)); flex-shrink: 0 }}
.drawer-top h2 {{ font-size: 18px; letter-spacing: -0.02em; margin-bottom: 8px }}
.drawer-chips {{ display: flex; flex-wrap: wrap; gap: 6px }}
.chip {{ display: inline-flex; align-items: center; height: 24px; padding: 0 8px; border-radius: 999px; border: 1px solid var(--border); background: var(--surface); color: var(--muted); font-size: 11px }}
.close-btn {{ position: absolute; top: 16px; right: 16px; width: 30px; height: 30px; border: 1px solid var(--border); border-radius: 8px; background: var(--surface); color: var(--fg); font-size: 16px; cursor: pointer; display: flex; align-items: center; justify-content: center }}
.drawer-body {{ flex: 1; min-height: 0; overflow-y: scroll; -webkit-overflow-scrolling: touch; padding: 16px }}
.drawer-body > .block {{ margin-bottom: 12px }}
.drawer-body > .block:last-child {{ margin-bottom: 0 }}
.block {{ border: 1px solid var(--border); border-radius: 12px; overflow: hidden; background: var(--surface) }}
.block-head {{ display: flex; justify-content: space-between; align-items: center; padding: 10px 12px; border-bottom: 1px solid var(--border); background: color-mix(in oklch, var(--bg) 72%, white) }}
.block-head strong {{ font-size: 11px; letter-spacing: 0.04em; text-transform: uppercase }}
.block-head span {{ color: var(--muted); font-family: var(--font-mono); font-size: 10px }}
.block-body {{ padding: 12px 14px; font-size: 13px; line-height: 1.6 }}
pre.block-body {{ margin: 0; white-space: pre-wrap; word-break: break-word; font-family: var(--font-mono); font-size: 12px; line-height: 1.65 }}
.rubric-row {{ display: grid; grid-template-columns: 1fr 54px; gap: 8px; padding: 8px 0; border-bottom: 1px solid var(--border); font-size: 12px }}
.rubric-row:last-child {{ border-bottom: 0 }}
.rubric-row .rid {{ font-weight: 650; margin-bottom: 2px }}
.rubric-row .reason {{ color: var(--muted) }}
.rubric-row b {{ text-align: right; font-variant-numeric: tabular-nums }}
.rubric-row b.yes {{ color: oklch(42% 0.13 150) }}
.rubric-row b.no {{ color: oklch(48% 0.16 28) }}
.assertion-row {{ display: grid; grid-template-columns: 1fr 54px; gap: 8px; padding: 10px 0; border-bottom: 1px solid var(--border); font-size: 12px }}
.assertion-row:last-child {{ border-bottom: 0 }}
.assertion-row .rid {{ font-weight: 650; margin-bottom: 6px }}
.assertion-detail {{ display: grid; grid-template-columns: 92px 1fr; gap: 4px 8px; color: var(--muted) }}
.assertion-detail strong {{ color: var(--text); font-weight: 650 }}
.assertion-row b {{ text-align: right; color: oklch(48% 0.16 28); font-variant-numeric: tabular-nums }}
.error-block {{ border-color: color-mix(in oklch, var(--red) 28%, var(--border)) }}
.error-block .block-head {{ background: color-mix(in oklch, var(--red) 7%, white); border-bottom-color: color-mix(in oklch, var(--red) 20%, var(--border)) }}
.error-item {{ padding: 8px 0; border-bottom: 1px solid var(--border) }}
.error-item:last-child {{ border-bottom: 0 }}
.error-type {{ font-weight: 650; font-size: 11px; text-transform: uppercase; letter-spacing: 0.04em; color: oklch(48% 0.16 28); margin-bottom: 4px }}
.error-msg {{ font-family: var(--font-mono); font-size: 12px; color: var(--muted); line-height: 1.5 }}
.warning-block {{ border-color: color-mix(in oklch, var(--yellow) 34%, var(--border)) }}
.warning-block .block-head {{ background: color-mix(in oklch, var(--yellow) 10%, white); border-bottom-color: color-mix(in oklch, var(--yellow) 25%, var(--border)) }}
.trace-actions {{ display: flex; justify-content: flex-start; margin-bottom: 12px }}
.trace-toggle {{ height: 34px; border: 1px solid var(--border); border-radius: 8px; background: var(--fg); color: white; padding: 0 12px; font: inherit; font-size: 12px; font-weight: 650; cursor: pointer }}
.trace-toggle:hover {{ background: color-mix(in oklch, var(--fg) 88%, var(--accent)) }}
.full-trace {{ display: grid; gap: 12px; margin-bottom: 12px }}
.full-trace[hidden] {{ display: none }}
.trace-shots {{ display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 10px }}
.trace-shot {{ border: 1px solid var(--border); border-radius: 12px; overflow: hidden; background: color-mix(in oklch, var(--bg) 72%, white) }}
.trace-shot figcaption {{ padding: 8px 10px; border-bottom: 1px solid var(--border); color: var(--muted); font-size: 11px; font-weight: 700; letter-spacing: 0.04em; text-transform: uppercase }}
.trace-shot img {{ display: block; width: 100%; height: auto; max-height: 520px; object-fit: contain; background: #050505 }}
.trace-event {{ border: 1px solid var(--border); border-radius: 12px; overflow: hidden; background: var(--surface) }}
.trace-event-head {{ display: flex; align-items: center; justify-content: space-between; gap: 8px; padding: 9px 11px; border-bottom: 1px solid var(--border); background: color-mix(in oklch, var(--bg) 72%, white) }}
.trace-event-head strong {{ font-size: 12px }}
.trace-kind {{ color: var(--muted); font-family: var(--font-mono); font-size: 10px; text-transform: uppercase }}
.trace-event pre {{ margin: 0; padding: 10px 11px; white-space: pre-wrap; word-break: break-word; font-family: var(--font-mono); font-size: 11px; line-height: 1.55; color: var(--muted) }}
.trace-event img {{ display: block; width: 100%; max-height: 620px; object-fit: contain; background: #050505; border-top: 1px solid var(--border) }}
.pager {{ padding: 12px 16px; color: var(--muted); font-size: 12px; border-top: 1px solid var(--border) }}
@media (max-width: 768px) {{
  .summary {{ grid-template-columns: repeat(2, 1fr) }}
  .capability-grid {{ grid-template-columns: repeat(2, minmax(0, 1fr)) }}
  .capability-head {{ display: block }}
  .capability-note {{ display: block; margin-top: 8px; text-align: left }}
  .efficiency-grid {{ grid-template-columns: repeat(2, 1fr) }}
  .failure-grid {{ grid-template-columns: 1fr }}
  .trace-shots {{ grid-template-columns: 1fr }}
  .drawer {{ width: 100vw }}
  .page {{ width: calc(100vw - 24px) }}
}}
</style>
</head>
<body>
<main class="page">
<header class="topbar">
  <div class="brand"><div class="mark"></div><strong>Agent Benchmark</strong></div>
  <div class="meta-info">{agent_url} &middot; {started} ~ {finished}</div>
</header>
<section class="summary">
  <article class="summary-card"><div class="label">Total</div><div class="value">{total}</div></article>
  <article class="summary-card"><div class="label">Pass Rate</div><div class="value">{pass_rate}</div></article>
  <article class="summary-card"><div class="label">Failed</div><div class="value">{failed}</div></article>
  <article class="summary-card"><div class="label">Skipped</div><div class="value">{skipped}</div></article>
</section>
{capability_metrics_html}
<section class="progress">
  <div class="progress-head">
    <span class="label">Execution Progress</span>
    <span class="count">{completed}/{total}</span>
  </div>
  <div class="progress-bar">
    <div class="seg pass" style="width:{pass_pct}%"></div>
    <div class="seg fail" style="width:{fail_pct}%"></div>
    <div class="seg skip" style="width:{skip_pct}%"></div>
  </div>
  <div class="progress-legend">
    <span><i class="pass"></i>Passed {passed}</span>
    <span><i class="fail"></i>Failed {failed}</span>
    <span><i class="skip"></i>Skipped {skipped}</span>
    <span><i class="fail"></i>Timeout {timeout}</span>
    <span><i class="fail"></i>Judge Error {judge_error}</span>
  </div>
</section>
{analysis_html}
<section class="panel">
  <div class="panel-header">
    <h2 class="panel-title">Task Records</h2>
    <span style="color:var(--muted);font-size:12px">click a row to inspect</span>
  </div>
  <div style="overflow:auto">
    <table class="task-table">
      <thead><tr>
        <th>Task ID</th>
        <th>Category</th>
        <th>Status</th>
        <th title="Rubric checks passed / total">Rubric</th>
        <th title="Number of tool invocations">Tool Calls</th>
        <th title="Wall-clock execution time">Latency</th>
      </tr></thead>
      <tbody>{rows_html}</tbody>
    </table>
  </div>
  <div class="pager">Showing {total} tasks &middot; {suite} &middot; run {run_id}</div>
</section>
</main>
<div class="drawer-backdrop" id="backdrop"></div>
<aside class="drawer" id="drawer">
  <div class="drawer-top" style="position:relative">
    <h2 id="dTitle">\u2014</h2>
    <div class="drawer-chips" id="dChips"></div>
    <button class="close-btn" id="closeBtn">&times;</button>
  </div>
  <div class="drawer-body" id="dBody"></div>
</aside>
<script>
const TASKS = {tasks_json};
const rows = document.querySelectorAll("tbody tr[data-task]");
const backdrop = document.getElementById("backdrop");
const closeBtn = document.getElementById("closeBtn");
function esc(s) {{ var d = document.createElement("div"); d.textContent = s || ""; return d.innerHTML; }}
function token(s) {{ return String(s || "").toLowerCase().replace(/[^a-z0-9_-]/g, "-"); }}
function formatTime(ms) {{
  if (ms < 1000) return ms + 'ms';
  var sec = (ms / 1000).toFixed(1);
  if (parseFloat(sec) < 60) return sec + 's';
  var min = Math.floor(parseFloat(sec) / 60);
  var rem = (parseFloat(sec) % 60).toFixed(1);
  return min + 'm' + rem + 's';
}}
function hasFullTrace(t) {{
  var ft = t.full_trace || {{}};
  return !!(ft.pre_screenshot || ft.post_screenshot || (ft.events && ft.events.length));
}}
function renderTraceShot(label, src) {{
  if (!src) return "";
  return '<figure class="trace-shot"><figcaption>' + esc(label) + '</figcaption><img src="' + esc(src) + '" alt="' + esc(label) + ' screenshot"></figure>';
}}
function renderTraceEvent(event, index) {{
  var kind = token(event.kind || "message");
  var detail = event.detail ? '<pre>' + esc(event.detail) + '</pre>' : '';
  return '<section class="trace-event ' + kind + '">' +
    '<div class="trace-event-head"><strong>' + esc(event.title || ("Trace event " + index)) + '</strong><span class="trace-kind">' + esc(event.kind || "message") + '</span></div>' +
    detail + '</section>';
}}
function renderFullTrace(t) {{
  var ft = t.full_trace || {{}};
  var shots = renderTraceShot("Pre screenshot", ft.pre_screenshot) + renderTraceShot("Post screenshot", ft.post_screenshot);
  var body = shots ? '<div class="trace-shots">' + shots + '</div>' : '';
  var events = ft.events || [];
  if (events.length) body += events.map(function(event, index) {{ return renderTraceEvent(event, index + 1); }}).join("");
  if (!body) body = '<div class="block"><div class="block-body">No trace artifacts were captured for this task.</div></div>';
  return body;
}}
function openDrawer(i) {{
  var t = TASKS[i]; if (!t) return;
  document.getElementById("dTitle").textContent = t.id;
  document.getElementById("dChips").innerHTML =
    '<span class="chip">' + esc(t.category) + '</span>' +
    '<span class="chip">' + esc(t.status) + '</span>' +
    '<span class="chip">' + esc(String(t.tool_calls_count)) + ' tools</span>' +
    '<span class="chip">' + formatTime(t.wall_ms) + '</span>' +
    (t.screenshots_taken ? '<span class="chip">' + Math.floor(t.screenshots_taken) + ' screenshots</span>' : '');
  var body = "";
  body += '<div class="block"><div class="block-head"><strong>Prompt</strong><span>user input</span></div><pre class="block-body">' + esc(t.prompt) + '</pre></div>';
  body += '<div class="block"><div class="block-head"><strong>Task Description</strong><span>for judge</span></div><div class="block-body">' + esc(t.description) + '</div></div>';
  if (hasFullTrace(t)) {{
    body += '<div class="trace-actions"><button id="traceToggle" class="trace-toggle" type="button">View full trace</button></div>';
    body += '<div id="fullTrace" class="full-trace" hidden>' + renderFullTrace(t) + '</div>';
  }}
  if (t.tool_calls_detail) {{
    body += '<div class="block"><div class="block-head"><strong>Tool Calls</strong><span>' + esc(String(t.tool_calls_count)) + ' calls</span></div><pre class="block-body">' + esc(t.tool_calls_detail) + '</pre></div>';
  }}
  if (t.artifacts_detail) {{
    body += '<div class="block"><div class="block-head"><strong>Artifacts</strong><span>files</span></div><pre class="block-body">' + esc(t.artifacts_detail) + '</pre></div>';
  }}
  if (t.error_log_detail) {{
    body += '<div class="block error-block"><div class="block-head"><strong>Error Log</strong><span>task failure details</span></div><pre class="block-body">' + esc(t.error_log_detail) + '</pre></div>';
  }}
  body += '<div class="block"><div class="block-head"><strong>Agent Response</strong><span>final reply</span></div><pre class="block-body">' + esc(t.response) + '</pre></div>';
  if (t.errors && t.errors.length) {{
    var err = t.errors.map(function(e) {{
      return '<div class="error-item"><div class="error-type">' + esc(e[0]) + '</div><div class="error-msg">' + esc(e[1]) + '</div></div>';
    }}).join("");
    body += '<div class="block error-block"><div class="block-head"><strong>❌ Errors</strong><span>' + t.errors.length + ' error(s)</span></div><div class="block-body">' + err + '</div></div>';
  }}
  if (t.hard_assertion_failures && t.hard_assertion_failures.length) {{
    var ha = t.hard_assertion_failures.map(function(r) {{
      return '<div class="assertion-row"><div><div class="rid">' + esc(r.label || r.id) + '</div>' +
        '<div class="assertion-detail"><strong>Requirement</strong><span>' + esc(r.requirement) + '</span>' +
        '<strong>Actual</strong><span>' + esc(r.actual) + '</span></div></div><b>no</b></div>';
    }}).join("");
    body += '<div class="block warning-block"><div class="block-head"><strong>⚠️ Hard Assertion Failures</strong><span>' + t.hard_assertion_failures.length + ' failure(s)</span></div><div class="block-body">' + ha + '</div></div>';
  }}
  if (t.rubric && t.rubric.length) {{
    var rb = t.rubric.map(function(r) {{
      var cls = r[2] === "yes" ? "yes" : r[2] === "no" ? "no" : "";
      return '<div class="rubric-row"><div><div class="rid">' + esc(r[0]) + '</div><div class="reason">' + esc(r[1]) + '</div></div><b class="' + cls + '">' + r[2] + '</b></div>';
    }}).join("");
    body += '<div class="block"><div class="block-head"><strong>Rubric</strong><span>' + t.rubric_pass + '/' + t.rubric_total + '</span></div><div class="block-body">' + rb + '</div></div>';
  }}
  if (t.trace_observations && t.trace_observations.length) {{
    var ob = t.trace_observations.map(function(r) {{
      var cls = r[2] === "yes" ? "yes" : "no";
      return '<div class="rubric-row"><div><div class="rid">' + esc(r[0]) + '</div><div class="reason">' + esc(r[1]) + '</div></div><b class="' + cls + '">' + r[2] + '</b></div>';
    }}).join("");
    body += '<div class="block"><div class="block-head"><strong>Trace observations</strong><span>informational</span></div><div class="block-body">' + ob + '</div></div>';
  }}
  document.getElementById("dBody").innerHTML = body;
  var traceToggle = document.getElementById("traceToggle");
  if (traceToggle) {{
    traceToggle.addEventListener("click", function() {{
      var fullTrace = document.getElementById("fullTrace");
      if (!fullTrace) return;
      var willOpen = fullTrace.hasAttribute("hidden");
      if (willOpen) fullTrace.removeAttribute("hidden"); else fullTrace.setAttribute("hidden", "");
      traceToggle.textContent = willOpen ? "Hide full trace" : "View full trace";
    }});
  }}
  rows.forEach(function(r) {{ r.classList.toggle("active", Number(r.dataset.task) === i); }});
  document.body.classList.add("open");
}}
function closeDrawer() {{ document.body.classList.remove("open"); }}
rows.forEach(function(r) {{ r.addEventListener("click", function() {{ openDrawer(Number(r.dataset.task)); }}); }});
backdrop.addEventListener("click", closeDrawer);
closeBtn.addEventListener("click", closeDrawer);
window.addEventListener("keydown", function(e) {{ if (e.key === "Escape") closeDrawer(); }});
</script>
</body>
</html>"""
