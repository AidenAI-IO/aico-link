from __future__ import annotations
import base64
import dataclasses as dc
import hashlib
import json
import os
import socket
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any

from runner.models import RubricVerdict
from runner.suite import RubricItem

JUDGE_PROMPT_VERSION = "v2"
DEFAULT_JUDGE_BASE_URL = "https://openrouter.ai/api/v1"
DEFAULT_JUDGE_API_KEY_ENV = "AIDEN_BENCHMARK_JUDGE_API_KEY"
JUDGE_USER_AGENT = "aiden-benchmark/1.0"

@dc.dataclass
class JudgeConfig:
    provider: str = "openai-compatible"
    model: str = "anthropic/claude-sonnet-4-6"
    api_key_env: str = DEFAULT_JUDGE_API_KEY_ENV
    base_url: str = DEFAULT_JUDGE_BASE_URL

@dc.dataclass
class JudgeOutput:
    verdicts: list[RubricVerdict]
    overall_notes: str
    cache_key: str
    raw_response: str
    image_count: int = 0
    image_labels: list[str] = dc.field(default_factory=list)

JUDGE_TEMPLATE = """You are evaluating whether an agent completed a task.

TASK GOAL: {description}

Below are:
- Pre-screenshot: state before the agent acted (when applicable)
- Post-screenshot: state after the agent finished (when applicable)
- Tool trace: every action the agent took
- Agent's final reply: what the agent said it did

For each rubric item, answer ONLY "yes" or "no" with a one-sentence reason
grounded in the screenshots/trace/response. Do not be lenient. If evidence
does not clearly show the required state, answer "no".
A tool call is an attempt, not proof of execution. Check its result: errors,
is_error=true, and ok=false are failures. Never credit a failed call as a
successfully performed action. The final reply alone is not proof of success.

RUBRIC:
{rubric_lines}

Respond as JSON only, no prose:
{{
  "items": [{{"id": "...", "verdict": "yes" or "no", "reason": "..."}}, ...],
  "overall_notes": "..."
}}"""

def _read_image_b64(p: Path) -> str:
    return base64.b64encode(p.read_bytes()).decode("ascii")

def _cache_key(pre: Path | None, post: Path | None, trace_json: str, rubric: list[RubricItem],
               description: str, final_response: str, model: str, base_url: str) -> str:
    h = hashlib.sha256()
    if pre is not None:
        h.update(pre.read_bytes())
    if post is not None:
        h.update(post.read_bytes())
    h.update(trace_json.encode("utf-8"))
    h.update(description.encode("utf-8"))
    h.update(final_response.encode("utf-8"))
    for r in rubric:
        h.update(r.id.encode()); h.update(r.check.encode())
    h.update(model.encode())
    h.update(base_url.encode())
    h.update(JUDGE_PROMPT_VERSION.encode())
    return h.hexdigest()

def _chat_completions_url(base_url: str) -> str:
    return f"{(base_url or DEFAULT_JUDGE_BASE_URL).rstrip('/')}/chat/completions"


def _resolve_judge_api_key(cfg: JudgeConfig) -> str:
    api_key = os.environ.get(cfg.api_key_env, "").strip()
    if not api_key:
        raise RuntimeError(f"missing env var {cfg.api_key_env}")
    return api_key

def _judge_images(
    pre_screenshot: Path | None,
    post_screenshot: Path | None,
) -> list[tuple[str, Path]]:
    images: list[tuple[str, Path]] = []
    seen: set[Path] = set()

    def add(label: str, path: Path | None) -> None:
        if path is None or not path.exists():
            return
        resolved = path.resolve()
        if resolved in seen:
            return
        seen.add(resolved)
        images.append((label, path))

    add("PRE-SCREENSHOT", pre_screenshot)
    add("POST-SCREENSHOT", post_screenshot)
    return images

def judge_task(
    description: str,
    rubric: list[RubricItem],
    pre_screenshot: Path | None,
    post_screenshot: Path | None,
    trace: dict[str, Any],
    final_response: str,
    cfg: JudgeConfig,
    cache_dir: Path | None = None,
) -> JudgeOutput:
    trace_json = json.dumps(trace, ensure_ascii=False, sort_keys=True)
    image_items = _judge_images(pre_screenshot, post_screenshot)
    image_labels = [label for label, _ in image_items]
    key = _cache_key(pre_screenshot, post_screenshot, trace_json, rubric, description,
                     final_response, cfg.model, cfg.base_url)
    if cache_dir is not None:
        cached = cache_dir / f"{key}.json"
        if cached.exists():
            data = json.loads(cached.read_text("utf-8"))
            verdicts = [RubricVerdict(**v) for v in data["verdicts"]]
            return JudgeOutput(verdicts=verdicts, overall_notes=data["overall_notes"],
                               cache_key=key, raw_response=data["raw_response"],
                               image_count=len(image_items), image_labels=image_labels)
    rubric_lines = "\n".join(f"{i+1}. {{\"id\": \"{r.id}\", \"check\": \"{r.check}\"}}"
                              for i, r in enumerate(rubric))
    prompt = JUDGE_TEMPLATE.format(
        description=description, rubric_lines=rubric_lines,
    )
    # Build OpenAI-compatible multimodal content
    user_content: list[dict[str, Any]] = [{"type": "text", "text": prompt}]
    for label, path in image_items:
        user_content += [
            {"type": "text", "text": f"{label}:"},
            {"type": "image_url", "image_url": {
                "url": f"data:image/jpeg;base64,{_read_image_b64(path)}"}},
        ]
    user_content += [
        {"type": "text", "text": f"TOOL TRACE:\n{trace_json}"},
        {"type": "text", "text": f"FINAL RESPONSE:\n{final_response}"},
    ]
    # Use an OpenAI-compatible chat completions endpoint.
    api_key = _resolve_judge_api_key(cfg)
    payload = json.dumps({
        "model": cfg.model,
        "messages": [{"role": "user", "content": user_content}],
        "max_tokens": 1024,
    }).encode("utf-8")
    req = urllib.request.Request(
        _chat_completions_url(cfg.base_url),
        data=payload,
        headers={
            "Authorization": f"Bearer {api_key}",
            "Content-Type": "application/json",
            "User-Agent": JUDGE_USER_AGENT,
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=120) as resp:
            if resp.status != 200:
                raise RuntimeError(f"judge HTTP {resp.status}")
            body = json.loads(resp.read())
    except urllib.error.HTTPError as e:
        raise RuntimeError(f"judge HTTP {e.code}: {e.read()[:200]!r}") from e
    except (socket.timeout, urllib.error.URLError) as e:
        raise RuntimeError(f"judge network error: {e}") from e
    raw = body["choices"][0]["message"]["content"]
    parsed = _parse_judge_json(raw)
    verdicts = [RubricVerdict(id=v["id"], verdict=v["verdict"], reason=v["reason"])
                for v in parsed["items"]]
    out = JudgeOutput(verdicts=verdicts, overall_notes=parsed.get("overall_notes", ""),
                      cache_key=key, raw_response=raw,
                      image_count=len(image_items), image_labels=image_labels)
    if cache_dir is not None:
        cache_dir.mkdir(parents=True, exist_ok=True)
        (cache_dir / f"{key}.json").write_text(json.dumps({
            "verdicts": [dc.asdict(v) for v in verdicts],
            "overall_notes": out.overall_notes,
            "raw_response": raw,
            "image_count": out.image_count,
            "image_labels": out.image_labels,
        }), encoding="utf-8")
    return out

def _parse_judge_json(raw: str) -> dict[str, Any]:
    s = raw.strip()
    start = s.find("{")
    end = s.rfind("}")
    if start == -1 or end == -1:
        raise ValueError(f"judge response has no JSON object: {raw[:200]}")
    return json.loads(s[start:end+1])
