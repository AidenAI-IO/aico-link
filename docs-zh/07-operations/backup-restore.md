---
sidebar_position: 1
---

# 用户数据备份与恢复

本文描述 Aico Link 固件所支持的备份与恢复约定。它是 Config Web、Bridge App WebView 和电脑端
CLI 的运维参考。详细的实现说明和历史设计方案有意不放在本文中。

## 范围

该功能在完整烧录固件前保护用户数据，并在新镜像首次启动后恢复这些数据。它涵盖：

- `/userdata`，包括 Agent 配置、记忆、会话、Skill、身份标识、音频以及持久化的用户主目录；
- `/mnt/sdcard` 下由 Aico Link 管理的 SD 卡数据；
- 备份清单中明确列出的配置和凭据；以及
- 完整性校验、事务式恢复、回滚和中断恢复的续做。

常规的 A/B OTA 升级会保留 userdata，不需要备份。在完整烧录 `update.img` 之前，必须把备份传到
设备之外。备份归档不得存放在被备份的 SD 卡上。

该功能不备份 Debian 根文件系统、OTA 下载状态、锁、swap、临时文件，或通过 SSH 修改的任意系统
文件。Loader 和 Maskrom 模式不提供备份 API。

## 现有的配置备份

现有路由仍然只针对配置：

| 操作 | 端点 | 格式 |
| --- | --- | --- |
| 导出 Agent 配置 | `GET /api/config/backup` | 分组 TOML |
| 导入 Agent 配置 | `PUT /api/config/backup` | 分组 TOML |

这些路由覆盖 `/userdata/agent/agent.toml`，不得改为接受二进制的用户数据归档。完整备份与恢复
使用独立的 `/api/backup/*` 和 `/api/restore/*` 路由，以免旧客户端把 TOML 与归档混淆。

## 设备 API

完整归档工作流通过以下路由提供：

| 方法和端点 | 用途 |
| --- | --- |
| `GET /api/backup/capabilities` | 报告支持的格式、存储、组件和传输限制。 |
| `POST /api/backup/jobs` | 创建自动的同设备备份任务。 |
| `GET /api/backup/jobs/{id}` | 读取任务状态和进度。 |
| `GET /api/backup/jobs/{id}/archive` | 以流式方式下载已完成的归档。 |
| `POST /api/restore/jobs` | 为上传的归档创建恢复任务。 |
| `GET /api/restore/jobs/{id}` | 读取恢复状态和进度。 |
| `PUT /api/restore/jobs/{id}/chunks/{index}` | 按顺序上传一个归档分块。当解析出的清单需要恢复计划时，上传会以 `restore_plan_required` 暂停。 |
| `POST /api/restore/jobs/{id}/plan` | 在上传剩余分块之前解析清单并创建恢复计划。成功响应报告 `uploading_and_staging`，并立即接受下一个分块；如果整个归档已上传完毕，则报告 `validating`。 |
| `POST /api/restore/jobs/{id}/validate` | 校验完整归档，包括兼容性和完整性。 |
| `POST /api/restore/jobs/{id}/apply` | 应用已校验的恢复事务。 |

完整工作流需要维护会话和同源授权。只要守卫放行，任何客户端都可获得该会话：通过 USB 链路，
或在开启 LAN 访问时通过 LAN（设置了设备密码时需已登录或已配对，参见
[登录与配对](../03-services/device-management-api.md#登录与配对)）。其他客户端会收到
`403 usb_required`。恢复可能替换 Wi-Fi 配置、LAN 访问状态和设备密码，因此通过 LAN 发起的恢复
可能会断开发起它的连接；如果开发板没有重新出现在 LAN 上，请通过 USB 恢复连接。恢复上传使用有
上限的分块（最大 4 MiB），服务端在创建恢复任务时报告协商后的分块大小。客户端必须依据返回的
任务状态和错误码判断，而不能假设任务被接受就已经完成。

## 备份组件

服务端会自动选择所有可用组件；UI 不提供组件勾选列表。没有 SD 卡时会跳过 SD 相关组件。

| 组件 | 数据 |
| --- | --- |
| `agent_config` | Agent、模型、设备和存储配置 |
| `system_environment` | API 密钥、token、密钥和代理变量 |
| `network` | Wi-Fi 和代理配置 |
| `ota_settings` | 用户 OTA 源、代理、凭据和已批准的公钥 |
| `agent_skills` | 用户 Skill 及 Skill 状态 |
| `agent_memory` | 长期、事件、通知和临时记忆 |
| `agent_sessions` | 对话、附件、元数据和工具产物 |
| `user_home` | 持久化的 `/root` 内容 |
| `preferences` | 本地播放偏好 |
| `device_identity` | 同设备模式下的机器、SSH 和蓝牙身份 |
| `audio_archive` | eMMC 音频归档 |
| `sd_managed_audio` | 由 Aico Link 管理的 SD 音频归档 |
| `sd_user_files` | SD 卡上 Aico Link 管理目录之外的用户文件 |
| `python_environment` | 动态安装的用户 Python 环境 |
| `diagnostics` | Agent 和系统诊断日志 |

`agent_memory` 和 `agent_sessions` 是原子组件：恢复时不得只替换索引、元数据目录或附件子目录。

## 身份与兼容性

新归档默认使用同设备模式。恢复过程会将归档中记录的硬件身份与目标设备比较；除非明确请求了受
支持的兼容模式，否则身份不匹配时拒绝恢复。分区布局、设备节点、锁路径和临时路径等不可变字段由
目标固件决定，这些字段永远不会从归档中恢复。

归档清单记录格式版本、固件约定、硬件身份、组件列表、大小和校验和。对于不支持的 schema、不兼容
的平台约定、缺失的组件或校验和失败，恢复必须在提交任何组件之前拒绝。

归档使用 `aico-backup` 标识和 `AICOBKP1` 头。受管理的 SD 音频存放在 `aico/audio`，恢复事务
使用 `.aico-restore`。备份时会从 `sd_user_files` 中排除这些受管理的根目录；恢复时会拒绝与之
重叠的用户文件，包括 FAT 大小写、末尾点号和短文件名别名。包含波浪号和数字后缀的顶层 DOS 8.3
名称被保留，因为归档无法确定它们对应的长文件名目标。创建归档前，请把此类用户文件移到其他目录。

## 事务模型

1. 获取全局备份/恢复锁并报告维护状态。
2. 在生成快照前停止或静默写入方，包括 Agent。
3. 在目标路径之外创建归档，或暂存上传的归档。
4. 校验归档头、清单、组件校验和、路径约束和可用空间。
5. 使用临时文件和目录、`fsync`、原子重命名以及父目录 `fsync` 应用组件。
6. 在完整组件通过校验之前保留旧文件。
7. 提交事务并重启受影响的服务。
8. 如果提交失败，尝试依据事务日志回滚。如果操作被中断，在服务读取受影响数据之前续做已记录的
   提交或回滚；不要假设每次中断都会自动恢复到最后一次已知良好的状态。

归档未加密，即使使用了压缩，其内容仍可被还原。按文件、按分块以及尾部的校验和提供的是完整性，
而非机密性。请把下载的归档视为敏感明文，只存放在有访问控制的位置。

## 客户端行为

Config Web 以引导式操作呈现完整备份与恢复。导出和导入仍是主要操作；Agent TOML 导入/导出仅
作为明确命名的高级操作提供。浏览器和 Bridge App 以有上限的分块传输归档，而不是在内存中缓冲
整个归档。

CLI 使用相同的设备 API，必须显示进度、校验失败、维护状态和最终事务结果。被取消的操作必须要么
完成当前的原子步骤，要么让设备能通过事务日志恢复。

## 实现状态

首个实现的设备端和 CLI 部分已可用。原生 Bridge App WebView 桥接仍是一项单独的集成任务。当
归档格式、组件集合或兼容规则发生变化时，请同时更新本文、清单版本和兼容性测试。
