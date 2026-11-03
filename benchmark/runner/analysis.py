from __future__ import annotations

import dataclasses as dc
import json
import os
import re
import socket
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any


DEFAULT_MODEL = "anthropic/claude-sonnet-4-6"
DEFAULT_MAX_LOG_BYTES = 64 * 1024
DEFAULT_MAX_CODE_BYTES = 128 * 1024
DEFAULT_TOTAL_CONTEXT_BYTES = 320 * 1024
DEFAULT_TIMEOUT_SEC = 180
DEFAULT_BASE_URL = "https://openrouter.ai/api/v1"
ROOT_CAUSE_CATEGORIES = {
    "suite_issue",
    "project_code_issue",
    "agent_behavior_issue",
    "benchmark_infra_issue",
    "environment_issue",
    "evaluation_issue",
    "insufficient_evidence",
}
BRIDGE_INACTIVE_SETUP_RE = re.compile(
    r"AicoAdapterError:\s*setup tool\s+\S+\s+failed:.*mobilegym bridge episode is not active",
    re.IGNORECASE,
)


@dc.dataclass
class AnalysisConfig:
    enabled: bool = False
    model: str = DEFAULT_MODEL
    base_url: str = DEFAULT_BASE_URL
    max_log_bytes: int = DEFAULT_MAX_LOG_BYTES
    max_code_bytes: int = DEFAULT_MAX_CODE_BYTES
    total_context_bytes: int = DEFAULT_TOTAL_CONTEXT_BYTES
    timeout_sec: int = DEFAULT_TIMEOUT_SEC
    api_key_env: str | None = None
    api_key_value: str | None = None


@dc.dataclass
class AnalysisResult:
    ok: bool
    markdown_path: Path | None = None
    json_path: Path | None = None
    error_path: Path | None = None
    warning: str = ""


class AnalysisError(RuntimeError):
    pass


SECRET_NAME_RE = re.compile(
    r"(?i)(?P<prefix>[\"']?\b(api[_-]?key|token|password|secret|bearer|authorization|OPENROUTER_API_KEY|MODEL_API_KEY|AICO_MODEL_API_KEY|AICO_BENCHMARK_ANALYSIS_API_KEY|AICO_BENCHMARK_JUDGE_API_KEY|AICO_CONTROL_TOKEN)\b[\"']?\s*[:=]\s*)(?P<quote>[\"']?)(?P<value>[^\"'\s,}]+)(?P=quote)?"
)
BASIC_AUTH_RE = re.compile(r"(?i)authorization\s*:\s*basic\s+[A-Za-z0-9._~+/=-]{8,}")
BEARER_RE = re.compile(r"(?i)bearer\s+[A-Za-z0-9._~+/=-]{12,}")
SK_KEY_RE = re.compile(r"\bsk-[A-Za-z0-9._-]{8,}\b")
JWTISH_RE = re.compile(r"\b[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b")


def _int_env(name: str, default: int) -> int:
    try:
        value = int(os.environ.get(name, "") or default)
    except ValueError:
        return default
    return value if value > 0 else default


def config_from_env() -> AnalysisConfig:
    enabled = os.environ.get("AICO_BENCHMARK_LLM_ANALYSIS", "").strip().lower() in {
        "1",
        "true",
        "yes",
        "on",
    }
    return AnalysisConfig(
        enabled=enabled,
        model=os.environ.get("AICO_BENCHMARK_ANALYSIS_MODEL")
        or os.environ.get("AICO_BENCHMARK_JUDGE_MODEL")
        or DEFAULT_MODEL,
        base_url=os.environ.get("AICO_BENCHMARK_ANALYSIS_BASE_URL")
        or os.environ.get("AICO_BENCHMARK_JUDGE_BASE_URL")
        or DEFAULT_BASE_URL,
        max_log_bytes=_int_env("AICO_BENCHMARK_ANALYSIS_MAX_LOG_BYTES", DEFAULT_MAX_LOG_BYTES),
        max_code_bytes=_int_env("AICO_BENCHMARK_ANALYSIS_MAX_CODE_BYTES", DEFAULT_MAX_CODE_BYTES),
        total_context_bytes=_int_env(
            "AICO_BENCHMARK_ANALYSIS_TOTAL_CONTEXT_BYTES", DEFAULT_TOTAL_CONTEXT_BYTES
        ),
        timeout_sec=_int_env("AICO_BENCHMARK_ANALYSIS_TIMEOUT_SEC", DEFAULT_TIMEOUT_SEC),
        api_key_env=os.environ.get("AICO_BENCHMARK_ANALYSIS_API_KEY_ENV") or None,
    )


def resolve_analysis_api_key(cfg: AnalysisConfig) -> tuple[str, str]:
    if cfg.api_key_value and cfg.api_key_value.strip():
        return "provided", cfg.api_key_value.strip()
    names: list[str] = []
    if cfg.api_key_env:
        names.append(cfg.api_key_env)
    else:
        env_name = os.environ.get("AICO_BENCHMARK_ANALYSIS_API_KEY_ENV")
        if env_name:
            names.append(env_name)
    names.extend(
        ["AICO_BENCHMARK_ANALYSIS_API_KEY", "AICO_BENCHMARK_JUDGE_API_KEY"]
    )
    for name in names:
        value = os.environ.get(name, "").strip()
        if value:
            return name, value
    raise AnalysisError(
        "missing analysis API key: set AICO_BENCHMARK_ANALYSIS_API_KEY or "
        "AICO_BENCHMARK_JUDGE_API_KEY"
    )


def redact_text(text: str, cfg: AnalysisConfig) -> str:
    if not text:
        return text
    redacted = BASIC_AUTH_RE.sub("Authorization: Basic [REDACTED]", text)
    redacted = BEARER_RE.sub("Bearer [REDACTED]", redacted)
    redacted = SECRET_NAME_RE.sub(
        lambda m: f"{m.group('prefix')}{m.group('quote')}[REDACTED]{m.group('quote')}",
        redacted,
    )
    redacted = SK_KEY_RE.sub("[REDACTED_SK_KEY]", redacted)
    redacted = JWTISH_RE.sub("[REDACTED_JWT]", redacted)
    names = [cfg.api_key_env] if cfg.api_key_env else []
    names.extend(
        [
            "AICO_BENCHMARK_ANALYSIS_API_KEY",
            "AICO_BENCHMARK_JUDGE_API_KEY",
            "AICO_CONTROL_TOKEN",
        ]
    )
    if cfg.api_key_value:
        redacted = redacted.replace(cfg.api_key_value, "[REDACTED_VALUE]")
    for name in names:
        if not name:
            continue
        value = os.environ.get(name, "")
        if value:
            redacted = redacted.replace(value, "[REDACTED_VALUE]")
    return redacted


def render_markdown(payload: dict[str, Any]) -> str:
    lines = ["# LLM 基准分析", "", str(payload.get("summary") or "未返回摘要。"), ""]
    counts = payload.get("classification_counts")
    if isinstance(counts, dict) and counts:
        lines += ["## 分类统计", ""]
        for key, value in sorted(counts.items()):
            lines.append(f"- `{key}`: {value}")
        lines.append("")
    clusters = payload.get("failure_clusters") if isinstance(payload.get("failure_clusters"), list) else []
    lines += ["## 失败聚类", ""]
    if not clusters:
        lines.append("- 未返回失败聚类。")
    for cluster in clusters:
        if not isinstance(cluster, dict):
            continue
        title = cluster.get("title") or cluster.get("suspected_cause") or "聚类"
        task_ids = ", ".join(str(t) for t in cluster.get("task_ids") or [])
        lines += [
            f"### {title}",
            "",
            f"- 分类: `{cluster.get('category', 'unknown')}`",
            f"- 置信度: `{cluster.get('confidence', 'unknown')}`",
        ]
        if task_ids:
            lines.append(f"- 任务: {task_ids}")
        if cluster.get("suspected_cause"):
            lines.append(f"- 疑似原因: {cluster['suspected_cause']}")
        evidence = cluster.get("evidence") if isinstance(cluster.get("evidence"), list) else []
        for item in evidence:
            lines.append(f"- 证据: {item}")
        lines.append("")
    recommendations = payload.get("recommendations") if isinstance(payload.get("recommendations"), list) else []
    lines += ["## 建议", ""]
    if not recommendations:
        lines.append("- 未返回建议。")
    for rec in recommendations:
        if isinstance(rec, dict):
            lines.append(
                f"- **{rec.get('priority', 'medium')}** `{rec.get('target', '')}`: {rec.get('suggestion', '')}"
            )
        else:
            lines.append(f"- {rec}")
    gaps = payload.get("evidence_gaps") if isinstance(payload.get("evidence_gaps"), list) else []
    if gaps:
        lines += ["", "## 证据缺口", ""]
        lines.extend(f"- {gap}" for gap in gaps)
    return "\n".join(lines).rstrip() + "\n"


FAIL_STATUSES = {"failed", "timeout", "error", "judge_error", "unknown", "worker_failed"}
DENYLIST_NAMES = {".env", "agent.toml", "control_token", "token", "id_rsa", "id_dsa", "id_ed25519"}
DENYLIST_SUFFIXES = {
    ".jpg",
    ".jpeg",
    ".png",
    ".gif",
    ".webp",
    ".pdf",
    ".zip",
    ".tar",
    ".gz",
    ".pyc",
    ".bin",
    ".pem",
    ".key",
    ".crt",
    ".p12",
}
CODE_SUFFIXES = {
    ".py",
    ".go",
    ".cpp",
    ".c",
    ".h",
    ".hpp",
    ".ts",
    ".tsx",
    ".js",
    ".jsx",
    ".md",
    ".toml",
    ".json",
    ".sh",
}


def collect_context(run_dir: Path, repo_root: Path, cfg: AnalysisConfig) -> dict[str, Any]:
    warnings: list[str] = []
    if (run_dir / "manifest.json").exists():
        ctx = _collect_native_context(run_dir, repo_root, cfg, warnings)
    else:
        ctx = _collect_mobilegym_context(run_dir, repo_root, cfg, warnings)
    ctx["collection_warnings"] = warnings
    _enforce_context_budget(ctx, cfg)
    return ctx


def _read_json(path: Path, warnings: list[str]) -> dict[str, Any]:
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except Exception as exc:
        warnings.append(f"failed to read JSON {path}: {exc}")
        return {}
    return data if isinstance(data, dict) else {}


def _read_jsonl(path: Path, warnings: list[str]) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except OSError as exc:
        warnings.append(f"failed to read JSONL {path}: {exc}")
        return rows
    for line in lines:
        if not line.strip():
            continue
        try:
            row = json.loads(line)
        except json.JSONDecodeError as exc:
            warnings.append(f"invalid JSONL row in {path}: {exc}")
            continue
        if isinstance(row, dict):
            rows.append(row)
    return rows


def _read_excerpt(path: Path, max_bytes: int, cfg: AnalysisConfig, warnings: list[str]) -> tuple[str, bool]:
    try:
        data = path.read_bytes()
    except OSError as exc:
        warnings.append(f"failed to read {path}: {exc}")
        return "", False
    truncated = len(data) > max_bytes
    if truncated:
        data = data[:max_bytes]
    return redact_text(data.decode("utf-8", errors="replace"), cfg), truncated


def _safe_child_path(root: Path, *parts: str) -> Path | None:
    try:
        root_resolved = root.resolve()
        candidate = root.joinpath(*parts).resolve()
        candidate.relative_to(root_resolved)
    except (OSError, ValueError):
        return None
    return candidate


def _safe_path_within(root: Path, path: Path) -> Path | None:
    candidate = path if path.is_absolute() else root / path
    try:
        root_resolved = root.resolve()
        candidate = candidate.resolve()
        candidate.relative_to(root_resolved)
    except (OSError, ValueError):
        return None
    return candidate


def _native_artifact_dirs(run_dir: Path, task_dir: Path | None, row: dict[str, Any]) -> list[Path]:
    dirs: list[Path] = []

    def add(path: Path | None) -> None:
        if path is None or path in dirs or not path.is_dir():
            return
        dirs.append(path)

    artifact_dir = str(row.get("artifact_dir") or "")
    if artifact_dir:
        add(_safe_path_within(run_dir, Path(artifact_dir)))
    if task_dir is not None:
        add(task_dir)
        try:
            attempt = int(row.get("attempt") or 0)
        except (TypeError, ValueError):
            attempt = 0
        if attempt > 1:
            add(_safe_child_path(task_dir, f"attempt_{attempt}"))
        for attempt_dir in sorted(task_dir.glob("attempt_*")):
            add(_safe_path_within(run_dir, attempt_dir))
    return dirs


def _join_artifact_excerpts(items: list[tuple[str, str]]) -> str:
    return "\n\n".join(f"### {path}\n{excerpt}" for path, excerpt in items if excerpt)


def _collect_native_context(
    run_dir: Path, repo_root: Path, cfg: AnalysisConfig, warnings: list[str]
) -> dict[str, Any]:
    manifest = _read_json(run_dir / "manifest.json", warnings)
    results = _read_jsonl(run_dir / "results.jsonl", warnings) if (run_dir / "results.jsonl").exists() else []
    summary_excerpt = ""
    if (run_dir / "summary.md").exists():
        summary_excerpt, _ = _read_excerpt(run_dir / "summary.md", cfg.max_log_bytes, cfg, warnings)
    suite_path = Path(str(manifest.get("suite_path") or ""))
    suite_excerpt = ""
    if suite_path.exists():
        suite_excerpt, _ = _read_excerpt(suite_path, cfg.max_log_bytes, cfg, warnings)
    failures = []
    terms: set[str] = set()
    for row in results:
        status = str(row.get("status") or "unknown")
        if status not in FAIL_STATUSES:
            continue
        task_id = str(row.get("task_id") or row.get("id") or "")
        task_dir = _safe_child_path(run_dir / "tasks", task_id)
        if task_id and task_dir is None:
            warnings.append(f"skipped unsafe task artifact path for task_id={task_id!r}")
        artifact_dirs = _native_artifact_dirs(run_dir, task_dir, row)
        trace_items: list[tuple[str, str]] = []
        history_items: list[tuple[str, str]] = []
        judge_items: list[tuple[str, str]] = []
        artifact_refs: list[str] = []
        screenshot_refs: list[str] = []
        for artifact_dir in artifact_dirs:
            for name, items in (
                ("trace.json", trace_items),
                ("history.json", history_items),
                ("judge.json", judge_items),
            ):
                path = _safe_child_path(artifact_dir, name)
                if path is not None and path.exists():
                    excerpt, _ = _read_excerpt(path, cfg.max_log_bytes, cfg, warnings)
                    rel = str(path.relative_to(run_dir))
                    items.append((rel, excerpt))
                    artifact_refs.append(rel)
            for path in sorted(artifact_dir.glob("**/*")):
                if path.suffix.lower() in {".jpg", ".jpeg", ".png", ".webp"}:
                    rel = str(path.relative_to(run_dir))
                    screenshot_refs.append(rel)
                    artifact_refs.append(rel)
        trace_excerpt = _join_artifact_excerpts(trace_items)
        history_excerpt = _join_artifact_excerpts(history_items)
        judge_excerpt = _join_artifact_excerpts(judge_items)
        errors = _extract_errors(row)
        terms.update(
            _terms_from_text(
                json.dumps(row, ensure_ascii=False)
                + "\n"
                + trace_excerpt
                + "\n"
                + history_excerpt
                + "\n"
                + judge_excerpt
            )
        )
        failures.append(
            {
                "task_id": task_id,
                "status": status,
                "rubric": row.get("rubric") or [],
                "hard_assertions": row.get("hard_assertions") or {},
                "errors": errors,
                "trace_excerpt": trace_excerpt,
                "history_excerpt": history_excerpt,
                "judge_excerpt": judge_excerpt,
                "log_refs": [],
                "artifact_refs": list(dict.fromkeys(artifact_refs)),
                "screenshot_refs": screenshot_refs,
            }
        )
    return {
        "run": {
            "id": str(manifest.get("run_id") or run_dir.name),
            "kind": "native",
            "suite": str(manifest.get("suite_path") or ""),
            "totals": manifest.get("totals") or {},
        },
        "suite": {"path": str(suite_path), "content_excerpt": suite_excerpt},
        "summary_excerpt": summary_excerpt,
        "failures": failures,
        "known_issues": _known_issue_hints(failures),
        "logs": [],
        "code": _collect_code_context(repo_root, terms, cfg, warnings),
    }


def _extract_errors(row: dict[str, Any]) -> list[str]:
    errors = []
    metrics = row.get("metrics") if isinstance(row.get("metrics"), dict) else {}
    for key in ("error", "agent_error", "judge_error"):
        value = metrics.get(key) or row.get(key)
        if value:
            errors.append(f"{key}: {value}")
    execution = row.get("execution") if isinstance(row.get("execution"), dict) else {}
    for key in ("error", "stop_reason"):
        if execution.get(key):
            errors.append(f"execution.{key}: {execution[key]}")
    return errors


def _terms_from_text(text: str) -> set[str]:
    terms = set()
    for token in re.findall(r"[A-Za-z_][A-Za-z0-9_./-]{3,80}", text):
        if "/" in token:
            terms.add(token.rsplit("/", 1)[-1])
        if token.lower() in {"error", "failed", "timeout", "status", "tool_call", "response"}:
            continue
        terms.add(token)
    return {term for term in terms if len(term) >= 4}


def _safe_source_file(path: Path, repo_root: Path) -> bool:
    if path.name in DENYLIST_NAMES or path.suffix.lower() in DENYLIST_SUFFIXES:
        return False
    try:
        parts = set(path.relative_to(repo_root).parts)
    except ValueError:
        return False
    if {"runs", "__pycache__", ".git", "node_modules", "memory", "skill-state", "log"} & parts:
        return False
    if any(part.lower() in {"secrets", "credentials", "private", "tokens"} for part in parts):
        return False
    if path.suffix.lower() not in CODE_SUFFIXES:
        return False
    return True


def _collect_code_context(
    repo_root: Path, terms: set[str], cfg: AnalysisConfig, warnings: list[str]
) -> list[dict[str, Any]]:
    if not terms:
        return []
    roots = [repo_root / "src" / "agent", repo_root / "benchmark" / "runner", repo_root / "benchmark" / "mobilegym", repo_root / "src"]
    snippets = []
    used = 0
    lowered_terms = {term.lower() for term in terms}
    seen: set[Path] = set()
    for root in roots:
        if not root.exists():
            continue
        for path in root.rglob("*"):
            if path in seen or not path.is_file() or not _safe_source_file(path, repo_root):
                continue
            seen.add(path)
            try:
                text = path.read_text(encoding="utf-8", errors="replace")
            except OSError as exc:
                warnings.append(f"failed to read source {path}: {exc}")
                continue
            lower = text.lower()
            matched = next((term for term in lowered_terms if term in lower or term in path.name.lower()), "")
            if not matched:
                continue
            excerpt = redact_text(text[: min(len(text), 8000)], cfg)
            used += len(excerpt.encode("utf-8"))
            snippets.append({"path": str(path.relative_to(repo_root)), "excerpt": excerpt, "reason": f"matched {matched}"})
            if used >= cfg.max_code_bytes:
                return snippets
    return snippets


def _enforce_context_budget(ctx: dict[str, Any], cfg: AnalysisConfig) -> None:
    if len(json.dumps(ctx, ensure_ascii=False).encode("utf-8")) <= cfg.total_context_bytes:
        return
    warning = "analysis context truncated to fit total byte budget"

    def over_budget() -> bool:
        return len(json.dumps(ctx, ensure_ascii=False).encode("utf-8")) > cfg.total_context_bytes

    if isinstance(ctx.get("suite"), dict) and over_budget():
        ctx["suite"]["content_excerpt"] = str(ctx["suite"].get("content_excerpt") or "")[:1000]
    if over_budget() and ctx.get("summary_excerpt"):
        ctx["summary_excerpt"] = str(ctx.get("summary_excerpt") or "")[:1000]
    for failure in ctx.get("failures", []):
        if not isinstance(failure, dict) or not over_budget():
            continue
        for key in ("trace_excerpt", "history_excerpt", "judge_excerpt"):
            if over_budget() and failure.get(key):
                failure[key] = str(failure.get(key) or "")[:1000]
    for item in ctx.get("logs", []):
        if isinstance(item, dict) and over_budget():
            item["excerpt"] = str(item.get("excerpt") or "")[:1000]
            item["truncated"] = True
    while over_budget() and ctx.get("code"):
        ctx["code"].pop()
    for failure in ctx.get("failures", []):
        if not isinstance(failure, dict) or not over_budget():
            continue
        failure["trace_excerpt"] = str(failure.get("trace_excerpt") or "")[:300]
        failure["history_excerpt"] = str(failure.get("history_excerpt") or "")[:300]
        failure["judge_excerpt"] = str(failure.get("judge_excerpt") or "")[:300]
    if over_budget():
        ctx["logs"] = []
        ctx["code"] = []
    if over_budget():
        compact_failures = []
        for failure in ctx.get("failures", []):
            if not isinstance(failure, dict):
                continue
            compact_failures.append(
                {
                    "task_id": failure.get("task_id"),
                    "status": failure.get("status"),
                    "errors": failure.get("errors") or [],
                    "artifact_refs": failure.get("artifact_refs") or [],
                }
            )
        ctx["failures"] = compact_failures
        if isinstance(ctx.get("suite"), dict):
            ctx["suite"]["content_excerpt"] = ""
        ctx["summary_excerpt"] = ""
    ctx.setdefault("collection_warnings", []).append(warning)


def _collect_mobilegym_context(
    run_dir: Path, repo_root: Path, cfg: AnalysisConfig, warnings: list[str]
) -> dict[str, Any]:
    summary = _read_json(run_dir / "summary.json", warnings) if (run_dir / "summary.json").exists() else {}
    rows_by_task: dict[str, dict[str, Any]] = {}
    task_metadata: dict[str, dict[str, Any]] = {}
    terms: set[str] = set()
    logs = []
    failures = []
    for log_path in sorted(run_dir.glob("**/*.log")):
        if log_path.name not in {"runner.log", "console.log", "compose.log"}:
            continue
        excerpt, truncated = _read_excerpt(log_path, cfg.max_log_bytes, cfg, warnings)
        logs.append({"path": str(log_path.relative_to(run_dir)), "excerpt": excerpt, "truncated": truncated})
        terms.update(_terms_from_text(excerpt))
    for path in sorted(run_dir.glob("**/results.jsonl")) + sorted(run_dir.glob("**/errors.jsonl")):
        for row in _read_jsonl(path, warnings):
            task_id = str(row.get("id") or row.get("task_id") or row.get("name") or path.parent.name)
            rows_by_task.setdefault(task_id, {}).update(row)
            rows_by_task[task_id].setdefault("artifact_refs", []).append(str(path.relative_to(run_dir)))
            terms.update(_terms_from_text(json.dumps(row, ensure_ascii=False)))
    selected_ids: list[str] = []
    for shard_json in sorted(run_dir.glob("**/shard.json")):
        meta = _read_json(shard_json, warnings)
        selected_ids.extend(str(task_id) for task_id in meta.get("selected_task_ids") or [])
        embedded = meta.get("task_metadata") if isinstance(meta.get("task_metadata"), dict) else {}
        for task_id, value in embedded.items():
            if isinstance(value, dict):
                task_metadata[str(task_id)] = value
        terms.update(_terms_from_text(json.dumps(meta, ensure_ascii=False)))
    for action_path in sorted(run_dir.glob("**/aico_bridge_actions.json")):
        excerpt, truncated = _read_excerpt(action_path, cfg.max_log_bytes, cfg, warnings)
        logs.append({"path": str(action_path.relative_to(run_dir)), "excerpt": excerpt, "truncated": truncated})
        terms.update(_terms_from_text(excerpt))
    task_ids = list(dict.fromkeys(selected_ids + sorted(rows_by_task)))
    for task_id in task_ids:
        row = dict(task_metadata.get(task_id) or {})
        row.update(rows_by_task.get(task_id, {}))
        status, reason = _mobilegym_status(row)
        if status not in FAIL_STATUSES:
            continue
        failures.append(
            {
                "task_id": task_id,
                "status": status,
                "rubric": row.get("rubric") or row.get("rubric_spec") or [],
                "hard_assertions": row.get("hard_assertions") or {},
                "errors": _extract_errors(row) or ([reason] if reason else []),
                "trace_excerpt": redact_text(json.dumps(row, ensure_ascii=False)[: cfg.max_log_bytes], cfg),
                "history_excerpt": redact_text(
                    json.dumps(row.get("aico_last_chat_history") or [], ensure_ascii=False)[: cfg.max_log_bytes],
                    cfg,
                ),
                "log_refs": [
                    item["path"]
                    for item in logs
                    if task_id in item.get("excerpt", "") or (reason and reason in item.get("excerpt", ""))
                ],
                "artifact_refs": row.get("artifact_refs") or [],
            }
        )
    known_issues = _known_issue_hints(failures)
    return {
        "run": {
            "id": str(summary.get("batch_id") or run_dir.name),
            "kind": "mobilegym",
            "suite": str(summary.get("suite") or "mobilegym"),
            "totals": {
                key: summary.get(key)
                for key in ("tasks", "passed", "failed", "timeout", "error", "unknown", "worker_failed")
                if key in summary
            },
        },
        "suite": {"path": "", "content_excerpt": ""},
        "failures": failures,
        "known_issues": known_issues,
        "logs": logs,
        "code": _collect_code_context(repo_root, terms, cfg, warnings),
    }


def _known_issue_hints(failures: list[dict[str, Any]]) -> list[dict[str, Any]]:
    bridge_inactive_tasks: list[str] = []
    bridge_inactive_evidence: list[str] = []
    for failure in failures:
        task_id = str(failure.get("task_id") or "")
        candidates = [str(item) for item in failure.get("errors") or []]
        for key in ("trace_excerpt", "history_excerpt", "judge_excerpt"):
            if failure.get(key):
                candidates.append(str(failure.get(key)))
        for text in candidates:
            match = BRIDGE_INACTIVE_SETUP_RE.search(text)
            if not match:
                continue
            if task_id and task_id not in bridge_inactive_tasks:
                bridge_inactive_tasks.append(task_id)
            evidence = _clean_error_evidence(match.group(0))
            if evidence not in bridge_inactive_evidence:
                bridge_inactive_evidence.append(evidence)
            break
    if not bridge_inactive_tasks:
        return []
    return [
        {
            "id": "mobilegym_setup_before_episode_start",
            "category": "benchmark_infra_issue",
            "task_ids": bridge_inactive_tasks,
            "summary": "MobileGym setup tools ran before the bridge episode was active.",
            "evidence": bridge_inactive_evidence,
            "suspected_cause": (
                "Aico Agent reset/setup lifecycle invoked setup tool_sequence before "
                "bridge /episode/start and daemon /api/mobilegym/episode/start bound an active episode."
            ),
        }
    ]


def _clean_error_evidence(text: str) -> str:
    text = str(text).strip()
    for prefix in ("error: ", "agent_error: ", "execution.error: "):
        if text.startswith(prefix):
            return text[len(prefix) :].strip()
    return text


def _mobilegym_status(row: dict[str, Any]) -> tuple[str, str]:
    error_text = str(row.get("error") or row.get("message") or "")
    execution = row.get("execution") if isinstance(row.get("execution"), dict) else {}
    stop_reason = str(execution.get("stop_reason") or row.get("stop_reason") or "")
    timeout_text = (error_text + stop_reason).lower()
    if "timeout" in timeout_text or "aicoadaptertimeout" in timeout_text:
        return "timeout", error_text or stop_reason
    if error_text or row.get("is_error") is True:
        return "error", error_text or stop_reason or "is_error"
    if row.get("is_success") is True or row.get("status") == "passed":
        return "passed", "passed"
    if row.get("is_success") is False or row.get("status") == "failed":
        return "failed", stop_reason or "failed"
    return "unknown", "missing or unrecognized result"


SYSTEM_PROMPT = """你是 benchmark 根因分析专家，不是 judge。
请分析 benchmark 结果、trace/history 摘要、运行日志和源码片段。
所有结论必须基于证据；证据不足时要明确说明。只返回 JSON。"""


def _analysis_user_prompt(context: dict[str, Any]) -> str:
    return """分析这个 benchmark run，并返回 JSON，必须包含这些 key：
summary, failure_clusters, recommendations, classification_counts, evidence_gaps。
所有自然语言字段请使用简体中文。
根因分类必须是以下之一：suite_issue, project_code_issue, agent_behavior_issue,
benchmark_infra_issue, environment_issue, evaluation_issue, insufficient_evidence。
如果 CONTEXT JSON 包含 known_issues，请把它们视为已有证据支持的根因提示，直接采用，
或者说明为什么更强证据推翻了它们。不要在失败摘要/日志没有直接证据时推断环境原因，
例如平台不匹配或目录缺失。对于 "setup tool ... mobilegym bridge episode is not active"，
优先归类为 MobileGym adapter episode/setup 生命周期顺序相关的 benchmark_infra_issue，
不要优先归因于 agent 行为。

CONTEXT JSON:
""" + json.dumps(context, ensure_ascii=False, indent=2)


def chat_analysis_model(cfg: AnalysisConfig, context: dict[str, Any], api_key: str) -> str:
    payload = json.dumps(
        {
            "model": cfg.model,
            "messages": [
                {"role": "system", "content": SYSTEM_PROMPT},
                {"role": "user", "content": _analysis_user_prompt(context)},
            ],
            "max_tokens": 4096,
        }
    ).encode("utf-8")
    req = urllib.request.Request(
        f"{(cfg.base_url or DEFAULT_BASE_URL).rstrip('/')}/chat/completions",
        data=payload,
        headers={"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=cfg.timeout_sec) as resp:
            if resp.status != 200:
                raise AnalysisError(f"analysis HTTP {resp.status}")
            body = json.loads(resp.read())
    except urllib.error.HTTPError as exc:
        raise AnalysisError(f"analysis HTTP {exc.code}: {exc.read()[:200]!r}") from exc
    except (socket.timeout, urllib.error.URLError) as exc:
        raise AnalysisError(f"analysis network error: {exc}") from exc
    try:
        content = body["choices"][0]["message"]["content"]
    except (KeyError, IndexError, TypeError) as exc:
        raise AnalysisError(f"unexpected analysis response shape: {exc}") from exc
    if not isinstance(content, str):
        raise AnalysisError("unexpected analysis response content type")
    return content


def extract_analysis_json(raw: str) -> dict[str, Any]:
    text = raw.strip()
    if text.startswith("```"):
        parts = text.split("```", 2)
        if len(parts) >= 2:
            text = parts[1]
            if text.startswith("json\n"):
                text = text[5:]
    start = text.find("{")
    end = text.rfind("}")
    if start == -1 or end == -1 or end <= start:
        raise AnalysisError(f"analysis response has no JSON object: {raw[:200]!r}")
    try:
        parsed = json.loads(text[start : end + 1])
    except json.JSONDecodeError as exc:
        raise AnalysisError(f"analysis response JSON parse failed: {exc}: {raw[:200]!r}") from exc
    return parsed if isinstance(parsed, dict) else {"summary": str(parsed)}


def _normalize_analysis_payload(context: dict[str, Any], payload: dict[str, Any]) -> dict[str, Any]:
    if not isinstance(payload, dict):
        return {"summary": str(payload)}
    payload = dict(payload)
    _apply_known_issues_to_payload(context, payload)
    _normalize_failure_cluster_categories(payload)
    return payload


def _apply_known_issues_to_payload(context: dict[str, Any], payload: dict[str, Any]) -> None:
    known_issues = context.get("known_issues") if isinstance(context.get("known_issues"), list) else []
    if not known_issues:
        return
    clusters = payload.get("failure_clusters")
    if not isinstance(clusters, list):
        clusters = []
    clusters = [cluster if isinstance(cluster, dict) else {} for cluster in clusters]
    payload["failure_clusters"] = clusters
    counts = payload.get("classification_counts")
    if not isinstance(counts, dict):
        counts = {}
        payload["classification_counts"] = counts

    for issue in known_issues:
        if not isinstance(issue, dict):
            continue
        issue_id = str(issue.get("id") or "")
        if issue_id == "mobilegym_setup_before_episode_start":
            _apply_bridge_inactive_issue(issue, payload)


def _apply_bridge_inactive_issue(issue: dict[str, Any], payload: dict[str, Any]) -> None:
    task_ids = [str(task_id) for task_id in issue.get("task_ids") or []]
    cluster = {
        "title": "MobileGym setup 早于 bridge episode 激活执行",
        "category": "benchmark_infra_issue",
        "confidence": "high",
        "task_ids": task_ids,
        "suspected_cause": issue.get("suspected_cause") or issue.get("summary") or "",
        "evidence": issue.get("evidence") or [],
    }
    clusters = payload.setdefault("failure_clusters", [])
    target_index = None
    for index, existing in enumerate(clusters):
        if not isinstance(existing, dict):
            continue
        text = json.dumps(existing, ensure_ascii=False).lower()
        if "mobilegym bridge episode is not active" in text or "bridge episode" in text:
            target_index = index
            break
    if target_index is None:
        clusters.insert(0, cluster)
    else:
        existing = clusters[target_index] if isinstance(clusters[target_index], dict) else {}
        merged = {**existing, **cluster}
        clusters[target_index] = merged
    counts = payload.setdefault("classification_counts", {})
    if isinstance(counts, dict):
        current = _safe_int(counts.get("benchmark_infra_issue"))
        counts["benchmark_infra_issue"] = max(current, len(task_ids) or 1)
    recommendations = payload.get("recommendations")
    if not isinstance(recommendations, list):
        recommendations = []
        payload["recommendations"] = recommendations
    lifecycle_rec = {
        "priority": "high",
        "target": "benchmark/mobilegym/adapter/aico_go_agent.py",
        "suggestion": "Run Aico suite global_reset/setup only after bridge /episode/start and daemon /api/mobilegym/episode/start activate the MobileGym episode.",
    }
    if not any("/api/mobilegym/episode/start" in json.dumps(item, ensure_ascii=False) for item in recommendations):
        recommendations.insert(0, lifecycle_rec)


def _normalize_failure_cluster_categories(payload: dict[str, Any]) -> None:
    clusters = payload.get("failure_clusters")
    if not isinstance(clusters, list):
        return
    fallback = _dominant_classification(payload.get("classification_counts"))
    for cluster in clusters:
        if not isinstance(cluster, dict):
            continue
        category = str(cluster.get("category") or "").strip()
        if category not in ROOT_CAUSE_CATEGORIES:
            cluster["category"] = fallback or "insufficient_evidence"
        if not str(cluster.get("confidence") or "").strip():
            cluster["confidence"] = "medium"


def _dominant_classification(counts: Any) -> str:
    if not isinstance(counts, dict):
        return ""
    positive = [
        (str(category), _safe_int(value))
        for category, value in counts.items()
        if str(category) in ROOT_CAUSE_CATEGORIES and _safe_int(value) > 0
    ]
    if len(positive) != 1:
        return ""
    return positive[0][0]


def _safe_int(value: Any) -> int:
    try:
        return int(value)
    except (TypeError, ValueError):
        return 0


def _atomic_write(path: Path, text: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(text, encoding="utf-8")
    tmp.replace(path)


def analyze_run(run_dir: Path, repo_root: Path, cfg: AnalysisConfig) -> AnalysisResult:
    if not cfg.enabled:
        return AnalysisResult(ok=False, warning="analysis disabled")
    try:
        key_name, api_key = resolve_analysis_api_key(cfg)
        context = collect_context(run_dir, repo_root, cfg)
        context = json.loads(redact_text(json.dumps(context, ensure_ascii=False), cfg))
        raw = chat_analysis_model(cfg, context, api_key)
        payload = extract_analysis_json(redact_text(raw, cfg))
        payload = _normalize_analysis_payload(context, payload)
        payload.setdefault("metadata", {})
        payload["metadata"].update({"model": cfg.model, "api_key_env": key_name, "run_dir": str(run_dir)})
        json_path = run_dir / "llm_analysis.json"
        md_path = run_dir / "llm_analysis.md"
        _atomic_write(json_path, json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True) + "\n")
        _atomic_write(md_path, render_markdown(payload))
        return AnalysisResult(ok=True, markdown_path=md_path, json_path=json_path)
    except Exception as exc:
        message = redact_text(str(exc), cfg)
        error_path = run_dir / "llm_analysis_error.txt"
        _atomic_write(error_path, message + "\n")
        return AnalysisResult(ok=False, error_path=error_path, warning=message)
