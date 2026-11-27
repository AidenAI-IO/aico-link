---
sidebar_position: 11
---

# Agent 文件报告

Agent 文件报告以浏览器可读的形式展示 Agent 的记忆文件和 Skill 文件。它会提取文件之间的引用关系，让你可以从一个文件跳转到它所引用的文件。

## 报告内容

生成器可识别以下引用标识符：

- `ep_...` 表示 episode；
- `proc_...` 表示 procedure；
- `app_...` 表示应用记录；
- `fail_...` 表示失败记录。

每个匹配的文件都会显示引用类型、标识符和解析后的路径。点击 **Jump** 会打开目标文件，将其滚动到可见区域并短暂高亮。

## 生成并查看报告

在设备上运行：

```bash
ssh aico@<DEVICE_IP>
sudo /usr/share/aico/agent-tools/view_agent_files.sh
```

`aico-business` 软件包会把生成器、模板和启动脚本安装到 `/usr/share/aico/agent-tools/` 下，无需单独部署工具。启动脚本接受一个可选的输出路径，其后可再跟一个 Agent 数据目录。Web 服务会使用其配置的 Agent 数据目录作为报告及其源文件的位置。

默认生成的报告为 `/userdata/agent/files_report.html`。你可以把它复制到本地机器：

```bash
scp aico@<DEVICE_IP>:/userdata/agent/files_report.html ~/Desktop/
open ~/Desktop/files_report.html
```

也可以通过 SSH 直接输出：

```bash
ssh aico@<DEVICE_IP> "cat /userdata/agent/files_report.html" > report.html
```

Agent Web 服务还提供：

- `GET /user_files`：查看当前报告；
- `GET /user_files?refresh=1`：重新生成报告并重定向到查看页面；
- `POST /user_files/regenerate`：启动异步重新生成。成功响应仅表示重新生成已开始，并不表示新报告已就绪。请在生成器完成后轮询或重新加载 `GET /user_files`。

这些路由由端口 8080 上的 Agent Web 端点提供。

## 本地实现

报告工具由以下文件实现：

- `scripts/generate_agent_files_report.py`；
- `scripts/agent_files_template.html`；
- `scripts/view_agent_files.sh`；
- `src/agent/internal/agent/user_files.go`。

源数据通常位于 `/userdata/agent/memory/` 和 `/userdata/agent/skills/` 下。报告只是一个生成的视图，文件变更后可以重新生成；它本身不是事实来源。
