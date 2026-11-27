# SCH V2.19 适配

原理图 `AIDEN_SCH_V2_19.pdf`（各页日期为 2026-11-23 和 2026-11-24）更换了 Aico Link SCH v1 的充电芯片和语音芯片，
并新增了一个扩展接口。本页列出与固件相关的变更，以及 RV1106 镜像和 GD32 MCU 镜像的处理方式。

编写本适配时没有可用的 SCH V2.19 开发板。下文内容均来自原理图、SGM41574 数据手册和 RV1106 pinctrl 表，
只有主机侧测试覆盖。见[尚未在硬件上验证](#尚未在硬件上验证)。

## 两个版本共用一份镜像

RV1106 镜像和 MCU 镜像都同时支持 SCH v1 和 SCH V2.19。

- **RV1106。** 两个版本用法不同的引脚，在另一个版本上都是悬空的，因此一份设备树可以描述两者。
  model 字符串保持 `Aico Link SCH v1`；用户态和镜像审计把它当作板系列名称使用。
- **MCU。** 固件在 I2C 总线上探测充电芯片：`0x6B` 处的 SGM41574 选择 V2.19 行为，`0x3F` 处的 MP2720 选择 v1 行为。
  在充电芯片应答之前，语音唤醒和外部唤醒输入会被忽略。MCU 调试日志用 `board:` 行报告识别结果。

## 影响固件的硬件变更

| 范围 | SCH v1 | SCH V2.19 | 处理方 |
| --- | --- | --- | --- |
| 充电芯片 | MP2720，I2C `0x3F` | SGM41574 升降压充电芯片（U71），I2C `0x6B`，5–20 V 输入，船运 FET U72 | MCU |
| 充电中断，MCU PA5 | 充电芯片 INT | 充电芯片 INT# 与 BQ27220 GPOUT 并接 | MCU（下降沿刷新逻辑不变） |
| 语音芯片 | ASRPRO | GX8002D（J9），3.3 V（U31）和 1.2 V 内核（U30）独立供电 | MCU |
| 语音唤醒，MCU PA0 | 高有效，空闲为低 | 经 R224 上拉到 MCU_3V3，低有效 | MCU |
| 语音内核供电使能，MCU PB0 | 未连接 | GX8002_CORE_PWR_EN | MCU |
| 语音串口，RV1106 管脚 91/90（UART4 M1） | ASRPRO 串口 | GX8002 UART0，经 R210/R173 | 不变（`/dev/ttyS4`） |
| 麦克风 | 板载麦克风，经 U25 缓冲后进入 codec MIC0；偏置来自 ASRPRO | 麦克风接在 FPC 连接器 J10 上，同样经 U25 缓冲后进入 codec MIC0；偏置和缓冲器供电来自 GX8002 的 3.3 V 电源轨。codec MIC1 和 MICBIAS 引到 J2-5/6/7 | 采集通路不变。它需要 MCU PA1 保持开启，而只要 RV1106 上电该条件就成立 |
| RV1106 管脚 87，GPIO1_D1 | RV_HOLD，连到 MCU PA8 | 未连接 | 设备树保留该输出；无实际作用 |
| MCU PA8 | RV_HOLD | 来自 J2-18 的 EXT_WAKE_IN | MCU，唤醒源 7 |
| RV1106 管脚 63，GPIO0_A4 | 未连接 | 来自 MCU PA12 的深度睡眠唤醒线，R205 下拉 | 设备树 `mcu-wake` 按键，MCU 输出脉冲 |
| RV1106 管脚 64/65，GPIO0_A5/A6 | 排针上的 UART1 RTS/CTS | J2-13/14 上的 I2C1，R226/R227 4.7 kΩ 上拉 | 设备树启用 I2C1 M0 |
| RV1106 管脚 77/83/84，GPIO1_B0/C6/C7 | 未连接 | J2-15/16/17 上的 GPIO | 保留为未占用的 GPIO |
| RV1106 管脚 7/8，GPIO3_D1/D2 | 排针上的 I2C3 | 未连接 | I2C3 保持禁用 |
| Recovery | SARADC 引脚上的按键 SW2 | 无按键；调试模式下 J2-18 的 RECOVERY_3V3 经 Q8 把该引脚拉低 | 不变（`recovery` 按键，低有效） |
| VDD_ARM 稳压器 | SY8892 | JW5292（U36），反馈和 PWM 网络相同 | PWM 稳压器配置不变 |
| 扩展接口 | J17 | J2，21 针，带模拟开关 U70 | 见下文 |

以下器件有变更但不影响固件：Type-C 连接器、5–20 V 转 5 V 转换器（MP2328）、1.8 V、DDR 和 RK628 1.1 V 转换器（JW5292）、
24 MHz 晶振以及扬声器连接器。

## 扩展接口 J2

J2 的 13–18 脚经过模拟开关 U70（TS3A27518E）。MODE 引脚（J2-12，由 R228 下拉）选择这些引脚承载的信号。
RV1106 和 MCU 都无法读取或驱动 MODE。

| J2 引脚 | MODE 为低（默认） | MODE 为高（调试） |
| ---: | --- | --- |
| 13 | RV1106 I2C1 SCL（GPIO0_A5） | GX8002 UART0 RX |
| 14 | RV1106 I2C1 SDA（GPIO0_A6） | GX8002 UART0 TX |
| 15 | RV1106 GPIO1_B0 | GX8002_CORE_PWR_EN |
| 16 | RV1106 GPIO1_C6 | MCU SWDIO |
| 17 | RV1106 GPIO1_C7 | MCU SWCLK |
| 18 | EXT_WAKE_IN（MCU PA8） | RECOVERY_3V3 |

固定引脚：1 VCHG，2 和 21 GND，3/4 RV1106 调试串口 TX/RX，5/6 RV_MIC1 P/N，7 CODEC_MICBIAS，
8/9 RV1106 UART1 RX/TX，10/11 MCU 调试串口 RX/TX，12 MODE，19 触摸键，20 MCU_3V3。

在调试模式下，把 J2-18 拉高会使 RV1106 的 recovery 引脚保持为低。复位期间保持该状态会进入 Loader 模式；
运行时则上报为 `recovery` 按键。

## RV1106 设备树

`rv1106g-aico-custom.dts` 的变更：

- **`mcu-wake` 按键。** GPIO0_A4 是一个 `gpio-keys` 条目，键值 `KEY_WAKEUP`，高有效，带 `wakeup-source`，
  并启用内部下拉，使该引脚在 SCH v1（悬空）上保持为低。这条线是开发板的深度睡眠唤醒源：GPIO0 属于 PMU bank，
  在深度睡眠中保持供电。每个脉冲还会在 `gpio-keys` 输入设备上表现为一次 `KEY_WAKEUP` 按下和释放；没有程序使用这些事件。
- **I2C1。** 在 M0 引脚上以 100 kHz 启用（`/dev/i2c-1`），供接在 J2 上的设备使用。总线上没有描述任何设备。
  在 SCH v1 上这两个引脚位于排针且没有上拉，总线空闲。不得选择 I2C1 M1：它会占用蓝牙 UART0 的引脚。
- **注释**标明了各节点适用的版本。`rv-hold` 输出、语音芯片用的 UART4、MCU 用的 UART5，以及 RK628、TF 卡、
  Wi-Fi/蓝牙、音频和 CPU 稳压器节点均未改动。

GPIO1_B0、GPIO1_C6 和 GPIO1_C7 没有被任何节点占用，用户态可以申请使用。GPIO1_C6 和 GPIO1_C7 同时也是 UART4 M1 的
CTS/RTS 引脚；语音串口不使用流控，不得选择它们。

这些变更需要重新构建镜像：设备树属于 BSP 启动镜像，而不属于 Agent 软件包。

## MCU 固件

对应的修改位于 `aico-mcu` 仓库；其 `docs/application.md` 完整描述了相关行为。概括而言，在 V2.19 上：

- **充电电流。** SGM41574 上电默认的快充电流限制为 2 A。MCU 将其设为 700 mA（1400 mAh 电芯的 0.5 C），
  关闭充电芯片的 I2C 看门狗以免该限制被复位，并在每次电池刷新时重新检查这两项。电量百分比、电压和充电状态
  仍通过原有的 `GET_BATTERY` 应答送达 RV1106。
- **GX8002 供电。** MCU 按原 ASR 供电策略同时切换 3.3 V 供电和 1.2 V 内核供电：RV1106 上电时，或开关为 ON
  且处于软件关机状态时保持开启；开关拨到 OFF 后关闭。
- **GX8002 唤醒。** 供电建立至少 500 ms 之后，唤醒线上稳定 30 ms 的低电平上报为唤醒源 `asr`（6）。
- **EXT_WAKE_IN。** 稳定 30 ms 的高电平上报为唤醒源 `external_input`（7）。它与触摸键一样可以启动处于软件关机
  状态的 RV1106，并与其他所有唤醒源一样受自锁开关的控制。
- **深度睡眠唤醒线。** MCU 每上报一次唤醒、每发出一次关机请求，除发送 UART 帧外，都会把 GPIO0_A4 拉高 100 ms，
  与 RV1106 是否处于挂起状态无关。会让 RV1106 上电的唤醒，则在系统就绪后与其 `wakeup` 事件一同发出信号。
  由自锁开关引起的上电不属于唤醒，不产生脉冲。
- **脉冲之后再发帧。** RV1106 持有挂起许可期间，MCU 先发脉冲，并把 `wakeup` 事件或关机请求保留到 `aico-power`
  上报恢复（`suspend_resume`，内核返回后的第一个动作）之后再发，最长等待 2 s。因此处于深度睡眠的 RV1106
  由脉冲唤醒后，仍能收到带唤醒源的事件。正在运行的 RV1106 则同时收到脉冲和事件，没有额外延迟。

## RV1106 用户态

- `mcu_service` 把唤醒源 7 命名为 `external_input`。目前没有应用对其作出响应。旧版服务会把它上报为
  `unknown`，并带有 `source_code: 7`。
- 语音通路不变：无论由哪颗芯片产生，Agent 都从 `asr` 唤醒事件开始语音交互。
- `aico-power sleep`（s2idle）不变，仍通过 UART5 唤醒。`mcu-wake` 按键增加了第二条唤醒路径。
- 在 SCH V2.19 上，`aico-power sleep` 现在是在上报恢复之后收到 `wakeup` 事件，而不再由这一帧来唤醒系统。
- `aico-power deep-sleep` 保持禁用。除上板验证外，该命令还无法区分没有唤醒线的 SCH v1 和 SCH V2.19，
  因为两者报告相同的 model 字符串。

## 尚未在硬件上验证

请在第一块 V2.19 开发板上，使用由本次修改构建的 MCU 和 RV1106 镜像检查以下各项：

- MCU 日志显示 `board: SGM41574 charger, SCH V2.19 (GX8002)`，且 SCH v1 开发板仍显示 MP2720 那一行。
- 700 mA 限制下的充电电流、充电截止和温度行为；通过 `GET /api/mcu/battery` 读取的电池状态。
- GX8002 的启动、唤醒输出极性和脉冲宽度，以及 `asr` 事件
  （`scripts/test_mcu_after_flash.py --expect-source asr`）。
- MODE 为低时的 EXT_WAKE_IN
  （`scripts/test_mcu_after_flash.py --expect-source external_input`）。
- 唤醒线：`mcu-wake` 出现在 `/proc/interrupts` 中，并且每个 `wakeup` 事件计数一次，无论是否执行了 `aico-power sleep`。
- 从脉冲到上报恢复的时间与 MCU 2 s 等待时间的对比，s2idle 和深度睡眠都要测。
- 在获得挂起许可之后、内核真正睡眠之前到达的唤醒。它的事件现在会被保留，`aico-power` 在 MCU 状态检查中
  不再能看到它；这次挂起必须靠 `mcu-wake` 按键抬高的内核 wakeup count 来中止。
- 在 J2-13/14 上接入设备后的 `/dev/i2c-1`。
- 搭配 JW5292 时，多个 CPU 频率下的 VDD_ARM。
- 调试模式下通过 J2-18 进入 recovery。
- GX8002 固件下载及其在 `/dev/ttyS4` 上的串口协议，本次修改未实现这部分。
