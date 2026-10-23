#!/usr/bin/env python3
"""Legacy entry point. Forwards to runner CLI inside benchmark/."""
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "benchmark"))

from runner.main import cli

if __name__ == "__main__":
    if len(sys.argv) > 1 and sys.argv[1] in {"run", "unit", "rejudge", "compare", "webui"}:
        sys.exit(cli(sys.argv[1:]))
    sys.exit(cli(["run", *sys.argv[1:]]))
