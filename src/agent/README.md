# Aico Go Agent

Aico is the AI Agent that runs on Aico Link hardware. This directory contains
its Go daemon, CLI demo, runtime, tools, Web UI, voice pipeline, and Skills
implementation. The official website is [aico.link](https://aico.link).

The Go module is `aico-agent`; device services, runtime paths, and environment
variables use the `aico` / `AICO_` names. For upgrade and integration details, see [Product Names and Technical Identifiers](../../docs/02-architecture/naming.md).

The full documentation is available here:

- [Agent overview](../../docs/04-agent/overview.md)
- [Agent configuration reference](../../docs/04-agent/configuration.md)
- [Tools HTTP API](../../docs/04-agent/tools-http-api.md)
- [Voice capabilities: VAD / STT / TTS](../../docs/04-agent/audio-features.md)
- [Skills mechanism](../../docs/04-agent/skills.md)
