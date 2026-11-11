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
    # Break @mentions and #/issue references to other repositories without changing meaning.
    value = re.sub(r"(?<![\w`])@(?=[A-Za-z0-9])", "@​", value)
    return value if len(value) <= limit else value[:limit] + " …"


def bullets(items: list, empty: str = "- None recorded") -> list[str]:
    return [f"- {clean(item, 600)}" for item in items] or [empty]


def reproduce_command(task: dict, key: str) -> str:
    """The local command that runs exactly the affected task from its suite."""
    task_id = key.split(":", 1)[1]
    return ("cd benchmark\n"
            f"uv run python -m runner run --suite {task['suite']} --task-id {task_id} \\\n"
            "  --agent-url http://127.0.0.1:8080")


def title(finding: dict) -> str:
    return clean(f"[auto-improve][{finding['key']['domain']}] {finding['issue']['title']}", 200).replace("\n", " ")


def labels(finding: dict) -> list[str]:
    return ["auto-improve", f"domain:{finding['key']['domain']}"]


def body(finding: dict, manifest: dict, run_url: str) -> str:
    issue, key = finding["issue"], finding["key"]
    meta = {**key, "signature": finding["signature"]}
    tasks = finding["tasks"]
    # The issue itself is this window's observation, so a retried publish does not also comment.
    lines = [f"<!-- auto-improve {json.dumps(meta, sort_keys=True)} -->",
             f"<!-- auto-improve-observation:{manifest['source']} -->", "", "## Summary", clean(issue["summary"]), ""]
    if finding.get("regression_of"):
        lines += ["Possibly a regression of " + ", ".join(f"#{n}" for n in finding["regression_of"]) + ".", ""]
    lines += ["## Impact", clean(issue["impact"]), "", "| Task | Environment | Failed daily runs | Reproduced on main |",
              "| --- | --- | --- | --- |"]
    for target in key["targets"]:
        reproduced = finding.get("reproduction", {}).get(target)
        state = ("yes" if reproduced["reproduced"] else f"no ({clean(reproduced['reason'], 120)})") if reproduced else "not run"
        lines.append(f"| `{target}` | {tasks.get(target, {}).get('environment', 'unknown')} | "
                     f"{finding['failures_by_target'].get(target, 0)}/{finding['window_runs']} | {state} |")
    lines += ["", "## Reproduce"]
    runnable = [t for t in key["targets"] if t in tasks and tasks[t].get("suite")]
    if runnable:
        lines += ["Start an agent daemon for the task's environment (see the benchmark quickstart), then run:", ""]
        for target in runnable:
            lines += ["```bash", reproduce_command(tasks[target], target), "```"]
    lines += ["", clean(issue["reproduction"]), "", "## Expected", clean(issue["expected"]), "",
              "## Actual", clean(issue["actual"]), "", "## Root cause", clean(issue["root_cause"]), "",
              "## Code locations", *[f"- `{loc}`" for loc in finding["code_locations"]], "",
              "## Evidence", *bullets(issue["evidence"]), "", "## Acceptance criteria", *bullets(issue["acceptance_criteria"])]
    if issue.get("fix_direction"):
        lines += ["", "## Suggested direction", "Non-binding; verify against the code before following it.", "",
                  clean(issue["fix_direction"])]
    lines += ["", "---", f"Filed by benchmark auto-improve from daily window `{manifest['source']}` on `main` "
              f"`{manifest['base_sha'][:12]}`. An independent read-only audit confirmed this finding against "
              "the fresh reproduction and retained traces.",
              f"Evidence artifacts (14-day retention): {run_url}" if run_url else "",
              "Close as not planned or label `auto-improve:false-positive` if the finding is wrong; "
              "the pipeline will not file these tasks again."]
    text = "\n".join(lines)
    return text if len(text) <= BODY_LIMIT else text[:BODY_LIMIT] + "\n\n(truncated)"


def observation(finding: dict, manifest: dict, run_url: str) -> str:
    """A follow-up on an existing open issue; the marker keeps it to one comment per window."""
    reproduced = [t for t, r in finding.get("reproduction", {}).items() if r["reproduced"]]
    lines = [f"<!-- auto-improve-observation:{manifest['source']} -->",
             f"Still observed in daily window `{manifest['source']}` on `main` `{manifest['base_sha'][:12]}`.", ""]
    for target in finding["key"]["targets"]:
        lines.append(f"- `{target}`: failed in {finding['failures_by_target'].get(target, 0)}/{finding['window_runs']} "
                     f"daily runs" + ("; reproduced on main" if target in reproduced else ""))
    if run_url:
        lines += ["", f"Evidence artifacts (14-day retention): {run_url}"]
    return "\n".join(lines)
