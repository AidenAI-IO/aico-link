"""Human-readable event timeline and score dimensions for collaboration runs."""
from __future__ import annotations

import json
from pathlib import Path
from typing import Any


def write_collaboration_report(evidence: dict[str, Any], evaluation: dict[str, Any], artifact_dir: Path) -> dict[str, Any]:
    events = evidence.get("events", [])
    rows = []
    for event in events:
        meta = event.get("collaboration") or {}
        payload = event.get("payload") or {}
        rows.append({
            "sequence": event.get("sequence"),
            "elapsed_ns": meta.get("observed_elapsed_ns"),
            "actor": meta.get("actor"),
            "kind": event.get("kind"),
            "turn_id": event.get("turn_id"),
            "task_id": event.get("task_id"),
            "goal_revision": meta.get("goal_revision"),
            "applied_revision": meta.get("applied_revision"),
            "call_id": event.get("call_id"),
            "response_id": event.get("response_id"),
            "update_ids": meta.get("update_ids", []),
            "text": payload.get("text") or payload.get("Text") or payload.get("transcript"),
            "status": payload.get("status"),
            "event_id": event.get("event_id"),
        })
    (artifact_dir / "collaboration-timeline.json").write_text(
        json.dumps(rows, ensure_ascii=False, indent=2), encoding="utf-8")
    claims = evaluation.get("claims", [])
    checks = evaluation.get("checks", [])
    summary = {
        "outcome": evaluation.get("outcome"),
        "claim_count": len(claims),
        "unsupported_claims": sum(c.get("verdict") == "unsupported" for c in claims),
        "indeterminate_claims": sum(c.get("verdict") == "indeterminate" for c in claims),
        "delivery_passed": sum(c.get("id", "").startswith("delivery.") and c.get("verdict") == "pass" for c in checks),
        "delivery_required": sum(c.get("id", "").startswith("delivery.") for c in checks),
        "event_count": len(events),
        "evidence_complete": evidence.get("complete", False),
        "timeline_artifact": "collaboration-timeline.json",
        "evidence_artifact": "collaboration.json",
        "evaluation_artifact": "collaboration-evaluation.json",
    }
    (artifact_dir / "collaboration-summary.json").write_text(
        json.dumps(summary, ensure_ascii=False, indent=2), encoding="utf-8")
    return summary
