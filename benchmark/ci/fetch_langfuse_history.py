"""Fetch benchmark history from Langfuse for dashboard and report generation."""

from __future__ import annotations

import os
from datetime import datetime, timedelta, timezone
from typing import Any

import requests


def fetch_benchmark_history(
    *,
    lookback_days: int = 90,
    branch_filter: str | None = None,
    dataset_prefix: str = "aiden-benchmark",
    limit: int = 500,
) -> list[dict[str, Any]]:
    """
    Fetch benchmark run history from Langfuse.

    Args:
        lookback_days: How many days of history to fetch
        branch_filter: Only include runs from this branch (e.g. "main"), or None for all
        dataset_prefix: Prefix for benchmark dataset names
        limit: Max number of runs to fetch per dataset

    Returns:
        List of benchmark run records, sorted by timestamp (oldest first).
        Each record contains run metadata and metrics.
    """
    public_key, secret_key, base_url = _langfuse_connection()
    cutoff = datetime.now(timezone.utc) - timedelta(days=lookback_days)

    # Get all datasets
    datasets_response = _api_get(base_url, "/api/public/datasets", public_key, secret_key)
    all_datasets = datasets_response.get("data", [])

    benchmark_datasets = [
        d for d in all_datasets
        if d.get("name", "").startswith(dataset_prefix)
    ]

    if not benchmark_datasets:
        return []

    history: list[dict[str, Any]] = []

    for dataset in benchmark_datasets:
        dataset_name = dataset["name"]

        # Get runs for this dataset
        runs_response = _api_get(
            base_url,
            f"/api/public/dataset-runs?datasetName={dataset_name}&limit={limit}",
            public_key,
            secret_key,
        )

        runs = runs_response.get("data", [])

        for run in runs:
            # Parse timestamp
            created_at_str = run.get("createdAt") or run.get("created_at")
            if not created_at_str:
                continue

            try:
                created_at = _parse_timestamp(created_at_str)
            except ValueError:
                continue

            if created_at < cutoff:
                continue

            # Extract metadata
            metadata = run.get("metadata", {})

            # Skip if not a benchmark run
            if metadata.get("aiden_benchmark") != "true":
                continue

            # Apply branch filter if specified
            git_branch = metadata.get("git_branch")
            if branch_filter and git_branch and git_branch != branch_filter:
                continue

            # Extract run info
            run_record = {
                "run_id": run.get("name"),
                "dataset_run_id": run.get("id"),
                "dataset_name": dataset_name,
                "timestamp": created_at.isoformat(),
                "commit_sha": metadata.get("git_sha", "unknown"),
                "git_dirty": metadata.get("git_dirty") == "true",
                "git_branch": git_branch,
                "suite": metadata.get("benchmark_suite"),
                "agent_model": metadata.get("agent_model"),
                "judge_model": metadata.get("judge_model"),
                "target_platform": metadata.get("target_platform"),
                "metrics_schema_version": metadata.get("metrics_schema_version"),
                # TODO: Extract aggregated scores from run if available
                # For now, we'll need to fetch metrics from local artifacts or compute them
            }

            history.append(run_record)

    return sorted(history, key=lambda x: x["timestamp"])


def _langfuse_connection() -> tuple[str, str, str]:
    """Get Langfuse connection credentials from environment."""
    public_key = os.environ.get("LANGFUSE_PUBLIC_KEY", "").strip()
    secret_key = os.environ.get("LANGFUSE_SECRET_KEY", "").strip()
    base_url = (
        os.environ.get("LANGFUSE_BASE_URL")
        or os.environ.get("LANGFUSE_HOST")
        or "https://cloud.langfuse.com"
    ).rstrip("/")

    if not public_key or not secret_key:
        raise ValueError(
            "LANGFUSE_PUBLIC_KEY and LANGFUSE_SECRET_KEY are required"
        )

    return public_key, secret_key, base_url


def _api_get(base_url: str, path: str, public_key: str, secret_key: str) -> dict[str, Any]:
    """Make GET request to Langfuse API."""
    response = requests.get(
        f"{base_url}{path}",
        auth=(public_key, secret_key),
        timeout=30,
    )
    response.raise_for_status()
    payload = response.json()

    if not isinstance(payload, dict):
        raise ValueError(f"Unexpected Langfuse response from {path}")

    return payload


def _parse_timestamp(ts: str) -> datetime:
    """Parse ISO timestamp from Langfuse."""
    # Langfuse typically returns ISO 8601 format
    dt = datetime.fromisoformat(ts.replace('Z', '+00:00'))
    return dt if dt.tzinfo else dt.replace(tzinfo=timezone.utc)


if __name__ == "__main__":
    # Quick test
    import sys
    try:
        history = fetch_benchmark_history(lookback_days=30)
        print(f"Found {len(history)} benchmark runs in last 30 days")
        if history:
            latest = history[-1]
            print(f"Latest: {latest['run_id']} at {latest['timestamp']}")
            print(f"  Commit: {latest['commit_sha']}")
            print(f"  Suite: {latest['suite']}")
            print(f"  Branch: {latest.get('git_branch', 'unknown')}")
    except Exception as e:
        print(f"Error: {e}", file=sys.stderr)
        import traceback
        traceback.print_exc()
        sys.exit(1)
