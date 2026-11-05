from __future__ import annotations

from pathlib import Path

from runner.skills_fingerprint import (
    SOURCE_EXTERNAL,
    SOURCE_UNAVAILABLE,
    SOURCE_WORKER,
    digest_of,
    fingerprint_skills,
    fingerprint_worker_configs,
)


def _write_skill(root: Path, name: str, body: str) -> None:
    skill_dir = root / name
    skill_dir.mkdir(parents=True)
    (skill_dir / "SKILL.md").write_text(f"---\nname: {name}\n---\n\n{body}\n", encoding="utf-8")


def test_digest_tracks_content_not_location_or_mtime(tmp_path: Path):
    first = tmp_path / "a" / "skills"
    _write_skill(first, "alpha", "Do alpha.")
    second = tmp_path / "b" / "skills"
    _write_skill(second, "alpha", "Do alpha.")

    assert fingerprint_skills(first) == fingerprint_skills(second)


def test_digest_changes_when_skill_text_changes(tmp_path: Path):
    skills = tmp_path / "skills"
    _write_skill(skills, "alpha", "Do alpha.")
    before = fingerprint_skills(skills)

    (skills / "alpha" / "SKILL.md").write_text("---\nname: alpha\n---\n\nDo beta.\n", encoding="utf-8")

    assert fingerprint_skills(skills)["sha256"] != before["sha256"]


def test_per_skill_digest_isolates_which_skill_changed(tmp_path: Path):
    skills = tmp_path / "skills"
    _write_skill(skills, "alpha", "Do alpha.")
    _write_skill(skills, "beta", "Do beta.")
    before = fingerprint_skills(skills)

    (skills / "beta" / "SKILL.md").write_text("---\nname: beta\n---\n\nDo gamma.\n", encoding="utf-8")
    after = fingerprint_skills(skills)

    assert after["skills"]["alpha"] == before["skills"]["alpha"]
    assert after["skills"]["beta"] != before["skills"]["beta"]
    assert after["sha256"] != before["sha256"]


def test_missing_directory_is_an_observed_empty_set(tmp_path: Path):
    fingerprint = fingerprint_skills(tmp_path / "absent")

    assert fingerprint["skills"] == {}
    assert fingerprint["sha256"]


def test_nested_files_contribute_to_the_skill_digest(tmp_path: Path):
    skills = tmp_path / "skills"
    _write_skill(skills, "alpha", "Do alpha.")
    before = fingerprint_skills(skills)["sha256"]

    (skills / "alpha" / "references").mkdir()
    (skills / "alpha" / "references" / "notes.md").write_text("extra\n", encoding="utf-8")

    assert fingerprint_skills(skills)["sha256"] != before


def test_worker_configs_are_read_back_from_disk(tmp_path: Path):
    config = tmp_path / "worker" / "config"
    (config / "skills" / "alpha").mkdir(parents=True)
    (config / "skills" / "alpha" / "SKILL.md").write_text("body", encoding="utf-8")

    combined = fingerprint_worker_configs([config])

    assert combined["source"] == SOURCE_WORKER
    assert set(combined["skills"]) == {"alpha"}


def test_disagreeing_workers_report_mixed_instead_of_a_single_digest(tmp_path: Path):
    configs = []
    for index, body in enumerate(("Do alpha.", "Do beta.")):
        config = tmp_path / f"worker{index}" / "config"
        skills = config / "skills"
        skills.mkdir(parents=True)
        (skills / "alpha").mkdir()
        (skills / "alpha" / "SKILL.md").write_text(body, encoding="utf-8")
        configs.append(config)

    combined = fingerprint_worker_configs(configs)

    assert combined["sha256"] is None
    assert combined["source"] == "mixed"
    assert len(combined["per_worker"]) == 2
    assert "," in combined["skills"]["alpha"]


def test_no_worker_skill_tree_is_unavailable_not_empty(tmp_path: Path):
    assert fingerprint_worker_configs([tmp_path / "nothing"])["source"] == SOURCE_UNAVAILABLE


def test_an_agent_url_run_is_reported_as_external(tmp_path: Path):
    """A daemon this process never configured has an unobservable skill tree."""
    config = tmp_path / "config"
    config.mkdir()
    fingerprint = fingerprint_worker_configs([])
    assert digest_of({"skills_fingerprint": {**fingerprint, "source": SOURCE_EXTERNAL}}) == SOURCE_EXTERNAL


def test_digest_of_flattens_a_manifest_for_telemetry():
    manifest = {"skills_fingerprint": {"sha256": "abc", "skills": {"alpha": "def"}}}
    assert digest_of(manifest) == "abc"


def test_digest_of_falls_back_to_source_when_no_single_digest_exists():
    mixed = {"skills_fingerprint": {"sha256": None, "source": "mixed"}}
    assert digest_of(mixed) == "mixed"


def test_digest_of_handles_absent_metadata():
    assert digest_of({}) == ""
    assert digest_of(None) == ""
    assert digest_of({"skills_fingerprint": {"source": SOURCE_EXTERNAL}}) == SOURCE_EXTERNAL
