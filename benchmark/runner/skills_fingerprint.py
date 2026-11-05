"""Content fingerprints for the skills an Agent worker actually loads.

A run reported skill *names* but never their text, so two runs with the same
name list could score differently because `SKILL.md` changed between them with
nothing in the manifest to say so. The fingerprint closes that gap: it is
computed from the copied skill tree inside a worker's config directory, so it
describes what the Agent loaded rather than what happened to be in the
repository checkout.

Digests are content-only. File order, mtime, permissions, and absolute paths
never contribute, because the same skill text must produce the same digest
regardless of which machine prepared the config.
"""

from __future__ import annotations

import hashlib
from pathlib import Path
from typing import Any, Iterable, Mapping

SCHEMA_VERSION = 1

# Provenance values for the fingerprint source.
SOURCE_WORKER = "worker-config"
SOURCE_EXTERNAL = "external-daemon"
SOURCE_UNAVAILABLE = "unavailable"


def _hash_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _sorted_files(root: Path) -> list[Path]:
    return sorted(
        (path for path in root.rglob("*") if path.is_file()),
        key=lambda path: path.relative_to(root).as_posix(),
    )


def _skill_digest(skill_dir: Path) -> str:
    digest = hashlib.sha256()
    for path in _sorted_files(skill_dir):
        relative = path.relative_to(skill_dir).as_posix()
        digest.update(relative.encode("utf-8"))
        digest.update(b"\0")
        digest.update(_hash_file(path).encode("ascii"))
        digest.update(b"\0")
    return digest.hexdigest()


def _tree_digest(skill_digests: Mapping[str, str]) -> str:
    digest = hashlib.sha256()
    for name in sorted(skill_digests):
        digest.update(name.encode("utf-8"))
        digest.update(b"\0")
        digest.update(skill_digests[name].encode("ascii"))
        digest.update(b"\0")
    return digest.hexdigest()


def fingerprint_skills(skills_dir: Path) -> dict[str, Any]:
    """Fingerprint every skill directory under `skills_dir`.

    A missing directory yields the empty-set digest, which is a real value:
    a worker that loaded no skills is not the same as one whose skills are
    unknown. Callers distinguish that case with `source`, not with None.
    """
    skills_dir = Path(skills_dir)
    digests: dict[str, str] = {}
    if skills_dir.is_dir():
        for entry in sorted(skills_dir.iterdir(), key=lambda item: item.name):
            if entry.is_dir():
                digests[entry.name] = _skill_digest(entry)
    return {
        "schema_version": SCHEMA_VERSION,
        "sha256": _tree_digest(digests),
        "skills": digests,
    }


def fingerprint_worker_configs(config_dirs: Iterable[Path]) -> dict[str, Any]:
    """Combine the skill trees of the workers that took part in a run.

    Hashing happens here, at run finalization, rather than when a worker's
    config is prepared. SkillOpt writes its candidate skill into the prepared
    config after `prepare_run_config` returns, so a fingerprint captured at
    prepare time would describe a superseded skill text.

    Workers can also legitimately disagree, because SkillOpt prepares one
    config per phase. A single digest is reported only when every worker
    agreed; otherwise the per-worker map is kept so the difference stays
    inspectable.
    """
    seen: dict[str, dict[str, Any]] = {}
    for config_dir in config_dirs:
        config_dir = Path(config_dir)
        skills_dir = config_dir / "skills"
        if not skills_dir.is_dir():
            continue
        seen[str(config_dir)] = fingerprint_skills(skills_dir)
    if not seen:
        return {"sha256": None, "skills": {}, "source": SOURCE_UNAVAILABLE}
    digests = {fingerprint["sha256"] for fingerprint in seen.values()}
    if len(digests) == 1:
        only = next(iter(seen.values()))
        return {
            "sha256": only["sha256"],
            "skills": dict(only.get("skills") or {}),
            "source": SOURCE_WORKER,
        }
    merged: dict[str, list[str]] = {}
    for fingerprint in seen.values():
        for name, digest in (fingerprint.get("skills") or {}).items():
            merged.setdefault(name, [])
            if digest not in merged[name]:
                merged[name].append(digest)
    return {
        "sha256": None,
        "skills": {name: ",".join(sorted(values)) for name, values in sorted(merged.items())},
        "source": "mixed",
        "per_worker": {
            path: {"sha256": fingerprint["sha256"], "skills": fingerprint.get("skills") or {}}
            for path, fingerprint in sorted(seen.items())
        },
    }


def digest_of(manifest: Mapping[str, Any] | None) -> str:
    """Flat comparable digest for telemetry metadata; never a dict."""
    if not isinstance(manifest, Mapping):
        return ""
    fingerprint = manifest.get("skills_fingerprint")
    if not isinstance(fingerprint, Mapping):
        return ""
    digest = fingerprint.get("sha256")
    if isinstance(digest, str) and digest:
        return digest
    source = fingerprint.get("source")
    return str(source) if isinstance(source, str) and source else ""
