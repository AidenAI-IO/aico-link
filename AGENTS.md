# Contribution Guide

This repository contains the Aiden development-board firmware, C++ hardware
services and SDK, the Go Agent runtime, Debian and OTA tooling, web
applications, benchmarks, and their tests. Keep changes focused on the
component being modified and preserve the interfaces between these parts.

## Required workflow

The test commands in this section apply when the current task changes this
repository's source, tests, scripts, configuration, or documentation, or when
the user explicitly asks to test this repository. Work confined to sibling
repositories (for example, `aiden-mcu` or `aiden-design`) does not trigger
`aiden-firmware` checks. An edit limited to this `AGENTS.md` guidance does not
require a firmware test run.

1. Work from the repository root unless a component's documentation says
   otherwise.
2. After changes within this repository, run the local test suite:

   ```bash
   make check
   ```

   This is the default and only required check for local development, including
   cross-component, release, and packaging changes. It covers the shared
   contracts, host C++ tests, focused Go and web checks, and a small benchmark
   sample. Do not replace it with an unscoped local test command; the test
   image supplies the pinned toolchains and dependencies.
3. CI runs the full validation profile before merging. Run it locally only when
   explicitly requested or when investigating a full-suite failure:

   ```bash
   make check-full
   ```

   Use `bash scripts/run_tests_in_docker.sh --list` to discover focused suites.
   Run `make check-docker` only when a test explicitly needs access to the host
   Docker socket.

If the required Docker environment is unavailable, report that limitation
instead of silently treating the change as tested.

## Setup and repository map

Clone the repository with its submodules:

```bash
git clone --recursive <repository-url>
```

For an existing checkout, initialize them with
`git submodule update --init --recursive`. The `pico-sdk` and
`benchmark/mobilegym/vendor/mobilegym` directories are submodules; avoid
editing their contents unless the task explicitly targets those dependencies.

The main areas are:

- `src/`: C++ SDK and hardware services, the Go Agent under `src/agent/`, and
  web components.
- `tests/`: C++ host tests, contract tests, web tests, script tests, and the
  authoritative `test-manifest.yaml`.
- `scripts/`: Docker test runners, Debian and OTA tooling, and release checks.
- `overlay-debian/`: Debian rootfs files, systemd units, and device helpers.
- `benchmark/` and `skillopt/`: Python benchmark and SkillOpt projects.
- `docs/`: build, architecture, operations, and contributor documentation.

The normal host workflow requires Docker. The pinned test image supplies the
supported compiler and language toolchains; do not assume that host-installed
dependencies reproduce CI.

## Repository conventions

- Write all documentation, documentation updates, and user-facing technical
  explanations in English. Keep terminology and links consistent with the
  existing documentation under `docs/`.
- Prefer small, reviewable changes that follow the existing source of truth.
  Update tests and relevant documentation when behavior, configuration,
  protocols, deployment, or operator workflows change.
- C and C++ code follows the C11/C++11 settings in `CMakeLists.txt`. Preserve
  existing formatting and public headers/interfaces. Put host-native tests in
  `tests/` and use the existing CMake test structure.
- Go code lives under `src/agent/`; run formatting through the repository's
  test workflow and do not hand-edit generated dependency metadata without a
  reason.
- Keep hardware-specific work separate from host-only changes. Do not flash
  devices, overwrite userdata, or change release artifacts unless the task
  explicitly requires it.
- Do not commit credentials, API keys, device-specific secrets, local
  environment files, build output, or generated reports. Treat files under
  `keys/` and deployment configuration as sensitive.
- Avoid modifying vendored or submodule content unless the task specifically
  targets that dependency. Preserve submodule state when working on the main
  project.

## Common entry points

- `Makefile`: Docker-backed build and test entry points.
- `src/`: C++ SDK/services, the Go Agent, and web components.
- `tests/`: C++, contract, web, script, and manifest-driven tests.
- `scripts/run_tests_in_docker.sh`: test profiles and individual suites.
- `docs/`: English build, architecture, operations, and contributor
  documentation.
- `README.md`: project scope, setup, hardware, and contribution overview.

Read the relevant component documentation before making changes, especially
the build and testing guides in `docs/01-getting-started/`.
