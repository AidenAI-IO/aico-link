---
sidebar_position: 12
---

# Shared Files and Downloads

The Agent accepts arbitrary file uploads independently of image/audio chat
attachments. Uploaded files and generated outputs are available to the Agent as
local paths and to Web/App users as downloads.

## Device storage and services

`share.mount` bind-mounts `/userdata/share` at `/share`, after userdata migration.
This uses the existing persistent userdata filesystem; it does not repartition
or format the device. Files survive Agent restarts and rootfs slot updates.
They consume userdata capacity and are not removed by clearing chat history.
Delete files explicitly with normal device administration when no longer needed.

The Agent serves `POST /api/files`, `GET /api/files`, and `/share/` on its existing
HTTP port (8080). The device service also sets `AICO_SHARE_ADDR=0.0.0.0:8000` to
start a listener exposing only these file routes. Both listeners follow the
device's LAN access switch: requests from the USB link and the board are
allowed, while other networks require LAN access to be enabled and a valid
device-password session. For example:

```text
Agent path: /share/report.pdf
Download:  http://192.168.42.1:8000/share/report.pdf
Web/App:   http://192.168.42.1:8080/share/report.pdf
```

Both listeners share the same storage. `AICO_SHARE_DIR` overrides the absolute
storage directory for desktop development; `AICO_SHARE_ADDR` is optional outside
the device service. The main Agent listener remains sufficient for uploads and
downloads when the additional listener is disabled or cannot bind (for example, when port
8000 is in use); a warning is logged without stopping chat. Bind a mounted persistent
directory and retain the default `/share` path on the device.

The initial device rollout requires an OTA with the updated platform-owned
`share.mount` and its matching business package (Agent binary, Agent unit, and
`aico-share-prepare.service`). A business-only upgrade on an older platform is
not sufficient: the new Agent service requires the mount. Release classification
recognizes the new mount unit as a platform change and advances the compatibility
contract. Subsequent Agent/Web updates can use the normal business-package path.
No device flashing or deployment is performed by the development workflow.

## Upload and attach

Upload exactly one multipart field named `file` per request, up to 100 MiB:

```bash
curl -F 'file=@report.csv' http://192.168.42.1:8080/api/files
```

A `201` response contains metadata:

```json
{
  "kind": "file",
  "name": "report.csv",
  "path": "/share/report.csv",
  "size": 42,
  "mime_type": "text/csv; charset=utf-8",
  "download_url": "/share/report.csv",
  "modified": "2026-11-05T00:00:00Z"
}
```

Send the returned path in a normal chat request:

```json
{
  "message": "Summarize this spreadsheet",
  "attachments": [{"kind": "file", "path": "/share/report.csv"}]
}
```

The server validates that the file exists inside the shared directory and derives
its metadata from storage. It supplies the local path to the Agent without
embedding binary data into provider requests. File references remain available
in chat history. A turn accepts up to 16 attachments, including up to 4 images.
Images selected through the image picker retain the existing visual input path;
files selected through the file picker are treated as local files, including
images and audio selected there.

Uploads stream to a hidden temporary file and become visible only after the
complete request is persisted. A duplicate filename receives a random suffix;
existing files are never overwritten. Failed uploads remove temporary files.
At boot, shared-storage preparation removes regular `.upload-*` files older than
one hour from the shared root to recover space after crashes or power loss.
Newer files, subdirectories, symbolic links, and published files are left intact.
Oversized uploads return `413`; invalid multipart data or filenames return `400`;
storage failures return `503` or `507`. Filenames must not be hidden, contain
path separators or control characters, or exceed 240 UTF-8 bytes.

## Generated outputs and other files

The Agent is instructed to save downloadable outputs under `/share` and return
Markdown links such as `[Download report](/share/report.pdf)`. Encode each path
segment when it contains spaces, parentheses, non-ASCII characters, `#`, or `?`.
For example, `report (final).pdf` becomes `report%20%28final%29.pdf`.

Any regular file placed in `/share`, including files created outside the Agent,
is available immediately without registration. Subdirectories are supported.
`GET /api/files?path=outputs` lists the direct children of `/share/outputs`;
omitting `path` lists the root. `/share/` provides a browser directory listing.
Hidden files, symbolic links, special files, and paths outside the shared root
are excluded. Downloads use attachment disposition, support HEAD and byte ranges,
and do not execute HTML or SVG as same-origin content.

The Web composer has **Add file** and **Shared files** controls. Download cards
appear for attached files and Agent output links. The phone App offers file
selection through the attachment menu and a shared-file browser. iOS downloads
to a temporary file and opens the system share sheet (including Save to Files);
Android opens the system save-document picker and streams to the selected
location. Neither platform requires broad storage access. Transfer errors are
shown in the UI; selecting or canceling a picker does not send a chat turn.

Both ports use the same `lanaccess.Guard` as the Agent. LAN clients sign in at
`/login` or `POST /api/auth/login`; browsers send the `aico_session` cookie and
native clients send `Authorization: Bearer <token>`. The cookie applies to the
same host across ports 80, 8080, and 8000. Download URLs contain no credentials
and do not grant access by themselves. Changing the device password or disabling
LAN access revokes existing sessions. USB transfers do not require a session.

When a Web upload receives `401 login_required`, the composer offers a login
link in a new tab, preserving the draft. Sign in and select the file again to
retry. Directory links and browser downloads use the guard's normal login
redirect. `403 lan_access_disabled` means LAN access must first be enabled over
USB; signing in cannot override that switch.

The App requests the device password when a file operation requires login and
shares an in-memory, board-origin-scoped token between uploads, listings, and
native downloads. Downloads check the session before opening a save picker;
the native request sends the token explicitly and refuses redirects. Passwords
are not persisted. Restarting the App may require another LAN sign-in.

Web transfers stay on the current origin, and cross-origin browser uploads are
rejected. All authenticated device sessions can access all explicitly shared
files; there is no per-file or per-user ACL. The board serves plain HTTP, so
enable LAN access only on a trusted network. Do not place credentials in this
directory or forward these ports to an untrusted network.
