---
sidebar_position: 5
---

# 构建与开发环境

## 克隆项目

```bash
git clone --recursive git@github.com:AidenAI-IO/aico-link.git aico-link-firmware
cd aico-link-firmware
```

BSP 直接内置在 `pico-sdk/` 中，普通克隆即可获得。`--recursive` 选项还会初始化 MobileGym 子模块。
对于已有的检出，可以用以下命令初始化或更新 MobileGym：

```bash
git submodule sync --recursive benchmark/mobilegym/vendor/mobilegym
git submodule update --init --recursive benchmark/mobilegym/vendor/mobilegym
```

## Docker 测试构建

主机只作为 Docker 客户端。构建和测试工具链运行在固定版本的 `docker/test/Dockerfile` 镜像中：

```bash
make check        # short local smoke/profile, Docker-only
make check-full   # all required suites; same gate as CI
```

`make check` 用于短周期的开发迭代；它不能替代完整的 CI 检查。测试套件的选择和命令位于
[`tests/test-manifest.yaml`](../../tests/test-manifest.yaml)。
完整的生产 ARM 冒烟验证使用内置的 SDK；Docker 冒烟镜像会在需要时准备固定版本的 OpenCV-Mobile 输入：

```bash
make check-production
```

quick 和 full 配置都会从内置的 `pico-sdk/` 源码编译并审计 TF 卡设备树。

完整的硬件目标依赖 Rockchip/Luckfox 的 armhf 库。要生成可在设备上运行的产物，请使用 Debian 交叉编译工作流。

Debian BSP 阶段还会针对所配置的内核重新构建厂商媒体模块。其兼容性辅助代码处理 `CONFIG_MEMCG`
所用的 `struct page` 布局，这是硬件 JPEG 编码所必需的。汇编和辅助代码会暂存在
`output/debian-system/media-compat/` 下；SDK 源码保持不变。请将重新构建的 `mpp_vcodec.ko`
和 `rockit.ko` 与匹配的内核一起部署。如果在较旧的平台上只更新应用程序包，
在这些模块更新之前 JPEG 会继续使用软件编码。

## Debian ARM 交叉编译

使用以下命令构建并审计 Debian armhf 应用程序包：

```bash
scripts/debian-apps/build-apps.sh all
```

支持 Git worktree。构建溯源信息会记录固件仓库的修订版本，其中包含内置的 `pico-sdk/` 源码。
SDK 的修改与其余固件源码一起跟踪。

该工作流会：

1. 构建固定版本的 Debian armhf 工具链容器和 opencv-mobile；
2. 使用 `cmake/toolchains/armhf-debian.cmake` 编译 C/C++ 程序；
3. 安装/使用 Go 1.26.0；
4. 为 `linux/arm GOARM=7` 交叉编译 Go Agent 和 BLE 守护进程；
5. 审计 `output/debian-apps/` 下的应用程序和共享库包。

应用阶段还会构建固定版本的 Ekho 9.0 源码，并打上美式英语 Flite SLT 女声补丁，
同时为 `aico-business` 暂存普通话数据。如果只想重新构建 Ekho，先运行
`scripts/debian-apps/build-apps.sh builder`，再运行 `scripts/debian-apps/build-apps.sh ekho`；
这一操作不需要 Go 或 OpenCV。`DEBIAN_APPS_EKHO_CACHE` 可以覆盖位于 `.cache/debian-apps/ekho`
的持久化源码/语音归档缓存。解压前会校验源码校验和。参见
[Ekho 构建输入](../../scripts/debian-apps/ekho/README.md)。

使用 `./debian_build.sh` 生成完整的已签名本地固件镜像集。它在一个工作流中构建应用程序、
RV1106 BSP、带业务软件包的 Debian rootfs、A/B 镜像以及本地 OTA 清单。

```bash
./debian_build.sh
```

rootfs 打包会保留 `overlay-debian/` 中的可执行权限。由 systemd 调用的辅助脚本在 Git 中要保持
可执行（`100755`）。必需的 `debian-systemd-overlay` 测试套件会检查这些权限，
镜像审计还会检查已安装的 Wi-Fi 日志保留辅助脚本，因为 Wi-Fi 驱动启动和 `wpa_supplicant`
都会通过 `ExecStartPre` 运行它。

如果只想构建并暂存业务 `.deb`（默认 `0.0.1-2`），请在 Linux amd64 上运行
`scripts/debian-package/release.sh build`。关于独立的 GitHub Release 发布和安装，参见
[Debian 业务软件包](../09-ota/debian-package.md)。

### Debian 归档镜像源

两个构建器镜像和设备 rootfs 都从 `http://mirrors.ustc.edu.cn/debian` 和 `/debian-security`
获取 Debian 软件包，这在 `scripts/debian-apps/debian.sources` 和
`scripts/debian-system/debian.sources` 中各声明一次（后者也会安装到设备 rootfs 中，
因此设备上的 `apt` 使用同一个镜像源；`container-build-rootfs.sh` 会从中读取 debootstrap URI）。
使用纯 http 是有意为之：apt 会根据 Debian 归档密钥环校验每个 InRelease 签名，
因此 https 对软件包完整性没有额外帮助，而且 http 使下载可以被 http 代理缓存。
从上海实测，`mirrors.aliyun.com` 会把 apt 和 debootstrap 这类 HTTP/1.1 客户端限速到约 300 kB/s
（只有其 HTTP/2 路径很快，基于 curl 的测速会显示这一点，而 apt 永远用不到），
而 ustc 为 apt 提供 5-10 MB/s 的速度。

这是一个实时镜像源，而不是 snapshot.debian.org 的时间戳快照：`trixie-security` 每天都在变化，
因此同一提交的两次构建可能得到不同的软件包版本。每次构建都会把它所使用的内容记录在
`output/debian-system/build-metadata.json`（`debian_mirror`、`debian_release_version`、
`debian_release_date`、`debian_updates_date`、`debian_security_date`）和 `packages.txt` 中。

### 可选的 CI APT 缓存

可复用的 GitHub Actions 工作流接受一个 `apt_cache_proxy` 输入；当该输入为空时，
使用仓库变量 `DEBIAN_SYSTEM_APT_CACHE_PROXY`。将其设置为 rootfs 容器可以访问的 HTTP 缓存代理 URL，
例如在 Docker 网桥网关为 `http://172.17.0.1:3128` 的 runner 上使用该地址。
默认不选择任何缓存，工作流也不会安装代理。

rootfs 构建器会通过候选代理下载镜像源的 `trixie` `InRelease`，校验其 Debian 归档签名并检查代号，
然后才选用该代理。探测有 20 秒的时间限制。缓存不可用或无效时，保留原有的下载设置。
已有的 `http_proxy`、`HTTP_PROXY`、`all_proxy` 或 `ALL_PROXY` 设置优先；HTTPS 代理设置保持不变。
这项验证发生在创建 rootfs 之前，如果已启用的缓存在之后失败，不会自动回退。

## macOS Apple Silicon + Colima

在 Apple Silicon 上，建议使用原生 `aarch64` 的 Colima 虚拟机，并通过 Docker 以
`--platform linux/amd64` 运行 Luckfox 镜像：

```bash
brew install docker docker-buildx colima
colima start --vm-type vz --vz-rosetta

docker buildx version
docker buildx ls

./debian_build.sh
```

不要为此工作流启动 `--arch x86_64` 的 Colima 虚拟机；保持原生虚拟机，让容器以 `linux/amd64` 运行。

## Makefile 快捷命令

```bash
make build          # Dockerized ARM production smoke build
make clean          # Remove generated build/
make check          # Quick Dockerized local feedback
make check-fast     # Alias for make check
make check-full     # All required suites, also run by CI
make test           # Alias for make check-full
make check-docker   # Docker-backed package contract tests
make check-production  # Dockerized ARM production smoke test
make test-clean     # Remove generated build-host/
```

`make check` 和 `make check-full` 都会构建 `docker/test/Dockerfile` 并执行
`tests/test-manifest.yaml`；两者只是测试套件配置不同。不需要主机编译器或主机语言运行时。
CI 始终使用 full 配置。

## 主要构建目标

| 目标 | 说明 |
| --- | --- |
| `libaico.a` | C++ 硬件 SDK 和服务公共库 |
| `libaico_image.a` | 图像处理库 |
| `frame_service` / `frame_service_cli` | HDMI 帧采集服务及 CLI |
| `audio_service` / `audio_service_cli` | 音频录制/播放服务及 CLI |
| `image_process` | 图像处理 CLI |
| `example_*` | 唤醒词、音频、摄像头、USB HID 示例 |
| `agent` | Go Agent 守护进程，包含运行时的 `config-web` 子命令；由应用交叉构建任务额外构建 |
| `ble_service` | Go BlueZ GATT/ANCS 守护进程，由应用交叉构建任务额外构建 |
