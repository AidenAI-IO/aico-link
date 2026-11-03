---
slug: /
sidebar_position: 0
title: Aico Link Hardware Documentation
---

# Aico Link Hardware Documentation

Build, configure, operate, and extend **Aico Link** hardware and **Aico**, the
AI Agent that runs on it. The official website is [aico.link](https://aico.link).

## Start Here

- [Try Aico on PC](01-getting-started/try-aico-on-pc.md) with Config Web,
  Agent Web, and optional environment-bridge control without a development board.
- Follow the [Newcomer Quickstart](01-getting-started/quickstart.md) for a physical device.
- Review [Hardware & Wiring](01-getting-started/hardware.md) before powering a self-assembled device.
- Set up the [Build Environment](01-getting-started/build.md), then [deploy to the device](01-getting-started/deployment.md).
- Use [Troubleshooting](07-operations/troubleshooting.md) when a service or peripheral is not working as expected.

## Core Reference

- [Product Names and Technical Identifiers](02-architecture/naming.md): hardware and
  Agent names, preferred SDK identifiers, and stable technical interfaces.
- [Architecture](02-architecture/overview.md): system boundaries, boot services, source layout, and runtime paths.
- [Device Services](03-services/agent.md): Agent, configuration, terminal, frame capture, audio, BLE, and USB HID services.
- [MCU Service](03-services/mcu-service.md): power handshake, wake long polling, hardware APIs and post-flash validation.
- [Agent](04-agent/overview.md): runtime configuration, tools, skills, memory, voice, and phone integration.
- [Agent Files Report](04-agent/user-files.md): inspect generated memory and
  Skills file reports and their cross-file references.
- [Persistent Python Packages](04-agent/python-packages.md): shared pip environment, storage layout, and cleanup policy.
- [SDK & Tools](05-sdk-and-tools/cpp-sdk.md): C++ SDK, examples, and image utilities.
- [Protocols](06-protocols/uds-protocol.md): Unix domain socket and service protocol contracts.
- [Wi-Fi Regulatory Domain](02-architecture/wifi-regulatory-domain.md): country-code
  configuration, runtime application, and rollback behavior.
- [User Data Backup and Restore](07-operations/backup-restore.md): supported
  backup components, compatibility rules, and transactional restore behavior.
- [OTA](08-ota/README.md): signed releases, A/B updates, rollback, verification, and custom distribution.

## Developer Workflows

- [Benchmark](09-benchmark/README.md): run and evaluate Agent tasks against device or simulated environments.
- [SkillOpt](10-skillopt/README.md): improve Agent skills through rollout and held-out validation.
