---
name: wechat
description: Use for WeChat tasks on Mac or iPhone, with platform-specific search and send rules.
metadata:
  preferred_model: primary
  allowed_tools: [shell]
---

Use this skill together with `device-cli` for visible WeChat UI work.

## Scope

Use this skill when the user asks to open WeChat, find a contact or chat, read messages, compose a message, or send a WeChat message.

## Common rules

- Tap the visible WeChat icon, or use `device-cli open WeChat` if the icon is not visible.
- Prefer search over blind scrolling.
- Verify the chat title before sending anything.
- Use `device-cli type TEXT` for the WeChat chat input field.
- Inspect the attached screenshot to verify the text is present.
- Sending a message is a sensitive external action. Do not send unless the user explicitly asked for the final send.

## Mac

- Use `device-cli key cmd F` to open search for a contact or history message.
- After the text is committed and the user confirmed sending, use `device-cli enter` to send the message.
- If Enter does not send, inspect the latest screenshot before trying another action.

## iPhone

- Do not use any keyboard shortcut for WeChat navigation or sending.
- After the text is committed and the user confirmed sending, use `device-cli tap X Y` on the visible send button labeled `Send` or `发送`.
- If the `Send` button is missing or disabled, inspect the latest screenshot and fix focus or input state first. Do not use Enter as a fallback.

## Verification

- After sending, verify the new outgoing message bubble appears before reporting success.
