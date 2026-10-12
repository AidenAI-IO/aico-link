"""Generate benchmark dashboard HTML from history data."""

from __future__ import annotations

import argparse
import json
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


def generate_dashboard(
    *,
    history: list[dict[str, Any]],
    output_file: Path,
    data_output: Path | None = None,
) -> None:
    """
    Generate interactive dashboard HTML from benchmark history.

    Args:
        history: List of benchmark run records from fetch_langfuse_history
        output_file: Path to write the HTML dashboard
        data_output: Optional path to write the JSON data file
    """
    # Analyze history
    stats = _compute_stats(history)

    # Save data if requested
    if data_output:
        data_output.parent.mkdir(parents=True, exist_ok=True)
        with data_output.open("w", encoding="utf-8") as f:
            json.dump({
                "generated_at": datetime.now(timezone.utc).isoformat(),
                "history": history,
                "stats": stats,
            }, f, indent=2)

    # Generate HTML
    html = _render_dashboard_html(history, stats)

    output_file.parent.mkdir(parents=True, exist_ok=True)
    output_file.write_text(html, encoding="utf-8")

    print(f"Dashboard generated: {output_file}")
    print(f"  Total runs: {len(history)}")
    if stats.get("latest"):
        latest = stats["latest"]
        print(f"  Latest run: {latest['run_id']} at {latest['timestamp']}")


def _compute_stats(history: list[dict[str, Any]]) -> dict[str, Any]:
    """
    Compute statistics from history data.

    Args:
        history: List of benchmark run records

    Returns:
        Dictionary containing:
        - total_runs: Total number of runs
        - latest: Most recent run record or None
        - by_suite: Count of runs per suite
        - by_branch: Count of runs per branch
        - date_range: Start and end timestamps
    """
    if not history:
        return {
            "total_runs": 0,
            "latest": None,
            "by_suite": {},
            "by_branch": {},
        }

    latest = history[-1] if history else None

    # Group by suite
    by_suite: dict[str, list[dict[str, Any]]] = {}
    for run in history:
        suite = run.get("suite", "unknown")
        if suite not in by_suite:
            by_suite[suite] = []
        by_suite[suite].append(run)

    # Group by branch
    by_branch: dict[str, list[dict[str, Any]]] = {}
    for run in history:
        branch = run.get("git_branch") or "unknown"
        if branch not in by_branch:
            by_branch[branch] = []
        by_branch[branch].append(run)

    return {
        "total_runs": len(history),
        "latest": latest,
        "by_suite": {k: len(v) for k, v in by_suite.items()},
        "by_branch": {k: len(v) for k, v in by_branch.items()},
        "date_range": {
            "start": history[0]["timestamp"] if history else None,
            "end": history[-1]["timestamp"] if history else None,
        }
    }


def _render_dashboard_html(history: list[dict[str, Any]], stats: dict[str, Any]) -> str:
    """
    Render dashboard HTML with embedded data.

    Data is safely embedded as JSON with ensure_ascii=True to prevent XSS attacks
    from malicious branch names or other user-controlled fields. The JSON is placed
    in a script tag which treats content as data, not executable code.
    """
    # Use ensure_ascii=True to escape all non-ASCII characters and special chars
    # This prevents XSS even if user-controlled data (like branch names) contains HTML/JS
    history_json = json.dumps(history, indent=2, ensure_ascii=True)
    stats_json = json.dumps(stats, indent=2, ensure_ascii=True)

    return f"""<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Aiden Benchmark Dashboard</title>
    <script src="https://cdn.jsdelivr.net/npm/chart.js@4.4.0/dist/chart.umd.min.js"></script>
    <style>
        * {{ margin: 0; padding: 0; box-sizing: border-box; }}
        body {{
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
            background: #f5f5f7;
            padding: 20px;
            color: #1d1d1f;
        }}
        .container {{
            max-width: 1400px;
            margin: 0 auto;
        }}
        header {{
            background: white;
            padding: 30px;
            border-radius: 12px;
            margin-bottom: 20px;
            box-shadow: 0 2px 8px rgba(0,0,0,0.1);
        }}
        h1 {{
            font-size: 36px;
            margin-bottom: 10px;
        }}
        .subtitle {{
            color: #6e6e73;
            font-size: 18px;
        }}
        .stats-grid {{
            display: grid;
            grid-template-columns: repeat(auto-fit, minmax(250px, 1fr));
            gap: 20px;
            margin-bottom: 20px;
        }}
        .stat-card {{
            background: white;
            padding: 24px;
            border-radius: 12px;
            box-shadow: 0 2px 8px rgba(0,0,0,0.1);
        }}
        .stat-value {{
            font-size: 42px;
            font-weight: 600;
            margin-bottom: 8px;
        }}
        .stat-label {{
            color: #6e6e73;
            font-size: 14px;
            text-transform: uppercase;
            letter-spacing: 0.5px;
        }}
        .chart-container {{
            background: white;
            padding: 30px;
            border-radius: 12px;
            box-shadow: 0 2px 8px rgba(0,0,0,0.1);
            margin-bottom: 20px;
        }}
        .chart-title {{
            font-size: 24px;
            margin-bottom: 20px;
        }}
        .runs-table {{
            background: white;
            padding: 30px;
            border-radius: 12px;
            box-shadow: 0 2px 8px rgba(0,0,0,0.1);
            overflow-x: auto;
        }}
        table {{
            width: 100%;
            border-collapse: collapse;
        }}
        th, td {{
            padding: 12px;
            text-align: left;
            border-bottom: 1px solid #e5e5e7;
        }}
        th {{
            font-weight: 600;
            color: #6e6e73;
            font-size: 12px;
            text-transform: uppercase;
        }}
        td {{
            font-size: 14px;
        }}
        .commit {{
            font-family: monospace;
            background: #f5f5f7;
            padding: 4px 8px;
            border-radius: 4px;
            font-size: 12px;
        }}
        a {{
            color: #0071e3;
            text-decoration: none;
        }}
        a:hover {{
            text-decoration: underline;
        }}
    </style>
</head>
<body>
    <div class="container">
        <header>
            <h1>🚀 Aiden Benchmark Dashboard</h1>
            <p class="subtitle">Real-time monitoring of benchmark performance</p>
        </header>

        <div class="stats-grid">
            <div class="stat-card">
                <div class="stat-value">{stats.get("total_runs", 0)}</div>
                <div class="stat-label">Total Runs</div>
            </div>
            <div class="stat-card">
                <div class="stat-value">{len(stats.get("by_suite", {}))}</div>
                <div class="stat-label">Suites</div>
            </div>
            <div class="stat-card">
                <div class="stat-value">{len(stats.get("by_branch", {}))}</div>
                <div class="stat-label">Branches</div>
            </div>
        </div>

        <div class="chart-container">
            <h2 class="chart-title">Benchmark Runs Over Time</h2>
            <canvas id="timelineChart"></canvas>
        </div>

        <div class="runs-table">
            <h2 class="chart-title">Recent Runs</h2>
            <table id="runsTable">
                <thead>
                    <tr>
                        <th>Time</th>
                        <th>Run ID</th>
                        <th>Suite</th>
                        <th>Commit</th>
                        <th>Branch</th>
                        <th>Model</th>
                        <th>Platform</th>
                    </tr>
                </thead>
                <tbody>
                </tbody>
            </table>
        </div>
    </div>

    <script>
        const history = {history_json};
        const stats = {stats_json};

        // Populate runs table
        const tbody = document.querySelector('#runsTable tbody');
        history.slice(-20).reverse().forEach(run => {{
            const row = tbody.insertRow();
            const timestamp = new Date(run.timestamp).toLocaleString();
            const commitShort = (run.commit_sha || 'unknown').substring(0, 8);

            row.innerHTML = `
                <td>${{timestamp}}</td>
                <td><code>${{run.run_id}}</code></td>
                <td>${{run.suite || 'unknown'}}</td>
                <td><span class="commit">${{commitShort}}</span></td>
                <td>${{run.git_branch || 'unknown'}}</td>
                <td>${{run.agent_model || 'unknown'}}</td>
                <td>${{run.target_platform || 'unknown'}}</td>
            `;
        }});

        // Timeline chart
        const ctx = document.getElementById('timelineChart');
        new Chart(ctx, {{
            type: 'line',
            data: {{
                labels: history.map(r => new Date(r.timestamp).toLocaleDateString()),
                datasets: [{{
                    label: 'Benchmark Runs',
                    data: history.map((_, i) => i + 1),
                    borderColor: '#0071e3',
                    backgroundColor: 'rgba(0, 113, 227, 0.1)',
                    tension: 0.4,
                    fill: true,
                }}]
            }},
            options: {{
                responsive: true,
                maintainAspectRatio: true,
                plugins: {{
                    legend: {{
                        display: false
                    }}
                }},
                scales: {{
                    y: {{
                        beginAtZero: true,
                        title: {{
                            display: true,
                            text: 'Cumulative Runs'
                        }}
                    }}
                }}
            }}
        }});
    </script>
</body>
</html>
"""


def cli(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--history-json", type=Path, help="JSON file with history data")
    parser.add_argument("--output", type=Path, required=True, help="Output HTML file")
    parser.add_argument("--data-output", type=Path, help="Optional JSON data output")
    parser.add_argument("--lookback-days", type=int, default=30, help="Days of history")
    parser.add_argument("--branch-filter", help="Filter to specific branch (e.g. main)")

    args = parser.parse_args(argv)

    # Load or fetch history
    if args.history_json and args.history_json.exists():
        with args.history_json.open() as f:
            data = json.load(f)
            history = data.get("history", [])
    else:
        # Fetch from Langfuse
        from ci.fetch_langfuse_history import fetch_benchmark_history
        history = fetch_benchmark_history(
            lookback_days=args.lookback_days,
            branch_filter=args.branch_filter,
        )

    generate_dashboard(
        history=history,
        output_file=args.output,
        data_output=args.data_output,
    )

    return 0


if __name__ == "__main__":
    raise SystemExit(cli())
