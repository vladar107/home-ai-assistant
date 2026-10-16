# 0088 — Tab names and icons by topic

Status: `proposed`

## Context

Backlog item `30c5bce5` asks that tabs get a name and an emoji from the topic
of their conversation. The user wants the emoji as the tab's icon.

Today:

- A tab the user opens keeps the name Telegram gave it.
- A tab made by Resume (plan 0084) is named after the conversation's title,
  which is its first user message cut short
  (`conversation/history.Title`).
- Feed tabs (plan 0087) have fixed names and no icon.

The Bot API (9.3 and later) lets a bot call `editForumTopic` in a private
chat to set a tab's name and its `icon_custom_emoji_id`. The icon must be one
of the custom emoji from `getForumTopicIconStickers`, a fixed set that Telegram
chooses. Any other emoji cannot be an icon. `ForumTopic.is_name_implicit`
marks a tab whose creator gave no name and that "likely needs to be changed by
the bot". The documentation does not say whether the bot may rename a tab the
user created in a private chat.

Plan 0085 records tabs in `telegram_tabs` with `named_by` (`bot`, `user`, or
`fixed`). It also logs each topic service message the bot receives, so it can
be seen what Telegram sends in private chats.

## Goal

- After the first exchange of a conversation, its tab gets a short name and
  an icon that fit the topic, in the user's language.
- A tab the user named, or renamed, keeps the user's name.
- Feed tabs, and later trip tabs, get fixed icons.

## Decisions

- **Check Telegram first.** With plan 0085 deployed, the operator creates a
  tab without a name, creates one with a name, and renames one. The
  `telegram.topic.service` logs then answer three questions:
  - Whether creates and renames reach the bot in a private chat.
  - Whether `is_name_implicit` is set.
  - Whether `editForumTopic` works on a tab the user created. This is checked
    once by hand through the Bot API.

  The findings go into this plan before chunk 2 starts, and the rules below
  are narrowed to what Telegram supports.
- **Who may be renamed.** Only tabs with `named_by = bot`: tabs the bot
  created for Resume, and user tabs whose creation had an implicit name.
  - Never a `user` tab, a `fixed` tab (feeds and trips), or the main view.
  - If the check shows that the bot does not see user tabs being created or
    renamed, only tabs the bot created are renamed, and each of them only
    once. The user may have renamed it unseen.
- **When.**
  - Once per conversation, after its first reply is delivered.
  - Migration 035 adds `icon_custom_emoji_id` and `named_session_id` to
    `telegram_tabs`. A tab whose `named_session_id` is the lane's current
    session is not named again, even across a restart.
  - `/new` or a resume into the tab starts a new conversation, which may
    rename the tab again.
- **How the name is chosen.**
  - A `NameTab` call on the principal's own subscription, built like
    `RewriteAlert`: no tools, no memory, a fresh session, and no turn slot.
  - It gets the first user message and the reply, both capped, and the list
    of allowed icons as emoji characters.
  - It answers strict JSON `{"name":"…","icon":"…"}`. The name is one line of
    at most 32 runes, with no links or control characters, and in the
    language of the conversation. The icon must be one of the listed
    characters.
  - Code validates the answer. An icon outside the set means no icon, and a
    bad name means no rename.
  - Any failure leaves the tab as it is, and `named_session_id` is still set,
    so nothing is retried.
  - The text stays in the user's own subscription, and nothing of it is
    logged.
- **The icon set.**
  - `getForumTopicIconStickers` is fetched at start and refreshed daily, and
    kept in memory as emoji to custom emoji ID.
  - If it cannot be fetched, tabs are named without icons.
- **Fixed icons.**
  - Feeds get icons by emoji, when the set has them: ⏰ Reminders,
    📰 Reports, 🏠 Home, 👀 Watches. New feed tabs are created with their
    icon, and existing ones get it at start.
  - Plan 0089 uses ✈️ for trip tabs.
  - When an emoji is missing from the set, that tab has no icon.
- **The registry keeps the result.** The name and icon ID are written back,
  so the `/sessions` and Open wording from plan 0085 uses the new name.

## Approach

### Chunks

1. **Check Telegram.** Done by the orchestrator, because it needs the live bot
   and the operator.
   - Acceptance: the three questions above are answered in this plan.
2. **Names and icons.** Jev: complex (0.94). Depends on chunk 1.
   - Migration 035.
   - The client gets `EditForumTopic` and `GetForumTopicIconStickers`.
   - The icon cache and `NameTab` in `conversation`.
   - The rename hook after a conversation's first delivered reply.
   - Fixed feed icons.
   - Acceptance, from integration tests with a fake model and a fake
     Telegram boundary:
     - A bot-named tab is renamed once per conversation, with an icon, and
       not again after a restart.
     - User, fixed, and main-view tabs are never renamed.
     - An icon outside the set gives no icon, and a long or empty name gives
       no rename.
     - A failed icon fetch still names tabs.
     - With topics off, nothing is called.
3. **Docs.** Done by the orchestrator. The README covers tab naming and how
   to keep your own name.

### Waves

- Wave 1: chunk 1.
- Wave 2: chunk 2.
- Wave 3: chunk 3.

## Files affected

- `internal/store/sqlite/`: `migrations/035_tab_names.sql`, `tabs.go`
- `internal/telegram/`: `client.go`, `service.go`, `feeds.go`, and the new
  `tabnames.go`
- `internal/conversation/`: the new `tabname.go`, `service.go`
- `internal/app/app.go`
- `README.md`, `docs/plans/README.md`

## Validation

- `gofmt`, a `go mod tidy` diff, `go vet`, `go test -race ./...`,
  `go build`, and `scripts/check-plans`.
- A live check:
  - Open a tab without a name and ask something, and see it renamed with an
    icon.
  - Rename it yourself and start `/new` there, and see your name stay.

## Risks and open questions

- If Telegram does not report the user's renames, the once-per-tab fallback
  above keeps the bot from overwriting them, at the cost of not renaming
  after `/new`.
- One extra short model call per conversation counts against the user's
  subscription.
- The icon set may change. It is refreshed daily, and an ID no longer offered
  is simply not used.
