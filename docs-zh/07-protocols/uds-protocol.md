---
sidebar_position: 1
---

# Unix 域套接字协议

本项目使用 Unix 域套接字连接 C++ 硬件服务与其他进程。该协议设计简单、跨语言，并能承载大块二进制数据。

## 消息信封

每条消息是一个完整的帧：

1. `uint32` 小端：JSON 头长度；
2. `uint64` 小端：二进制 payload 长度；
3. UTF-8 JSON 头字节；
4. 可选的二进制 payload 字节。

```text
+----------------------+-----------------------+----------------+------------------+
| header_len: uint32le | payload_len: uint64le | JSON header    | binary payload   |
+----------------------+-----------------------+----------------+------------------+
```

JSON 头描述请求或响应；大块二进制数据（如原始 HDMI 帧、PCM 数据块）放在 payload 中，以避免 base64 编码开销。

## C++ 实现

| 文件 | 说明 |
| --- | --- |
| `src/uds_message.*` | 信封读写 |
| `src/uds_client.*` | 单次请求/响应客户端 |
| `src/uds_server.*` | bind/listen/accept/线程生命周期，以及分发到服务处理函数 |
| `src/frame_ipc.*` | Frame Service 兼容封装 |

`ble_service` 在 `/run/ble_service/ble_service.sock` 上用 Go 实现了同样的信封。其
`status`、`wake`、`pairing_start`、`disconnect`、`pairing_forget`、`events_since` 以及
Android `notification_publish` 操作记录在 [BLE Service](../04-services/ble-service.md) 中。

`mcu_service` 使用同样的信封，不带二进制 payload，每个连接一个操作。其查询、修改以及可取消的
事件长轮询记录在 [MCU Service](../04-services/mcu-service.md) 中。

## 跨语言客户端

Go 或其他语言不需要 C++ ABI / cgo，只需：

1. 连接服务套接字；
2. 写入 12 字节的信封前缀；
3. 写入 JSON 头和可选的 payload；
4. 按同样格式读取响应。

## 大整数处理

部分字段可能超出 JavaScript 安全整数范围。跨语言客户端应与现有服务保持一致：必要时将大整数编码为 JSON 字符串。

## 状态值

各服务共享 `AicoServiceStatus`，常见值包括：

- `OK`
- `NO_NEW_FRAME`
- `FRAME_NOT_FOUND`
- `SESSION_NOT_FOUND`
- `SERVICE_RECOVERING`
- `TIMEOUT`
- `TRANSPORT_ERROR`
- `INTERNAL_ERROR`

具体业务语义参见各服务的协议文档。
