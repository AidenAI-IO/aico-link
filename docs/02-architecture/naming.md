---
sidebar_position: 7
---

# Product Names and Technical Identifiers

**Aico Link** is the hardware platform. **Aico** is the AI Agent that runs on
it. The official website is [aico.link](https://aico.link).

Use Aico Link for boards, firmware, hardware services, USB/Bluetooth device
names, and deployment. Use Aico for the Agent's conversation, tools, memory,
skills, and voice behavior. The phone application is the Aico companion app.

## Canonical Technical Names

The product naming applies to source identifiers, build artifacts, installed
services, configuration, and telemetry:

| Area | Names |
| --- | --- |
| C++ headers and logging | `aico_sdk.h`, `aico_log.h`, `AICO_LOG_*` |
| C++ namespaces | `aico`, `aico_image`, `aico_vad`, `aico_demo` |
| Shared service status | `AicoServiceStatus` |
| CMake targets and archives | `aico`, `aico_image`; `libaico.a`, `libaico_image.a` |
| Go module | `aico-agent` |
| Python packages and SkillOpt backend | `aico-benchmark`, `aico-skillopt`, `AicoDeviceBackend` |
| Device services and package | `aico-agent.service`, `aico.target`, `aico-business` |
| Device user/group and runtime paths | `aico`, `/usr/lib/aico`, `/usr/share/aico`, `/run/aico` |
| Environment and build variables | `AICO_*` |
| Built-in tool skill | `config/skills/aico` |
| OTA signing-key path and boot parameter | `/usr/share/keyrings/aico-ota.pem`, `aico.slot_suffix` |
| Backup archive magic and SD filesystem label | `AICOBKP1`, `AICO` |
| APT release metadata | `Origin: Aico`, `Label: Aico business packages` |
| Telemetry trace and tag | `aico-episode`, `aico-hardware` |

Documentation commands and examples use these names. Repository source files,
service units, helper scripts, and benchmark suite paths use the corresponding
`aico` spelling. A local checkout may use any directory name; clone examples
choose `aico-link-firmware` explicitly.

## Initial Setup and Integrations

This is the initial Aico release. Build firmware, configure devices, and create
integrations using the canonical names above. Use the complete Aico Link
firmware image for initial setup, then follow the regular OTA or business
package update workflow for subsequent releases.

C++ consumers use the Aico headers, namespaces, and archives. Go imports,
service references, environment variables, API clients, benchmark settings,
and telemetry queries use the `aico` identifiers documented here.

The device account is `aico` with UID 1000. Its home is `/home/aico`, backed by
`/userdata/home/aico`. The root user's persistent home remains
`/userdata/userhome`. The Agent stores its user configuration in
`/userdata/agent`, its bundled self-knowledge skill uses `aico`, and managed
SD-card data lives under the card's `aico` directory. Backup exports use the
`aico-backup` format and `AICOBKP1` magic.

For deployment and updates, see [Deployment](../01-getting-started/deployment.md),
[OTA](../08-ota/README.md), and
[Debian Business Packages](../08-ota/debian-package.md).

## Existing External Names

Existing GitHub organization, release, and submodule URLs remain unchanged.
The firmware repository was renamed from `AidenAI-IO/aiden-firmware` to
[`AidenAI-IO/aico-link`](https://github.com/AidenAI-IO/aico-link); clone from
the new name and use the signed APT repository at
`https://aidenai-io.github.io/aico-link/apt`. Releases published before the
rename keep the former name in their immutable `release.json` records, and the
release planner treats both names as the same repository.

Sibling checkout paths such as `aiden-mcu`, `aiden-design`, and `aiden-app`
refer to those existing repositories; this firmware change does not rename
their checkouts or source interfaces.

The companion app is maintained separately. Its currently installed system
labels and build identifiers, such as `Aiden Ready` and
`AidenLiveActivityExtension`, remain accurate references to that app.
