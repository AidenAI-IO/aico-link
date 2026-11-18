"""Regression tests for incremental feedback and weekly report delivery."""

import io
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

    def get(self, path):
        if path.endswith("/issues/17"):
            self.issue_fetches += 1
            return self.issue, False
        return {"visibility": "public"}, False

    def pages(self, path, **params):
        if path.endswith("/issues"):
            pull = dict(self.issue, number=18, pull_request={})
            return [self.issue, pull]
        return self.comments


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
        analysis.run_review(CONFIG, state, FIRST, github, review, "")
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
        sections = analysis.validate_review(result, {**evidence, "example-18": {
            "label": "Example #18", "url": "https://github.com/org/example/issues/18",
        }}, {"example-17", "example-18"}, 3)
        self.assertEqual([], sections["scenarios"])
        self.assertEqual([comment], sections["opportunities"][0]["sources"])


class SourceConfigTest(unittest.TestCase):
    def test_repository_links_become_stable_sources(self):
        self.assertEqual([
            {"id": "wangzan101-mika", "name": "Mika", "repository": "wangzan101/Mika"},
            {"id": "r-1st-app-x", "name": "app.x", "repository": "1st/app.x"},
        ], analysis.sources_from("github.com/wangzan101/Mika/issues,\n 1st/app.x.git"))
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
    def test_failed_preview_keeps_checkpoint_and_retry_uses_same_urls_and_report(self):
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
            with patch.object(delivery, "verify") as verify:
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
            delivery.store(bucket, f"{delivery.PUBLIC}/latest.json", b'{"run_number": 2}', "application/json")
            with self.assertRaisesRegex(ValueError, "predates the latest publication"):
                delivery.restore(bucket, folder, 1)

    def test_unknown_lark_delivery_does_not_send_a_second_card(self):
        bucket = MemoryBucket()
        with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, {
            "BENCHMARK_REPORT_BASE_URL": "https://reports.example/",
            "INDUSTRY_ANALYSIS_LARK_WEBHOOK_URL": "https://example.test/bot",
            "INDUSTRY_ANALYSIS_LARK_WEBHOOK_SECRET": "test",
        }):
            folder = Path(tmp)
            (folder / "report").mkdir()
            analysis.write_json(folder / "report" / "opportunities.json", {
                "cutoff": FIRST, "coverage": [], "opportunities": [{"title": "Resume tasks", "impact": "Less rework"}],
            })
            with patch.object(delivery, "send_lark", side_effect=TimeoutError) as send:
                with self.assertRaisesRegex(ValueError, "delivery status is unknown"):
                    delivery.notify(bucket, folder, 1)
                delivery.notify(bucket, folder, 1)
            send.assert_called_once()


if __name__ == "__main__":
    unittest.main()
