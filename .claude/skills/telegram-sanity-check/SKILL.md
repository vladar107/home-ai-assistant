---
name: telegram-sanity-check
description: "Sanity-check a bot feature in the operator's real Telegram, in the bot chat only, through the native app with computer use, falling back to Telegram Web in Chrome. Use when the operator asks to check, try, or look at a feature in Telegram, or after shipping a user-visible change they want seen working. Not a regression suite: one feature, a few steps, then clean up."
argument-hint: WHAT_TO_CHECK
---

# Telegram sanity check

Check one feature the way a user sees it, in the operator's own Telegram account, without
touching anything but the bot chat.

## Boundaries

- Open only the bot chat. Its username is in `CLAUDE.local.md`; never write it into tracked files.
- Never open, read, scroll, search, or click any other chat, folder, contact, channel, or the chat
  list. Never read message previews or notifications from other chats, and never quote or
  describe them. If another chat appears on screen, look away from it and go on.
- Never send anything outside the bot chat, never forward, never change Telegram settings.
- Write only what the check needs. Do not ask the bot to remember, save, or schedule anything
  unless that is the feature under test; if it is, remove it again at the end.
- Stop and ask the operator before anything that costs money, contacts a third party, or
  changes real household devices.

## Pick the client

1. **Native Telegram via computer use** (default): it shows exactly what the operator sees,
   widgets included. Needs the built-in `computer-use` MCP server, enabled with `/mcp`, and macOS
   Accessibility and Screen Recording permissions for the terminal. Approve only the Telegram app.
2. **Telegram Web in Chrome**, only as a fallback when computer use is unavailable. Needs Claude in
   Chrome, connected with `claude --chrome` or `/chrome`.

If neither is connected, tell the operator which command enables it and stop.

## Steps

1. Ask the operator to turn on Do Not Disturb (Focus) on the Mac, so other chats' notifications
   do not appear on screen.
2. Open the bot chat directly: in the native app, `open "tg://resolve?domain=<bot username>"`; in
   Chrome, `https://web.telegram.org/a/#@<bot username>`. Do not navigate there through the chat
   list.
3. Start a fresh tab: send a first message such as "Sanity check: <feature>" from the main view, so
   the client opens a new tab for it. Work only in that tab.
4. Run the check: send the messages, press the buttons, and read the replies. Take a screenshot of
   each step that shows the result, cropped to the bot chat.
5. Clean up: in the test tab, send `/close` and confirm. The bot ends that tab's conversation and
   deletes the tab. If the check created anything else (a reminder, a watch, a parcel, a memory),
   remove it through the bot before closing.
6. Report to the operator: what was checked, each step's result, the screenshots, anything that
   looked wrong, and what was cleaned up. Mention anything that could not be removed.
