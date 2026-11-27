---
sidebar_position: 7
---

# 产品名称与技术标识符

**Aico Link** 是硬件平台。**Aico** 是运行在其上的 AI Agent。官方网站是 [aico.link](https://aico.link)。

开发板、固件、硬件服务、USB/蓝牙设备名称以及部署使用 Aico Link。Agent 的对话、工具、记忆、Skill
和语音行为使用 Aico。手机应用是 Aico 配套 App。

## 标准技术名称

产品命名适用于源码标识符、构建产物、已安装的服务、配置和遥测：

| 领域 | 名称 |
| --- | --- |
| C++ 头文件和日志 | `aico_sdk.h`、`aico_log.h`、`AICO_LOG_*` |
| C++ 命名空间 | `aico`、`aico_image`、`aico_vad`、`aico_demo` |
| 共享服务状态 | `AicoServiceStatus` |
| CMake 目标和归档 | `aico`、`aico_image`；`libaico.a`、`libaico_image.a` |
| Go 模块 | `aico-agent` |
| Python 软件包和 SkillOpt 后端 | `aico-benchmark`、`aico-skillopt`、`AicoDeviceBackend` |
| 设备服务和软件包 | `aico-agent.service`、`aico.target`、`aico-business` |
| 设备用户/组和运行时路径 | `aico`、`/usr/lib/aico`、`/usr/share/aico`、`/run/aico` |
| 环境变量和构建变量 | `AICO_*` |
| 内置工具 Skill | `config/skills/aico` |
| OTA 签名密钥路径和启动参数 | `/usr/share/keyrings/aico-ota.pem`、`aico.slot_suffix` |
| 备份归档魔数和 SD 文件系统卷标 | `AICOBKP1`、`AICO` |
| APT 发布元数据 | `Origin: Aico`、`Label: Aico business packages` |
| 遥测 trace 和标签 | `aico-episode`、`aico-hardware` |

文档中的命令和示例使用这些名称。仓库源码文件、服务单元、辅助脚本和基准测试套件路径使用对应的 `aico` 拼写。
本地检出可以使用任意目录名；克隆示例显式选择了 `aico-link-firmware`。

## 初始设置与集成

这是 Aico 的首个版本。请使用上述标准名称构建固件、配置设备并创建集成。初始设置使用完整的 Aico Link 固件镜像，
之后的版本则遵循常规的 OTA 或业务软件包更新流程。

C++ 使用方使用 Aico 的头文件、命名空间和归档。Go 导入、服务引用、环境变量、API 客户端、基准测试设置和遥测查询
使用本文记录的 `aico` 标识符。

设备账户是 `aico`，UID 为 1000。其主目录为 `/home/aico`，由 `/userdata/home/aico` 提供存储。
root 用户的持久化主目录仍为 `/userdata/userhome`。Agent 把用户配置保存在 `/userdata/agent` 中，
其内置的自我认知 Skill 使用 `aico`，受管理的 SD 卡数据位于卡上的 `aico` 目录下。
备份导出使用 `aico-backup` 格式和 `AICOBKP1` 魔数。

关于部署和更新，参见 [部署](../01-getting-started/deployment.md)、[OTA](../09-ota/README.md) 和
[Debian 业务软件包](../09-ota/debian-package.md)。

## 现有的外部名称

现有的 GitHub 组织、发布版本和子模块 URL 保持不变。固件仓库已从 `AidenAI-IO/aiden-firmware` 重命名为
[`AidenAI-IO/aico-link`](https://github.com/AidenAI-IO/aico-link)；请从新名称克隆，并使用位于
`https://aidenai-io.github.io/aico-link/apt` 的已签名 APT 仓库。重命名之前发布的版本在其不可变的
`release.json` 记录中保留旧名称，发布规划器会把两个名称视为同一个仓库。

`aiden-mcu`、`aiden-design` 和 `aiden-app` 等同级检出路径指的是那些现有仓库；本次固件改动不会重命名它们的检出或源码接口。

配套 App 是单独维护的。它当前已安装的系统标签和构建标识符（例如 `Aiden Ready` 和
`AidenLiveActivityExtension`）仍然是对该 App 的准确引用。
