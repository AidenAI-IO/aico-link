# Persistent Python Packages

## Overview

The board uses the firmware-provided `/usr/bin/python3` and pip. The Agent may
install a missing dependency through its existing Shell tool; there is no
package-install tool, virtual environment, lock file, or second Python runtime.

Runtime-installed packages use pip's standard user base:

```text
/userdata/agent/python/
├── bin/
└── lib/python<major>.<minor>/site-packages/
```

`PYTHONUSERBASE` is fixed at `/userdata/agent/python`. pip naturally separates
import packages by Python version under `lib/`; package CLIs share `bin/`.

## Firmware and Startup

The Debian production package set installs `/usr/bin/python3` and
`/usr/bin/pip3`. Runtime code must not upgrade the rootfs copies of pip,
setuptools, or wheel.

Before `aico-agent.service` starts, `aico-python-prepare` validates and
prepares:

| Path | Mode | Purpose |
| --- | --- | --- |
| `/userdata/agent/python` | `0755` | Persistent pip user base |
| `/userdata/tmp` | `01777` | Disposable pip download and extraction data |

The Go runtime validates the same paths before enabling the Shell temporary
directory override. Symlinks are rejected when used as either managed root.

## Environment

`/etc/profile.d/aico-python.sh` exports the shared Python environment:

```text
PYTHONUSERBASE=/userdata/agent/python
PIP_USER=1
PIP_NO_CACHE_DIR=1
PIP_DISABLE_PIP_VERSION_CHECK=1
PIP_BREAK_SYSTEM_PACKAGES=1
```

Login shells source this profile normally. The systemd unit sets the same fixed
values after loading `/run/aico/system.env`, so external environment settings
cannot redirect Agent package writes. `PIP_BREAK_SYSTEM_PACKAGES=1` only bypasses
Debian's externally-managed check; `PIP_USER=1` and `PYTHONUSERBASE` still keep
writes under `/userdata/agent/python`.

`TMPDIR` is not global. The Agent Shell adds only:

```text
TMPDIR=/userdata/tmp
```

This applies to foreground, background, and PTY commands and keeps pip work out
of the small `/tmp` tmpfs without redirecting temporary files for every service.

## Installing and Using Packages

The Agent prompt tells the model to prefer the standard library and installed
packages. When installation is necessary, it should use one exact top-level
version, require a wheel, and verify the resulting environment:

```bash
/usr/bin/python3 -m pip install --only-binary=:all: 'packaging==24.2'
/usr/bin/python3 -m pip check
```

`pip check` should use a bounded Shell timeout of at least 120 seconds. The
Agent should avoid installation while `/run/agent/storage_level` is not
`normal` or `/userdata` is visibly low on space.

Python imports use the managed user site automatically:

```bash
/usr/bin/python3 script.py
```

Package CLIs are not added to the global `PATH`. Add the user-base `bin`
directory only for the invocation that needs it:

```bash
PATH="$PATH:$PYTHONUSERBASE/bin" package-command
```

Direct pip installation is intentionally non-transactional. Failed installs may
leave partial package files; the Agent should inspect the error and report or
retry with another exact compatible version rather than loop indefinitely.

## StorageMonitor Cleanup

StorageMonitor registers the `python_userbase` cleaner.

Normal and manual force cleanup:

- Remove direct children of `/userdata/tmp` older than 24 hours.
- Remove stale symlinks themselves without following their targets.
- Preserve recent temporary entries because they may belong to a running pip
  command.
- Preserve everything under `/userdata/agent/python`.

Emergency cleanup runs only while the effective storage level is `emergency`:

- Clear `/userdata/agent/python` and recreate it as an empty `0755` directory.
- Continue using the 24-hour rule for `/userdata/tmp`.
- Remove installed third-party packages; imports and package CLIs remain
  unavailable until the packages are reinstalled.

Manual `force=true` at a lower storage level does not clear the user base.

Manual cleanup can target the cleaner through the existing API:

```json
{
  "force": false,
  "targets": ["python_userbase"]
}
```

## Reboot and OTA

Packages persist across reboot and A/B rootfs updates because they live on
`/userdata`. If an OTA keeps the same Python major/minor version, compatible
packages remain importable. A different Python version uses a different
`lib/python<major>.<minor>/site-packages` directory under the same user base;
old files remain until Emergency cleanup.

The shared `bin/` directory is not versioned. If a package CLI becomes
incompatible after OTA, reinstall that package before using the CLI.

## Limitations

- Downloaded packages execute with the Agent's privileges; exact versions and
  wheels do not make an untrusted package safe.
- The feature does not install system libraries or build source distributions.
- A user package can shadow a firmware Python package because both Agent and
  user shells share `PYTHONUSERBASE`.
- The MVP has no package lock, installation mutex, rollback, quota, manifest,
  or separate package database.
- Concurrent Agent and manual SSH pip operations are unsupported.

## Verification

Host coverage verifies environment inheritance, directory permissions, Shell
TMPDIR propagation, cleanup levels, symlink safety, and prompt guidance.

On a board, verify the complete path with:

1. `/usr/bin/python3 -m pip --version`.
2. Install an exact wheel through Agent Shell and run `pip check`.
3. Import the package with `/usr/bin/python3` and invoke any CLI with scoped
   `PATH`.
4. Restart the Agent and confirm the package remains available.
5. Confirm normal/force cleanup preserves packages and Emergency cleanup clears
   them while leaving an empty user-base directory ready for reinstall.

## Related Documentation

- [Firmware Build and Flashing](../01-getting-started/firmware.md)
- [StorageManager and StorageMonitor](storage-manager.md)
- [Agent Overview](overview.md)
