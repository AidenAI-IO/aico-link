# Aico Link C++ SDK

This directory contains the C++ SDK, hardware services, and example programs
for Aico Link hardware. Aico, the AI Agent, uses these services through their
existing protocols.

C++ code includes `aico_sdk.h` and `aico_log.h`, uses the `aico` namespace
and `AICO_LOG_*` macros, and links the `aico` CMake target. The image-processing
target is `aico_image`; the archives are `libaico.a` and `libaico_image.a`. See [Product Names and Technical Identifiers](../docs/02-architecture/naming.md).

The full documentation is available here:

- [C++ SDK reference](../docs/05-sdk-and-tools/cpp-sdk.md)
- [Example programs](../docs/05-sdk-and-tools/examples.md)
- [Frame Service](../docs/03-services/frame-service.md)
- [Audio Service](../docs/03-services/audio-service.md)
- [Unix Domain Socket protocol](../docs/06-protocols/uds-protocol.md)
