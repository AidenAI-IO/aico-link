"""Temporary raw-wire Gemini tool probe; never persist credentials or audio."""
from __future__ import annotations

import asyncio
import copy
import json
import os
from urllib.parse import urlencode

from websockets.asyncio.client import connect


async def probe(name: str, *, grouped: bool, combined: bool, thinking: bool) -> dict:
    declarations = [
        {"name": "diagnostic_echo", "description": "Return the supplied harmless nonce.",
         "behavior": "NON_BLOCKING", "parameters": {"type": "OBJECT", "properties": {
             "nonce": {"type": "STRING"}}, "required": ["nonce"]}},
        {"name": "diagnostic_unused", "description": "Unused diagnostic function.",
         "behavior": "NON_BLOCKING", "parameters": {"type": "OBJECT", "properties": {}}},
    ]
    model = "gemini-3.8-live-extended-thinking" if thinking else "gemini-3.8-live"
    setup = {
        "model": "models/" + model,
        "generationConfig": {"responseModalities": ["AUDIO"]},
        "systemInstruction": {"parts": [{"text": "Call diagnostic_echo when asked. Never invent its result."}]},
        "tools": ([{"functionDeclarations": declarations}] if grouped else
                  [{"functionDeclarations": [d]} for d in declarations]),
        "outputAudioTranscription": {},
        "inputAudioTranscription": {},
    }
    if thinking:
        setup["generationConfig"]["thinkingConfig"] = {"thinkingLevel": "LOW"}
    result = {"variant": name, "model": model, "setup_ack": False,
              "tool_calls": [], "transcript": "", "statuses": [], "completed": False}
    endpoint = "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent?"
    endpoint += urlencode({"key": os.environ["BENCHMARK_REALTIME_API_KEY"]})
    try:
        async with asyncio.timeout(45):
            async with connect(endpoint, max_size=8 * 1024 * 1024) as ws:
                await ws.send(json.dumps({"setup": setup}))
                while True:
                    frame = json.loads(await ws.recv())
                    if "error" in frame:
                        result["api_error"] = True
                        break
                    if "setupComplete" in frame:
                        result["setup_ack"] = True
                        content = {"turns": [{"role": "user", "parts": [{"text":
                            "Call diagnostic_echo with nonce 237 now, then say the returned nonce."}]}],
                            "turnComplete": combined}
                        await ws.send(json.dumps({"clientContent": content}))
                        if not combined:
                            await ws.send(json.dumps({"clientContent": {"turnComplete": True}}))
                    calls = frame.get("toolCall", {}).get("functionCalls", [])
                    if calls:
                        result["tool_calls"].extend(copy.deepcopy(calls))
                        await ws.send(json.dumps({"toolResponse": {"functionResponses": [
                            {"id": c["id"], "name": c["name"], "response": {
                                "result": c.get("args", {}), "scheduling": "INTERRUPT"}}
                            for c in calls]}}))
                    sc = frame.get("serverContent", {})
                    result["transcript"] += sc.get("outputTranscription", {}).get("text", "")
                    status = sc.get("interactionStatus")
                    if status:
                        result["statuses"].append(status)
                    if sc.get("turnComplete") and (status == "IDLE" or (not thinking and not status)):
                        result["completed"] = True
                        break
    except TimeoutError:
        result["error"] = "timeout"
    except Exception as exc:
        # Exception strings may embed the credential-bearing WebSocket URL.
        result["error"] = type(exc).__name__
    return result


async def main() -> None:
    variants = [
        ("extended_split_tools_split_input", False, False, True),
        ("extended_grouped_tools_split_input", True, False, True),
        ("extended_grouped_tools_combined_input", True, True, True),
        ("standard_grouped_tools_combined_input", True, True, False),
    ]
    for name, grouped, combined, thinking in variants:
        result = await probe(name, grouped=grouped, combined=combined, thinking=thinking)
        print(json.dumps(result), flush=True)


if __name__ == "__main__":
    asyncio.run(main())
