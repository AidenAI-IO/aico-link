# Three-Channel Releases

All production releases use **Aiden Channel Release** (`.github/workflows/release.yml`),
which can be triggered manually or called by the primary and backup build entry points.
**Debian Build (scheduled / primary)** checks `main` hourly and automatically publishes
runtime changes to `dev`. `staging` and `prod` remain manual; fallback and standalone
business-package workflows only produce build artifacts.

## Release decisions

Each channel compares against its own last successful release, including file contents,
paths, modes, and the SDK gitlink commit in the Git tree. Drafts, failed builds, and
workflow artifacts do not advance the baseline.

| Comparison result | Build and publish | Base contract |
| --- | --- | --- |
| First release in the channel | Full OTA, flash image, and matching `.deb` | Allocate a new contract |
| Only business or in-scope configuration changes (including new files) | `.deb` | Inherit the channel's latest OTA contract and base identifier |
| System changes or mixed business/system changes | Full OTA, flash image, and matching `.deb` | Allocate a new contract |
| Documentation/tests only, or no changes | No build or release | Unchanged |
| `force_ota` selected | Full OTA, even with no source changes | Allocate a new contract |

The planner first classifies in-scope overlay files as `config` (included in the business
fingerprint) according to `scripts/debian-system/config-package.json`, then matches
ignore, system, and business in that order from `scripts/release/policy.json`.
The first configuration ownership transition forces a new OTA; later in-scope files do
not expand the contract. See [Runtime Configuration Managed by the Business Package](system-config-package.md).
`src/` and `assets/business/` are normally business; OTA implementation, Go dependency
declarations, CMake files, and factory Agent configuration are system. Files not listed
as business, including out-of-scope overlays, SDK, kernel, partitions, base dependencies,
and build/release scripts, default to system. Non-payload Markdown and `.gitignore` files
do not affect releases. Markdown in bundled skills, config-web assets, and business assets
or the embedded Agent Web UI remains business content. Markdown and `.gitignore` under
`overlay-debian/` follow the normal overlay classification because they can be copied
into the image or business package. Release notes are separate assets, not image contents.

Deletions and renames participate in comparison. Squash merges and rebases are supported:
the previous release commit need not be an ancestor of the candidate commit, but both
must share Git history; selecting an older ancestor of the previous release is still
rejected. With an existing baseline, a shallow clone fetches the complete history of the
previous and candidate commits from `origin` before checking their common ancestor;
fetch failure or a timeout over 300 seconds aborts planning. Full clones do not fetch
extra history. Classification and file lists compare the two release Git trees directly,
not differences from their common ancestor. Thus an identical-content squash does not
trigger a release, while system-content changes still require a new OTA contract.
The Full comparison link in release notes also uses the two-endpoint comparison; commit
summaries retain messages from the new history.
GitHub release notes no longer list individual files. OTA releases add a notice in the form
`Commit <sha> (and more) requires OTA update.`, with the SHA linked to the first system-change
commit found from oldest to newest in the release range; `(and more)` appears only when further
relevant commits exist. The search is not limited by the 100-commit summary, skips reverted paths
and squash/rebase commits whose system tree matches the published baseline, and falls back to the
source commit for forced OTAs, first base releases, or configuration-ownership migrations without
an attributable system commit. Detailed file classification remains in the plan and `release.json`.

External Debian repositories, signing keys, Actions secrets, and floating downloads are
outside Git comparison. Use `force_ota` when these inputs change; source differences
cannot detect them automatically.

## Versions and contracts

Release tags look like `dev-v0.0.2`, `staging-v0.0.3`, and `prod-v0.0.4`.
All channels share one version sequence. By default, patch is incremented above the
maximum published version; the first managed release is `0.0.2`, above the existing
local package `0.0.1-2`. A higher `MAJOR.MINOR.PATCH` may be entered manually.
Published package versions are `${version}-1`; editing `version.sh` is no longer used
to assign published versions.

Contracts are positive JSON integers, such as `"platform_contract": 1`, not version
strings. They also increment globally: the first managed OTA is `1`, followed by `2`,
`3`, and so on. Different bases never share a contract number across channels. Each
channel's first release builds its own base, so publishing the same commit to a new
channel still creates an OTA and a new contract.

For example, if dev's OTA contract is `4`, its next two business packages still depend on
`[4, 5)` and bind the same `base_release`, channel, and system-source fingerprint.
After another dev system OTA, later dev packages use the new contract; staging/prod
continue to use their own published bases.

`AIDEN_PLATFORM_CONTRACT` is decimal text such as `1`; generated JSON fields and package
compatibility bounds `min` / `max_exclusive` are integers. The old `"1.0.0"` format is rejected.

`scripts/release/contract.py` generates all of the following from the release plan:

- `/usr/lib/aiden/platform/contract.json` in the rootfs.
- `release-manifest.json` and pre-install checks in the `.deb`.
- The `platform-contract.json` release asset.

When a managed business package is installed, `preinst` checks the platform, contract,
channel, base tag, and source fingerprint before unpacking; mismatches abort and require
the matching OTA first. dpkg's existing service stop, restore, and failure-callback
logic remains. The initial rootfs receives the contract before `.deb` installation, and
the final image audit checks it again. A contract identifies compatibility and does not
replace signatures. The `.deb` contains in-scope configuration and systemd units, not
platform libraries or partition images. Platform contracts, business manifests, and
release records declare configuration ownership with `runtime_config: 1`; old records
without the field are treated as 0.

## Actions operation

After merging the workflow into the GitHub default branch:

1. Open **Aiden Channel Release**, choose channel and source_ref (default main;
   a commit SHA is accepted).
2. Keep `plan_only=true` and inspect the plan, contract, and change list in Summary and
   the `release-plan` artifact.
3. Build with `plan_only=false`; set `publish=false` to generate downloadable validation artifacts only.
4. Set `publish=true` for the actual release. The release job runs through the channel's GitHub Environment.

The primary **Debian Build (scheduled / primary)** (`build-scheduled.yml`) and backup
**Debian Build (backup / self-hosted-02)** (`build-backup.yml`) expose the same
`channel`, `source_ref`, `version`, `force_ota`, `plan_only`, and `publish` parameters.
OTAs are fixed to `aiden-hosted-01` and `aiden-hosted-02`, respectively; business packages
are still built on GitHub-hosted Ubuntu. Manual runs of both entry points default to
`channel=dev`, `source_ref=main`, `plan_only=false`, `publish=true`, `dry_run=false`,
and `apt_only=false`, which builds and publishes validated artifacts.
Select `plan_only` to inspect a plan; clear `publish` to generate artifacts only.
`dry_run=true` takes precedence and checks only the selected machine environment,
signing key, and toolchain, without planning, building, or publishing.
`apt_only=true` refreshes only the signed APT repository from existing releases, still
subject to the precedence of `dry_run`.

The primary entry point restores the `17 * * * *` schedule, checking every hour at
minute 17 UTC (also `:17` in Beijing/Singapore time; actual startup may be delayed by
scheduling and queueing). Scheduled runs use `main`, `dev`, `plan_only=false`,
`publish=true`, and `force_ota=false`; manual-input defaults do not depend on the
scheduled event. Every run directly reuses the release planner inside the global release
lock and compares the source fingerprint of the channel's last successful release,
including `dev` prereleases. No changes, documentation/tests only, and identical-content
squash merges produce `kind=none`, with no build or release. Business changes produce a
`.deb`, system changes a full OTA, and failed builds do not advance the baseline;
the check runs again the next hour.

All three entry points share classification, contract allocation, channel Environment,
draft upload, and download verification. The called release workflow holds the same
global release lock. The primary and backup entry points' `primary-build` and
`backup-build` concurrency groups must not be renamed to that release lock, or nested
calls will wait on one another. A new run does not cancel a running build.

The repository requires `OTA_ED25519_PRIVATE_KEY` (Ed25519 PEM, OTA builds only) and
optionally `AGENT_CONFIG_TOML`. Business-package builds need neither secret. Publishing
uses the workflow's `GITHUB_TOKEN`; only the publish job has `contents: write`. Calling
jobs in the primary and backup workflows must also declare that maximum permission so
it can be passed to the called publish job; planning and build jobs remain read-only.
Repository Actions permissions must allow it. Configuring approvers and allowed release
branches for the `staging` and `prod` Environments is recommended.

Firmware can use `aiden-hosted-01`, `aiden-hosted-02`, or `ubuntu-24.04`; business
packages use GitHub-hosted Ubuntu. Firmware reuses `build.yml` SDK cleanup, toolchain,
cache, signing, and final audit. The supplied source_ref is resolved once and all later
steps pin that SHA; the commit must contain this release tooling.

All channels share one workflow concurrency group, and release history is rechecked
before publication. GitHub concurrency is not an unlimited queue: one running and one
waiting job are retained per group, and a later waiting job can replace the earlier one.
Do not start multiple releases in parallel; a running release is not canceled by a new job.

## Local scripts

Planning requires only Git, Python 3.11+, and an authenticated GitHub CLI. Builds run on
Linux amd64 (for example, luhaodev) and require Docker and the existing firmware build
environment; macOS can inspect plans and artifacts.

```bash
python3 scripts/release/release.py plan \
  --repo AidenAI-IO/aiden-firmware --channel dev

# Clean working tree; HEAD must match source_commit in the plan.
# OTA builds also require the signing/trusted-public-key variables used by debian_build.sh.
python3 scripts/release/release.py build --plan output/release/plan.json
python3 scripts/release/release.py verify output/release/assets

# Run only when explicitly ready to publish externally.
python3 scripts/release/release.py publish output/release/assets
```

`plan --history history.json` uses an offline release-record array for preview/testing.
An empty array means no managed releases; it does not mean online history was read.
The publisher always rereads GitHub and does not accept offline history as the basis
for publication. `--force-ota` forces a base refresh; `--version` selects a version above
the global published maximum.

The release directory contains `.deb`, the business manifest, platform contract,
automated release notes, `release.json`, and `SHA256SUMS`. OTA releases also contain
the signed manifest, boot A/B, rootfs, compressed flash image, flash-image checksum file,
and OTA verification public key. userdata is not released as a separate OTA partition.

## Release integrity and retrying failures

`release.json` records the source SHA, Git tree, channel, version, previous release,
platform baseline, system/business fingerprints, change list, and size/hash of every
asset. Published managed tags must have valid records; read failures abort planning
rather than being mistaken for a first release. Old `debian-*`/`business-v*` tags are
not baselines for the new process. Do not delete or modify published managed tags,
assets, or records: later versions and contract allocation depend on them.

The publisher verifies tag targets, creates the draft, uploads, fully redownloads and
checks all files, and only then makes the release public. OTA also verifies the Ed25519
signature, image hashes, and tag-pinned download URLs. Any history change during
publication aborts public release. Published versions cannot be overwritten. After an
upload failure, retain the original `channel-release-assets` artifact and rerun
`publish` with the same files; a draft with different build contents is not overwritten.
Rebuilding changes build times and signatures and cannot replace the original assets
when resuming the same draft.

dev/staging are prereleases. Only prod OTA can update GitHub Latest; prod business
packages do not take Latest. New OTA clients read `repo` and `channel` from device
configuration, paginate to find the channel's highest-version OTA, skip business packages,
and verify the signed manifest's channel. Empty channels and old configurations continue
to use the old Latest entry point; old devices must first be fully reflashed with the
new base image for their channel to obtain channel selection.

After publication, the signed GitHub Pages APT repository is refreshed automatically.
Devices run `apt update && apt upgrade` to upgrade business packages for their channel
and current base contract; system updates continue through A/B OTA. For initial setup,
signing keys, system package pins, and index maintenance, see
[GitHub APT Repository](apt-repository.md).
