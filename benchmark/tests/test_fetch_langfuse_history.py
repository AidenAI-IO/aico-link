"""Tests for fetch_langfuse_history module."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone
from unittest.mock import patch
from urllib.parse import quote

from ci.fetch_langfuse_history import (
    fetch_benchmark_history,
    _parse_timestamp,
)


def test_parse_timestamp_with_z_suffix():
    """Test parsing ISO 8601 timestamp with Z suffix."""
    result = _parse_timestamp("2024-01-15T18:17:00Z")
    assert result == datetime(2024, 1, 15, 18, 17, 0, tzinfo=timezone.utc)


def test_parse_timestamp_with_timezone():
    """Test parsing ISO 8601 timestamp with explicit timezone."""
    result = _parse_timestamp("2024-01-15T18:17:00+00:00")
    assert result == datetime(2024, 1, 15, 18, 17, 0, tzinfo=timezone.utc)


def test_fetch_benchmark_history_filters_by_branch():
    """Test that branch filter correctly excludes non-matching runs."""
    # Use recent dates to pass lookback filter
    recent = datetime.now(timezone.utc) - timedelta(days=1)
    recent_str = recent.isoformat()

    mock_response = {
        "data": [
            {
                "name": "aiden-benchmark-test",
                "id": "dataset-1",
            }
        ]
    }

    mock_runs_response = {
        "data": [
            {
                "id": "run-1",
                "name": "ci-12345-1",
                "createdAt": recent_str,
                "metadata": {
                    "aiden_benchmark": "true",
                    "git_branch": "main",
                    "git_sha": "abc123",
                    "benchmark_suite": "test",
                },
            },
            {
                "id": "run-2",
                "name": "ci-12346-1",
                "createdAt": recent_str,
                "metadata": {
                    "aiden_benchmark": "true",
                    "git_branch": "feature",
                    "git_sha": "def456",
                    "benchmark_suite": "test",
                },
            },
        ]
    }

    with patch("ci.fetch_langfuse_history._api_get") as mock_api_get:
        mock_api_get.side_effect = [mock_response, mock_runs_response]

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
    recent_str = recent.isoformat()

    mock_response = {
        "data": [{"name": "aiden-benchmark-test", "id": "dataset-1"}]
    }

    mock_runs_response = {
        "data": [
            {
                "id": "run-1",
                "name": "ci-12345-1",
                "createdAt": recent_str,
                "metadata": {
                    "aiden_benchmark": "true",
                    "git_sha": "abc123",
                },
            },
            {
                "id": "run-2",
                "name": "other-run",
                "createdAt": recent_str,
                "metadata": {
                    # Missing aiden_benchmark marker
                    "git_sha": "def456",
                },
            },
        ]
    }

    with patch("ci.fetch_langfuse_history._api_get") as mock_api_get:
        mock_api_get.side_effect = [mock_response, mock_runs_response]

        with patch("ci.fetch_langfuse_history._langfuse_connection") as mock_conn:
            mock_conn.return_value = ("pk", "sk", "http://test")

            result = fetch_benchmark_history(lookback_days=30)

    assert len(result) == 1
    assert result[0]["run_id"] == "ci-12345-1"


def test_fetch_benchmark_history_url_encodes_dataset_names():
    """Test that dataset names with special characters are URL-encoded."""
    recent = datetime.now(timezone.utc) - timedelta(days=1)
    recent_str = recent.isoformat()

    mock_response = {
        "data": [
            {
                "name": "aiden-benchmark:memory_v1",  # Contains colon
                "id": "dataset-1",
            }
        ]
    }

    mock_runs_response = {
        "data": [
            {
                "id": "run-1",
                "name": "ci-12345-1",
                "createdAt": recent_str,
                "metadata": {
                    "aiden_benchmark": "true",
                    "git_sha": "abc123",
                },
            },
        ]
    }

    with patch("ci.fetch_langfuse_history._api_get") as mock_api_get:
        mock_api_get.side_effect = [mock_response, mock_runs_response]

        with patch("ci.fetch_langfuse_history._langfuse_connection") as mock_conn:
            mock_conn.return_value = ("pk", "sk", "http://test")

            result = fetch_benchmark_history(lookback_days=30)

    # Verify the API was called with URL-encoded dataset name
    calls = mock_api_get.call_args_list
    assert len(calls) == 2

    # Second call should be for dataset-runs with encoded name
    dataset_runs_call = calls[1]
    url_path = dataset_runs_call[0][1]  # Second positional arg is the path

    # Verify the dataset name is URL-encoded (colon becomes %3A)
    expected_encoded = quote("aiden-benchmark:memory_v1", safe='')
    assert expected_encoded in url_path
    assert f"datasetName={expected_encoded}" in url_path

    assert len(result) == 1

