---
sidebar_position: 6
---

# BLE Service: Pairing, Phone Notifications, and Background Wake

`ble_service` owns Aiden's Bluetooth Low Energy integration. It uses BlueZ on
`hci0` in two roles at the same time:

- BLE Peripheral: advertises an Aiden Wake service that the iOS companion app
  subscribes to.
- ANCS Consumer: subscribes to Apple's Notification Center Service exposed by
  the paired iPhone and normalizes system notification events.
- Android notification sink: accepts normalized notification changes forwarded
  by the companion app over the USB-restricted Agent HTTP API.

BLE is intentionally narrow. It does not carry Phone Bridge tool commands,
results, Live Activity text, or task metadata, and phone notification events
are not written to Agent memory. Phone Bridge commands continue to use
WebSocket or the existing HTTP queue. A Live Activity Wake prompts the iOS
native layer to read the newest task snapshot over USB ECM.

## Boot and Persistence

The Pico Zero kernel must merge both fragments:

```text
aiden-zram.config rv1106-bt.config
```

Runtime startup order is:

```text
aiden-wifi-driver.service -> aiden-bluetooth-attach.service -> bluetooth.service -> aiden-ble.service
```

`aiden-bluetooth-attach.service` loads the AES/CMAC crypto required by LE SMP, then attaches the
AIC8800D80 controller on `/dev/ttyS0` at 1.5 Mbaud without powering it through the
legacy `HCIDEVUP` ioctl. Debian `bluetooth.service` bind-mounts the BlueZ state directory,
starts the daemon, and lets BlueZ power `hci0` through the management API. This
ordering is required for the kernel to register the LE SMP fixed channel used
by encrypted GATT reads. If the controller disappears, the watchdog repeats
the same initialization sequence before restarting BlueZ:

```text
/userdata/ble_service/bluetooth -> /var/lib/bluetooth
```

This preserves the iPhone bond across reboot and firmware rootfs replacement.

## Pairing and Bonding

The advertised name is derived from the configured base name and the final
four hexadecimal digits of the adapter address, for example `Aiden-12AB`.
This is display-only. The full adapter identity is exposed as `board_identity`
over the USB status API and as six bytes of manufacturer-specific data in the BLE
advertisement. The app requires the service UUID, display name, and full
identity to match before pairing, so nearby boards with colliding name suffixes
cannot be selected accidentally.

The Wake characteristic has an encrypted read operation. The companion app
performs that read after connecting, causing iOS to start SMP and show the
system pairing sheet. No HOGP service is registered; USB remains the only HID
control path.

`ble_service` starts non-pairable and non-discoverable even when no bond exists.
The iOS app explicitly calls the Agent pairing API over USB ECM; only then does
the service open a five-minute pairing window. The app reads the board's stable
`device_name` and collision-resistant `board_identity` first and only connects
a Wake-service advertiser carrying both values. The window closes only when a
Wake subscription and a successful encrypted Wake read both happen inside the
current window, in either order. A notification subscription by itself does
not close it: the Wake CCCD has no encryption requirement, so iOS can subscribe,
or restore a cached CCCD, on a link it cannot decrypt yet. That happens when
iOS has forgotten the board while BlueZ still holds the old key. Closing the
window at that point would make the kernel answer the phone's next SMP Pairing
Request with "Pairing not supported". An existing bond does not prevent an
explicit Connect action from reopening the window. Outside that window, only
the selected trusted phone is authorized. `PAIRING_WINDOW_SECONDS` in
`/etc/aiden_ble_service.conf` controls the maximum user-initiated window.

The five-minute value is an upper bound that tolerates Bluetooth permission and
iOS confirmation delays; the app starts scanning immediately and successful
pairing closes the window early. A service restart never opens the window by
itself. While a user-initiated window is active, the service also reconciles the
actual BlueZ `Pairable` and `Discoverable` properties because bond removal and
other BlueZ state changes can reset them independently of the service state.
The Wake advertisement is registered once per `ble_service`/BlueZ lifetime and
keeps a stable service UUID and board identity. Opening, refreshing, or closing
the pairing window never unregisters or recreates the advertisement; only the
adapter pairing properties and pairing-agent authorization change.

The app reports only the live connection state. A bond is a reconnect cache,
not proof that the App Wake session is connected. Every explicit Connect action
reopens the board window; CoreBluetooth reuses a saved peripheral when possible
and otherwise performs the system pairing flow.

An ACL/CoreBluetooth connection is not reported as an authenticated Bluetooth
connection until the encrypted Wake read succeeds. If iOS has forgotten the
device while BlueZ still has its old key, that read returns insufficient
encryption. The app stops reconnecting after the first such failure and marks a
board-bond reset as required instead of repeatedly showing the system pairing
sheet.

## Wake GATT Contract

| Item | UUID |
| --- | --- |
| Wake Service | `a1de0001-7c4b-4f52-8d9a-6b4f6e6f7469` |
| Wake Characteristic | `a1de0002-7c4b-4f52-8d9a-6b4f6e6f7469` |

The characteristic supports encrypted `read` and standard `notify`. BlueZ 5.65
on the board rejects CCCD writes for `encrypt-notify` even after bonding, so the
read is the authentication and pairing trigger while the notification carries
only a non-sensitive poll signal. A notification is a 12-byte little-endian
value:

```text
byte 0      protocol version (1)
byte 1      reason (0 unknown, 1 Phone Bridge queue, 2 manual, 3 system,
            4 Live Activity state refresh, 5 planned USB re-enumeration)
bytes 2-3   reserved
bytes 4-11  uint64 wake sequence
```

The iOS app treats this only as a native wake hook. Reason 1 opens the narrow
background Phone Bridge command window; reason 4 reads
`/api/live-activity/current` and updates ActivityKit locally; reason 5 starts a
grace period so a planned USB gadget re-enumeration is not treated as a cable
disconnect. No command or task payload is decoded from BLE.

## Phone Notification Event Shape

The service subscribes to ANCS Notification Source and Data Source, requests
notification attributes through Control Point, and stores a bounded in-memory
ring. Events include:

- monotonic string `id` for UDS cursors;
- `source`, source notification/event IDs, and the companion `device_id` when
  the event came from Android;
- ANCS `notification_uid`, event type, flags, category, and category count;
- app identifier, title, subtitle, message, and date when attribute retrieval
  succeeds;
- `metadata_complete` and `metadata_error` when a disconnect or protocol error
  prevents a complete attribute response.

The ring defaults to 512 events. Reboot clears events but does not clear the
Bluetooth bond.

ANCS subscription is independent of the companion app's Wake subscription.
Wake availability controls only on-demand background Phone Bridge execution;
it must not delay Notification Source or Data Source subscription because that
changes the iOS/BlueZ GATT initialization order and can leave attribute reads
without Data Source responses.

On Android, the companion app uses `NotificationListenerService` after the
user grants Notification Access. It filters the Aiden app's own notifications
and group summaries, queues added/modified/removed events locally, and posts
batches to `/api/phone-notifications/events` over USB ECM. Each source event has
a stable `source_event_id`; `ble_service` deduplicates retries while that event
remains in the bounded ring. Android notification forwarding does not require
Bluetooth pairing.

## UDS API

Default socket:

```text
/run/ble_service/ble_service.sock
```

The framing is the common 12-byte UDS envelope documented in
[Unix Domain Socket Protocol](../06-protocols/uds-protocol.md).

### `status`

```json
{"op":"status"}
```

Returns adapter, GATT registration, pairing window, trusted bond,
advertisement, Wake subscriber, connected iPhone, ANCS subscription, cursor,
the last Wake delivery result, and last-error state. A disconnected trusted
device always reports `wake_subscriber=false` and `ancs_subscribed=false`.

### `wake`

```json
{"op":"wake","reason":"phone_bridge"}
```

Returns a string `wake_id` and `delivered`. `delivered=true` confirms only that
the Wake notification was delivered to the subscribed iOS central. It does not
confirm HTTP queue polling, native tool execution, or result posting; clients
must continue waiting for the Phone Bridge command result. `delivered=false`
means no iOS central was subscribed, and the HTTP queue remains intact.

### `events_since`

```json
{"op":"events_since","since":"42","generation":"<service-generation>","limit":50}
```

Returns the current `generation` with events after the cursor. Start with
`since=0`, save the returned generation, and include it on incremental reads.
After `ble_service` restarts, a stale generation returns `reset_required=true`
with no events; retry with `since=0`. Within one generation, `truncated=true`
means the requested cursor is older than the bounded ring's retained history.

### `notification_publish`

```json
{
  "op": "notification_publish",
  "phone_id": "android-...",
  "events": [
    {
      "source_id": "0|com.example.mail|42|null|1000",
      "source_event_id": "<sha256>",
      "event": "added",
      "app_identifier": "com.example.mail",
      "title": "New message",
      "message": "Hello"
    }
  ]
}
```

Accepts 1-8 Android notification events, validates bounded metadata fields,
sets `source=android` and the request `device_id`, and returns accepted and
duplicate counts. This operation is intended for the Agent HTTP bridge rather
than arbitrary local publishers.

### `pairing_start`

```json
{"op":"pairing_start"}
```

Opens or refreshes the configured connection window. Existing bonds are kept
and do not cause a conflict; the window closes after the App completes the
encrypted read and subscribes to Wake notifications, or when the deadline
expires.

### `disconnect`

```json
{"op":"disconnect"}
```

Calls BlueZ `Device1.Disconnect` for the current phone connection, closes the
pairing window, disables advertising, clears live Wake/ANCS state, and keeps the
paired/trusted device plus its bond keys. Reconnecting requires another explicit
Connect action, which calls `pairing_start`; the existing bond can then be
reused without repeating system pairing.

### `pairing_forget`

```json
{"op":"pairing_forget"}
```

Calls BlueZ `Adapter1.RemoveDevice` for board-side bonds and returns the removal
count plus the latest Bluetooth status. It remains a local maintenance
operation; the Agent exposes it only through the USB-restricted
`/api/bluetooth/pairing/reset` recovery endpoint. Normal disconnects do not use
it and preserve both sides of the system bond.

## Agent HTTP API

The companion app reaches the pairing operations through the Agent on USB ECM:

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/api/bluetooth/status` | Read BLE runtime and live connection state |
| `POST` | `/api/bluetooth/pairing/start` | Open or refresh the user-initiated connection window |
| `POST` | `/api/bluetooth/pairing/reset` | Remove a confirmed stale board bond before one fresh pairing attempt |
| `POST` | `/api/bluetooth/disconnect` | Disconnect the physical BLE/ANCS link without deleting the bond |
| `POST` | `/api/phone-notifications/events` | Ingest a batch of Android notification changes |

Bluetooth control and phone-notification writes are accepted only over the
board's USB ECM address (`192.168.42.1/24`) or loopback; requests arriving
through Wi-Fi and other listeners receive `403`. Android notification bodies
use this local USB path. The normal iOS app flow first disables its
CoreBluetooth reconnect loop, then asks BlueZ to disconnect the shared physical
link. BLE keys, iOS ANCS bodies, and Phone Bridge command payloads never pass
through these endpoints.

## Operations

```bash
systemctl status aiden-bluetooth-attach.service
systemctl status bluetooth.service
systemctl status aiden-ble.service
```

Logs:

```text
/var/log/aiden-hciattach.log
/var/log/ble_service/ble_service.log
/userdata/agent/log/agent.log
```

`bluetoothd` writes to the systemd journal (`journalctl -u bluetooth`). At its
default level it does not log SMP pairing results, so use `btmon` to capture
pairing failures on the air.

`ble_service.log` records device connection changes, pairing-window
transitions with their reason (`app_connected`, `deadline`, user disconnect),
windows kept open because the Wake subscription was not authenticated,
encrypted Wake reads, pairing-agent decisions, and board-bond removals.
`agent.log` records each App-initiated `pairing/start`, `pairing/reset`, and
`disconnect` call together with the resulting board state.

Useful checks:

```bash
hciconfig -a
bluetoothctl show
ls -l /userdata/ble_service/bluetooth
```

Before the app requests pairing, `bluetoothctl show` should report
`Pairable: no` and `Discoverable: no`. After `pairing_start` both become `yes`
until the App finishes the authenticated Wake connection or the configured
deadline expires.
