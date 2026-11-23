"""Minimal server-side WebSocket framing (RFC 6455) for the Phone Bridge relay.

The Go Agent's proxy dials ``/api/phone-bridge?benchmark_relay=1``, writes one
command and reads one response per connection. This covers that exchange on
top of ``ThreadingHTTPServer`` without adding a WebSocket dependency.
"""

from __future__ import annotations

import base64
import hashlib
import struct
from typing import BinaryIO


WEBSOCKET_GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
OP_CONTINUATION = 0x0
OP_TEXT = 0x1
OP_BINARY = 0x2
OP_CLOSE = 0x8
OP_PING = 0x9
OP_PONG = 0xA
MAX_MESSAGE_BYTES = 1 << 20


class WebSocketClosed(Exception):
    pass


class WebSocketProtocolError(Exception):
    pass


def accept_key(client_key: str) -> str:
    digest = hashlib.sha1((client_key.strip() + WEBSOCKET_GUID).encode("ascii")).digest()
    return base64.b64encode(digest).decode("ascii")


class ServerWebSocket:
    def __init__(self, rfile: BinaryIO, wfile: BinaryIO):
        self._rfile = rfile
        self._wfile = wfile

    def receive(self) -> tuple[int, bytes] | None:
        """Return the next data message, or ``None`` after a close handshake."""
        opcode: int | None = None
        message = bytearray()
        while True:
            fin, frame_opcode, payload = self._read_frame()
            if frame_opcode == OP_CLOSE:
                self.send(OP_CLOSE, payload[:2])
                return None
            if frame_opcode == OP_PING:
                self.send(OP_PONG, payload)
                continue
            if frame_opcode == OP_PONG:
                continue
            if frame_opcode in (OP_TEXT, OP_BINARY) and opcode is None:
                opcode = frame_opcode
            elif frame_opcode != OP_CONTINUATION or opcode is None:
                raise WebSocketProtocolError(f"unexpected frame opcode {frame_opcode}")
            message += payload
            if len(message) > MAX_MESSAGE_BYTES:
                raise WebSocketProtocolError("message too large")
            if fin:
                return opcode, bytes(message)

    def send(self, opcode: int, data: bytes) -> None:
        header = bytearray([0x80 | opcode])
        length = len(data)
        if length < 126:
            header.append(length)
        elif length < 1 << 16:
            header.append(126)
            header += struct.pack("!H", length)
        else:
            header.append(127)
            header += struct.pack("!Q", length)
        self._wfile.write(bytes(header) + data)
        self._wfile.flush()

    def _read_frame(self) -> tuple[bool, int, bytes]:
        first, second = self._read_exact(2)
        length = second & 0x7F
        if length == 126:
            (length,) = struct.unpack("!H", self._read_exact(2))
        elif length == 127:
            (length,) = struct.unpack("!Q", self._read_exact(8))
        if not second & 0x80:
            raise WebSocketProtocolError("client frames must be masked")
        if length > MAX_MESSAGE_BYTES:
            raise WebSocketProtocolError("frame too large")
        mask = self._read_exact(4)
        payload = bytes(byte ^ mask[index % 4] for index, byte in enumerate(self._read_exact(length)))
        return bool(first & 0x80), first & 0x0F, payload

    def _read_exact(self, size: int) -> bytes:
        data = self._rfile.read(size) if size else b""
        if len(data) < size:
            raise WebSocketClosed("connection closed")
        return data
