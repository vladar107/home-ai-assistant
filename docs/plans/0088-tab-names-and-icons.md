# 0088 — Tab names and icons by topic

Status: `done`

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
  - Chunk 2 went ahead with the conservative rule below before the check,
    because the check needed plan 0085 deployed.
  - **What the check found** on the operator's bot, after the deploy:
    - Creates and renames both reach the bot. A tab the user opens sends
      `forum_topic_created` directly. A tab that was open before the bot
      recorded it is seen only through the root its messages reply to. A
      user's rename arrives as `forum_topic_edited` from the user.
    - `is_name_implicit` is set on a tab the user opened without typing a
      name.
    - `editForumTopic` works on a tab the user created.
  - **What changed after it.** The bot renamed three tabs the user had opened
    without a name, one of them a tab from before the deploy. The user wants
    tabs that already exist left alone, and new ones named. So:
    - A tab first seen through the root its messages reply to is recorded as
      `user`, whatever its name, because it was open before the bot knew it.
    - Migration 036 marks every tab the registry knew of as `user`, so only
      tabs opened after it are named by topic.
    - Telegram does report the user's renames, so the once-per-conversation
      claim stays.
- **Who may be renamed.** Only tabs with `named_by = bot`: tabs the bot
  created for Resume, and user tabs whose creation had an implicit name.
  - Never a `user` tab, a `fixed` tab (feeds and trips), or the main view.
  - If the check shows that the bot does not see user tabs being created or
    renamed, only tabs the bot created are renamed, and each of them only
    once. The user may have renamed it unseen.
  - **The rule in force until the check.** A `bot` tab is renamed at most once
    per conversation, through `named_session_id`. Plan 0085 already records a
    user tab as `bot` when its `forum_topic_created` arrives with
    `is_name_implicit`, and as `user` otherwise. A user tab whose service
    message never arrives gets no row and is never renamed, which is the safe
    fallback. A rename the user makes that reaches the bot turns the tab into
    `user` for good.
  - The whole rule is one condition, `named_by = 'bot'`, in the store's claim
    (`ClaimTabName`), so widening it later touches one place.
  - Telegram reports the user's renames, so a renamed tab turns into `user`
    and keeps its name.
- **When.**
  - Once per conversation, after its first reply is delivered.
  - The conversation service hands each plain reply delivered in a tab, with
    the user's message, to a `TabReplies` hook. The hook runs in the
    background, so the tab's next turn does not wait for it, and Shutdown
    cancels and waits for it. A continuation's or a reconsider's message is
    the bot's own, so it is not handed on.
  - The Telegram service claims the tab in the registry first, then asks
    `NameTab`. Before `editForumTopic` it reads the tab again, and goes on
    only while the tab is still `bot` and still claimed by this conversation.
    So a rename the user made meanwhile wins, and so does a later
    conversation in the tab that claimed it while this name was chosen. The
    name and icon are written back last, under the same two conditions.
  - A name dropped for either reason is logged as `telegram.tab.name_failed`
    with `reason=superseded`, not as `telegram.tab.named`.
  - One window remains: a user rename that is still in the poll queue when
    the bot edits the tab can be overwritten in Telegram once. The registry
    keeps the user's name, and the user renaming the tab again fixes it.
  - An edit that would change neither the name nor the icon is skipped,
    because Telegram refuses it.
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
    bad name means no rename. An answer in a code fence is still read, and an
    emoji matches with or without its variation selector.
  - The reply can carry text steered from outside, and the name is shown in
    `/sessions`, `SessionInTab`, and `CloseTabQuestion`, where Telegram turns
    bare domains and mentions into links. So a name is refused when it has:
    - a format, line separator, or paragraph separator character (Cf, Zl,
      Zp), such as a bidi override or isolate or a zero-width character
    - any other rune that is not printable, apart from a plain space
    - `@`
    - a domain-like token: a letter or digit, a dot, and two or more letters,
      as in `example.com`
    - `://`, `www.`, or `t.me/`
  - Any failure leaves the tab as it is, and `named_session_id` is still set,
    so nothing is retried.
  - The text stays in the user's own subscription, and nothing of it is
    logged.
- **The icon set.**
  - `getForumTopicIconStickers` is fetched at start and refreshed daily, and
    kept in memory as emoji to custom emoji ID.
  - The refresh happens when a tab is being named and the set is a day old.
    `IconFor` never fetches: it answers from the set as last read. The fetch
    runs outside the cache's lock, so `IconFor` never waits for Telegram.
  - If it cannot be fetched, tabs are named without icons. A failed fetch is
    tried again an hour later, at the next naming.
  - When Telegram lists an emoji twice, its first icon is kept.
  - The cache lives in the Telegram service (`tabnames.go`), not in
    `conversation`, because it reads a Telegram method. `NameTab` gets the
    emoji list and returns one of them.
  - `IconFor(emoji)` returns the custom emoji ID for a fixed icon, so feed and
    trip tabs can use it.
- **Fixed icons.**
  - Feeds get icons by emoji, when the set has them: ⏰ Reminders,
    📰 Reports, 🏠 Home, 👀 Watches.
  - The feed resolver sets the icon right after it makes a feed tab, and on
    the next message of a feed tab that has none yet, such as one made before
    this plan or before the set was read. Only the icon is edited, never the
    fixed name, and the registry records it (`SetFixedTabIcon`).
  - A feed tab whose icon is settled, or was refused, is not asked again until
    a restart. A failure is logged as `telegram.feed.failed` with stage `icon`
    and costs only the icon.
  - Plan 0089 uses ✈️ for trip tabs.
  - When an emoji is missing from the set, that tab has no icon.
  - Plan 0087 was built at the same time, so this plan's migration is 035.
- **The registry keeps the result.** The name and icon ID are written back,
  so the `/sessions` and Open wording from plan 0085 uses the new name.

## Approach

### Chunks

1. **Check Telegram.** Done by the orchestrator, because it needs the live bot
   and the operator.
   - Acceptance: the three questions above are answered in this plan.
   - Pending until plan 0085 is deployed.
2. **Names and icons.** Jev: complex (0.94). Depends on chunk 1, and went
   ahead of it with the conservative rule.
   - Migration 035.
   - The client gets `EditForumTopic` and `GetForumTopicIconStickers`.
   - `NameTab` in `conversation`, and the icon cache in `telegram`.
   - The rename hook after a conversation's first delivered reply.
   - `IconFor` for fixed icons, and the feed icons in the feed resolver.
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

- `internal/store/sqlite/`: `migrations/035_tab_names.sql`,
  `migrations/036_existing_tab_names.sql`, `tabs.go`, `tabs_test.go`
- `internal/telegram/`: `client.go`, `service.go`, `tabs.go`, the new
  `tabnames.go` and `tabnames_integration_test.go`, the tab simulator in
  `tabs_integration_test.go`, and the harness in
  `conversation_integration_test.go`, `feeds.go`, `feeds_integration_test.go`
- `internal/conversation/`: the new `tabname.go` and `tabname_test.go`,
  `service.go`, `types.go`
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
