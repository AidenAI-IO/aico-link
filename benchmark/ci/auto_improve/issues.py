"""Render confirmed findings as issues written for a person working with a coding agent.

Model-written text is untrusted: mentions, HTML comments and oversized fields are
neutralized here, and every structural element (metadata, task facts, commands) is
produced by this module from validated data.
"""
from __future__ import annotations

import json
import re

FIELD_LIMIT = 4000
BODY_LIMIT = 60000


def clean(text: object, limit: int = FIELD_LIMIT) -> str:
    """Keep model text inert: no notifications, hidden markers, or unbounded length."""
    value = str(text or "").replace("\r", "")
    value = value.replace("<!--", "&lt;!--").replace("-->", "--&gt;")
    # Break @mentions and links to other issues so filing creates no notification or backlink.
    value = re.sub(r"(?<![\w`])@(?=[A-Za-z0-9])", "@\u200b", value)
    value = re.sub(r"\b([\w.-]+/[\w.-]+)#(\d+)", "\\1#\u200b\\2", value)
    value = re.sub(r"https?://github\.com/([\w.-]+/[\w.-]+)/(issues|pull)/(\d+)",
                   "github.com/\\1/\\2/\u200b\\3", value)
    return value if len(value) <= limit else value[:limit] + " …"


def bullets(items: list, empty: str = "- None recorded") -> list[str]:
    return [f"- {clean(item, 600)}" for item in items] or [empty]


def reproduce_command(task: dict, key: str) -> str:
    """The runner invocation CI uses for this task: isolated daemons, the case's mode and platform."""
    task_id = key.split(":", 1)[1]
    flags = [f"--suite {task['suite']}", f"--task-id {task_id}", "--auto-agent-setup"]
    if task.get("execution_mode", "backend-text") != "backend-text":
        flags.append(f"--execution-mode {task['execution_mode']}")
    if task.get("target_platform", "auto") != "auto":
        flags.append(f"--target-platform {task['target_platform']}")
    if task.get("environment") not in (None, "isolated"):
        flags.append("--environment-url <environment bridge URL>")
    return "cd benchmark\nuv run python -m runner run " + " \\\n  ".join(flags)


def revision(manifest: dict) -> str:
    """Where the analysis ran: the branch and commit, never assumed to be main."""
    ref = str(manifest.get("execution_ref", "")).removeprefix("refs/heads/") or "local"
    return f"`{ref}` `{manifest['base_sha'][:12]}`"


def window(manifest: dict) -> str:
    """A readable daily window; the exact run identities stay in hidden markers."""
    dates = sorted(d for d in (s.get("date") for s in manifest.get("sources", [])) if d)
    count = len(manifest.get("sources", [])) or len(manifest["source"].split("__"))
    span = (f"{dates[0]}" if dates[0] == dates[-1] else f"{dates[0]} to {dates[-1]}") if dates else "the latest window"
    empty = len(manifest.get("empty_sources", []))
    return f"{count} daily run{'s' if count != 1 else ''} ({span}" + (f"; {empty} ran no tasks)" if empty else ")")


def title(finding: dict) -> str:
    return clean(f"[auto-improve][{finding['key']['domain']}] {finding['issue']['title']}", 200).replace("\n", " ")


def labels(finding: dict) -> list[str]:
    # The domain is already in the title; one label keeps filtering simple.
    return ["auto-improve"]


def body(finding: dict, manifest: dict, run_url: str) -> str:
    issue, key = finding["issue"], finding["key"]
    meta = {**key, "signature": finding["signature"]}
    tasks = finding["tasks"]
    # The issue itself is this window's observation, so a retried publish does not also comment.
    lines = [f"<!-- auto-improve {json.dumps(meta, sort_keys=True)} -->",
             f"<!-- auto-improve-observation:{manifest['source']} -->", "", "## Summary", clean(issue["summary"]), ""]
    if finding.get("regression_of"):
        lines += ["Possibly a regression of " + ", ".join(f"#{n}" for n in finding["regression_of"]) + ".", ""]
    lines += ["## Impact", clean(issue["impact"]), "",
              f"| Task | Environment | Failed daily runs | Reproduced on {revision(manifest)} |",
              "| --- | --- | --- | --- |"]
    for target in key["targets"]:
        reproduced = finding.get("reproduction", {}).get(target)
        state = ("yes" if reproduced["reproduced"] else f"no ({clean(reproduced['reason'], 120)})") if reproduced else "not run"
        lines.append(f"| `{target}` | {tasks.get(target, {}).get('environment', 'unknown')} | "
                     f"{finding['failures_by_target'].get(target, 0)}/{finding['window_runs']} | {state} |")
    lines += ["", "## Reproduce"]
    runnable = [t for t in key["targets"] if t in tasks and tasks[t].get("suite")]
    if runnable:
        lines += ["With the agent and judge model settings from the benchmark quickstart exported, run "
                  "(the runner starts isolated agent daemons itself):", ""]
        for target in runnable:
            lines += ["```bash", reproduce_command(tasks[target], target), "```"]
        if any(tasks[t].get("environment") not in (None, "isolated") for t in runnable):
            lines += ["Start the task's environment bridge first and pass its URL, as described in the "
                      "benchmark quickstart."]
    lines += ["", clean(issue["reproduction"]), "", "## Expected", clean(issue["expected"]), "",
              "## Actual", clean(issue["actual"]), "", "## Root cause", clean(issue["root_cause"]), "",
              "## Code locations", *[f"- `{loc}`" for loc in finding["code_locations"]], "",
              "## Evidence", "Paths under `finding-*/` are in the run's evidence artifact linked below. "
              f"`finding-{finding['number']}/daily-observations.json` holds each daily run's outcome for these tasks.", "",
              *bullets(issue["evidence"]), "", "## Acceptance criteria", *bullets(issue["acceptance_criteria"])]
    if issue.get("fix_direction"):
        lines += ["", "## Suggested direction", "Non-binding; verify against the code before following it.", "",
                  clean(issue["fix_direction"])]
    lines += ["", "---", f"Filed by benchmark auto-improve from {window(manifest)}, analyzed on {revision(manifest)}. "
              "An independent read-only audit confirmed this finding against the fresh reproduction and "
              "retained traces.",
              f"Evidence artifacts (14-day retention): {run_url}" if run_url else "",
              "If the finding is wrong, close the issue as not planned (or label it `auto-improve:false-positive` and close it); "
              "the pipeline will not file these tasks again."]
    text = "\n".join(lines)
    return text if len(text) <= BODY_LIMIT else text[:BODY_LIMIT] + "\n\n(truncated)"


def observation(finding: dict, manifest: dict, run_url: str) -> str:
    """A follow-up on an existing open issue; the marker keeps it to one comment per window."""
    reproduced = [t for t, r in finding.get("reproduction", {}).items() if r["reproduced"]]
    lines = [f"<!-- auto-improve-observation:{manifest['source']} -->",
             f"Still observed in {window(manifest)}, analyzed on {revision(manifest)}.", ""]
    for target in finding["key"]["targets"]:
        lines.append(f"- `{target}`: failed in {finding['failures_by_target'].get(target, 0)}/{finding['window_runs']} "
                     f"daily runs" + ("; reproduced" if target in reproduced else ""))
    if run_url:
        lines += ["", f"Evidence artifacts (14-day retention): {run_url}"]
    return "\n".join(lines)
