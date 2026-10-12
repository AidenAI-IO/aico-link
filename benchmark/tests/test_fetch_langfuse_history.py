"""Tests for fetch_langfuse_history module."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone
from unittest.mock import MagicMock, patch

from ci.fetch_langfuse_history import (
    fetch_benchmark_history,
    _get_timestamp,
)


def test_get_timestamp_from_datetime_object():
    """Test extracting timestamp from datetime attribute."""
    recent = datetime.now(timezone.utc) - timedelta(days=1)

    run = MagicMock()
    run.created_at = recent

    result = _get_timestamp(run)
    assert result == recent


def test_get_timestamp_from_string():
    """Test parsing ISO 8601 timestamp string."""
    run = MagicMock()
    run.created_at = "2024-01-15T18:17:00Z"

    result = _get_timestamp(run)
    assert result == datetime(2024, 1, 15, 18, 17, 0, tzinfo=timezone.utc)


def test_fetch_benchmark_history_filters_by_branch():
    """Test that branch filter correctly excludes non-matching runs."""
    recent = datetime.now(timezone.utc) - timedelta(days=1)

    # Mock dataset
    mock_dataset = MagicMock()
    mock_dataset.name = "aiden-benchmark:test"

    # Mock runs
    run1 = MagicMock()
    run1.name = "ci-12345-1"
    run1.id = "run-abc123"
    run1.created_at = recent
    run1.metadata = {
        "aiden_benchmark": "true",
        "git_branch": "main",
        "git_sha": "abc123",
        "benchmark_suite": "test",
    }

    run2 = MagicMock()
    run2.name = "ci-12346-1"
    run2.id = "run-def456"
    run2.created_at = recent
    run2.metadata = {
        "aiden_benchmark": "true",
        "git_branch": "feature",
        "git_sha": "def456",
        "benchmark_suite": "test",
    }

    mock_runs = MagicMock()
    mock_runs.data = [run1, run2]

    with patch("langfuse.Langfuse") as MockLangfuse:
        mock_client = MockLangfuse.return_value
        mock_client.api.datasets.get_many.return_value.data = [mock_dataset]
        mock_client.get_dataset_runs.return_value = mock_runs

        with patch("ci.fetch_langfuse_history._langfuse_connection") as mock_conn:
            mock_conn.return_value = ("pk", "sk", "http://test")

            result = fetch_benchmark_history(
                lookback_days=30,
                branch_filter="main",
            )

    assert len(result) == 1
    assert result[0]["git_branch"] == "main"
    assert result[0]["run_id"] == "ci-12345-1"


def test_fetch_benchmark_history_skips_non_benchmark_runs():
    """Test that runs without aiden_benchmark marker are skipped."""
    recent = datetime.now(timezone.utc) - timedelta(days=1)

    mock_dataset = MagicMock()
    mock_dataset.name = "aiden-benchmark:test"

    run1 = MagicMock()
    run1.name = "ci-12345-1"
    run1.id = "run-abc123"
    run1.created_at = recent
    run1.metadata = {
        "aiden_benchmark": "true",
        "git_sha": "abc123",
    }

    run2 = MagicMock()
    run2.name = "other-run"
    run2.id = "run-def456"
    run2.created_at = recent
    run2.metadata = {
        # Missing aiden_benchmark marker
        "git_sha": "def456",
    }

    mock_runs = MagicMock()
    mock_runs.data = [run1, run2]

    with patch("langfuse.Langfuse") as MockLangfuse:
        mock_client = MockLangfuse.return_value
        mock_client.api.datasets.get_many.return_value.data = [mock_dataset]
        mock_client.get_dataset_runs.return_value = mock_runs

        with patch("ci.fetch_langfuse_history._langfuse_connection") as mock_conn:
            mock_conn.return_value = ("pk", "sk", "http://test")

            result = fetch_benchmark_history(lookback_days=30)

    assert len(result) == 1
    assert result[0]["run_id"] == "ci-12345-1"


def test_fetch_benchmark_history_handles_missing_datasets():
    """Test graceful handling when dataset has no runs."""
    mock_dataset = MagicMock()
    mock_dataset.name = "aiden-benchmark:test"

    with patch("langfuse.Langfuse") as MockLangfuse:
        mock_client = MockLangfuse.return_value
        mock_client.api.datasets.get_many.return_value.data = [mock_dataset]
        # Simulate error when getting runs
        mock_client.get_dataset_runs.side_effect = Exception("No runs found")

        with patch("ci.fetch_langfuse_history._langfuse_connection") as mock_conn:
            mock_conn.return_value = ("pk", "sk", "http://test")

            # Should not crash, just return empty list
            result = fetch_benchmark_history(lookback_days=30)

    assert len(result) == 0
