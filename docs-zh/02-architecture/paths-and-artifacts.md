---
sidebar_position: 4
---

# 路径、产物与配置速查

## 构建产物

| 路径 | 说明 |
| --- | --- |
| `build-host/bin/aico_tests` | 主机原生 C++ 测试二进制文件 |
| `output/debian-apps/apps/bin/` | 交叉编译的应用程序和诊断二进制文件 |
| `output/debian-apps/apps/lib/` | 暂存到 rootfs platform/lib 的运行时库 |
| `output/debian-apps/apps-audit/` | ELF、依赖和白名单审计结果 |
| `output/debian-system/rootfs.ext4` | 可复现的 Debian armhf rootfs 镜像 |
| `output/debian-system/image/` | 经过审计的系统分区和出厂镜像 |
| `output/debian/image/update.img` | 最终可直接烧录的本地固件 |
| `output/debian/image/manifest.json` | 本地签名的 OTA 清单 |

## 重要源码路径

| 路径 | 说明 |
| --- | --- |
| `src/aico_sdk.h` | C++ SDK API |
| `src/frame_service_main.cpp` | Frame Service 入口 |
| `src/audio_service_main.cpp` | Audio Service 入口 |
| `src/agent/internal/configweb/` | Go Config Web 实现（由 `agent config-web` 提供服务） |
| `src/config_web/src/` | Config Web 的 React/TypeScript 应用、共享状态、API 客户端、样式和测试 |
| `src/config_web/dist/` | 构建后的 Config Web 静态资源（自动生成；不提交） |
| `src/system_env_*` | 严格的持久化环境到运行时环境的生成 |
| `src/agent/cmd/daemon` | Go Agent 守护进程 |
| `src/agent/cmd/ota` | OTA CLI |
| `src/agent/cmd/abctl` | A/B 元数据诊断工具 |
| `src/agent/internal/agent` | Agent 运行时和工具 |
| `src/agent/internal/ota` | OTA 下载、槽位、健康检查和状态机 |
| `overlay-debian/` | Debian rootfs overlay 和 systemd 集成 |
| `assets/business/` | 业务软件包的模型和通知音 |
| `overlay-debian/usr/lib/aico/platform/lib/` | 平台运行时库，包括由 `aico-platform-ldconfig` 注册的 VQE AEC/波束成形库 |
| `scripts/debian-apps/` | 应用程序交叉构建和审计 |
| `scripts/debian-system/` | rootfs、BSP、镜像组装和审计 |
| `tests/` | 主机原生 C++ 测试 |

## 设备默认路径

| 路径 | 说明 |
| --- | --- |
| `/usr/lib/aico/` | 打包的应用程序和由基础系统负责的服务辅助脚本 |
| `/usr/lib/aico/platform/lib/` | 厂商和应用程序运行时库 |
| `/usr/lib/aico/models/` | VAD、轮次检测、降噪和 UI grounding 模型 |
| `/usr/share/aico/config-web/` | 由 `agent config-web` 提供的 Config Web 静态资源 |
| `/userdata/agent/agent.toml` | Agent 配置 |
| `/userdata/agent/python/` | 持久化的 pip userbase |
| `/userdata/agent/skills/` | Agent Skill |
| `/userdata/agent/memory/` | Agent 记忆 |
| `/userdata/agent/log/` | Agent 和 LLM HTTP 日志 |
| `/userdata/system/env` | 持久化的设备环境变量来源 |
| `/userdata/system/wifi-proxies.json` | 为每个已保存的 Wi-Fi SSID 选择的模式和可选的上游代理 URL |
| `/run/aico/system.env` | 经过校验的运行时环境变量 |
| `/userdata/debian/wifi/wpa_supplicant-wlan0.conf` | Wi-Fi 配置 |
| `/userdata/debian/ota/config.json` | Debian OTA 仓库和出厂基线 |
| `/userdata/ota/` | 专用的 OTA 状态和下载分区 |
| `/usr/share/keyrings/aico-ota.pem` | OTA 清单的 Ed25519 公钥 |
| `/run/frame_service/frame_service.sock` | Frame 服务套接字 |
| `/run/audio_service/audio_service.sock` | 音频服务套接字 |
| `/run/ble_service/ble_service.sock` | BLE 服务套接字 |
| `/run/mcu_service/mcu_service.sock` | MCU 服务套接字 |
| `/run/aico/agent-mcu-wakeup.json` | Agent 的 ASR 事件游标，在同一次启动内跨 Agent 重启保留 |
| `/run/aico-mcu-poweroff/intent.json` | 本次启动的关机意图，保留到最终关机 |
| `/run/aico/storage.state` | Config Web 和 Agent 共享的 StorageManager 状态 |
| `/run/agent/storage_level` | 当前的 StorageMonitor 级别 |

`/run/*_service/` 下的服务套接字权限为 `0660 root:aico`。音频和 frame 单元声明了 `Group=aico` 和
`SupplementaryGroups=audio video`，因此普通的 `aico` 用户可以对它们运行诊断 CLI。如果套接字显示为
`0755 root:root`，或者客户端得到 `TRANSPORT_ERROR`，说明单元丢失了这些组设置，或者进程在意外的 umask 下运行。

`root` 和 `aico` 的登录 shell 都会通过 `/etc/profile.d/aico-path.sh` 把 `/usr/lib/aico` 追加到 `PATH`。
`/etc/sudoers.d/20-aico-path` 也把它加入 sudo 的 `secure_path`，因此已安装的工具可以直接按名称调用，
包括 `sudo ota status`。安装此配置后，请重新连接 SSH 或 Web 终端；已有的 shell 可以通过
`. /etc/profile.d/aico-path.sh` 加载它。子 shell 会继承导出的路径。

## 配置来源

| 文件 | 说明 |
| --- | --- |
| `overlay-debian/etc/aico_boot.conf` | 产品功能开关 |
| `overlay-debian/etc/aico_frame_service.conf` | Frame 启动和采集策略 |
| `overlay-debian/etc/aico_audio_service.conf` | 音频套接字和音量状态 |
| `overlay-debian/etc/aico_ble_service.conf` | BLE 服务参数 |
| `overlay-debian/etc/systemd/system/` | Debian 服务和挂载单元 |
| `overlay-debian/etc/systemd/network/` | systemd-networkd 配置 |
| `overlay-debian/etc/profile.d/aico-python.sh` | 固定的持久化 Python userbase |
| `assets/business/models/` | 业务 VAD 模型 |
| `assets/business/audio/` | 业务通知音频 |
| `overlay-debian/usr/share/aico/audio/config_aivqe.json` | 平台 VQE 配置 |
| `overlay-debian/etc/ld.so.conf.d/aico-platform.conf` | 从当前活动槽位注册平台库 |
| `AGENT_CONFIG_PATH` | 镜像组装所需的外部 Agent 配置 |
| `OTA_PUBLIC_KEY_PATH` | rootfs 和镜像组装所需的外部 OTA 校验密钥 |

## 常用命令

```bash
# Build and test (all compiler/test commands run in Docker)
make check             # Quick local feedback
make check-full        # Complete CI test gate
make check-production  # ARM production smoke
scripts/debian-apps/build-apps.sh all
./debian_build.sh

# Flash on Linux (after entering Loader/Maskrom)
FLASH_TOOL=pico-sdk/tools/linux/Linux_Upgrade_Tool/upgrade_tool
IMAGE=output/debian/image/update.img
scripts/flash.sh inspect --tool "${FLASH_TOOL}"
sudo scripts/flash.sh flash --tool "${FLASH_TOOL}" \
  --image "${IMAGE}" --sha256 "$(awk '{print $1}' "${IMAGE}.sha256")" \
  --confirm-erase-all-data

# Flash on macOS
./upgrade_tool/upgrade_tool uf ./output/debian/image/update.img

# Service status
systemctl status aico-frame.service --no-pager
systemctl status aico-audio.service --no-pager
systemctl status aico-wifi-proxy.service --no-pager
systemctl status aico-agent.service --no-pager
systemctl --failed

# Frame debugging
frame_service_cli --socket /run/frame_service/frame_service.sock health
frame_service_cli --socket /run/frame_service/frame_service.sock screenshot --out /tmp/screenshot.bmp

# Audio debugging
audio_service_cli --socket /run/audio_service/audio_service.sock health
audio_service_cli --socket /run/audio_service/audio_service.sock get-volume

# Config Web management API
curl http://<device-ip>/api/storage/status

# Agent runtime API
curl http://<device-ip>:8080/api/tools
curl http://<device-ip>:8080/api/storage/monitor/status
curl -X POST -H 'Content-Type: application/json' -d '{"force":false,"targets":[]}' http://<device-ip>:8080/api/storage/cleanup

# OTA
/usr/lib/aico/ota status
/usr/lib/aico/ota update
/usr/lib/aico/abctl read /dev/disk/by-partlabel/misc

# Logs
tail -f /userdata/agent/log/agent.log
journalctl -u aico-frame.service -u aico-agent.service
```

诊断 CLI 属于 apps 构建产物，一般不包含在业务软件包中；`frame_service_cli` 和 `audio_service_cli`
因 OTA 健康检查而被包含在内。进行有限的设备测试时，把所需的 CLI 复制到 `/userdata`。

## 持久化日志

| 路径 | 说明 |
| --- | --- |
| `/userdata/agent/log/agent.log` | Agent 监管程序和运行时输出；由 `aico-agent-log-retention.timer` 限制大小 |
| `/userdata/agent/log/llm-http-*.log` | 按会话分区的 LLM HTTP JSONL |
| `/var/log/frame_service/frame_service.log` | Frame 服务输出 |
| `/var/log/audio_service/audio_service.log` | 音频服务输出 |
| `/var/log/ble_service/ble_service.log` | BLE 服务输出 |
| `/var/log/ota/ota.log` | OTA 健康检查输出 |
| `/var/log/adb/adb-startup.log` | adb host 启动输出 |

当 StorageMonitor 报告 `critical` 或 `emergency` 时，Agent 运行时会根据 `storage_settings.storage.degraded_mode`
下配置的策略裁剪受管理的日志和 Python 临时数据。

## EDID 文件

开发用的 EDID 位于 `edid/`。Debian rootfs 会把生产用的 TC358743 EDID 安装到：

```text
/usr/share/aico/edid/hdmi_1080p30_cta.hex
```
