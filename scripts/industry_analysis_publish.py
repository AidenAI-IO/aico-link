"""Manage the industry opportunity report's OSS checkpoint and publication."""

from __future__ import annotations

import argparse
from datetime import UTC, datetime
import hashlib
from http.client import IncompleteRead
import json
import os
from pathlib import Path
import re
import sys
import time
from urllib.parse import urlsplit
from urllib.request import Request, urlopen

import oss2

from benchmark.ci.notify_lark import send_lark
from scripts.industry_analysis import LABELS, json_text, read_json, stamp, write_json


PUBLIC = "industry-analysis/weekly"
PRIVATE = "industry-analysis/private"
TYPES = {
    "manifest.json": "application/json",
    "opportunities.json": "application/json",
    "report.zh-CN.md": "text/markdown; charset=utf-8",
    "team-share.zip": "application/zip",
    "index.html": "text/html; charset=utf-8",
}


def oss_bucket():
    key, secret = os.getenv("OSS_ACCESS_KEY_ID"), os.getenv("OSS_ACCESS_KEY_SECRET")
    if not key or not secret:
        raise ValueError("Set the existing OSS_ACCESS_KEY_ID and OSS_ACCESS_KEY_SECRET")
    return oss2.Bucket(
        oss2.AuthV4(key, secret), "https://oss-cn-hongkong.aliyuncs.com", "firmware-agent",
        region="cn-hongkong", connect_timeout=20,
    )


def load(bucket, key, default=None):
    try:
        with bucket.get_object(key) as response:
            return json.load(response)
    except oss2.exceptions.NoSuchKey:
        return default


def store(bucket, key, data, content_type, *, private=False):
    # Keep the update and its ACL in the same PUT.
    headers = {
        "Content-Type": content_type,
        "Cache-Control": "no-store" if private else "no-cache, max-age=0, must-revalidate",
    }
    if private:
        headers["x-oss-object-acl"] = "private"
    if content_type.startswith("text/html"):
        headers["Content-Disposition"] = "inline"
    bucket.put_object(key, data, headers=headers)


def save_private(bucket, key, value):
    store(bucket, f"{PRIVATE}/{key}", json_text(value).encode(), "application/json", private=True)


def restore(bucket, folder, run):
    # The private checkpoint is the commit record. latest.json follows it and is
    # repaired from it, so an interrupted pointer update cannot skip feedback.
    checkpoint = load(bucket, f"{PRIVATE}/checkpoint.json", {})
    committed = checkpoint.get("published_run", 0)
    if committed > run:
        raise ValueError("This run predates the latest publication. Use a preview for a historic review.")
    if committed:
        repair_latest_pointer(bucket, committed)
    if committed == run:
        return True
    folder.mkdir(parents=True, exist_ok=True)
    base = load(bucket, f"{PRIVATE}/runs/{run}/base.json")
    if base is None or base["checkpoint"].get("published_run", 0) != committed:
        base = {"checkpoint": checkpoint, "cutoff": stamp(datetime.now(UTC))}
        save_private(bucket, f"runs/{run}/base.json", base)
    write_json(folder / "base.json", base)
    prepared_path = folder / "prepared.json"
    prepared_path.unlink(missing_ok=True)
    if prepared := load(bucket, f"{PRIVATE}/runs/{run}/prepared.json"):
        # A prepared checkpoint still records the run it was built from.
        if prepared["checkpoint"].get("published_run", 0) == committed:
            write_json(prepared_path, prepared)
    return False


def repair_latest_pointer(bucket, run):
    key = f"{PUBLIC}/latest.json"
    latest = {"run_number": run, "report": f"{run}/index.html"}
    if load(bucket, key) != latest:
        store(bucket, key, json_text(latest).encode(), "application/json")


def public_url(run):
    base = os.getenv("BENCHMARK_REPORT_BASE_URL", "").strip().rstrip("/")
    parsed = urlsplit(base)
    if parsed.scheme != "https" or not parsed.hostname or parsed.username or parsed.query or parsed.fragment:
        raise ValueError("Configure BENCHMARK_REPORT_BASE_URL as an HTTPS base URL")
    return f"{base}/{PUBLIC}/{run}"


def valid_webhook_url(url):
    if not all(32 < ord(char) < 127 for char in url):
        return False
    try:
        webhook = urlsplit(url)
        if webhook.scheme != "https" or webhook.fragment or webhook.port == 0:
            return False
    except ValueError:
        return False
    authority = re.fullmatch(r"(\[[0-9a-f:.]+\]|[a-z0-9.-]+)(?::[0-9]+)?", webhook.netloc, re.I)
    if not authority:
        return False
    host = authority[1]
    if host.startswith("["):
        return True  # urlsplit already validates bracketed IP addresses.
    host = host.removesuffix(".")
    label = r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?"
    return len(host) <= 253 and all(re.fullmatch(label, part, re.I) for part in host.split("."))


def verify(url, expected):
    for attempt in range(5):
        if attempt:
            time.sleep(5)
        try:
            request = Request(url, headers={
                "User-Agent": "Mozilla/5.0 (X11; Linux x86_64) Chrome/131.0.0.0 Safari/537.36",
                "Cache-Control": "no-cache",
            })
            with urlopen(request, timeout=20) as response:
                content = response.read(len(expected) + 10000)
                if url.endswith(".html"):
                    if (
                        response.headers.get_content_type() != "text/html"
                        or response.headers.get("Content-Disposition", "").lower().startswith("attachment")
                        or response.headers.get("x-oss-force-download", "").lower() == "true"
                    ):
                        continue
                    # Cloudflare Web Analytics inserts its beacon and a newline before </body>.
                    content = re.sub(rb'<script[^>]*static\.cloudflareinsights\.com[^>]*>\s*</script>\n?', b"", content)
                if hashlib.sha256(content).digest() == hashlib.sha256(expected).digest():
                    return
        except (OSError, IncompleteRead):
            continue
    raise ValueError(f"Published {url.rsplit('/', 1)[-1]} did not match the verified local file")


def publish(bucket, folder, run):
    url = public_url(run)
    committed = load(bucket, f"{PRIVATE}/checkpoint.json", {}).get("published_run", 0)
    if committed == run:
        repair_latest_pointer(bucket, run)
        summary(f"[Open the industry opportunity report]({url}/index.html)")
        return
    checkpoint = read_json(folder / "checkpoint.json")
    if committed > run or checkpoint.get("published_run", 0) != committed:
        raise ValueError("The collection checkpoint advanced. Restore the run before publishing.")
    report_dir = folder / "report"
    report = {name: (report_dir / name).read_bytes() for name in TYPES}
    manifest = json.loads(report["manifest.json"])
    if any(
        manifest.get(name) != hashlib.sha256(data).hexdigest()
        for name, data in report.items() if name != "manifest.json"
    ):
        raise ValueError("Report manifest does not match the generated files")
    save_private(bucket, f"runs/{run}/prepared.json", {
        "report": json.loads(report["opportunities.json"]), "checkpoint": checkpoint,
    })
    for name, data in report.items():
        store(bucket, f"{PUBLIC}/{run}/{name}", data, TYPES[name])
    for name in ("index.html", "team-share.zip"):
        verify(f"{url}/{name}", report[name])
    checkpoint["published_run"] = run
    save_private(bucket, "checkpoint.json", checkpoint)
    repair_latest_pointer(bucket, run)
    summary(f"[Open the industry opportunity report]({url}/index.html)")


def summary(message):
    print(message)
    if path := os.getenv("GITHUB_STEP_SUMMARY"):
        with open(path, "a", encoding="utf-8") as file:
            file.write(message + "\n")


def notify(bucket, run):
    if load(bucket, f"{PRIVATE}/checkpoint.json", {}).get("published_run") != run:
        raise ValueError("Only the latest committed publication can be notified.")
    report = load(bucket, f"{PRIVATE}/runs/{run}/prepared.json")["report"]
    if not report["opportunities"]:
        summary("No material opportunity changes to notify.")
        return
    url, secret = (
        os.getenv("INDUSTRY_ANALYSIS_LARK_WEBHOOK_URL"),
        os.getenv("INDUSTRY_ANALYSIS_LARK_WEBHOOK_SECRET"),
    )
    if not url or not secret:
        summary("Lark notification skipped. Configure both INDUSTRY_ANALYSIS_LARK_WEBHOOK Secrets.")
        return
    path = f"runs/{run}/lark.json"
    receipt = load(bucket, f"{PRIVATE}/{path}", {})
    if receipt.get("state") in {"sent", "sending"}:
        summary(f"Lark notification already recorded as {receipt['state']} for this report.")
        return
    if not valid_webhook_url(url):
        raise ValueError("Configure INDUSTRY_ANALYSIS_LARK_WEBHOOK_URL as a valid HTTPS webhook URL.")

    base = public_url(run)

    def text(value):
        return re.sub(r"([\\*_~`\[\]<>])", r"\\\1", str(value))

    names = "、".join(row["source"] for row in report["coverage"])
    elements = [{"tag": "markdown", "content": f"{report['cutoff'][:10]} · {text(names)}"}]
    if overview := report.get("overview"):
        elements.append({"tag": "markdown", "content": f"**{text(overview['finding'])}**"})
    elements.append({"tag": "hr"})
    elements += [
        {"tag": "markdown", "content": f"**{index}. {text(item['title'])}**\n{text(item['impact'])}"}
        for index, item in enumerate(report["opportunities"], 1)
    ]
    elements += [{"tag": "hr"}, {"tag": "action", "actions": [
        {"tag": "button", "text": {"tag": "plain_text", "content": label}, "type": button_type, "url": f"{base}/{name}"}
        for label, button_type, name in (
            (LABELS["open"], "primary", "index.html"), (LABELS["download"], "default", "team-share.zip"),
        )
    ]}]
    card = {
        "header": {"template": "blue", "title": {"tag": "plain_text", "content": LABELS["title"]}},
        "elements": elements,
    }

    # A timeout leaves the delivery unknown. A retry must not post a second card.
    save_private(bucket, path, {"state": "sending"})
    try:
        send_lark(card, webhook_url=url, webhook_secret=secret)
    except RuntimeError:
        save_private(bucket, path, {"state": "rejected"})
        raise ValueError("Lark rejected the notification") from None
    except (OSError, ValueError, IncompleteRead):
        raise ValueError(
            f"Lark delivery status is unknown. Clear {PRIVATE}/{path} only after confirming "
            "the card did not arrive and the report is still the latest publication."
        ) from None
    save_private(bucket, path, {"state": "sent"})
    summary("Lark notification sent.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("stage", choices=("restore", "publish", "notify"))
    parser.add_argument("--workdir", type=Path, required=True)
    parser.add_argument("--run", type=int, required=True)
    args = parser.parse_args()
    if args.run < 1:
        raise ValueError("The Actions run number must be positive")
    bucket = oss_bucket()
    if args.stage == "restore":
        committed = restore(bucket, args.workdir, args.run)
        if path := os.getenv("GITHUB_OUTPUT"):
            with open(path, "a", encoding="utf-8") as file:
                file.write(f"committed={str(committed).lower()}\n")
    elif args.stage == "publish":
        publish(bucket, args.workdir, args.run)
    else:
        notify(bucket, args.run)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, oss2.exceptions.OssError) as error:
        detail = str(error) if type(error) is ValueError else type(error).__name__
        if isinstance(error, oss2.exceptions.OssError):
            # OSS error codes identify permission or signing problems without exposing credentials.
            detail += f" {error.status} {error.code}"
        print(f"Industry report {sys.argv[1] if len(sys.argv) > 1 else 'publication'} failed: {detail}", file=sys.stderr)
        sys.exit(1)
