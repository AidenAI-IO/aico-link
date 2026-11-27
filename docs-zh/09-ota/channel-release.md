# 三通道发布

所有生产发布都使用 **Aico Link Channel Release**（`.github/workflows/release.yml`），它可以手动
触发，也可以由主构建和备用构建入口调用。**Debian Build (scheduled / primary)** 每小时检查一次
`main`，并自动将运行时变更发布到 `dev`。`staging` 和 `prod` 仍需手动发布；回退工作流和独立的
业务软件包工作流只生成构建产物。

## 发布决策

每个通道都与自身上一次成功的发布比较，包括文件内容、路径、模式以及 Git 树中的 vendored SDK
文件。草稿、失败的构建和工作流产物不会推进基线。

| 比较结果 | 构建并发布 | 基础约定 |
| --- | --- | --- |
| 通道中的首次发布 | 完整 OTA、烧录镜像和配套的 `.deb` | 分配新约定 |
| 仅业务或范围内的配置变更（包括新文件） | `.deb` | 继承该通道最新 OTA 的约定和基础标识 |
| 系统变更或业务/系统混合变更 | 完整 OTA、烧录镜像和配套的 `.deb` | 分配新约定 |
| 仅文档/测试，或无变更 | 不构建也不发布 | 不变 |
| 选择了 `force_ota` | 完整 OTA，即使源码没有变更 | 分配新约定 |

规划器先根据 `scripts/debian-system/config-package.json` 将范围内的 overlay 文件归类为 `config`
（计入业务指纹），然后按 `scripts/release/policy.json` 依次匹配 ignore、system 和 business。
首次发生配置归属转移时会强制生成新的 OTA；之后新增的范围内文件不会扩展约定。参见
[由业务软件包管理的运行时配置](system-config-package.md)。`src/` 和 `assets/business/` 通常属于
业务；OTA 实现、Go 依赖声明、CMake 文件和出厂 Agent 配置属于系统。未列为业务的文件，包括范围外
的 overlay、SDK、内核、分区、基础依赖以及构建/发布脚本，默认都属于系统。不随产物发布的 Markdown 和
`.gitignore` 文件不影响发布。内置 Skill、config-web 资源、业务资源或内嵌 Agent Web UI 中的
Markdown 仍属于业务内容。`overlay-debian/` 下的 Markdown 和 `.gitignore` 遵循常规的 overlay 分类，
因为它们可能被复制到镜像或业务软件包中。发布说明是独立的资源，不属于镜像内容。

删除和重命名都参与比较。支持 squash 合并和 rebase：上一次发布的提交不必是候选提交的祖先，但
两者必须共享 Git 历史；选择上一次发布的更旧祖先仍会被拒绝。存在基线时，浅克隆会在检查共同祖先
之前，从 `origin` 获取上一次提交和候选提交的完整历史；获取失败或超过 300 秒超时都会中止规划。
完整克隆不会额外获取历史。分类和文件列表直接比较两个发布的 Git 树，而不是与共同祖先的差异。
因此，内容完全相同的 squash 不会触发发布，而系统内容的变更仍需要新的 OTA 约定。发布说明中的
Full comparison 链接同样使用两端点比较；提交摘要保留新历史中的提交信息。
GitHub 发布描述以通道和类型元数据开头，而不是重复发布标题，并且不再列出单个文件。OTA 发布会
添加形如 `Commit <sha> (and more) requires OTA update.` 的提示，其中 SHA 链接到在发布范围内从旧到
新找到的第一个系统变更提交；只有存在更多相关提交时才会出现 `(and more)`。该搜索不受 100 条提交
摘要的限制，会跳过已被还原的路径以及系统树与已发布基线一致的 squash/rebase 提交；对于强制 OTA、
首次基础发布，或无法归因到某个系统提交的配置归属迁移，则回退为源提交。详细的文件分类仍保留在
计划和 `release.json` 中。

外部 Debian 仓库、签名密钥、Actions secret 和浮动下载不在 Git 比较范围内。这些输入变化时请使用
`force_ota`；源码差异无法自动检测到它们。

## 版本与约定

发布标签形如 `dev-v0.0.2`、`staging-v0.0.3` 和 `prod-v0.0.4`。所有通道共享一个版本序列。默认在
已发布的最大版本之上递增 patch；第一个受管发布是 `0.0.2`，高于现有的本地软件包 `0.0.1-2`。也可以
手动输入更高的 `MAJOR.MINOR.PATCH`。已发布的软件包版本为 `${version}-1`；不再通过编辑
`version.sh` 来分配发布版本。

约定是正的 JSON 整数，例如 `"platform_contract": 1`，而不是版本字符串。它们同样全局递增：第一个
受管 OTA 是 `1`，之后是 `2`、`3`，依此类推。不同的基础在各通道之间永远不会共用同一个约定编号。
每个通道的首次发布都会构建自己的基础，因此把同一个提交发布到新通道时仍会生成 OTA 和新约定。

例如，如果 dev 的 OTA 约定是 `4`，它接下来的两个业务软件包仍依赖 `[4, 5)`，并绑定相同的
`base_release`、通道和系统源码指纹。在 dev 再发布一次系统 OTA 之后，后续的 dev 软件包使用新约定；
staging/prod 继续使用各自已发布的基础。

`AICO_PLATFORM_CONTRACT` 是十进制文本，例如 `1`；生成的 JSON 字段以及软件包兼容性边界 `min` /
`max_exclusive` 都是整数。旧的 `"1.0.0"` 格式会被拒绝。

`scripts/release/contract.py` 根据发布计划生成以下所有内容：

- rootfs 中的 `/usr/lib/aico/platform/contract.json`。
- `.deb` 中的 `release-manifest.json` 和安装前检查。
- `platform-contract.json` 发布资源。

安装受管业务软件包时，`preinst` 会在解包前检查平台、约定、通道、基础标签和源码指纹；不匹配时
中止，并要求先安装对应的 OTA。dpkg 现有的服务停止、恢复和失败回调逻辑保持不变。初始 rootfs 在
安装 `.deb` 之前就已获得约定，最终镜像审计会再次检查它。约定用于标识兼容性，不能替代签名。
`.deb` 包含范围内的配置和 systemd 单元，不包含平台库或分区镜像。平台约定、业务清单和发布记录
通过 `runtime_config: 1` 声明配置归属；没有该字段的旧记录视为 0。

## Actions 操作

将工作流合并到 GitHub 默认分支后：

1. 打开 **Aico Link Channel Release**，选择 channel 和 source_ref（默认 main；也接受提交 SHA）。
2. 保持 `plan_only=true`，在 Summary 和 `release-plan` 产物中检查计划、约定和变更列表。
3. 使用 `plan_only=false` 构建；设置 `publish=false` 则只生成可下载的验证产物。
4. 设置 `publish=true` 进行实际发布。发布任务通过该通道的 GitHub Environment 运行。

业务发布和 OTA 发布都会使用 `docker/dev/Dockerfile`，从计划中的源提交发布 Aico Link 容器镜像。
该镜像支持 `linux/amd64` 和 `linux/arm64`，并使用 GitHub 发布标签，例如
`ghcr.io/aidenai-io/aico-link:dev-v0.0.2`。你可以直接在 Docker 中使用 Actions 运行摘要里的镜像
引用，也可以在 Launcher 的 **New instance** 对话框中使用。如果 GHCR 软件包是私有的，拉取镜像前
请用具有读取权限的账号完成 Docker 认证。

镜像会在 GitHub 发布公开之前推送。镜像推送失败会阻止发布；如果之后的发布步骤失败，该镜像标签
可能已经存在于 GHCR 中。仅规划和仅构建的运行不会推送镜像。

主入口 **Debian Build (scheduled / primary)**（`build-scheduled.yml`）和备用入口
**Debian Build (backup / self-hosted-02)**（`build-backup.yml`）提供相同的 `channel`、`source_ref`、
`version`、`force_ota`、`plan_only` 和 `publish` 参数。OTA 分别固定在 `aico-hosted-01` 和
`aico-hosted-02` 上构建；业务软件包仍在 GitHub 托管的 Ubuntu 上构建。两个入口的手动运行默认值都是
`channel=dev`、`source_ref=main`、`plan_only=false`、`publish=true`、`dry_run=false` 和
`apt_only=false`，即构建并发布经过验证的产物。选择 `plan_only` 可检查计划；取消 `publish` 则只
生成产物。`dry_run=true` 优先级最高，只检查所选机器的环境、签名密钥和工具链，不做规划、构建或
发布。`apt_only=true` 只根据现有发布刷新签名的 APT 仓库，仍受 `dry_run` 优先级约束。

主入口恢复了 `17 * * * *` 定时计划，每小时在 UTC 第 17 分钟检查一次（北京/新加坡时间同样是
`:17`；实际启动可能因调度和排队而延迟）。定时运行使用 `main`、`dev`、`plan_only=false`、
`publish=true` 和 `force_ota=false`；手动输入的默认值不依赖定时事件。每次运行都在全局发布锁内直接
复用发布规划器，并与该通道上一次成功发布（包括 `dev` 预发布）的源码指纹比较。无变更、仅文档/
测试以及内容相同的 squash 合并会得到 `kind=none`，不构建也不发布。业务变更生成 `.deb`，系统变更
生成完整 OTA，构建失败不会推进基线；下一个小时会再次检查。

三个入口共享分类、约定分配、通道 Environment、草稿上传和下载校验。被调用的发布工作流持有同一个
全局发布锁。主入口和备用入口的 `primary-build` 和 `backup-build` 并发组不得重命名为该发布锁，
否则嵌套调用会互相等待。新的运行不会取消正在进行的构建。

仓库需要 `OTA_ED25519_PRIVATE_KEY`（Ed25519 PEM，仅 OTA 构建使用），`AGENT_CONFIG_TOML` 可选。
业务软件包构建两者都不需要。发布使用工作流的 `GITHUB_TOKEN`；只有发布任务拥有 `contents: write`
和 `packages: write`。主工作流和备用工作流中的调用任务也必须声明这些权限，才能把它们传递给被调用
的发布任务；规划和构建任务保持只读。仓库的 Actions 权限必须允许发布到 GitHub Releases 和 GHCR。
建议为 `staging` 和 `prod` Environment 配置审批人和允许发布的分支。

固件可以使用 `aico-hosted-01`、`aico-hosted-02` 或 `ubuntu-24.04`；业务软件包使用 GitHub 托管的
Ubuntu。固件复用 `build.yml` 中的 SDK 清理、工具链、缓存、签名和最终审计。传入的 source_ref 只
解析一次，之后所有步骤都固定使用该 SHA；该提交必须包含本发布工具。

规范的 runner 输入通过仓库变量 `AICO_RUNNER_PRIMARY` 和 `AICO_RUNNER_BACKUP` 解析。它们的默认值
仍是现有的 GitHub 注册名 `aiden-hosted-01` 和 `aiden-hosted-02`；当这些外部注册变化时请设置这两个
变量。Benchmark 任务使用同一个主 runner 变量。

所有通道共享一个工作流并发组，并且在发布之前会重新检查发布历史。GitHub 并发并不是无限队列：
每个组只保留一个运行中和一个等待中的任务，后来的等待任务可能替换之前的那个。不要并行启动多个
发布；新的任务不会取消正在运行的发布。

## 本地脚本

规划只需要 Git、Python 3.11+ 和已认证的 GitHub CLI。构建在 Linux amd64（例如 luhaodev）上运行，
需要 Docker 和现有的固件构建环境；macOS 可以检查计划和产物。

```bash
python3 scripts/release/release.py plan \
  --repo AidenAI-IO/aico-link --channel dev

# Clean working tree; HEAD must match source_commit in the plan.
# OTA builds also require the signing/trusted-public-key variables used by debian_build.sh.
python3 scripts/release/release.py build --plan output/release/plan.json
python3 scripts/release/release.py verify output/release/assets

# Run only when explicitly ready to publish externally.
python3 scripts/release/release.py publish output/release/assets
```

`plan --history history.json` 使用离线的发布记录数组进行预览/测试。空数组表示没有受管发布，并不
表示已读取在线历史。发布程序总是重新读取 GitHub，不接受离线历史作为发布依据。`--force-ota` 强制
刷新基础；`--version` 选择一个高于全局已发布最大值的版本。

发布目录包含 `.deb`、业务清单、平台约定、自动生成的发布说明、`release.json` 和 `SHA256SUMS`。
OTA 发布还包含签名清单、boot A/B、rootfs、压缩的烧录镜像、烧录镜像校验和文件以及 OTA 校验公钥。
userdata 不会作为单独的 OTA 分区发布。

## 发布完整性与失败重试

`release.json` 记录源 SHA、Git 树、通道、版本、上一次发布、平台基线、系统/业务指纹、变更列表
以及每个资源的大小/哈希。已发布的受管标签必须有有效的记录；读取失败会中止规划，而不会被误当作
首次发布。旧的 `debian-*`/`business-v*` 标签不是新流程的基线。不要删除或修改已发布的受管标签、
资源或记录：后续版本和约定分配都依赖它们。

发布程序会验证标签目标、创建草稿、上传、完整重新下载并检查所有文件，然后才公开发布。OTA 还会
校验 Ed25519 签名、镜像哈希以及固定到标签的下载 URL。发布期间若历史发生任何变化，都会中止公开
发布。已发布的版本不能被覆盖。上传失败后，请保留原始的 `channel-release-assets` 产物，并用相同
的文件重新运行 `publish`；构建内容不同的草稿不会被覆盖。草稿不是规划基线，因此之后的运行可能会
从更新的提交规划出相同的版本。当某个未公开版本的现有标签指向其他提交时，发布程序会删除该标签及
其草稿，并在计划的提交上重新创建，而不是阻塞该通道。只读的 GitHub 历史请求会在失败前重试。重新
构建会改变构建时间和签名，在续传同一草稿时无法替代原始资源。

dev/staging 是预发布。只有 prod OTA 可以更新 GitHub Latest；prod 业务软件包不会占用 Latest。新的
OTA 客户端从设备配置中读取 `repo` 和 `channel`，分页查找该通道中版本最高的 OTA，跳过业务软件包，
并校验签名清单中的通道。通道为空和旧配置继续使用旧的 Latest 入口；旧设备必须先用其通道的新基础
镜像完整重新烧录，才能获得通道选择功能。

发布后，签名的 GitHub Pages APT 仓库会自动刷新。设备运行 `apt update && apt upgrade` 即可升级其
通道和当前基础约定对应的业务软件包；系统更新继续通过 A/B OTA 进行。初始设置、签名密钥、系统
软件包 pin 和索引维护参见 [GitHub APT 仓库](apt-repository.md)。
