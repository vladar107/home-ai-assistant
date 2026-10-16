# 0090 — Commands sent from the main view stay there

Status: `done`

## Context

Backlog item `94a1f124` says that commands such as `/sessions` or backlog
management must not open a new tab. They should answer where the user is.

With topics on, the Telegram client turns every message sent from the main
view into a new tab, and commands are no exception. The production logs show
the same order each time `/sessions` was sent from the main view, at 15:17 and
16:56 on 2026-10-16:

1. A `forum_topic_created` from the user arrives, with an implicit name.
2. About a second later, the command arrives inside that new tab.

The bot answers in the tab the update came from (plan 0083), so the answer
lands in the new tab, and the main view gets nothing. The bot cannot stop the
client from making the tab. The BotFather setting that forbids users to create
tabs would stop it, but it would also stop the user opening tabs on purpose.

## Goal

A command whose message is the first one in a tab the user has just opened is
treated as sent from the main view:

- It runs for the main view's conversation, and its answer goes to the main
  view.
- The tab Telegram made for it is deleted.

An ordinary first message still opens its tab, as today. With topics off,
nothing changes.

## Decisions

- **Fresh tabs are remembered briefly.** When a `forum_topic_created` from
  the user arrives as its own service message, the service records the tab and
  the time in memory.
  - If the tab's first message from the user is a command and arrives within
    two minutes of the creation, the command is treated as sent from the main
    view. Any other first message clears the entry.
  - The entry is cleared only once the command is answered, so an update that
    fails and is retried is still moved.
  - Entries past their window are dropped whenever a new tab is noted, so tabs
    that never get a message are not kept.
  - A restart between the two forgets the entry. That window is about a
    second, so the cost is one command answered in its new tab, as today.
- **The command runs in the main lane.**
  - The update's context is moved to thread 0 before the command is handled,
    so `/sessions`, `/new`, `/cancel`, `/close`, and the rest act on the main
    view.
  - The answer is not sent as a reply to the command message, because a reply
    to a message in a tab would land in that tab.
  - Both paths share one command dispatch, so a command that gets special
    handling later is moved the same way.
  - Each moved command is logged as `telegram.tab.command_moved` with IDs
    only.
- **The tab is deleted after the answer is sent.**
  - `deleteForumTopic` removes the tab, with the command message in it, and
    the registry row and caches are forgotten.
  - A failed delete is logged as `telegram.tab.command_tab_left` with IDs
    only, and the tab stays. The answer has already reached the main view. A
    delete Telegram answers with "thread not found" counts as done.
- **A tab opened on purpose is treated the same way when its first message is
  a command.** The client gives the bot no way to tell a tab opened from the
  main view apart from one opened with a button. The tab holds nothing but
  the command, and the answer reaches the main view.

## Approach

### Chunks

1. **Re-route and delete.** Done by the orchestrator, since it is one small
   change in one package.
   - In `internal/telegram`: record fresh tabs in `processTopicService`.
     `processUpdate` then takes the entry, re-routes the command to the main
     lane, sends without a reply, and deletes the tab.
   - Acceptance, from integration tests against the tab simulator:
     - `/sessions` as the first message of a tab the user just opened is
       answered in the main view, and the tab is deleted and forgotten.
     - An ordinary first message stays in its tab.
     - A command later in a tab, or more than two minutes after creation,
       stays in the tab.
     - A failed delete still leaves the answer in the main view.
     - A command whose answer failed is retried and still answered in the main
       view.
     - With topics off, nothing changes.
2. **Docs.** The README's tabs paragraph.

## Files affected

- `internal/telegram/`: `service.go`, `tabs.go`, `tabs_integration_test.go`
- `README.md`, `docs/plans/README.md`

## Validation

- `gofmt`, a `go mod tidy` diff, `go vet`, `go test -race ./...`,
  `go build`, and `scripts/check-plans`.
- A live check: send `/sessions` from the main view, and see the answer there
  and no new tab left behind.

## Risks and open questions

- The command message itself disappears with the tab. Only its answer stays,
  in the main view.
- A command sent first in a tab opened on purpose acts on the main view:
  `/new` starts a new main-view conversation, `/cancel` cancels a turn running
  there, and `/close` answers that the main view cannot be closed and deletes
  the new tab.
- If the client ever sends the command before the creation, the entry is not
  there yet, and the command is answered in its tab, as today.
