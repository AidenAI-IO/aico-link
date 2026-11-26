---
slug: /
sidebar_position: 0
title: Aico Link 硬件文档
---

# Aico Link 硬件文档

构建、配置、运行和扩展 **Aico Link** 硬件，以及在其上运行的 AI Agent **Aico**。
官方网站是 [aico.link](https://aico.link)。

## 从这里开始

- [使用真实手机运行本地 Agent](01-getting-started/local-agent-real-phone.md)：
  在 macOS/Linux 上通过 Android ADB 直接运行 Agent，或在 Mac 上通过 iPhone 镜像连接真实 iPhone，
  Aico Link 由软件模拟。
- [在 PC 上试用 Aico](01-getting-started/try-aico-on-pc.md)：在 Docker 沙箱中模拟 Aico Link，
  提供 Config Web、Agent Web 和可选的环境桥接控制，让你在硬件发货前试用 Aico。它不是官方产品。
- 按照 [新手快速上手](01-getting-started/quickstart.md) 使用 Aico Link 开发板。
- 在给自行组装的设备上电之前，先阅读 [硬件与接线](01-getting-started/hardware.md)。
- 搭建 [构建环境](01-getting-started/build.md)，然后 [部署到设备](01-getting-started/deployment.md)。
- 当服务或外设未按预期工作时，参阅 [故障排查](07-operations/troubleshooting.md)。

## 核心参考

- [产品名称与技术标识符](02-architecture/naming.md)：硬件和 Agent 名称、首选 SDK 标识符，
  以及稳定的技术接口。
- [架构](02-architecture/overview.md)：系统边界、启动服务、源码布局和运行时路径。
- [设备服务](03-services/agent.md)：Agent、配置、终端、帧采集、音频、BLE 和 USB HID 服务。
- [MCU 服务](03-services/mcu-service.md)：电源握手、唤醒长轮询、硬件 API 以及烧录后验证。
- [Agent](04-agent/overview.md)：运行时配置、工具、Skill、记忆、语音和手机集成。
- [Agent 文件报告](04-agent/user-files.md)：查看生成的记忆和 Skill 文件报告，
  以及它们之间的跨文件引用。
- [持久化 Python 软件包](04-agent/python-packages.md)：共享 pip 环境、存储布局和清理策略。
- [SDK 与工具](05-sdk-and-tools/cpp-sdk.md)：C++ SDK、示例和镜像工具。
- [协议](06-protocols/uds-protocol.md)：Unix 域套接字和服务协议约定。
- [Wi-Fi 监管域](02-architecture/wifi-regulatory-domain.md)：国家代码配置、
  运行时应用和回滚行为。
- [通过 USB 上网](07-operations/usb-internet.md)：Mac 和 Android 的互联网共享，
  由默认关闭的 Config Web 设置控制。
- [用户数据备份与恢复](07-operations/backup-restore.md)：支持的备份组件、
  兼容性规则和事务性恢复行为。
- [OTA](08-ota/README.md)：签名发布版本、A/B 更新、回滚、校验和自定义分发。

## 开发者工作流

- [Benchmark](09-benchmark/README.md)：在设备或模拟环境上运行并评估 Agent 任务。
- [SkillOpt](10-skillopt/README.md)：通过 rollout 和留出集验证改进 Agent Skill。
