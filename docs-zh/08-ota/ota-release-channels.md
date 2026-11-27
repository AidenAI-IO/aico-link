---
sidebar_position: 9
---

# OTA 发布通道

`dev`、`staging` 和 `prod` 三个通道都可以在 **Aico Link Channel Release** 工作流中显式选择。
主构建工作流还会每小时检查一次 `main`，并将有变化的内容发布到固定的 `dev` 通道；`staging` 和
`prod` 仍需手动发布。分类、版本分配、约定、仓库设置和重试参见 [通道发布](channel-release.md)。

| 通道 | GitHub 发布类型 | 设备发现 |
| --- | --- | --- |
| dev | Prerelease | 最高的 dev OTA 版本 |
| staging | Prerelease | 最高的 staging OTA 版本 |
| prod | 正式发布 | 最高的 prod OTA 版本 |

每个通道都与自身上一次成功的发布比较。仅业务变更时发布 `.deb`；系统变更时发布签名的
boot/rootfs OTA 镜像，并建立新的平台约定。固件发现过程会跳过仅含软件包的发布。只有 prod OTA
发布才会更新 GitHub Latest。

`/userdata/debian/ota/config.json` 中的出厂配置包含 `repo` 和 `channel`。受管设备会查询发布
列表，选择所属通道中最高的 OTA 版本，并验证签名清单与配置的通道一致。该检查同样适用于显式的
`--manifest-url` 覆盖。

没有通道的旧设备，或使用旧的 local/stable 通道标签的设备，会继续使用 `/releases/latest`。烧录
某个通道的完整镜像即可配置新的基线及其通道配置。不要为了让软件包能够安装而编辑约定文件；它的
内容描述的是实际构建和安装的系统。

通过 `manifest_url` 仍可使用自定义的静态分发；请使用为该自定义源配置的测试设备。参见
[外部开发者指南](ota-external-developers.md)。
