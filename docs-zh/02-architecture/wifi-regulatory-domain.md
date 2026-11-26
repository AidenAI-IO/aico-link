---
sidebar_position: 6
---

# Wi-Fi 监管域

## 当前实现

Aico Link 使用 AIC8800 驱动的自管理监管域路径。`/userdata/debian/wifi/wpa_supplicant-wlan0.conf`
中持久化的 `country=` 值是权威的用户配置，但 `wpa_supplicant` 自身不会把该值应用到自管理的 PHY 上。

因此，Agent 通过 AIC8800 的 nl80211 厂商命令应用国家代码：

1. 迁移服务创建默认的 `country=CN` 配置。
2. Wi-Fi 驱动服务加载驱动。
3. Agent 将持久化的国家代码应用到每个 Wi-Fi PHY。
4. 在 PHY 国家代码应用之后，`wpa_supplicant` 才启动。
5. 运行时变更会更新候选配置、将其应用到 PHY、在需要时重启 supplicant，并且只有在操作成功后才提交配置。

`00` 不会作为自动或全球通用的回退选项提供。在该驱动上，`00` 表示自管理的默认规则，国家选择 API 会拒绝它。

## 配置与 API

主要实现分布在以下位置：

| 领域 | 位置 |
| --- | --- |
| 配置加载和规范化 | `src/agent/internal/configweb/wifi.go` |
| 国家代码驱动集成 | `src/agent/internal/wifiregion/` |
| 驱动和服务启动 | `overlay-debian/usr/lib/aico/aico-wifi-driver` 及相关的 systemd 单元 |
| 配置 UI | `src/config_web/src/pages/WiFi.tsx` |
| 持久化的 Wi-Fi 配置 | `/userdata/debian/wifi/wpa_supplicant-wlan0.conf` |

区域相关的端点如下：

| 方法和端点 | 用途 |
| --- | --- |
| `GET /api/network/wifi/region` | 返回当前区域、来源、候选项和支持的选项。 |
| `POST /api/network/wifi/region/resolve` | 使用 `{"browser_timezone":"..."}` 解析候选项，不记录用户意图。 |
| `PUT /api/network/wifi/region` | 应用显式选择，例如 `{"country":"US"}`。 |

更新会将用户的显式选择应用到 PHY 和候选 supplicant 配置，然后返回一个 `region` 对象（包含所选的 `country`
及其 `source`）以及 Wi-Fi 状态。该 API 不保证响应会单独返回持久化的配置或 sidecar 内容。
sidecar 持久化只是尽力而为的元数据；如果 sidecar 无法写入，配置文件仍然是权威来源。

配置文件始终是权威来源。如果 sidecar 与配置文件不一致，sidecar 会被丢弃，并根据配置文件重新生成。

## 失败处理

应用新区域是事务性的。如果驱动应用、supplicant 启动或首次连接尝试失败，服务会尝试恢复之前的 PHY 国家代码和持久化配置。
客户端必须检查返回的 `wifi_rollback.ok`、`disk_error` 和 `radio_error` 字段，而不能假定回滚已成功。
临时候选文件会在可能的情况下被删除。

不能利用连接请求隐式地更改区域。如果连接请求中的国家与已解析的区域不同，服务器会拒绝该请求，并要求客户端先调用区域端点。

厂商命令返回后，PHY 状态可能需要一小段时间才能被观察到。读取方和测试应轮询最多三秒，而不要假定能立即读回。

## 验证范围

该实现已针对以下情形完成验证：

- 不重启的情况下 CN → US → CN 切换；
- 完整重启后的持久化，并且 PHY 国家代码在 `wpa_supplicant` 启动之前应用；
- supplicant 候选配置被拒绝后的回滚；
- 初次连接失败后的回滚；
- 元数据 sidecar 过期时的恢复；以及
- 每次变更后读回 PHY 国家代码和信道规则。

发布之前，必须把对外提供的国家列表缩小到产品已认证的区域。当前实现中的表格只是驱动可识别国家的上限。
已连接网络的回归测试还必须覆盖在线更改区域后的关联和 DHCP。

验收时，请检查每个 Wi-Fi PHY 上的国家代码，而不仅仅是全局监管域的值；验证 CN 和 US 预期的信道和发射功率限制；
确认启动时在 `wpa_supplicant` 之前应用国家代码；并在已连接的网络上测试关联和 DHCP。
