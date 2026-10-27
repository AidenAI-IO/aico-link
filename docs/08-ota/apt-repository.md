# GitHub APT Repository

Business package repository: `https://aidenai-io.github.io/aiden-firmware/apt`.
GitHub Pages hosts the signed `InRelease`, `Release.gpg`, package indexes, and `.deb` files.
Packages come only from published Releases in the three channels; unpublished Actions
build artifacts are never added to the repository.

## Upgrading a board

New base images with this feature automatically select a repository from
`/usr/lib/aico/platform/contract.json`. For example, dev with contract `1` uses
`dev-c1`; staging/prod each use the repository for their own contract.

```bash
sudo apt update && sudo apt upgrade
dpkg-query -W aico-business
sudo /usr/lib/aico/ota --config /userdata/debian/ota/config.json self-check
```

APT/dpkg automatically stops and restores previously running business services. User
configuration and userdata are preserved. Business and live configuration upgrades do
not require a device reboot; changes to network/SSH/USB configuration prompt for a later
reboot without actively interrupting connections. For the scope and markers, see
[Runtime Configuration Managed by the Business Package](system-config-package.md).
Packages for a new contract do not appear as candidates for an old contract. A device
switches to the repository for the new base contract only after installing the new OTA.
The package's preinst still checks the channel, contract, base release tag, and system fingerprint.

Config Web's "Check and install updates" action (`POST /api/ota/updates`) starts
`aico-hybrid-update.service`, which proceeds in this order:

1. Run `ota check` against the current channel's GitHub Release, verifying the signed
   manifest, version, and build time. Run `ota update` only when new firmware is available.
   The check itself does not download images, write partitions, or change OTA transaction state.
2. After switching slots and rebooting into the new firmware, wait for the existing OTA
   health service. Continue only when both the requested version and build time are
   `committed`; a rollback or failed health confirmation ends the job without upgrading
   the business package.
3. When no new firmware is available, or the new firmware has been confirmed, refresh
   only the signed indexes from `aico-business.sources`. Simulate
   `apt-get --only-upgrade install aico-business` and install only if that package has
   an update. Targeting the package avoids upgrading other installed software through
   a general `apt-get upgrade`; a plan that changes other packages or removes any package
   is rejected. Platform repository pins, APT signature verification, and package
   contract checks still apply.

The update service runs independently of Config Web, so stopping and restarting the
portal during a business package upgrade does not interrupt the job. Its marker is
stored at `/userdata/ota/hybrid-update.pending`, allowing it to resume after an OTA
reboot; `GET /api/ota/status` continues to provide logs and progress. A successful
business package upgrade does not actively reboot the device. Any later reboot required
by runtime configuration is still indicated by `/run/aico-business-reboot-required.json`.
APT refresh or installation failures are reported as failures, not as "up to date".
The worker limits each OTA metadata check, APT index refresh, and APT simulation
to 600 seconds. APT index refresh is attempted up to three times, with 15- and
30-second delays so association, DHCP, or the package source can become available
after an OTA reboot. Exhausted retries and metadata timeouts release the update
locks and clear the request marker. Firmware flashing and package installation
are not subject to this metadata timeout.
After an interrupted package installation, repair the dpkg state before retrying manually.

`/etc/apt/preferences.d/aico-business` assigns business packages from the matching
repository a priority of 990 and Debian repository packages a priority of 1, below the
installed-package priority of 100. Normal `apt upgrade` therefore upgrades only business
packages. Debian system packages are updated through OTA, preventing system libraries
from changing while the base contract remains unchanged. Explicitly installing Debian
tools that are not already installed is still supported. If you add third-party system
repositories, configure their pins separately. Maintainers intentionally changing system
libraries can explicitly select a version or adjust the pin, then establish a base
contract through a new OTA.

`apt upgrade` does not reinstall an already installed version. To replace a build package
with the published package of the same version:

```bash
sudo apt install --reinstall aico-business
```

## Initial setup for existing managed devices

Copy the public key and configuration tool from a reviewed source checkout. Do not execute
scripts directly from unverified network responses.

```bash
scp overlay-debian/usr/share/keyrings/aico-archive-keyring.asc \
    overlay-debian/usr/lib/aico/aico-apt-source luckfox:/tmp/
ssh -t luckfox 'sudo python3 /tmp/aico-apt-source --public-key /tmp/aico-archive-keyring.asc'
ssh -t luckfox 'sudo apt update && sudo apt upgrade'
```

Public key fingerprint: `D40CD0F29C3A65449439857F53CFFB430DBD167D`.
Trust only the repository's `Signed-By` public key; do not use `apt-key` or `trusted=yes`.
The tool rejects old images without a channel or a complete contract. These devices
must first be fully reflashed with the managed base image for their channel.
Repository configuration, public keys, and system package pins belong to the platform
and are delivered through OTA, not modified by the business package.

## Publishing and maintenance

Initial setup:

1. In the GitHub repository, select **GitHub Actions** under Settings → Pages → Source.
2. Store the ASCII-armored OpenPGP signing private key corresponding to the committed
   public key in the repository Actions secret `APT_SIGNING_PRIVATE_KEY`. Use a dedicated
   APT key; do not reuse the OTA Ed25519 PEM key.
3. Push and merge this workflow, then manually run **Aico Link APT Repository** to build the
   index from published versions. Before merging, you can also select the feature branch
   in **Debian Build (backup / self-hosted-02)**, enable `apt_only`, and keep
   `dry_run=false` to refresh only the APT index, ignoring build and new-release parameters.

Both primary and backup releases call this workflow after successfully making the GitHub
Release public. It does not rebuild business packages, allocate versions, or change the
release comparison baseline. The signing secret is checked before publication.
An APT refresh failure does not undo an already public Release; fix the problem and rerun
the APT workflow separately.

The index does not set `Valid-Until`, so no scheduled refresh is required; it is updated
only after publication or a manual trigger. APT still verifies index signatures and
package checksums, and new versions in all three channels are still published manually.
The Pages `github-pages` Environment must allow the branch running the release workflow.

The latest three published packages are retained for each channel/contract combination,
and repositories for old contracts remain available. Indexes use APT by-hash downloads;
package paths are fixed by contract and release tag. Run `apt update` again if an old
cache references a package that has been cleaned up. Deployment stops if the site exceeds
900 MiB, preserving the old site. At that point, archive unsupported contracts to other
static storage and adjust the generation policy; do not simply delete contract
repositories still used by devices.

Source records, SHA256, package architecture/version, and embedded contracts must all pass
validation before the entire site is signed and deployed atomically. The private key is
placed only in the signing step's temporary GnuPG directory; after cleanup, only public
site content is uploaded.

Local validation on Linux:

```bash
python3 scripts/test_apt_repository.py
GNUPGHOME=/path/to/private-gnupg python3 scripts/apt/repository.py \
  --repo AidenAI-IO/aiden-firmware --output output/apt/site
```

Tests run a real `apt update` and simulated `apt upgrade`, covering contract isolation,
system package pins, incorrect public keys, tampered indexes, signed metadata without
expiration, and release package validation failures. Keep a secure backup of the signing
private key. When rotating public keys, first distribute transitional trust for both old
and new keys to devices through OTA, then switch the repository signing key.
