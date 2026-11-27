# 持久化 Python 软件包

## 概览

开发板使用固件提供的 `/usr/bin/python3` 和 pip。Agent 可以通过现有的 Shell 工具安装缺失的依赖；不存在软件包安装工具、虚拟环境、锁文件或第二个 Python 运行时。

运行时安装的软件包使用 pip 的标准 user base：

```text
/userdata/agent/python/
├── bin/
└── lib/python<major>.<minor>/site-packages/
```

`PYTHONUSERBASE` 固定为 `/userdata/agent/python`。pip 会自然地在 `lib/` 下按 Python 版本分隔可导入的软件包；软件包的命令行工具共用 `bin/`。

## 固件与启动

Debian 生产软件包集会安装 `/usr/bin/python3` 和 `/usr/bin/pip3`。运行时代码不得升级 rootfs 中的 pip、setuptools 或 wheel。

在 `aico-agent.service` 启动之前，`aico-python-prepare` 会校验并准备：

| 路径 | 权限 | 用途 |
| --- | --- | --- |
| `/userdata/agent/python` | `0755` | 持久化的 pip user base |
| `/userdata/tmp` | `01777` | 可丢弃的 pip 下载和解压数据 |

Go 运行时在启用 Shell 临时目录覆盖之前，也会校验相同的路径。任何一个受管根目录如果是符号链接，都会被拒绝。

## 环境

`/etc/profile.d/aico-python.sh` 导出共享的 Python 环境：

```text
PYTHONUSERBASE=/userdata/agent/python
PIP_USER=1
PIP_NO_CACHE_DIR=1
PIP_DISABLE_PIP_VERSION_CHECK=1
PIP_BREAK_SYSTEM_PACKAGES=1
```

登录 shell 会正常加载此 profile。systemd 单元在加载 `/run/aico/system.env` 之后设置同样的固定值，因此外部环境设置无法重定向 Agent 的软件包写入位置。`PIP_BREAK_SYSTEM_PACKAGES=1` 只会绕过 Debian 的 externally-managed 检查；`PIP_USER=1` 和 `PYTHONUSERBASE` 仍会把写入限定在 `/userdata/agent/python` 下。

`TMPDIR` 不是全局设置。Agent Shell 只添加：

```text
TMPDIR=/userdata/tmp
```

这适用于前台、后台和 PTY 命令，使 pip 的工作文件不占用容量很小的 `/tmp` tmpfs，同时又不会为每个服务都重定向临时文件。

一旦存在截图，Agent Shell 还会提供模型最近看到的那张截图的路径，以便命令能用标准库读取屏幕：

```text
AICO_SCREENSHOT_JPEG=<dir>/screen.jpg   # the bytes the model received
AICO_SCREENSHOT_PPM=<dir>/screen.ppm    # the same image as binary PPM (P6, RGB)
AICO_SCREENSHOT_WIDTH=<width>
AICO_SCREENSHOT_HEIGHT=<height>
```

`<dir>` 是一个随机命名的 `aico-agent-screenshot-*` 目录，由 Agent 在首次导出时创建于其系统临时目录（在控制器上为 `/tmp`，一个 tmpfs）中，因此导出永远不会写入闪存，两个 Agent 进程永远不会共用它，其他用户也无法预先创建它。由于截图可能显示隐私内容，该目录（`0700`）和文件（`0600`）只有 Agent 的用户可以读取。该目录在进程生命周期内保留。只有在命令或后台会话启动前捕获了更新的截图时，这些文件才会被重写，每个文件都通过一次 rename 完成。后台会话保留其启动时的变量，因此每张新截图都需要用一条新的一次性命令来读取。一旦截图捕获后端被撤销（HID 或设备配置发生变化），这些文件会立即被删除；Agent 关闭时目录也会被删除。其像素坐标是截图自身的坐标：图像已经裁剪到手机屏幕范围，因此在其中测得的点无需再换算到原始捕获帧中的偏移。传入坐标时使用坐标工具所声明的单位：在视觉坐标协议下按测得值传入，否则按模型拿到的宽度和高度归一化到 0-1000。

## 安装和使用软件包

Agent 提示词会告诉模型优先使用标准库和已安装的软件包。必须安装时，应使用一个精确的顶层版本，要求使用 wheel，并验证安装后的环境：

```bash
/usr/bin/python3 -m pip install --only-binary=:all: 'packaging==24.2'
/usr/bin/python3 -m pip check
```

`pip check` 应使用至少 120 秒的有界 Shell 超时。当 `/run/agent/storage_level` 不是 `normal`，或 `/userdata` 明显空间不足时，Agent 应避免安装。

Python 导入会自动使用受管的 user site：

```bash
/usr/bin/python3 script.py
```

软件包的命令行工具不会被添加到全局 `PATH`。只在需要的那次调用中添加 user base 的 `bin` 目录：

```bash
PATH="$PATH:$PYTHONUSERBASE/bin" package-command
```

直接使用 pip 安装有意设计为非事务性的。失败的安装可能会留下不完整的软件包文件；Agent 应检查错误，然后报告或改用另一个精确的兼容版本重试，而不是无限循环。

## StorageMonitor 清理

StorageMonitor 注册了 `python_userbase` 清理器。

普通清理和手动强制清理：

- 删除 `/userdata/tmp` 下超过 24 小时的直接子项。
- 删除过期的符号链接本身，而不跟随其目标。
- 保留较新的临时条目，因为它们可能属于正在运行的 pip 命令。
- 保留 `/userdata/agent/python` 下的所有内容。

紧急清理仅在实际存储级别为 `emergency` 时运行：

- 清空 `/userdata/agent/python`，并将其重新创建为权限 `0755` 的空目录。
- 对 `/userdata/tmp` 继续使用 24 小时规则。
- 删除已安装的第三方软件包；在重新安装这些软件包之前，相关导入和命令行工具都不可用。

在较低存储级别下手动执行 `force=true` 不会清空 user base。

可以通过现有 API 让手动清理只针对该清理器：

```json
{
  "force": false,
  "targets": ["python_userbase"]
}
```

## 重启与 OTA

软件包位于 `/userdata` 上，因此在重启和 A/B rootfs 更新后依然保留。如果 OTA 保持相同的 Python 主/次版本，兼容的软件包仍可导入。不同的 Python 版本会使用同一 user base 下不同的 `lib/python<major>.<minor>/site-packages` 目录；旧文件会一直保留到紧急清理为止。

共享的 `bin/` 目录不区分版本。如果某个软件包的命令行工具在 OTA 后变得不兼容，请在使用前重新安装该软件包。

## 限制

- 下载的软件包以 Agent 的权限执行；精确版本和 wheel 并不能让不可信的软件包变得安全。
- 该功能不安装系统库，也不构建源码分发包。
- 由于 Agent 和用户 shell 共享 `PYTHONUSERBASE`，用户软件包可能会遮蔽固件自带的 Python 软件包。
- 该 MVP 没有软件包锁、安装互斥锁、回滚、配额、清单或单独的软件包数据库。
- 不支持 Agent 与手动 SSH 同时执行 pip 操作。

## 验证

主机测试覆盖了环境继承、目录权限、Shell TMPDIR 传递、清理级别、符号链接安全以及提示词指引。

在开发板上，用以下步骤验证完整路径：

1. `/usr/bin/python3 -m pip --version`。
2. 通过 Agent Shell 安装一个精确版本的 wheel，并运行 `pip check`。
3. 用 `/usr/bin/python3` 导入该软件包，并在限定范围的 `PATH` 下调用其命令行工具。
4. 重启 Agent，确认该软件包仍然可用。
5. 确认普通/强制清理会保留软件包，而紧急清理会清除它们，并留下一个空的 user base 目录以便重新安装。

## 相关文档

- [固件构建与烧录](../01-getting-started/firmware.md)
- [StorageManager 与 StorageMonitor](storage-manager.md)
- [Agent 概览](overview.md)
