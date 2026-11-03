---
sidebar_position: 1
---

# Unix Domain Socket Protocol

This project uses Unix domain sockets to connect C++ hardware services with other processes. The protocol is designed to be simple, cross-language, and capable of carrying large binary data blocks.

## Message Envelope

Each message is a complete frame:

1. `uint32` little-endian: JSON header length;
2. `uint64` little-endian: binary payload length;
3. UTF-8 JSON header bytes;
4. Optional binary payload bytes.

```text
+----------------------+-----------------------+----------------+------------------+
| header_len: uint32le | payload_len: uint64le | JSON header    | binary payload   |
+----------------------+-----------------------+----------------+------------------+
```

JSON header describes the request or response; large binary data (such as raw HDMI frame, PCM chunk) goes in the payload to avoid base64 encoding overhead.

## C++ Implementation

| File | Description |
| --- | --- |
| `src/uds_message.*` | Envelope read/write |
| `src/uds_client.*` | Single request/response client |
| `src/uds_server.*` | bind/listen/accept/thread lifecycle and dispatch to service handler |
| `src/frame_ipc.*` | Frame service compatibility wrapper |

`ble_service` implements the same envelope in Go at
`/run/ble_service/ble_service.sock`. Its `status`, `wake`, `pairing_start`,
`disconnect`, `pairing_forget`, `events_since`, and Android
`notification_publish` operations are documented in
[BLE Service](../03-services/ble-service.md).

`mcu_service` uses the same envelope with no binary payload and one operation
per connection. Its queries, mutations and cancellable event long polling are
documented in [MCU Service](../03-services/mcu-service.md).

## Cross-Language Clients

Go or other languages don't need C++ ABI / cgo, just:

1. Connect to service socket;
2. Write 12-byte envelope prefix;
3. Write JSON header and optional payload;
4. Read response in the same format.

## Large Integer Handling

Some fields may exceed JavaScript safe integer range. Cross-language clients should remain consistent with existing services: encode large integers as JSON strings when necessary.

## Status Values

Services share `AicoServiceStatus`, common values include:

- `OK`
- `NO_NEW_FRAME`
- `FRAME_NOT_FOUND`
- `SESSION_NOT_FOUND`
- `SERVICE_RECOVERING`
- `TIMEOUT`
- `TRANSPORT_ERROR`
- `INTERNAL_ERROR`

See individual service protocols for specific business semantics.
