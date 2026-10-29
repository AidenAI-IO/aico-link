"""Stdio MCP bridge for the benchmark's shared business tool catalog."""
from __future__ import annotations

import json
import base64
from pathlib import Path
import sys
import urllib.error
import urllib.request


def _action_failed(output: object) -> bool:
    if not isinstance(output, dict):
        return False
    action = output.get("action_output", output)
    if isinstance(action, str):
        try:
            action = json.loads(action)
        except ValueError:
            return False
    return isinstance(action, dict) and action.get("ok") is False


def invoke(endpoint: str, name: str, args: dict) -> tuple[dict, dict]:
    request = urllib.request.Request(
        f"{endpoint}/api/tools/{name}",
        data=json.dumps({"input": args}).encode(),
        headers={"Content-Type": "application/json"}, method="POST",
    )
    with urllib.request.urlopen(request, timeout=300) as response:
        payload = json.load(response)
    output = payload.get("output", "")
    try:
        decoded = json.loads(output)
    except (TypeError, ValueError):
        decoded = output
    failed = bool(payload.get("is_error")) or payload.get("tool_error") is not None or _action_failed(decoded)
    content = []
    if (isinstance(decoded, dict) and isinstance(decoded.get("data"), str)
            and decoded.get("format") in {"jpeg", "jpg", "png", "webp"}):
        image = decoded["data"]
        metadata = {key: value for key, value in decoded.items() if key != "data"}
        content.append({"type": "text", "text": json.dumps(metadata, ensure_ascii=False)})
        content.append({"type": "image", "data": image,
                        "mimeType": "image/jpeg" if decoded["format"] == "jpg" else f"image/{decoded['format']}"})
    else:
        content.append({"type": "text", "text": output if isinstance(output, str) else json.dumps(output)})
    audit = {"tool_name": name, "raw_input": payload.get("raw_input"),
             "is_error": failed, "tool_error": payload.get("tool_error"),
             "duration_ms": payload.get("duration_ms"),
             "observation": "screenshot observation" if len(content) > 1 else content[0]["text"],
             "has_screenshot": len(content) > 1}
    return {"content": content, "isError": failed}, audit


def serve(config: dict) -> None:
    tools = {item["name"]: item for item in config["tools"]}
    audit_path = Path(config["audit_path"])
    call_number = 0
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
                    result, audit = invoke(config["endpoint"], name, args)
                except (OSError, ValueError, urllib.error.URLError) as exc:
                    result = {"content": [{"type": "text", "text": str(exc)}], "isError": True}
                    audit = {"tool_name": name, "raw_input": json.dumps(args), "is_error": True,
                             "tool_error": str(exc), "observation": str(exc), "has_screenshot": False}
                call_number += 1
                if audit["has_screenshot"]:
                    image = result["content"][1]
                    suffix = ".png" if image["mimeType"] == "image/png" else ".jpg"
                    shot = audit_path.parent / f"step_{call_number}{suffix}"
                    shot.write_bytes(base64.b64decode(image["data"]))
                    audit["screenshot_file"] = shot.name
                with audit_path.open("a", encoding="utf-8") as stream:
                    stream.write(json.dumps(audit, ensure_ascii=False) + "\n")
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
