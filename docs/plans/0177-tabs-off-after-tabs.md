# 0177 — Turning tabs off after using them

Status: `in-progress`

## Context

Chat tabs (plan 0083) follow BotFather's topics switch, which the service reads from `getMe` at
start-up. With it off, updates carry no thread, feeds and object tabs resolve to the main view, and
the tab commands and tools refuse. That path was built for a bot that never had tabs. The operator
now wants to turn tabs off on a bot that used them for weeks, and rows from that time still name
tabs:

- **Conversations active in a tab stay active there.** Every lane keeps one active conversation,
  and with tabs off only the main view's lane is reachable. A reply to one of their messages does
  not return to them, because the conversation is active in another lane. `/sessions` lists them
  as open in another tab, and Resume refuses with "That conversation is open in another tab."
- **Persisted work restores its thread.** Task reports, approval offers and resends, provenance
  answers, and conversation requests queued in a tab put their stored thread on the send context,
  and the client sends `message_thread_id`. Sends that may fall back retry in the main view when
  Telegram answers "thread not found"; the rest fail, and the tab observer counts the refusals
  against tabs that no longer matter.

Feeds, object tabs, tab naming, and the `/sessions` UI already behave with tabs off and need no
change.

## Goal

- After the operator turns topics off in BotFather and restarts the service, the chat works as
  one conversation. Nothing is sent with a thread. Every conversation that was open in a tab can
  be resumed from the main view, through `/sessions` while it is among the listed ones, or by
  replying to one of its messages.
- The tab code stays, dormant. Turning topics on again brings the tabs back, subject to the usual
  retention of closed conversations.
- No tab's conversation is carried into the main view by hand; the operator starts a new one
  there after the switch.

## Decisions

- **The client drops the thread while topics are off.** The client keeps the bot's topics flag,
  set from every `getMe`, and is the one place that knows it; `Tabs.tabsOn` reads it from there.
  While the flag is off, the send and indicator threads are 0 whatever the context carries, decided
  before a route is resolved, so no route looks up or makes a tab. This guards every persisted
  thread in one place, instead of a check at each of the places that restore one.
- **The flag is asked for when needed.** Modules and the conversation service start before
  Telegram start-up, so a send can carry a thread before any `getMe` was answered. Such a send
  asks for it, one ask at a time across goroutines. When the ask fails, the send keeps its thread,
  as today, and the next send asks again.
- **Start-up closes the conversations still active in a tab.** With topics off, Telegram start-up
  closes every active conversation whose lane is a tab, with the existing `tab_gone` reason, and
  logs each closed lane and session. A closed conversation is resumable: `/sessions` offers Resume,
  and a reply to one of its messages switches the main view to it. `tab_gone` also leaves the
  lane's turn and model choice alone, and if topics come back on, writing in the old tab reopens
  its conversation there. Turns already queued in a tab lane still run, and with the client guard
  their replies land in the main view.
- **Approvals need no move.** Every start expires the pending approvals before Telegram starts,
  so none offered in a tab is left to press after the switch.
- **The switch is read at start-up, as before.** Flipping topics in BotFather takes effect at
  the next restart. The close runs on every start with topics off and does nothing once no
  conversation is left in a tab.
- **A reaction's reconsider turn runs in the main view.** With topics off, a 👎 on a reply first
  sent in a tab queues its reconsider turn in the main view, which switches to the reply's
  conversation, instead of in the tab the reply was sent to, which would make that conversation
  active in a tab again.

## Approach

1. **Client guard (wave 1).** Keep the topics flag in the client, set by `GetMe`, with a
   single-flight ask for a send that carries a thread before the flag is known. Return 0 from the
   send and indicator threads while it is off, before any route is resolved. Make `Tabs.tabsOn`
   read the client's flag instead of its own copy. Integration tests with topics off: no request
   of any method carries `message_thread_id`, including chat replies, typing, drafts, task reports,
   approval offers and resends, and provenance answers; and a send with a stored thread before
   start-up carries none.
2. **Start-up with topics off (wave 2, after the guard).** Add a store method that closes every
   active session with a non-zero thread as `tab_gone`, returning the lanes and sessions closed,
   and a `conversation.Service` method over it. Call it from `telegram.Service.Start` when `getMe`
   reports topics off.
   Integration tests, seeding tab state with topics on and restarting with topics off:
   - a session active in a tab is closed; `/sessions` lists it with Resume, and resuming it makes
     it the main view's conversation;
   - a reply to one of its earlier answers switches the main view to it;
   - a request queued in a tab lane before the restart delivers its reply in the main view;
   - a second start changes nothing, and a start with topics on closes nothing;
   - restarting with topics on again, a message in the old tab reopens its conversation.
3. **Docs.** In the README's Tabs section, say what turning topics off does to existing tabs and
   that it takes a restart.
4. **Operator step.** Deploy, turn topics off in BotFather, and restart.

## Files affected

- `internal/telegram/client.go`, `internal/telegram/feeds.go`, `internal/telegram/tabset.go`,
  `internal/telegram/tabnames.go`, `internal/telegram/trips.go`, `internal/telegram/service.go`,
  and the Telegram integration tests and harness
- `internal/conversation/service.go`, `internal/conversation/types.go`,
  `internal/conversation/reaction.go`
- `internal/store/sqlite/conversation.go`
- `README.md`, `docs/plans/README.md`

## Validation

- `gofmt`, the `go mod tidy` check, `go vet`, `go test ./...`, the race suite, `go build`, and
  `scripts/check-plans`.
- The existing tab tests still pass, since the guard and the start-up steps apply only with
  topics off.
- Live: after the deploy with topics off, a message in the main view is answered there, a
  reminder lands there, and `/sessions` resumes an earlier tab conversation.

## Risks and open questions

- Closing has the effects of any close: the conversation's photos are stripped from its history
  at the next hourly prune, its attachments are deleted at the next start-up, and it is deleted
  after 90 days unless resumed. Turning tabs back on later restores only what is left.
- `/sessions` lists 10 of the most recently active conversations, so an older tab conversation
  is reached by replying to one of its messages.
- A turn queued in a tab while the service restarts runs in its tab's lane, beside the main view.
  Resuming its conversation in the main view while that turn runs could put two turns on one
  runtime session, and the turn cannot read its conversation's attachments, since the
  conversation is closed. The window is the length of one turn after a restart.
- An approval notice is kept for the lane of the tab its proposal was made in. A proposal a
  queued tab turn makes after the restart, and the expiry notices of proposals made in a tab,
  reach no main-view turn. The card itself still answers its press.
- A misread switch would close every tab's conversation on restart. The log names each closed
  lane and session, so they can be put back by hand.
- How Telegram shows a chat's existing topics once the bot turns them off is not documented.
  The plan relies only on the bot no longer naming threads.
