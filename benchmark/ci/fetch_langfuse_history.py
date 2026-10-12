"""Fetch benchmark history from Langfuse for dashboard and report generation."""

from __future__ import annotations

import os
from datetime import datetime, timedelta, timezone
from typing import Any


def fetch_benchmark_history(
    *,
    lookback_days: int = 90,
    branch_filter: str | None = None,
    dataset_prefix: str = "aiden-benchmark",
) -> list[dict[str, Any]]:
    """
    Fetch benchmark run history from Langfuse using the Python SDK.

    Args:
        lookback_days: How many days of history to fetch
        branch_filter: Only include runs from this branch (e.g. "main"), or None for all
        dataset_prefix: Prefix for benchmark dataset names

    Returns:
        List of benchmark run records, sorted by timestamp (oldest first).
        Each record contains run metadata and metrics.
    """
    try:
        from langfuse import Langfuse
    except ImportError as exc:
        raise RuntimeError(
            "Langfuse SDK is not installed; run `uv sync` in benchmark/"
        ) from exc

    public_key, secret_key, base_url = _langfuse_connection()
    client = Langfuse(
        public_key=public_key,
        secret_key=secret_key,
        base_url=base_url,
    )

    cutoff = datetime.now(timezone.utc) - timedelta(days=lookback_days)

    # Get all datasets to find benchmark datasets
    # SDK doesn't have a list_datasets method, so we'll iterate through known dataset names
    # or fetch them via the API property
    all_datasets = client.api.datasets.get_many().data

    benchmark_datasets = [
        d for d in all_datasets
        if d.name and d.name.startswith(dataset_prefix)
    ]

    if not benchmark_datasets:
        return []

    history: list[dict[str, Any]] = []

    for dataset in benchmark_datasets:
        dataset_name = dataset.name

        # Use SDK's get_dataset_runs method
        try:
            runs = client.get_dataset_runs(dataset_name=dataset_name)
        except Exception as e:
            # Dataset might not have any runs, skip it
            print(f"Warning: Could not fetch runs for {dataset_name}: {e}")
            continue

        for run in runs.data if hasattr(runs, 'data') else runs:
            # Parse timestamp
            created_at = _get_timestamp(run)
            if not created_at or created_at < cutoff:
                continue

            # Extract metadata
            metadata = run.metadata if hasattr(run, 'metadata') else {}

            # Skip if not a benchmark run
            if metadata.get("aiden_benchmark") != "true":
                continue

            # Apply branch filter if specified
            git_branch = metadata.get("git_branch")
            if branch_filter and git_branch and git_branch != branch_filter:
                continue

            # Extract run info
            run_name = run.name if hasattr(run, 'name') else str(run.get("name", ""))
            run_id = run.id if hasattr(run, 'id') else str(run.get("id", ""))

            run_record = {
                "run_id": run_name,
                "dataset_run_id": run_id,
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
    """
    Get Langfuse connection credentials from environment.

    Returns:
        Tuple of (public_key, secret_key, base_url)

    Raises:
        ValueError: If required credentials are not set
    """
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


def _get_timestamp(run: Any) -> datetime | None:
    """
    Extract timestamp from a dataset run object.

    Args:
        run: Dataset run object from Langfuse SDK

    Returns:
        Timezone-aware datetime or None if not found
    """
    # Try different attribute names
    created_at = None
    if hasattr(run, 'created_at'):
        created_at = run.created_at
    elif hasattr(run, 'createdAt'):
        created_at = run.createdAt
    elif isinstance(run, dict):
        created_at = run.get('created_at') or run.get('createdAt')

    if not created_at:
        return None

    # If it's already a datetime, return it
    if isinstance(created_at, datetime):
        return created_at if created_at.tzinfo else created_at.replace(tzinfo=timezone.utc)

    # Parse string timestamp
    if isinstance(created_at, str):
        try:
            dt = datetime.fromisoformat(created_at.replace('Z', '+00:00'))
            return dt if dt.tzinfo else dt.replace(tzinfo=timezone.utc)
        except ValueError:
            return None

    return None


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
            print(f"  Branch: {latest['git_branch']}")
    except Exception as e:
        print(f"Error: {e}", file=sys.stderr)
        sys.exit(1)
