"""Quick memory benchmark runner — runs 4 representative tasks against the live agent.

Tasks chosen one per group (save / use / lifecycle / negative).
Skips the LLM judge; uses hard assertions only for fast feedback.
"""
import json
import os
import sys
import time
import urllib.request
import subprocess
from pathlib import Path

AGENT_URL = os.environ.get("AICO_BENCH_AGENT_URL", "http://device.example:8080")
SSH_HOST = os.environ.get("AICO_BENCH_SSH_HOST", "root@device.example")
MEMORY_DIR = "/userdata/agent/memory"


def http_post(path, body=None, timeout=180):
    req = urllib.request.Request(
        f"{AGENT_URL}{path}",
        data=json.dumps(body or {}).encode("utf-8"),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read().decode("utf-8")
    except Exception as e:
        return 0, str(e)


def ssh(cmd):
    return subprocess.run(
        ["ssh", SSH_HOST, cmd],
        capture_output=True, text=True, timeout=15,
    ).stdout.strip()


def clear_state():
    ssh(f"rm -rf {MEMORY_DIR}/long_term {MEMORY_DIR}/session")
    http_post("/api/clear")


def chat(message, timeout=180):
    status, body = http_post("/api/chat", {"message": message}, timeout=timeout)
    if status != 200:
        return None, [], f"HTTP {status}: {body[:200]}"
    data = json.loads(body)
    return data.get("response", ""), data.get("history", []), None


def tool_calls_in(history, name):
    return [m for m in history if m.get("type") == "tool_call" and m.get("tool_name") == name]


def list_long_term():
    out = ssh(f"ls {MEMORY_DIR}/long_term/memories/ 2>/dev/null")
    return [f for f in out.split("\n") if f.endswith(".md")]


def read_long_term(filename):
    return ssh(f"cat {MEMORY_DIR}/long_term/memories/{filename}")


def run_task(task_id, fn):
    print(f"\n{'='*60}\n[{task_id}]")
    t0 = time.monotonic()
    try:
        result = fn()
    except Exception as e:
        result = ("FAIL", f"runner exception: {e}")
    wall = int((time.monotonic() - t0) * 1000)
    status, msg = result
    print(f"  status: {status}  wall: {wall}ms")
    print(f"  {msg}")
    return task_id, status, wall, msg


def task_save_explicit_preference():
    clear_state()
    response, history, err = chat("记住我喜欢用 dark mode，所有界面都给我切深色。", timeout=120)
    if err:
        return "FAIL", err
    saves = tool_calls_in(history, "save_memory")
    if not saves:
        return "FAIL", f"agent didn't call save_memory. response: {response[:120]!r}"
    inp = json.loads(saves[0].get("tool_input", "{}"))
    files = list_long_term()
    info = (
        f"save_memory called {len(saves)}x; "
        f"type={inp.get('type')!r} priority={inp.get('priority')} content={inp.get('content','')[:60]!r}; "
        f"files={files}"
    )
    if inp.get("type") != "preference":
        return "FAIL", f"expected type=preference, got {inp.get('type')!r}"
    return "PASS", info


def task_forget_on_request():
    clear_state()
    # Save 3 prefs naturally
    for msg in [
        "记住我喜欢用 dark mode。",
        "记住我希望回答简短。",
        "记住所有列表都用 markdown 格式。",
    ]:
        _, h, err = chat(msg, timeout=90)
        if err:
            return "FAIL", f"preload failed at '{msg[:30]}': {err}"
        if not tool_calls_in(h, "save_memory"):
            return "FAIL", f"agent didn't save for '{msg[:30]}'"
    files_before = list_long_term()
    http_post("/api/clear")
    response, history, err = chat("请把关于 dark mode 的偏好忘掉。", timeout=120)
    if err:
        return "FAIL", err
    forgets = tool_calls_in(history, "forget_memory")
    recalls = tool_calls_in(history, "recall_memory")
    files_after = list_long_term()
    info = (
        f"files_before={len(files_before)} after={len(files_after)}; "
        f"recall calls: {len(recalls)}; forget calls: {len(forgets)}; response: {response[:80]!r}"
    )
    if not recalls and not forgets:
        return "FAIL", f"agent didn't call recall or forget. {info}"
    if not forgets:
        return "FAIL", f"agent recalled but didn't forget. {info}"
    return "PASS", info


def task_no_recall_when_in_context():
    clear_state()
    # Turn 1: state a fact
    _, _, err = chat("记一下，今天会议室是 B201。", timeout=90)
    if err:
        return "FAIL", f"turn1 failed: {err}"
    # Turn 2: same session, ask about it (don't clear history)
    response, history, err = chat("会议室在哪？", timeout=90)
    if err:
        return "FAIL", err
    recall_count = (
        len(tool_calls_in(history, "recall_memory"))
        + len(tool_calls_in(history, "recall_session_chunks"))
    )
    info = f"recall calls in turn 2: {recall_count}; response: {response[:120]!r}"
    if "B201" not in response:
        return "FAIL", f"response doesn't mention B201. {info}"
    if recall_count > 0:
        return "PARTIAL", f"answered correctly but called recall unnecessarily. {info}"
    return "PASS", info


def main():
    tasks = [
        ("save_explicit_preference", task_save_explicit_preference),
        ("forget_on_request", task_forget_on_request),
        ("no_recall_when_in_context", task_no_recall_when_in_context),
    ]
    results = []
    for tid, fn in tasks:
        results.append(run_task(tid, fn))
    print(f"\n{'='*60}\nSUMMARY")
    for tid, status, wall, _ in results:
        print(f"  {status:8s} {tid:30s} {wall}ms")


if __name__ == "__main__":
    main()
