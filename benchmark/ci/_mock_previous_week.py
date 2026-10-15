"""TEMPORARY: fill the previous 7-day window with mock scores so week-over-week changes can be previewed."""
import hashlib, json, sys
from ci.weekly_report import summarize_weekly
from ci.fetch_langfuse_history import parse_timestamp

path = sys.argv[1]
data = json.load(open(path))
end = parse_timestamp(data["generated_at"])
current = summarize_weekly(data["history"], end=end)
start, previous_start = parse_timestamp(current["start"]), parse_timestamp(current["previous_start"])
rates = {s["suite"]: s["pass_at_1"] for s in current["suites"]}
# Deterministic offsets per suite: a mix of gains, drops, a flat suite, and one without a previous week.
offsets = [0.12, -0.08, 0.0, 0.05, -0.15, 0.20, -0.03, None]
mocked = []
for record in data["history"]:
    if not previous_start <= parse_timestamp(record["timestamp"]) < start:
        mocked.append(record); continue
    rate = rates.get(record["suite"])
    offset = offsets[int(hashlib.sha256(record["suite"].encode()).hexdigest(), 16) % len(offsets)]
    if rate is None or offset is None:
        continue  # This suite gets no previous week.
    previous_rate = min(max(rate - offset, 0.0), 1.0)
    record = {**record, "missing_metrics": [], "tasks": 100, "eligible_tasks": 100, "passed": round(previous_rate * 100)}
    mocked.append(record)
data["history"] = mocked
data["weekly"] = summarize_weekly(mocked, end=end)
json.dump(data, open(path, "w"))
