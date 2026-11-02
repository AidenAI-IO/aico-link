"""Stdio MCP bridge from an external harness to the benchmark tool gateway.

The gateway owns the allowlist, failure semantics, and audit; this bridge only
translates MCP requests and images. Any MCP-capable harness can share it.
"""
from __future__ import annotations

import json
from pathlib import Path
import sys
import urllib.error
import urllib.request


def invoke(endpoint: str, name: str, args: dict) -> dict:
    request = urllib.request.Request(
        f"{endpoint}/api/tools/{name}",
        data=json.dumps({"input": args}).encode(),
        headers={"Content-Type": "application/json"}, method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=300) as response:
            payload = json.load(response)
    except urllib.error.HTTPError as exc:
        payload = json.loads(exc.read() or b"{}")
        payload.setdefault("is_error", True)
    output = payload.get("output", payload.get("error", ""))
    try:
        decoded = json.loads(output)
    except (TypeError, ValueError):
        decoded = output
    content = []
    if (isinstance(decoded, dict) and isinstance(decoded.get("data"), str)
            and decoded.get("format") in {"jpeg", "jpg", "png", "webp"}):
        metadata = {key: value for key, value in decoded.items() if key != "data"}
        content.append({"type": "text", "text": json.dumps(metadata, ensure_ascii=False)})
        content.append({"type": "image", "data": decoded["data"],
                        "mimeType": "image/jpeg" if decoded["format"] == "jpg" else f"image/{decoded['format']}"})
    else:
        content.append({"type": "text", "text": output if isinstance(output, str) else json.dumps(output)})
    return {"content": content, "isError": bool(payload.get("is_error"))}


def serve(config: dict) -> None:
    tools = {item["name"]: item for item in config["tools"]}
    for line in sys.stdin:
        request = {}
        try:
            request = json.loads(line)
            method = request.get("method")
            if method == "initialize":
                result = {"protocolVersion": "2024-11-05", "capabilities": {"tools": {}},
                          "serverInfo": {"name": "aiden-benchmark-tools", "version": "1.0"}}
            elif method == "tools/list":
                result = {"tools": [{"name": tool["name"], "description": tool.get("description", ""),
                                     "inputSchema": tool["args_schema"]} for tool in tools.values()]}
            elif method == "tools/call":
                params = request.get("params") or {}
                name = params.get("name")
                if name not in tools:
                    raise ValueError(f"Unknown business tool: {name}")
                args = params.get("arguments") or {}
                if not isinstance(args, dict):
                    raise ValueError("Tool arguments must be an object")
                try:
                    result = invoke(config["endpoint"], name, args)
                except (OSError, ValueError, urllib.error.URLError) as exc:
                    result = {"content": [{"type": "text", "text": str(exc)}], "isError": True}
            elif method == "ping":
                result = {}
            elif "id" not in request:
                continue
            else:
                raise ValueError(f"Unsupported MCP method: {method}")
            if "id" in request:
                sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": request["id"], "result": result}) + "\n")
                sys.stdout.flush()
        except Exception as exc:
            if "id" in request:
                sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": request["id"],
                                             "error": {"code": -32603, "message": str(exc)}}) + "\n")
                sys.stdout.flush()


if __name__ == "__main__":
    serve(json.loads(Path(sys.argv[1]).read_text(encoding="utf-8")))
