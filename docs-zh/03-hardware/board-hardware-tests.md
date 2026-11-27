# 开发板硬件测试

`scripts/board/board_hw_test.sh` 在开发板上对新板或返修的 Aico 开发板执行硬件检查，并生成一份报告。
它与 `scripts/board/` 中的其他脚本互为补充：`board_low_level_smoke.sh`（服务、HID、HDMI 和音频冒烟测试）、
`speaker_test.sh`（可听测试音）、`mcu_touch_test.sh`（触摸按键）和 `board_jpeg_smoke.py`（JPEG 请求）。
RTC 测试 `scripts/board_rtc_test.sh` 在主机上运行，通过 USB 驱动开发板。

这些套件以 root 身份在开发板上运行。它们只使用本地服务 socket 和命令行工具：不经过 Agent、Config Web、
SSH 或 HTTP API。分析辅助程序 `board_hw_tools.py` 只使用 Python 标准库。

## 运行

所有开发板侧脚本都位于 `scripts/board/`。把该目录复制到开发板，并在附带终端的情况下（`ssh -t`）运行入口脚本，
以便交互式提示可以读取回答：

```bash
scp -r scripts/board aico@192.168.42.1:/tmp/
ssh -t aico@192.168.42.1 'sudo sh /tmp/board/board_hw_test.sh --interactive \
    --modes all --hotplug --sleep-test --charger --ble-connect \
    --iperf-server <lan-host>'
```

不加 `--interactive` 时，需要人工参与的步骤（说话、确认 LED、触摸按键、唤醒词、插入 TF 卡、插拔线缆和充电器）
会报告为 `[SKIP]`，因此无人值守的运行不会等待输入。如果交互式运行期间操作员的终端断开，本次运行会为丢失的输入报告一条
`[FAIL]`，并跳过其余交互步骤；未得到回答的确认永远不会被计为硬件结果。

入口脚本的常用选项：

| 选项 | 作用 |
| --- | --- |
| `--only audio,mcu` / `--skip hdmi` | 选择套件：`lowlevel`、`audio`、`hdmi`、`mcu`、`peripherals` |
| `--modes all` / `--modes 1080p30,1080p60` | HDMI：依次切换输入模式，然后恢复原来的选择（需要 root） |
| `--hotplug` | HDMI：拔出并重新插入线缆（需配合 `--interactive`） |
| `--sleep-test` | HDMI：在线缆保持连接时关闭并重新打开信号源显示（需配合 `--interactive`） |
| `--expect-size 1920x1080` | HDMI：要求的采集尺寸 |
| `--charger` | MCU：拔出并重新插入充电器（需配合 `--interactive`） |
| `--power-key` | MCU：短按电源开关（需配合 `--interactive`；仅当短按不会关闭开发板时使用） |
| `--iperf-server HOST[:PORT]` | 对局域网电脑上的 `iperf3 -s` 双向测试 Wi-Fi TCP 吞吐量（开发板上需要 `iperf3`） |
| `--download-url URL` | 用 curl 测试 Wi-Fi 下载速度，例如由局域网电脑提供的大文件 |
| `--ble-connect` / `--ble-pair` | BLE：等待手机连接；`--ble-pair` 会先打开配对窗口（需配合 `--interactive`） |
| `--config FILE` | 所有套件的设置，参见[配置与扩展](#配置与扩展) |
| `--artifact-dir DIR`、`--report PATH` | 采集数据、截图和报告的写入位置（默认 `/tmp`） |

每个套件也可以单独运行；`sh board_hw_<suite>.sh --help` 会列出其选项和限值。只有在没有任何检查失败时，退出状态才为 0。

### 配置与扩展

这些套件不包含任何站点相关的值。不带选项时，它们只测试开发板自身能验证的内容：检测到 Wi-Fi 接口，
并报告已加入的网络，但不要求特定网络。服务器、URL、期望的网络名称（`EXPECT_SSID`）和阈值写在设置文件中：
复制 `scripts/board/board_hw.conf.example`（其中列出了每个变量及其默认值），然后通过 `--config FILE`
（或 `AICO_HW_CONFIG=FILE`）传给入口脚本或单个套件。该文件是由 `VARIABLE=value` 行组成的 shell 文件；
优先级依次为内置默认值、设置文件、命令行选项。

新增测试无需修改现有脚本：

- **套件**：新增 `scripts/board/board_hw_<name>.sh`，并带有头部行
  `# aico-hw-suite: <order> <name> - <description>`。脚本 source `board_hw_common.sh`，调用
  `hw_start <name> "$@"`，设置默认值，调用 `hw_load_config`，解析自己的选项，最后以 `hw_finish` 结束。
  入口脚本根据该头部行列出并运行它，它也会得到相同的证据包。
- **外设检查组**：在 `board_hw_peripherals.sh` 中新增 `group_<name>()`，并把名称加入 `ALL_GROUPS`；
  `--only` 和 `--skip`（逗号分隔列表）用于选择检查组。

## 各套件检查内容

### 音频（`board_hw_audio.sh`）

| 检查 | 通过条件（默认） |
| --- | --- |
| ALSA 声卡、DAC/ADC 混音器控件 | 存在播放和录音声卡 |
| audio_service 健康状态、音量设置/读取 | 健康检查有响应；设置的音量能读回 |
| 麦克风通道 | 设备允许时采集 2 个通道，否则 1 个。audio_service 只使用左声道，因此保持恒定的右声道（该输入上没有麦克风）会被报告并排除在分析之外；左声道恒定或全为零则失败 |
| 本底噪声（安静房间，3 s） | 最安静的 100 ms 窗口在 -95 到 -45 dBFS 之间；无削波；直流偏移在 2% 以内 |
| 扬声器到麦克风，直接走 ALSA | 通过将各频率电平与测试音计划（300 Hz 到 6 kHz，-12 dBFS）进行匹配，在每个有效通道上找到该计划，这种方法能容忍直流偏移和启动时的爆音；500 Hz 到 4 kHz 的 SNR >= 15 dB；1 kHz 处 THD <= 10%；频响差 <= 20 dB；往返时延 <= 600 ms（近似值，因为 arecord 的启动时间不精确）；各通道相差在 6 dB 以内 |
| 经 audio_service 的同一回环 | 同上，但走产品播放路径并启用扬声器功放 |
| 音量曲线 | 在 audio_service 的每个音量档（30、60、90）上，1 kHz 电平都上升超过 1 dB |
| audio_service 录音 | 2 s 经过处理（VQE/DTLN）的音频，非空且不全为零 |
| 语音（交互式） | 语音比最安静窗口高出 15 dB；操作员确认回放清晰 |

回环测试在同一房间内用开发板扬声器播放、用开发板麦克风录音，因此请保持房间安静。ALSA 采集与 audio_service
并行运行；如果设备忙，请加上 `--stop-audio-service` 重新运行，它会在 ALSA 步骤期间停止该服务，并在退出时重启。
退出时会恢复原来的音量。

### HDMI（`board_hw_hdmi.sh`）

| 检查 | 通过条件 |
| --- | --- |
| 接收器驱动 | 本次启动中有 RK628 或 TC358743 的 probe 消息；无接收器/CIF 错误 |
| 设备与锁定 | 存在 `/dev/video0`；`media-ctl -p` 中有 HDMI 子设备；`v4l2-ctl --query-dv-timings` 返回非零有效尺寸；`/sys/kernel/debug/rk628/*/hdmirx/status` 中没有未锁定的通道 |
| frame_service | 状态为 `RUNNING`；无连续采集失败 |
| 采集速率 | 序列号每秒至少前进 10 帧（按需模式：每次请求得到一帧新画面） |
| 最新帧 | 不过期；尺寸非零；符合 `--expect-size` |
| 截图内容 | 黑色像素少于 98%；存在一定的亮度变化和颜色 |
| 信号源变化（交互式） | 信号源变化后第二张截图与之前不同；操作员确认图像 |
| 输入模式（`--modes`） | 对每种模式：`aico-hdmi-mode` 应用该模式；采集在 90 s 内锁定（`--settle-seconds`）；接收器时序与该模式（30 或 60）相差在 2 Hz 以内；1920x1080 帧；采集速率；截图内容。结束后恢复原来的选择，包括“没有已保存模式”的情况 |
| 热插拔（交互式，`--hotplug`） | 拔出后 30 s 内报告信号丢失；重新插入后 90 s 内恢复采集（超过 30 s 给出警告）；报告 MCU 插入检测事件 |
| 信号源休眠（交互式，`--sleep-test`） | 线缆保持连接、信号源显示关闭时，30 s 内报告信号丢失（采集停止、帧过期、帧龄超过 3 s 或没有接收器时序）；唤醒后恢复采集 |
| JPEG 通路 | `board_jpeg_smoke.py` 运行 3 次迭代 |
| 锁定稳定性 | 测试期间没有新的锁定失败内核日志（热插拔除外） |

运行前请连接一个处于唤醒状态的信号源。休眠中的主机可能保持显示器枚举但不发送视频；参见
[RK628F 硬件适配](rk628f-adaptation.md)。

### MCU（`board_hw_mcu.sh`）

| 检查 | 通过条件 |
| --- | --- |
| mcu_service | 单元处于 active 状态，socket 存在 |
| 链路 | `get_state`：UART 已连接，RV 状态为 `ready`，有协议版本，有最近的帧，无无效事件 |
| 电池 | 电量百分比 0-100；3000-4500 mV；各次采样间漂移小于 150 mV；充电状态为 `not_charging`、`charging` 或 `full` |
| 唤醒定时器 | 设为 3600 s，读回后取消。已有定时器处于活动状态时跳过 |
| LED | 红色和白色：亮、闪烁、灭均被接受并能读回（使用 `--interactive` 时由操作员确认），之后恢复为自动模式 |
| 触摸按键、唤醒词（交互式） | 30 s 内收到 `touch_key` 和 `asr` 唤醒事件 |
| 充电器（交互式，`--charger`） | 拔出和重新插入时收到 `external_power` 事件；充电状态随之变化 |
| 电源开关（交互式，`--power-key`） | 短按时收到 `power_switch` 事件。仅当短按不会关闭开发板时使用 |

### 外设（`board_hw_peripherals.sh`）

| 检查组 | 通过条件 |
| --- | --- |
| system | 无失败单元；无内核错误（警告）；温度区不高于 80 C；无 CPU 电压/稳压器错误 |
| wifi | 存在 Wi-Fi 接口（第一个无线接口，或 `--wifi-iface`）；已关联（指定 `--expect-ssid` 时关联到该网络）；RSSI >= -75 dBm（警告）；有 IPv4 地址；网关 ping 丢包 <= 20%，并给出平均 RTT 和抖动；`--dns-host` 的 DNS 解析（警告）；NTP（警告）；使用 `--internet` 时测试 HTTPS（任意 HTTP 状态，目标为 `--https-url`）；使用 `--iperf-server` 或 `--download-url` 时，测试绑定到 Wi-Fi 接口的吞吐量（低于 5 Mbit/s 时警告，`--min-wifi-mbps`） |
| bluetooth | 存在 `hci0`；ble_service 适配器已上电；GATT 已注册并在广播（警告）；使用 `--interactive --ble-connect` 时，手机（Aico App 或 BLE 扫描器）在 90 s 内连接，链路已配对且 GATT 服务可解析；使用 `--ble-pair` 时，配对窗口打开并保存新的绑定 |
| usb | 存在 `/dev/hidg0..2`；gadget 已绑定到 UDC；主机已完成配置；`usb0` 有地址 |
| storage | `/userdata` 可写；写入 32 MiB 并完整读回；写入 >= 5 MB/s、读取 >= 10 MB/s（警告）；无 MMC I/O 错误 |
| TF 卡 | 通过 `/sys/block/mmcblk*/device/type` 检测到 SD 卡；在其挂载点上写入并读回 4 MiB |
| npu | 存在 NPU、RGA 和 MPP 设备节点；`rknn_vad --self-test` 和 `ui_grounding_helper --self-test`（警告，因为 Agent 常驻的辅助程序可能占用 CMA） |
| rtc | `/dev/rtc0` 在走时，且与系统时钟相差在 5 s 以内。断电保持需要在主机上运行 `board_rtc_test.sh` |

`--only` 和 `--skip` 接受逗号分隔的检查组。`--ble-pair` 会新增一个已绑定设备，而 `--modes`
在运行期间会改变已保存的 HDMI 选择（结束后会恢复），因此两者都需要显式启用。没有风扇检查：当前开发板上没有风扇控制。

## 限值与校准

默认值只是起点，而不是产品规格。在把它们用作验收限值之前，请在预定的测试环境中对多块已知良好的开发板运行这些套件，
然后收紧或放宽选项（`--max-noise-dbfs`、`--min-snr-db`、`--min-fps`、`--min-rssi`、`--max-temp-c`；
回环和统计限值是 `board_hw_tools.py` 的选项）。把报告中的测量值保留下来，作为后续开发板的基线。

## 运行证据

每次运行都会写入一个证据目录 `<artifact-dir>/aico_hw_<run-id>/`（默认在 `/tmp`），并在旁边生成它的 tar 包。
该目录每次都会新建，且只对运行测试的用户可见；运行绝不会复用已有目录，因此每次运行都请传入新的 `--run-id`
（如果默认的时间戳 id 已被占用，会追加进程 id）。它们共同证明测试在这块开发板上运行过、运行时间、使用了哪些脚本以及测得了什么：

| 文件 | 内容 |
| --- | --- |
| `failures.txt` | 运行失败时从这里开始看：先列出所有 FAIL，再列出所有 WARN，包括 UTC 时间、开机后秒数、套件和小节、消息，以及检查在其下打印的详细行（接收器状态、错误回复）。`run.json` 的 `failures` 中也有同样的列表 |
| `<suite>.log` | 每个套件的完整控制台记录（低层冒烟测试为 `lowlevel.log`，入口脚本为 `all.log`），头部给出开始时间、运行时长、boot id、主机、操作员和命令行，尾部给出结束时间和退出码 |
| `events.jsonl` | 每个小节、检查结果、提示和操作员回答各占一行 JSON，在发生时实时记录，包含 UTC 时间、开机后秒数及所属小节 |
| `run.json` | Run id、开始和结束时间、退出码、开发板身份（boot id、序列号、MAC 地址、型号、内核、`/etc/aico-release`、已安装的 `aico*` 软件包、OS 版本、运行时长、操作员）、运行结束时的时钟状态（NTP 是否启用并同步、时区、RTC 时间及其与系统时钟的偏差，用于判断时间戳是否可信）、各套件的计数、时间跨度和退出码、失败项、每个脚本的 SHA-256，以及每个产物的大小和 SHA-256 |
| `kernel.log` | 自本次运行开始以来记录的内核消息（接收器锁定、USB、音频、MMC）；不包含更早的消息。没有 `journalctl` 时，使用时间戳晚于开始时间的 `dmesg` 行 |
| `*.pcm`、`*.json`、`*.bmp` | 麦克风采集数据（16 位小端，采样率和通道数以报告为准）、回环测量结果和截图 |
| `report.txt` | 入口脚本的报告 |
| `SHA256SUMS` | 目录中每个文件的 SHA-256 |

控制台最后几行会打印 run id、开发板序列号和 boot id、每个套件的结果和时间跨度、时钟状态、每个失败项及其时间和小节
（详情见 `failures.txt`），以及 tar 包路径及其 SHA-256。请保留该 tar 包，或至少将其 SHA-256 与开发板序列号一起记录，
作为测试记录；在解压后的目录中运行 `sha256sum -c SHA256SUMS` 可以证明之后没有任何改动。把采集数据复制到主机上即可收听，
把截图复制到主机上即可查看。

## 主机测试

`scripts/test_board_hw_tests.sh`（主机侧）在 `script-contracts` 套件中运行。它检查 shell 语法、帮助文本和仅限本地的策略，
运行分析自测、通过伪造的 `aplay`/`arecord` 进行回环测试（正常、麦克风失效和单通道失效三种情况）、截图分析，
针对伪造的 mcu_service socket 运行 MCU 套件，使用伪造的 `frame_service_cli`、`aico-hdmi-mode` 和 `v4l2-ctl`
测试 HDMI 输入模式切换，并针对伪造的 ble_service socket 测试带配对的 BLE 手机连接。
