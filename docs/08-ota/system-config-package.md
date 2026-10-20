# Runtime Configuration Managed by the Business Package

Runtime configuration is included directly in `aiden-business`; there is no separate
`aiden-system-config` package. Each version has a single `.deb`, so programs, assets,
configuration, and maintainer scripts are upgraded or downgraded together. Even a few
lines of configuration can be published through APT with the business package. Whether
a reboot is required and whether an OTA is required are two independent decisions.

## Define the scope once; include new files automatically

`scripts/debian-system/config-package.json` defines the boundary between the package and
the platform using namespace rules and platform exclusions, rather than a per-file
allowlist. `scripts/debian-package/system_config.py` automatically generates each
version's actual file inventory from `overlay-debian/`. Packaging, rootfs copy exclusions,
and image audits share this inventory; release classification uses the same rules.

| Scope included in the business package | Examples / purpose |
| --- | --- |
| `/etc/aiden/**`, `/etc/aiden_*.conf`, `/etc/default/aiden-*` | Prefer dedicated directories for new features; supports existing audio, BLE, startup, and swap configuration |
| `/etc/locale.conf` | Default locale; Debian provides the compatibility symlink `/etc/default/locale → ../locale.conf` |
| `/etc/profile.d/aiden-*.sh`, `/etc/sudoers.d/*-aiden-*` | root/aiden terminal environment and rules for preserving proxy settings through sudo |
| `/etc/ssh/sshd_config.d/*-aiden.conf` | Aiden SSH settings |
| `/etc/systemd/system/aiden*.{service,path,timer}`, `aiden.target` | Business and device helper services, subject to platform exclusions |
| `*.service.d/*-aiden.conf`, `*.device.d/*-aiden-optional.conf` | Aiden service and device drop-ins |
| `/etc/systemd/network/{20-wlan0,30-usb0}.network`, `*-aiden-*.network` | Network configuration |
| `/etc/dnsmasq.d/usb0.conf`, `aiden*.conf` | USB DHCP settings |
| `/etc/systemd/journald.conf.d/*-aiden.conf` | Logging settings |
| `/etc/tmpfiles.d/aiden*.conf`, `/etc/udev/rules.d/*-aiden-*.rules` | Directory and device permission rules |
| `/usr/lib/aiden/aiden-*` | Aiden helper scripts, subject to platform exclusions |
| `/usr/share/aiden/hdmi/*.hex` | RK628 runtime EDID profiles for persistent 1080p60/1080p30 selection |

For example, adding `overlay-debian/etc/aiden/camera.conf` or
`overlay-debian/usr/lib/aiden/aiden-new-helper` automatically includes it in the `.deb`
without changing the inventory or contract. New services should be started through
dependencies of `aiden.target` or existing services. APT does not run `preset-all` or
change the administrator's enablement choices. An `[Install] WantedBy=` entry alone
does not automatically enable a new service.

Platform exclusions include foundational scripts and units for OTA, partition
expansion/slots, userdata migration, machine identity, SSH identity, user directories,
environment preparation, Wi-Fi drivers, and media modules. Platform libraries, the
kernel, drivers, mount units, APT sources and public keys, OTA trust roots, kernel EDID defaults, and
base VQE configuration also remain platform-owned.
`/etc/bluetooth/main.conf` is already owned by Debian's `bluez` package and is not taken
over directly; vendor configuration should preferably use Aiden drop-ins supported by
the service. A file being under `/etc` does not make it eligible for takeover.

Changes to the boundary rules themselves or to out-of-scope files remain system changes
and require OTA. In-scope changes that actually depend on new libraries, drivers, startup
prerequisites, or an ABI also require `force_ota`. Automatic path classification cannot
prove semantic compatibility.

## Installation, configuration, and activation

Debian 13's canonical locale path is `/etc/locale.conf`. The business package owns only
this regular conffile and preserves the system-created `/etc/default/locale` symlink,
avoiding `.dpkg-new` files left by dpkg on an existing symlink. Configuration contents,
permissions, and ownership are checked before rootfs compression and image packaging,
and again during the final image audit.

Files are installed as real dpkg payloads; hooks do not overwrite system files with `cp`.
The package's `/usr/lib/aiden/runtime-config.json` records paths, permissions, hashes,
and activation methods:

- Files are owned by root: ordinary configuration uses `0644`, executable helpers `0755`,
  and sudoers files `0440`.
- Files under `/etc` are registered as conffiles. When both upstream and local copies
  change, dpkg handles them according to the interactive choice. Unattended installations
  can explicitly use `--force-confold`, but some new settings will then not replace local changes.
- Runtime state, credentials, and user settings under `/userdata` and `/run` are not packaged.
- Deleting a source file stops including it in new packages; dpkg may retain an obsolete
  conffile. To fully remove or rename one, add a reversible `dpkg-maintscript-helper`
  migration in that release and test upgrades and downgrades. Disappearing from the
  inventory must not be treated as permission to delete administrator configuration.

`preinst/prerm` records and stops previously running business services, and records the
actual configuration hashes and permissions before installation. `postinst` validates
the actual sudo configuration, applies Aiden tmpfiles, runs daemon-reload, restores
previously running business services, and finally restores the proxy restart listener.
Previously stopped business services are not started automatically. Validation or startup
failures preserve recovery records; after fixing the issue, run `sudo dpkg --configure -a`
to continue. Offline rootfs builds skip online service stop/start operations and reboot markers.

In the rules, `live` means no device reboot is required: business configuration takes
effect when business services are restored, while terminal profiles and locale settings
take effect at the next login or in a new terminal. Other in-scope files require a later
reboot by default, including newly added configuration not yet reviewed as live. APT does
not automatically restart SSH, systemd-networkd, USB, or the device. The following markers
are written only when the installed contents or permissions of deferred-activation files
actually change:

```text
/run/reboot-required
/run/reboot-required.pkgs
/run/aiden-business-reboot-required.json   # Specific paths that require a reboot to take effect
```

Business-code-only upgrades, live-configuration-only changes, reinstalling identical
configuration, and dpkg retaining local configuration do not create unnecessary reboot
requirements. Markers remain until reboot and are not overwritten by later installations.
Replacing a script file does not immediately switch the code of an already running script
process. The default reboot policy avoids actively interrupting device connections during
remote installation.

## Publishing and initial ownership transition

The primary entry point checks `main` hourly and publishes changes to `dev`. All three
channels support manual releases; `staging` and `prod` are not published automatically.
The primary entry point and `build-backup.yml` use the same implementation.

| Changes since the channel's previous release | Artifacts | Contract |
| --- | --- | --- |
| Business or in-scope configuration additions/modifications/deletions | One `aiden-business` package | Inherited |
| Platform or boundary changes, `force_ota` | OTA images and matching business package | Incremented |
| Initial adoption of configuration ownership | OTA images and matching business package | Incremented once |
| Documentation/tests only, or no changes | No release | Unchanged |

The initial OTA transitions these configuration files from direct overlay copies to
business-package ownership and declares `runtime_config: 1` in the platform contract.
The business manifest and release record also record this version; older releases without
the field are read as `0` for compatibility. preinst rejects installing the new package
on an old platform without this declaration. Historical Releases are not rewritten.
The planner automatically allocates a new base contract; subsequent in-scope configuration
changes no longer increment it.

The publisher and APT index generator validate the package inventory, file contents/modes,
conffiles, channel, and contract. Successful publication refreshes the signed repository
on GitHub Pages. The index still has no `Valid-Until`, and the latest three published
versions are retained per channel/contract. Business updates modify only the active
rootfs; switching back to an old OTA slot restores that slot's own package and configuration.

```bash
sudo apt update && sudo apt upgrade
dpkg-query -W aiden-business
cat /run/aiden-business-reboot-required.json  # May not exist if no reboot is required

# Downgrade within the same contract (the example version must still be in the repository)
sudo apt install --allow-downgrades aiden-business=0.0.8-1
```

Inspect the actual scope locally without building:

```bash
python3 scripts/debian-package/system_config.py inventory /tmp/runtime-config.json
```

Linux validation: `bash scripts/test_system_config_package.sh` runs real dpkg upgrades
and downgrades, conffile preservation, failure recovery, and the production packaging
process in a disposable container. Its programs are test doubles, not releasable board
binaries. Build production packages with `scripts/debian-package/release.sh build` or
the three-channel workflow.
