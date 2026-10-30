---
sidebar_position: 4
---

# Skills and RoleProfile Mechanism

The Agent automatically discovers `SKILL.md` files from the config directory, displays an Available skills catalog in the system prompt, and loads the complete instructions of relevant skills on demand via `skill_read`. Activated skills are injected into the Agent's `RoleProfile`, which contains the system prompt, skill instructions, and available tools.

## Directory Structure

```text
/userdata/agent/
└── skills/
    └── my-skill/
        └── SKILL.md
```

Scan rules:

```text
configDir/skills/**/SKILL.md
```

## Example

```markdown
---
name: ui-operator
description: Prefer screenshot-driven UI inspection before clicking.
metadata:
  device_types: [iOS, Android]
  allowed_tools: [screenshot, touch_gesture, keyboard_tap, enter_text]
---

Take a screenshot before interacting with an unfamiliar UI.
Prefer describing what you see before clicking.
```

`[basic_settings.device].device_type` and `metadata.device_types` use the same canonical values:
`iOS`, `Android`, `macOS`, `windows`, and `linux`. Lowercase aliases are accepted
for compatibility, but documentation and new skill metadata should use the
canonical form.

## Currently Implemented Capabilities

- Automatically discover `SKILL.md`;
- Display Available skills in the prompt and load complete `SKILL.md` at runtime via `skill_read`;
- Inject skill instructions into the Agent's system prompt via `RoleProfile`;
- Parse `allowed_tools` metadata for validation and future compatibility, but tool availability is currently controlled by the static Agent tool catalog rather than by active skills;
- Filter custom skills by `metadata.device_types` against the global `device_type` state;
- Provide `skill_list` / `skill_read` / `skill_manage` / `skill_mark_used` to the Agent; the HTTP Tool API exposes only the non-maintenance ones (`skill_list` / `skill_read`);
- `skill_read` supports reading `SKILL.md` as well as UTF-8 supporting files under `references/`, `templates/`, `scripts/`, `assets/`;
- `skill_read` loads a file in full when it fits the model context; each file is capped at 64 KiB. When context is tight, the Agent prunes or compacts older context before the next model call, and current-turn pruning does not replace a fully loaded skill with a placeholder. If the file cannot fit alongside the current request and skills already loaded this turn, its full result is saved as an artifact. The Agent receives a bounded preview marked as incomplete and can read the artifact in bounded ranges; it must not treat the preview as the complete skill instructions;
- Record view/use/modify statistics in `usage.json`;
- Support `active` / `stale` / `archived` lifecycle states; `skill_list` filters out archived by default;
- For skills with `source: agent` or `created_by: agent`, `skill_list` automatically executes lifecycle based on last use time: enter `stale` after 90 days of non-use, enter `archived` after 180 days; this automatic scan runs at most once every 24 hours;
- `skill_mark_used` automatically restores `stale` / `archived` skills to `active`.

## Agent Execution

The Agent uses a streamlined execution loop that handles tool calls and generates responses in a single unified flow. All tools are available to the Agent throughout execution, and the Agent directly manages its own task breakdown and execution strategy.

Tool execution is integrated into the main loop with callback handlers for streaming responses and episode recording.

## Device-Specific Skills

Custom skills can declare which configured target device types they support:

```markdown
---
name: wechat-login-android
description: WeChat login flow for Android.
metadata:
  device_types: [Android]
  allowed_tools: [screenshot, touch_gesture, enter_text, quick_action]
---

Follow the Android-specific login flow.
```

Rules:

- Omit `metadata.device_types` for a generic skill that applies to every device type;
- Supported values match `[basic_settings.device].device_type`: `iOS`, `Android`, `macOS`, `windows`, and `linux`;
- Common aliases such as `ios`, `android`, `mac`, `macos`, and `win` are normalized when loading the skill;
- `Available skills` only shows skills compatible with the current global `device_type`;
- `skill_list` and `skill_read` also hide incompatible skills by default;
- For explicit inspection or maintenance, pass `include_incompatible: true` to `skill_list` or `skill_read`;
- `skill_manage` validates `metadata.device_types` on create/edit/patch, but can still manage any skill file under `configDir/skills`.

For platform-specific workflows, prefer separate skills such as `wechat-login-android` and `wechat-login-ios` when the instructions differ substantially. Use a single skill with multiple `device_types` only when most of the flow is shared and the body can branch cleanly on the global `device_type`.

## Parsed but Not Fully Enforced Fields

- `preferred_model`
- `allowed_tools`
- `allowed_children`

These fields can serve as metadata for future extensions but should not be relied upon for current runtime enforcement.

## HTTP Tool Skill Export

The Agent provides:

```text
GET /api/tool-skills
```

This endpoint generates a skill bundle describing the Aico HTTP Tool API, making it convenient for external Agents like Codex to call device tools in a unified manner.

## Usage Recommendations

- Skill content should describe high-level strategies; do not hardcode volatile coordinates;
- For UI operation skills, recommend requiring screenshot before clicking;
- `allowed_tools` should be kept narrow for documentation and forward compatibility, but current runtime tool availability does not expand based on active skills;
- `allowed_tools` can only reference currently registered tools or `delegate_<child>` form child Agent delegation pseudo-tools;
- Do not write one-time task progress, temporary state, secrets, raw logs, or personal facts into skills; these do not belong to reusable procedures.
