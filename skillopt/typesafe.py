"""Minimal client for TypeSafe's System One API (the Jev decision model).

Used by the optional sample-quality classifier in score.py. Jev answers typed
questions (noul = yes probability, choice, score) about a state; it never
generates text. Requests use stdlib urllib like the benchmark runner, so
HTTPS_PROXY applies (the API is region-restricted).
"""
from __future__ import annotations

import json
import os
import socket
import time
import urllib.error
import urllib.request
from typing import Any

DEFAULT_BASE_URL = "https://api.typesafe.ai/v1"
DEFAULT_MODEL = "jev-preview"
DEFAULT_API_KEY_ENV = "TYPESAFE_API_KEY"
USER_AGENT = "aiden-skillopt/1.0"

_RETRY_STATUSES = {429, 529}
_MAX_RETRIES = 2


def resolve_api_key(api_key_env: str = DEFAULT_API_KEY_ENV) -> str:
    api_key = os.environ.get(api_key_env, "").strip()
    if not api_key:
        raise RuntimeError(f"missing env var {api_key_env}")
    return api_key


def ask(
    state: Any,
    questions: dict[str, dict[str, Any]],
    *,
    api_key: str,
    base_url: str = DEFAULT_BASE_URL,
    model: str = DEFAULT_MODEL,
    assets: dict[str, dict[str, str]] | None = None,
    timeout: float = 60,
) -> dict[str, Any]:
    """Send one System One request, retrying 429/529 with backoff."""
    body: dict[str, Any] = {"model": model, "state": state, "questions": questions}
    headers = {
        "Authorization": f"Bearer {api_key}",
        "Content-Type": "application/json",
        "User-Agent": USER_AGENT,
    }
    if assets:
        body["assets"] = assets
    if assets or any(q.get("type") == "box" for q in questions.values()):
        headers["TypeSafe-Danger-Zone-Images"] = "true"
    payload = json.dumps(body, ensure_ascii=False).encode("utf-8")
    url = f"{(base_url or DEFAULT_BASE_URL).rstrip('/')}/systemone"
    if not url.lower().startswith(("https://", "http://")):
        raise RuntimeError(f"unsupported typesafe base_url scheme: {url!r}")
    for attempt in range(_MAX_RETRIES + 1):
        req = urllib.request.Request(url, data=payload, headers=headers, method="POST")
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                return json.loads(resp.read())
        except urllib.error.HTTPError as e:
            if e.code in _RETRY_STATUSES and attempt < _MAX_RETRIES:
                time.sleep(0.4 * (2 ** attempt))
                continue
            raise RuntimeError(f"typesafe HTTP {e.code}: {e.read()[:200]!r}") from e
        except (socket.timeout, urllib.error.URLError) as e:
            raise RuntimeError(f"typesafe network error: {e}") from e
    raise RuntimeError("typesafe request retries exhausted")


def answer(response: dict[str, Any], question_id: str, question_type: str) -> dict[str, Any]:
    got = (response.get("answers") or {}).get(question_id)
    if not isinstance(got, dict) or got.get("type") != question_type:
        raise RuntimeError(f"typesafe response has no {question_type} answer {question_id!r}")
    return got
