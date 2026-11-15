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
Delete them through `DELETE /api/files`; see [Deleting files](#deleting-files).

The Agent serves `POST /api/files`, `GET /api/files`, `DELETE /api/files`, and
`/share/` on its existing
HTTP port (8080). The device service also sets `AICO_SHARE_ADDR=0.0.0.0:8000` to
start a listener exposing only these file routes. Both listeners follow the
device's LAN access switch and device password: other networks require LAN
access to be enabled, and while a device password is set every client except
the board itself needs a session. For example:

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
The Web **Add file** picker accepts mixed selections. Images are detected by MIME
type or image filename extension and retain the existing visual input path;
other files, including audio, are uploaded as local files. Pasted images use the
same visual input path. Reaching the image limit still allows adding other files
until the combined attachment limit is reached.
An individual read or upload failure does not stop later files in the selection.
The composer reports failed and skipped counts with the first failure reason.
A login-required response stops the batch and preserves the existing draft.

### Chat images

Images sent inline with a chat turn (base64 `data`, from the picker, paste, or
the API) still reach the model as visual input, and are also saved to the shared
root as if uploaded through `POST /api/files`: they are listed with
`shared_by: user`, a taken name gets a random suffix, and a name without an
image extension gets one from the MIME type. The Agent receives the saved path
next to the image, and the history entry carries its `download_url`. A failed
save is logged and does not stop the turn. Turns sent while a realtime voice
session handles chat refuse attachments and save nothing.

Uploads stream to a hidden temporary file and become visible only after the
complete request is persisted. A duplicate filename receives a random suffix;
existing files are never overwritten. Failed uploads remove temporary files.
At boot, shared-storage preparation removes regular `.upload-*` files older than
one hour from the shared root to recover space after crashes or power loss.
Newer files, subdirectories, symbolic links, and published files are left intact.
Oversized uploads return `413`; invalid multipart data or filenames return `400`;
storage failures return `503` or `507`. Filenames must not be hidden, contain
path separators or control characters, or exceed 240 UTF-8 bytes.

## How the Agent sees shared files

The Agent reads and writes `/share` as an ordinary directory on the controller,
through `shell` and Python. An attached file reaches the model as its absolute
path in the user message.

To look at an image file, the Agent calls `read_image` with its path. The tool
reads PNG, JPEG, or GIF (first frame) files below `/share` only, with the same
rules as downloads: hidden entries, symbolic links, and paths outside the root
are refused. Every image is fully decoded first, so a truncated file is
refused rather than sent. A GIF's first frame is placed on its full canvas.
Images whose longer side exceeds 1568 px are scaled down and sent
as JPEG, with transparency flattened onto white; files over 25 MiB or 40
megapixels are refused. The image is attached to the next model request with a
caption naming the file and stating that it is not the connected display, so
its coordinates are not used for `touch_gesture`; it is also not recorded as
the latest display screenshot. The transcript keeps only the
path, size, and format. `read_image` is a backend tool: the realtime foreground
cannot attach images, so it does not receive it.

Each user turn's state message also carries a `shared_files` entry that lists
the 20 most recently modified files below `/share`, newest first, with size,
`shared by user` or `shared by aico`, and modification time in the configured
time zone. Hidden entries and symbolic links are skipped, as in listings. When
more files exist, the entry says so and the Agent lists the directory with
`shell`. Each refresh checks at most 5,000 entries, reading directories in
batches and the most recently modified directories first; when the share is
larger, the entry says it covers only the files checked. The entry is omitted while `/share` holds no files. The realtime
foreground does not receive it; device work that needs the files runs in the
backend Agent.

## Generated outputs and other files

The Agent is instructed to save downloadable outputs under `/share` and, whenever
a reply mentions any shared file (an upload, a chat image, or its own output),
to write it as a Markdown link whose text is the filename and whose target is
its `/share/` path, such as `[report.pdf](/share/report.pdf)`. Encode each path
segment when it contains spaces, parentheses, non-ASCII characters, `#`, or `?`.
For example, `report (final).pdf` becomes `report%20%28final%29.pdf`.

Any regular file placed in `/share`, including files created outside the Agent,
is available immediately without registration. Subdirectories are supported.
`GET /api/files?path=outputs` lists the direct children of `/share/outputs`;
omitting `path` lists the root. `/share/` provides a browser directory listing.
Hidden files, symbolic links, special files, and paths outside the shared root
are excluded. Each file entry carries `shared_by`: `user` for files uploaded
through `POST /api/files`, and `aico` for everything else, such as Agent outputs.
Uploads are recorded in the hidden `/share/.uploads.json`; deleting a file
through the API drops its record. Files uploaded before this record existed are
reported as `aico`. Downloads use attachment disposition, support HEAD and byte ranges,
and do not execute HTML or SVG as same-origin content.

The Web composer has an **Add file** control. In the Web conversation, attached
files and every link to a file under `/share/` (including a bare `/share/...`
path in a reply) render as shared-file links. Hovering one shows a small preview
card with the file's size and owner and, where supported, an image thumbnail,
the start of a text file, or a scaled-down HTML page. Clicking opens the same
preview dialog as the inspector's shared-file list, with a download button;
modified clicks (for example Ctrl or Cmd) keep the browser's default and download
in a new tab. Previews cover images (including SVG, shown as an image), PDF,
audio, video, text, and HTML. HTML renders in a sandboxed frame without
`allow-same-origin`: its scripts run in an opaque origin that cannot read the
Web UI, its storage, or the session cookie, and the board refuses its requests
as cross-site. Chat images also show inline in the message. The phone App offers file
selection through the attachment menu and a shared-file browser. iOS downloads
to a temporary file and opens the system share sheet (including Save to Files);
Android opens the system save-document picker and streams to the selected
location. Neither platform requires broad storage access. Transfer errors are
shown in the UI; selecting or canceling a picker does not send a chat turn.

## Deleting files

`DELETE /api/files?path=<relative path>` removes one regular file, or one empty
folder, below the shared root. The path uses the same form as listings, for
example `path=outputs/report.pdf`.

| Response | Meaning |
|---|---|
| `204` | Deleted |
| `400` | `path` is empty; the shared root itself cannot be deleted |
| `404` | Not found, or not deletable: hidden entries, symbolic links (at any depth), special files, and paths outside the shared root all answer as absent |
| `409` | The folder still has entries; delete its contents first |
| `403` | A cross-origin browser request; same-origin Web pages and native clients are allowed |

Deletion never removes a tree in one request and never follows a symbolic
link, so a link inside `/share` cannot be used to delete files elsewhere.
Chat messages that already reference a deleted file keep their link; its
preview then reports that the file no longer exists, and downloading it fails
with `404`. Deletion uses the same access rules as
uploads (see below).

Both ports use the same `lanaccess.Guard` as the Agent. LAN clients sign in at
`/login` or `POST /api/auth/login`; USB clients pair at `POST /api/auth/pair`.
Browsers send the `aico_session` cookie and native clients send
`Authorization: Bearer <token>`. The cookie applies to the same host across
ports 80, 8080, and 8000. Download URLs contain no credentials and do not grant
access by themselves. Changing or clearing the device password revokes
existing sessions. Without a device password no session is needed.

When a Web upload receives `401 login_required`, the composer offers a login
link in a new tab, preserving the draft. Sign in and select the file again to
retry. Directory links and browser downloads use the guard's normal login
redirect. `403 lan_access_disabled` means LAN access must first be enabled;
signing in cannot override that switch.

The App pairs over USB and sends its token on uploads, listings, and native
downloads, over USB and Wi-Fi alike. Downloads check the session before opening
a save picker; the native request sends the token explicitly and refuses
redirects. When the token stops working, the App pairs again over USB.

Web transfers stay on the current origin, and cross-origin browser uploads are
rejected. All authenticated device sessions can access all explicitly shared
files; there is no per-file or per-user ACL. The board serves plain HTTP, so
enable LAN access only on a trusted network. Do not place credentials in this
directory or forward these ports to an untrusted network.
