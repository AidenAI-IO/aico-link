"""Regression tests for incremental feedback and weekly report delivery."""

import io
from http.client import IncompleteRead
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from zipfile import ZipFile

import oss2

from scripts import industry_analysis as analysis
from scripts import industry_analysis_publish as delivery


SOURCE = {"id": "example", "name": "Example", "repository": "org/example"}
CONFIG = {"sources": [SOURCE], "bootstrap_days": 30, "max_issues": 4, "max_opportunities": 3, "focus_areas": []}
FIRST = "2026-11-18T04:00:00Z"
SECOND = "2026-11-19T04:00:00Z"
THIRD = "2026-11-20T04:00:00Z"


class Feedback:
    def __init__(self):
        self.issue = {
            "number": 17, "title": "Interrupted task restarts", "body": "My completed actions run twice",
            "state": "open", "updated_at": "2026-11-18T03:00:00Z", "comments": 0, "user": {"id": 1},
        }
        self.comments = []
        self.issue_fetches = 0
        self.detail_since = []

    def get(self, path):
        if path.endswith("/issues/17"):
            self.issue_fetches += 1
            return self.issue, False
        return {"visibility": "public"}, False

    def pages(self, path, **params):
        if path.endswith("/issues"):
            pull = dict(self.issue, number=18, pull_request={})
            return [self.issue, pull]
        if path.endswith("/issues/17/comments"):
            self.detail_since.append(params.get("since"))
        return sorted(
            (row for row in self.comments if path.endswith("/issues/comments")
             or row["issue_url"].rsplit("/", 1)[-1] == path.split("/")[-2]),
            key=lambda row: row["updated_at"],
        )


class MemoryBucket:
    def __init__(self):
        self.objects = {}

    def get_object(self, key):
        if key not in self.objects:
            raise oss2.exceptions.NoSuchKey(404, {}, b"", {})
        return io.BytesIO(self.objects[key][0])

    def put_object(self, key, data, headers):
        self.objects[key] = (data, headers)


class FeedbackReviewTest(unittest.TestCase):
    def test_overlap_skips_processed_content_and_old_comment_edits_are_reviewed(self):
        github = Feedback()
        state = {}
        payloads = []

        def review(payload):
            payloads.append(payload)
            return {"opportunities": [], "feedback": [], "scenarios": []}, {}

        analysis.run_review(CONFIG, state, FIRST, github, review, "")
        state["sources"]["example"] = state["sources"].pop("org/example")
        analysis.run_review(CONFIG, state, FIRST, github, review, "")
        self.assertEqual({"org/example"}, state["sources"].keys())
        self.assertEqual(1, len(payloads))
        self.assertEqual(["example-17"], [issue["id"] for issue in payloads[0]["issues"]])
        self.assertEqual(0, github.issue_fetches)

        github.issue["comments"] = 1
        github.comments.append({
            "id": 123, "body": "This only happens on restart",
            "issue_url": "https://api.github.com/repos/org/example/issues/17",
            "html_url": "https://github.com/org/example/issues/17#issuecomment-123",
            "updated_at": "2026-11-19T02:00:00Z", "author_association": "MEMBER", "user": {"id": 2},
        })
        analysis.run_review(CONFIG, state, SECOND, github, review, "")
        self.assertEqual(2, len(payloads))
        self.assertEqual("maintainer", payloads[1]["issues"][0]["comments"][0]["role"])
        github.comments[0].update(body="Correction: it happens before restart", updated_at="2026-11-20T02:00:00Z")
        analysis.run_review(CONFIG, state, THIRD, github, review, "")
        self.assertEqual(3, len(payloads))
        self.assertEqual("example-17-c123", payloads[2]["issues"][0]["comments"][0]["id"])
        self.assertEqual(["2026-11-18T03:00:00Z", "2026-11-19T02:00:00Z"], github.detail_since)

    def test_comment_scan_resumes_after_finishing_a_timestamp_across_pages(self):
        github = Feedback()
        github.issue["comments"] = 1
        times = ("2026-11-18T01:00:00Z", "2026-11-18T02:00:00Z",
                 "2026-11-18T02:00:00Z", "2026-11-18T03:30:00Z")
        github.comments = [
            {"id": index, "html_url": f"https://github.com/org/example/{target}#issuecomment-{index}",
             "issue_url": f"https://api.github.com/repos/org/example/issues/{target.split('/')[1]}",
             "updated_at": when, "author_association": "MEMBER", "body": f"Feedback {index}"}
            for index, (target, when) in enumerate(zip(("pull/18", "pull/18", "issues/17", "issues/19"), times), 1)
        ]
        calls = []

        def get(path, **params):
            if path.endswith("/issues/19"):
                return dict(github.issue, number=19), False
            if not params:
                return github.get(path)
            rows = [row for row in github.pages(path, **params) if row["updated_at"] > params["since"]]
            # Keep pages small so the boundary timestamp spans two API requests.
            start = (params["page"] - 1) * 2
            calls.append((path, params["since"], params["page"]))
            return rows[start:start + 2], len(rows) > start + 2

        api = analysis.GitHub(None)
        state = {}
        with patch.object(api, "get", side_effect=get), patch.object(analysis, "SCAN_LIMIT", 2):
            status, issues, _ = analysis.collect(api, CONFIG, state, FIRST)
            self.assertEqual("partial", status[0]["result"])
            self.assertEqual({"comments": times[2]}, state["sources"]["org/example"]["scan_resume"])
            self.assertEqual(["example-17-c3"], [comment["id"] for comment in issues[0]["comments"]])
            status, issues, _ = analysis.collect(api, CONFIG, state, SECOND)
        self.assertEqual("ok", status[0]["result"])
        self.assertIn(("org/example/issues/comments", times[2], 1), calls)
        self.assertEqual(["example-19-c4"], [comment["id"] for comment in issues[0]["comments"]])

    def test_feedback_and_scenarios_are_rendered_even_without_a_new_opportunity(self):
        source = {"label": "Example #17", "url": "https://github.com/org/example/issues/17"}
        comment = {"label": "Example #17", "url": source["url"] + "#issuecomment-123"}
        result = {
            "overview": {"finding": "Queued task shown as paused.", "next_step": "", "evidence": ["example-17-c123"]},
            "feedback": [{
                "issue": "example-17", "kind": "clarification", "summary": "Resume does nothing after a pause.",
                "follow_up": "The task was still queued.", "evidence": ["example-17-c123"],
            }],
            "scenarios": [{
                "title": "Resume after takeover", "goal": "Continue after a manual step.",
                "observation": "Queued shown as paused.", "limits": "No independent resume failure.",
                "feedback": ["example-17"],
            }],
            "opportunities": [],
        }
        evidence = {"example-17": source, "example-17-c123": comment}
        sections = analysis.validate_review(result, evidence, {"example-17"}, 3)
        with tempfile.TemporaryDirectory() as tmp:
            folder = Path(tmp)
            analysis.render({"cutoff": FIRST, "coverage": [], **sections}, folder)
            markdown = (folder / "report.zh-CN.md").read_text()
            self.assertLess(markdown.index("Queued task shown as paused."), markdown.index("The task was still queued."))
            self.assertIn(comment["url"], markdown)
            with ZipFile(folder / "team-share.zip") as archive:
                page = archive.read("index.html").decode()
                self.assertIn('id="feedback-example-17"', page)
                self.assertIn('href="#feedback-example-17"', page)
                self.assertIn("No independent resume failure.", page)
        # A citation outside the feedback section drops that row, not the report.
        result["scenarios"][0]["feedback"] = ["example-18"]
        result["opportunities"] = [{
            "title": "Show queued state", "scenario": "Resume", "impact": "Confusion", "aico_coverage": "Unknown",
            "proposal": "Label queued tasks", "validation": "Label shown", "evidence": ["example-17-c123", "example-18"],
        }]
        other_issue = {
            "label": "Example #18", "url": "https://github.com/org/example/issues/18",
        }
        sections = analysis.validate_review(
            result, {**evidence, "example-18": other_issue}, {"example-17", "example-18"}, 3,
        )
        self.assertEqual([], sections["scenarios"])
        self.assertEqual([comment], sections["opportunities"][0]["sources"])
        # An opportunity can cite a comment that is absent from its feedback row.
        result["feedback"][0].update(kind="run", evidence=["example-17"])
        sections = analysis.validate_review(result, evidence, {"example-17"}, 3)
        with tempfile.TemporaryDirectory() as tmp:
            folder = Path(tmp)
            analysis.render({"cutoff": FIRST, "coverage": [], **sections}, folder)
            opportunity = (folder / "index.html").read_text().split("</article>", 1)[0]
            self.assertIn(f'href="{comment["url"]}"', opportunity)
        result["feedback"][0].update(kind="clarification", evidence=["example-17", "example-18-c1"])
        other_comment = {**other_issue, "url": other_issue["url"] + "#issuecomment-1"}
        sections = analysis.validate_review(
            result, {**evidence, "example-18": other_issue, "example-18-c1": other_comment},
            {"example-17", "example-18"}, 3,
        )
        self.assertEqual([], sections["feedback"])
        self.assertIsNone(sections["overview"])


class SourceConfigTest(unittest.TestCase):
    def test_repository_links_become_stable_sources(self):
        sources = analysis.sources_from("github.com/wangzan101/Mika/issues,\n 1st/app.x.git")
        self.assertEqual(["wangzan101/Mika", "1st/app.x"], [source["repository"] for source in sources])
        self.assertEqual(sources[0]["id"], analysis.sources_from("WANGZAN101/mika")[0]["id"])
        first, second = analysis.sources_from("a-b/c a/b-c")
        self.assertNotEqual(first["id"], second["id"])
        for invalid in ("https://gitlab.com/org/app", "gitlab.com/org/app", "org", "org/.."):
            with self.assertRaisesRegex(ValueError, "Invalid GitHub repository"):
                analysis.sources_from(invalid)
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "config.json"
            analysis.write_json(path, {key: CONFIG[key] for key in CONFIG if key != "sources"})
            with self.assertRaisesRegex(ValueError, "Duplicate GitHub repository"):
                analysis.config_from(path, "org/App https://github.com/org/app")
            with self.assertRaisesRegex(ValueError, "INDUSTRY_ANALYSIS_REPOS"):
                analysis.config_from(path, " ")


class PublicationTest(unittest.TestCase):
    def test_verification_retries_a_truncated_response_and_checks_the_hash(self):
        with patch.object(delivery.time, "sleep"), patch.object(
            delivery, "urlopen", side_effect=[IncompleteRead(b"part"), io.BytesIO(b"stale"), io.BytesIO(b"report")],
        ) as fetch:
            delivery.verify("https://reports.example/team-share.zip", b"report")
        self.assertEqual(3, fetch.call_count)

    def test_publish_retry_keeps_verified_files_and_repairs_the_latest_pointer(self):
        bucket = MemoryBucket()
        with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, {
            "BENCHMARK_REPORT_BASE_URL": "https://reports.example/",
        }):
            folder = Path(tmp)
            delivery.restore(bucket, folder, 1)
            report = {"cutoff": FIRST, "opportunities": [], "coverage": [], "model_usage": {}}
            analysis.render(report, folder / "report")
            analysis.write_json(folder / "checkpoint.json", {"sources": {}})
            with patch.object(delivery, "verify", side_effect=ValueError("preview unavailable")):
                with self.assertRaisesRegex(ValueError, "preview unavailable"):
                    delivery.publish(bucket, folder, 1)
            self.assertNotIn(f"{delivery.PRIVATE}/checkpoint.json", bucket.objects)
            self.assertNotIn(f"{delivery.PUBLIC}/latest.json", bucket.objects)
            delivery.restore(bucket, folder, 1)
            self.assertEqual(report, analysis.read_json(folder / "prepared.json")["report"])
            latest_key = f"{delivery.PUBLIC}/latest.json"
            original_put = bucket.put_object

            def interrupt_pointer(key, data, headers):
                if key == latest_key:
                    raise OSError("pointer upload interrupted")
                original_put(key, data, headers)

            with patch.object(delivery, "verify") as verify, patch.object(bucket, "put_object", interrupt_pointer):
                with self.assertRaisesRegex(OSError, "pointer upload interrupted"):
                    delivery.publish(bucket, folder, 1)
            self.assertEqual(
                ["https://reports.example/industry-analysis/weekly/1/index.html",
                 "https://reports.example/industry-analysis/weekly/1/team-share.zip"],
                [call.args[0] for call in verify.call_args_list],
            )
            archive = bucket.objects["industry-analysis/weekly/1/team-share.zip"][0]
            with ZipFile(io.BytesIO(archive)) as files:
                self.assertNotIn(b"team-share.zip", files.read("index.html"))
                self.assertEqual(report, json.loads(files.read("opportunities.json")))
            self.assertEqual("private", bucket.objects[f"{delivery.PRIVATE}/checkpoint.json"][1]["x-oss-object-acl"])
            self.assertNotIn(latest_key, bucket.objects)
            before = dict(bucket.objects)
            with patch.object(bucket, "put_object", wraps=bucket.put_object) as put:
                self.assertTrue(delivery.restore(bucket, folder / "retry", 1))
                delivery.publish(bucket, folder / "retry", 1)
            self.assertEqual([latest_key], [args.args[0] for args in put.call_args_list])
            self.assertEqual(1, delivery.load(bucket, latest_key)["run_number"])
            self.assertEqual(before, {key: value for key, value in bucket.objects.items() if key != latest_key})
            delivery.save_private(bucket, "checkpoint.json", {"published_run": 2})
            with self.assertRaisesRegex(ValueError, "predates the latest publication"):
                delivery.restore(bucket, folder, 1)

    def test_prepared_analysis_stays_invalid_after_restoring_a_new_base(self):
        bucket = MemoryBucket()
        delivery.save_private(bucket, "runs/2/base.json", {"checkpoint": {}, "cutoff": FIRST})
        delivery.save_private(bucket, "runs/2/prepared.json", {"checkpoint": {}, "report": {}})
        with tempfile.TemporaryDirectory() as tmp:
            folder = Path(tmp)
            delivery.restore(bucket, folder, 2)
            self.assertTrue((folder / "prepared.json").is_file())
            delivery.save_private(bucket, "checkpoint.json", {"published_run": 1, "recent_titles": ["Earlier run"]})
            for _ in range(2):
                delivery.restore(bucket, folder, 2)
                self.assertFalse((folder / "prepared.json").exists())
                base = analysis.read_json(folder / "base.json")
                self.assertEqual(["Earlier run"], base["checkpoint"]["recent_titles"])

    def test_unknown_lark_delivery_does_not_send_a_second_card(self):
        bucket = MemoryBucket()
        with patch.dict(os.environ, {
            "BENCHMARK_REPORT_BASE_URL": "https://reports.example/",
            "INDUSTRY_ANALYSIS_LARK_WEBHOOK_URL": "https://example.test/bot",
            "INDUSTRY_ANALYSIS_LARK_WEBHOOK_SECRET": "test",
        }):
            delivery.save_private(bucket, "checkpoint.json", {"published_run": 1})
            delivery.save_private(bucket, "runs/1/prepared.json", {"report": {
                "cutoff": FIRST, "coverage": [], "opportunities": [{"title": "Resume tasks", "impact": "Less rework"}],
            }})
            with patch.object(delivery, "send_lark", side_effect=TimeoutError) as send:
                with self.assertRaisesRegex(ValueError, "delivery status is unknown"):
                    delivery.notify(bucket, 1)
                delivery.notify(bucket, 1)
            send.assert_called_once()


if __name__ == "__main__":
    unittest.main()
