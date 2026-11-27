---
sidebar_position: 7
---

# 测试与验证

## Docker 测试入口

主机只作为 Docker 客户端使用。编译器、测试运行器、Python 软件包、Node.js、Go 工具链和 ARM 交叉编译器
都由固定版本的 `docker/test/Dockerfile` 镜像提供。

本地迭代时，运行 **快速反馈** 配置：

```bash
make check
```

它会运行共享约定测试、C++ 主机测试（`aico_tests` 目标）、Go 格式检查/vet 和专项约定测试、Web 测试，
以及一小部分基准测试样例和 SkillOpt。它有意 **不** 运行全部 Go 测试、全部 879 个基准测试用例、
长时间运行的 shell/发布测试，以及 30 秒的 CTest 看门狗检查。它不能替代完整的测试门禁。合并之前，请运行：

```bash
make check-full
```

如果 PR 只改动 `docs/`、`docs-zh/` 或根目录的 `README.md`、`AGENTS.md`，CI 会跳过测试任务。
其他 PR 和推送到 `main` 的提交会运行 `make check-full`，包括 quick 配置中省略的测试套件。
`make test` 也是完整检查的别名。迭代时如果只想运行一个测试套件，使用
`bash scripts/run_tests_in_docker.sh --suite go-unit`（或 `bash scripts/run_tests_in_docker.sh --list` 中的其他名称）。
所有命令仍在同一个 Docker 测试镜像中运行。

需要主机 Docker 套接字的、基于 Docker 的软件包测试是显式的，并与常规清单分开，因为该套接字会授予对
Docker 守护进程的访问权限：

```bash
make check-docker
```

Linux CI runner 还会使用 `--docker-socket --host-network` 在测试镜像中运行 `docker-sandbox-smoke`。
这些选项是显式的，不属于本地的 `make check` 或 `make check-full`。

测试套件列表和快速命令位于 [`tests/test-manifest.yaml`](../../tests/test-manifest.yaml)。
被选中的必需测试套件如果缺少依赖或出现意外跳过，运行器就会失败。`output/test-summary.json`
中的 JSON 结果会记录所用配置以及实际执行的测试套件。

生产交叉构建冒烟测试使用内置的 `pico-sdk/` 源码，并需要固定版本的 RKNN 归档。
Docker 冒烟运行器会在尚未缓存时下载固定版本的 OpenCV-Mobile 源码并校验其校验和：

```bash
make check-production
```

测试覆盖 UDS 消息、Frame Service 协议、Audio Service 协议、Ring Buffer、Wi-Fi / Agent TOML 配置解析、图像处理等模块。

## Frame 服务验证

服务运行后：

```bash
frame_service_cli --socket /run/frame_service/frame_service.sock health
frame_service_cli --socket /run/frame_service/frame_service.sock screenshot --out /tmp/screenshot.bmp
frame_service_cli --socket /run/frame_service/frame_service.sock latest-frame --out /tmp/frame.raw
frame_service_cli --socket /run/frame_service/frame_service.sock list-frames
```

在开发机上临时运行服务时，默认套接字为 `/tmp/frame_service.sock`：

```bash
/usr/lib/aico/frame_service --socket /tmp/frame_service.sock
frame_service_cli --socket /tmp/frame_service.sock health
```

## 音频服务验证

```bash
audio_service_cli --socket /run/audio_service/audio_service.sock health
audio_service_cli --socket /run/audio_service/audio_service.sock get-volume
audio_service_cli --socket /run/audio_service/audio_service.sock set-volume --volume 80
```

录制为 PCM：

```bash
audio_service_cli --socket /run/audio_service/audio_service.sock record-stream --seconds 3 > /tmp/record.pcm
```

播放 PCM：

```bash
cat /tmp/record.pcm | audio_service_cli --socket /run/audio_service/audio_service.sock play-stream --rate 16000 --ch 1 --bits 16
```

## USB HID 验证

`example_usb_hid` 不包含在设备镜像中。先用
`scripts/debian-apps/build-apps.sh all` 构建，再临时复制到设备：

```bash
scp output/debian-apps/apps/bin/example_usb_hid root@<device-ip>:/userdata/example_usb_hid
```

然后在设备上按复制后的路径运行：

```bash
sudo /userdata/example_usb_hid setup composite
sudo /userdata/example_usb_hid keyboard tap ENTER
sudo /userdata/example_usb_hid keyboard text "hello from pico"
sudo /userdata/example_usb_hid touch click 16000 16000
```

测试完成后用 `sudo rm /userdata/example_usb_hid` 删除该副本。

## Agent 验证

在设备上检查服务：

```bash
systemctl status aico-agent.service --no-pager
```

Agent Web UI 在所有输入模式下都可用。打开：

```text
http://<device-ip>:8080
```

工具 API：

```bash
curl http://<device-ip>:8080/api/tools
curl -X POST http://<device-ip>:8080/api/tools/shell \
  -H 'Content-Type: application/json' \
  -d '{"input":{"command":"pwd"}}'
```

实时的 Anthropic 中转测试需要 `AICO_RELAY_BASE_URL` 和 `AICO_RELAY_AUTH_TOKEN`；
`AICO_RELAY_MODEL` 可选，用于覆盖其模型。它不使用 shell 中的 `ANTHROPIC_*` 变量，
因此开发工具所用的凭据不会自动启用该测试。视觉坐标评估使用相同的中转凭据，并需要 `AICO_VISUAL_LIVE=1`、
`AICO_VISUAL_MODEL`（要评估的模型）和 `AICO_VISUAL_RESULTS`（评估结果的输出目录）。
两项测试都以 Bearer 认证的方式发送中转 token。Anthropic 模型请求会发送 `User-Agent: Claude Code`。

## 音频模型往返脚本

`scripts/test_audio_roundtrip.sh` 用于通过 OpenRouter 音频模型生成音频，再把音频作为输入发回进行识别：

```bash
export OPENROUTER_KEY=sk-or-...
./scripts/test_audio_roundtrip.sh
```

可选设置代理：

```bash
export HTTP_PROXY=http://127.0.0.1:7890
export HTTPS_PROXY=http://127.0.0.1:7890
```
