"""Dashboard measurement definitions, shared by score retrieval and charts.

Units and populations follow docs/09-benchmark/metrics-reference.md. These
are run distributions over measured eligible attempts, not task-level traces.
"""


def _series(metric: str, statistic: str, label: str) -> dict[str, str]:
    return {"metric": metric, "statistic": statistic, "label": label}


METRIC_CHARTS = [
    {
        "id": "latency", "title": "Task execution latency", "unit": "s", "scale": 0.001,
        "description": "p50 and p90 across measured eligible attempts, including failures. Excludes setup, judging and timeout recovery.",
        "series": [_series("task_wall_ms", "p50", "p50"), _series("task_wall_ms", "p90", "p90")],
    },
    {
        "id": "calls", "title": "Agent work per attempt", "unit": "per attempt", "scale": 1,
        "description": "Mean counts per measured eligible attempt. Device actions are recognized device-changing tools; LLM calls require usage evidence. Vision calls are inferred from image observations.",
        "series": [_series("tool_calls", "mean", "Tool calls"), _series("device_actions", "mean", "Device actions"),
                   _series("llm_calls", "mean", "LLM calls"), _series("vision_llm_calls", "mean", "Vision LLM calls")],
    },
    {
        "id": "timings", "title": "Recorded timings · p50", "unit": "s", "scale": 0.001,
        "description": "These timings overlap and must not be added together. Vision LLM time is inferred, not isolated vision processing; screenshots measure runner captures only. Missing instrumentation leaves gaps.",
        "series": [_series("llm_time_ms", "p50", "LLM"), _series("vision_llm_time_ms", "p50", "Vision LLM"),
                   _series("device_execution_ms", "p50", "Device execution"),
                   _series("screenshot_capture_ms", "p50", "Screenshot capture"),
                   _series("time_to_first_token_ms", "p50", "First token")],
    },
    {
        "id": "tokens", "title": "Tokens per attempt", "unit": "tokens", "scale": 1,
        "description": "Reported Agent usage per measured eligible attempt. Excludes judge and setup usage. Inspect sample counts before comparing runs.",
        "series": [_series("total_tokens", "p50", "p50"), _series("total_tokens", "p90", "p90")],
    },
    {
        "id": "cost", "title": "Cost per attempt", "unit": "USD", "scale": 1,
        "description": "Only explicitly reported monetary cost is shown. The current runner does not calculate cost; unavailable measurements are not zero or an estimate from token counts.",
        "series": [_series("cost_usd", "mean", "Mean"), _series("cost_usd", "p90", "p90")],
    },
    {
        "id": "reliability", "title": "Tool errors, replans and retries", "unit": "per attempt", "scale": 1,
        "description": "Mean counts per measured eligible attempt. Tool errors can be recovered; replans require explicit evidence. Retry counts are currently unpopulated. Benchmark repeats (k) are not Agent retries.",
        "series": [_series("tool_errors", "mean", "Tool errors"), _series("replan_count", "mean", "Replans"),
                   _series("retry_count", "mean", "Retries")],
    },
]

RELIABILITY_METRICS = {"tool_errors", "replan_count", "retry_count"}
DISTRIBUTION_FIELDS: dict[str, set[str]] = {}
for chart in METRIC_CHARTS:
    for series in chart["series"]:
        DISTRIBUTION_FIELDS.setdefault(series["metric"], {"count"}).add(series["statistic"])


def distribution_score_name(metric: str, statistic: str) -> str:
    prefix = "reliability" if metric in RELIABILITY_METRICS else "efficiency"
    return f"{prefix}.{metric}.{statistic}"


DISTRIBUTION_SCORE_NAMES = tuple(
    distribution_score_name(metric, statistic)
    for metric, fields in DISTRIBUTION_FIELDS.items() for statistic in sorted(fields)
)
