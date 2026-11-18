---
sidebar_label: Industry Opportunity Analysis
---

# Industry Opportunity Analysis Weekly Report

The [GitHub Actions workflow](../../.github/workflows/industry-analysis-weekly.yml)
reviews public GitHub issue feedback on similar products for Aico opportunities.
It groups related user scenarios, checks coverage against public Aico
documentation, and generates a Chinese report. The report leads with a cited
finding and a next step, followed by suggestion cards ordered by the number and
severity of related reports, the user scenarios with what the feedback does not
prove, and the source feedback. Feedback can appear even when it yields no
opportunity.

When feedback supports a benchmark extension, the report links an existing suite
and task from the checked out CI catalog. It records the proposed delta, test
environment and independent pass condition. A candidate may require a new fixture
or a CI entry that is currently disabled.

List the repositories in the `INDUSTRY_ANALYSIS_REPOS` repository variable,
separated by commas or new lines. Each entry can be a GitHub link or
`owner/name`. The report names each source after its repository, and its
checkpoint is keyed by the repository, so removing and later re-adding a link
resumes where it stopped. Set product priorities and limits in
[the analysis settings](../../config/industry-analysis.json). A newly added
repository looks back 30 days by default.
Later runs resume from each source's stored checkpoint with a two-day overlap.
A single call analyzes up to 24 changed issues by default, with up to five
opportunities selected across sources. Recent changes get priority, with part of
the budget reserved for older pending feedback. Excess items are queued for the next run.
Coverage shows source errors and pending issues. The model sees bounded excerpts
from each issue and up to four recent or recently edited comments. The report
retains the selected opportunities and recent titles used for deduplication.

## Preview and rollout

Once the workflow is on the default branch, open **Actions → Industry Analysis
Weekly Report → Run workflow**. Keep **dry_run** checked for the first run.
Optionally enter one configured repository. A preview starts from the configured
lookback and downloads as an Actions artifact. Only include publishable content
as previews are accessible through the public repository.

| Setting | Scope | Purpose |
| --- | --- | --- |
| `AUTO_IMPROVE_REVIEW_MODEL`, `AUTO_IMPROVE_REVIEW_BASE_URL`, `AUTO_IMPROVE_REVIEW_API_KEY` | Existing variables and secret | Model for opportunity analysis. Like Benchmark Auto-Improve, the workflow calls it through the pinned Claude Code CLI in a session without tools. |
| `OSS_ACCESS_KEY_ID`, `OSS_ACCESS_KEY_SECRET` | Existing secrets | Write the report and read/write checkpoints in `firmware-agent`. Verify RAM permissions for the new prefixes and for setting the checkpoint ACL. |
| `BENCHMARK_REPORT_BASE_URL` | Existing variable | HTTPS base URL for published reports. Verify that the domain routes the new prefix to the bucket. |
| `INDUSTRY_ANALYSIS_REPOS` | Variable | Public GitHub repositories to review, as links or `owner/name`, separated by commas or new lines. |
| `INDUSTRY_ANALYSIS_ENABLED` | Variable | Set to `true` to enable the Monday 09:47 Asia/Shanghai schedule. A scheduled run publishes the report and sends the Lark card when there are opportunities. |
| `INDUSTRY_ANALYSIS_LARK_WEBHOOK_URL`, `INDUSTRY_ANALYSIS_LARK_WEBHOOK_SECRET` | Secrets | Webhook URL and signing secret for the industry analysis group's Lark bot. |

Uncheck **dry_run** on the default branch to verify OSS publication. Use
**send_notification** to explicitly send that published report to the configured
bot. The workflow uploads to
`oss://firmware-agent/industry-analysis/weekly/<run_number>/` and verifies the
HTML and ZIP through
`${BENCHMARK_REPORT_BASE_URL}/industry-analysis/weekly/<run_number>/index.html`
and the corresponding `team-share.zip` path. The domain serves shareable content.
The checkpoint, pending IDs and delivery receipts use private object ACLs under
`industry-analysis/private/`. Verify anonymous reads of that prefix are denied
by both the OSS and domain access policies before rollout.

A rerun reuses its report address and resumes from the saved analysis once it
exists. Publication advances the collection checkpoint after online verification.
A confirmed Lark send is recorded once per report. A webhook timeout leaves
delivery marked as unknown. Check the group before clearing that run's receipt and
retrying. A later published run supersedes older runs for checkpoint updates.

For a local preview, install the Claude Code CLI, make the model settings
available in the environment, and optionally set `GH_TOKEN` for the GitHub read
limit.

```bash
INDUSTRY_ANALYSIS_REPOS=https://github.com/wangzan101/Mika \
  python3 -m scripts.industry_analysis --output /tmp/aico-industry-preview
```

The full test profile runs the `industry-analysis` suite. Run it alone with
`bash scripts/run_tests_in_docker.sh --suite industry-analysis`.
