---
sidebar_position: 9
---

# OTA Release Channels

All three channels, `dev`, `staging`, and `prod`, can be selected explicitly in
the **Aico Link Channel Release** workflow. The primary build workflow also checks
`main` hourly and publishes changed content to the fixed `dev` channel;
`staging` and `prod` remain manual. See [Channel Releases](channel-release.md) for
classification, version allocation, contracts, repository setup and retries.

| Channel | GitHub release type | Device discovery |
| --- | --- | --- |
| dev | Prerelease | Highest dev OTA version |
| staging | Prerelease | Highest staging OTA version |
| prod | Normal release | Highest prod OTA version |

Each channel compares against its own previous successful release. Business-only
changes publish a `.deb`; system changes publish signed boot/rootfs OTA images
and establish a new platform contract. Package-only releases are skipped during
firmware discovery. Only prod OTA releases update GitHub Latest.

The factory config at `/userdata/debian/ota/config.json` carries `repo` and
`channel`. Managed devices query the release list, select their channel's highest
OTA version, and verify that the signed manifest matches the configured channel.
This check also applies to explicit `--manifest-url` overrides.

Legacy devices with no channel, or old local/stable channel labels, continue to
use `/releases/latest`. Flash a channel's full image to provision the new base
and its channel config. Do not edit the contract file to make a package install;
its contents describe the system that was actually built and installed.

Custom static distribution remains available through `manifest_url`; use a test
device configured for that custom source. See [External Developer Guide](ota-external-developers.md).
