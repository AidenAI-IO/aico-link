"""GitHub discovery and artifact transport; model execution never receives this token."""
from __future__ import annotations

from datetime import datetime, timedelta, timezone
import io
import json
from pathlib import Path, PurePosixPath
import stat
import urllib.error
import urllib.request
import urllib.parse
import zipfile

from ci.auto_improve.policy import require_main_branch

MAX_ARCHIVE_BYTES = 512 * 1024 * 1024


def timestamp(value: str) -> datetime:
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


def choose_source(runs: list[dict], processed: set[str], *, branch: str, now: datetime,
                  delay_hours: float = 3, requested: str = "", prefer_latest: bool = False) -> dict | None:
    require_main_branch(branch)
    eligible = []
    for run in runs:
        identity = f"{run['id']}-{run.get('run_attempt', 1)}"
        if (run.get("status") != "completed" or run.get("event") != "schedule" or
                run.get("head_branch") != branch or run.get("conclusion") == "cancelled"):
            continue
        if requested and str(run["id"]) != requested:
            continue
        age = now - timestamp(run["updated_at"])
        if age < timedelta(hours=delay_hours) or age > timedelta(days=13):
            continue
        if not requested and identity in processed:
            continue
        eligible.append(run)
    select = max if prefer_latest else min
    return select(eligible, key=lambda r: r["created_at"]) if eligible else None


def extract_archive(data: bytes, destination: Path) -> None:
    """Treat artifacts as data, rejecting zip-slip, symlinks, and oversized entries."""
    destination.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(io.BytesIO(data)) as archive:
        if sum(info.file_size for info in archive.infolist()) > MAX_ARCHIVE_BYTES:
            raise ValueError("Unpacked artifact exceeds budget")
        for info in archive.infolist():
            path = PurePosixPath(info.filename)
            mode = info.external_attr >> 16
            if (path.is_absolute() or ".." in path.parts or "\\" in info.filename or
                    (stat.S_IFMT(mode) not in {0, stat.S_IFREG, stat.S_IFDIR})):
                raise ValueError("Unsafe artifact path")
        archive.extractall(destination)


class GitHub:
    def __init__(self, repository: str, token: str):
        if not repository or repository.count("/") != 1 or not token:
            raise ValueError("GitHub repository and token are required")
        self.repository, self.token = repository, token
        self.base = f"https://api.github.com/repos/{repository}/"

    def request(self, path: str, payload: dict | None = None):
        url = "https://api.github.com/" + path if path.startswith("search/") else self.base + path
        request = urllib.request.Request(url, headers={
            "Authorization": f"Bearer {self.token}", "Accept": "application/vnd.github+json",
            "X-GitHub-Api-Version": "2022-11-28", "User-Agent": "aico-auto-improve",
            "Content-Type": "application/json"},
            data=json.dumps(payload).encode() if payload is not None else None)
        with urllib.request.urlopen(request, timeout=60) as response:
            return json.load(response)

    def pages(self, path: str, key: str | None = None, *, cutoff: datetime | None = None) -> list:
        out = []
        for page in range(1, 21):
            data = self.request(f"{path}{'&' if '?' in path else '?'}per_page=100&page={page}")
            rows = data[key] if key else data
            if isinstance(data, dict) and data.get("incomplete_results"):
                raise RuntimeError("GitHub search returned incomplete results")
            out.extend(rows)
            if len(rows) < 100 or (cutoff and rows and timestamp(rows[-1]["created_at"]) < cutoff):
                return out
        raise RuntimeError(f"GitHub pagination budget exceeded: {path}")

    def issues(self) -> list[dict]:
        # Fetch labeled records and search body markers to cover failed labeling.
        labeled = self.pages("issues?state=all&labels=auto-improve")
        query = urllib.parse.quote(f'repo:{self.repository} "auto-improve:" in:body')
        found = self.pages(f"search/issues?q={query}&sort=created&order=desc", "items")
        items = {item["number"]: item for item in labeled + found}
        for item in items.values():
            if item.get("comments"):
                item["discussion"] = self.pages(f"issues/{item['number']}/comments")
            if item.get("pull_request"):
                item["reviews"] = self.pages(f"pulls/{item['number']}/reviews")
        return list(items.values())

    def download(self, artifact: dict, destination: Path) -> None:
        if artifact.get("expired") or artifact.get("size_in_bytes", 0) > MAX_ARCHIVE_BYTES:
            raise ValueError("Artifact expired or exceeds transfer budget")
        # GitHub redirects to a signed blob URL. Do not forward its bearer token there.
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, req, fp, code, msg, headers, newurl):
                return None
        request = urllib.request.Request(self.base + f"actions/artifacts/{artifact['id']}/zip",
                                         headers={"Authorization": f"Bearer {self.token}"})
        try:
            response = urllib.request.build_opener(NoRedirect).open(request, timeout=60)
        except urllib.error.HTTPError as exc:
            if exc.code not in {301, 302, 303, 307, 308}:
                raise
            location = exc.headers["Location"]
            if not location.startswith("https://"):
                raise ValueError("Artifact redirect must use HTTPS")
            response = urllib.request.urlopen(location, timeout=60)
        with response:
            data = response.read(MAX_ARCHIVE_BYTES + 1)
        if len(data) > MAX_ARCHIVE_BYTES:
            raise ValueError("Artifact exceeds transfer budget")
        extract_archive(data, destination)

    def discover(self, branch: str, requested: str = "", delay_hours: float = 3,
                 *, include_processed: bool = False) -> dict | None:
        require_main_branch(branch)
        cutoff = datetime.now(timezone.utc) - timedelta(days=14)
        created = urllib.parse.quote(">=" + cutoff.strftime("%Y-%m-%d"))
        runs = self.pages(f"actions/workflows/benchmark.yml/runs?event=schedule&created={created}", "workflow_runs")
        # Artifacts are newest first; old expired entries do not affect recent claims.
        artifacts = self.pages("actions/artifacts", "artifacts", cutoff=cutoff)
        processed = {a["name"].removeprefix("auto-improve-source-") for a in artifacts
                     if not a.get("expired") and a["name"].startswith("auto-improve-source-")}
        source = choose_source(runs, processed if not include_processed else set(),
                               branch=branch, now=datetime.now(timezone.utc),
                               requested=requested, delay_hours=delay_hours, prefer_latest=include_processed)
        if not source:
            if requested:
                raise ValueError("Requested source is not a completed eligible daily run")
            return None
        identity = f"{source['id']}-{source.get('run_attempt', 1)}"
        matches = [a for a in self.pages(f"actions/runs/{source['id']}/artifacts", "artifacts")
                   if a["name"] == f"benchmark-{identity}" and not a.get("expired")]
        if len(matches) != 1:
            raise ValueError(f"Source {identity} has no unique available benchmark artifact")
        return {"run": source, "artifact": matches[0], "identity": identity}
