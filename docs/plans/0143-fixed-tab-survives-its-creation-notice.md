# 0143 — A fixed tab survives its own creation notice

Status: `done`

## Context

When the bot makes a tab for a feed, a trip, or a project, it creates the topic and then records
the tab with its subject. Telegram also sends the bot a service message saying the topic was
created, and the poller records a tab from that message as an ordinary tab the bot named.

The two writes race. When the service message is recorded first, the subject insert fails on the
tab's unique key. The bot then takes the tab for unrecorded, deletes it, and the delivery goes to
the main view. On the deployed bot this happened to a feed tab: the log shows the service message
handled a few milliseconds before the insert failed, and the next delivery three hours later made
the tab again.

## Goal

A tab the bot makes for a subject is recorded as that subject's tab whichever write lands first,
and the delivery that made it lands in it.

## Decisions

- **Recording a subject tab takes over the row its creation notice made.** The notice of a tab
  the assistant made is recorded as a tab the assistant named, which the deployed bot's log
  confirms. The insert becomes an upsert: a row for the same tab that has no subject and that the
  user did not name gets the subject, the fixed name, and `fixed` naming. A row that already
  serves a subject, or that the user named, is not taken over and the insert fails as it does
  today.
- **The creation notice never undoes a subject.** Adding a tab from a service message already
  leaves a known row as it is; that stays, and a test pins it for a row with a subject.
- **Nothing else changes.** Deleting the tab when the record truly fails stays as the fallback.

## Approach

1. Store: the upsert in `AddSubjectTab`, with the two refusals above.
2. Tests: a store test for each order of the two writes and for the refusals; a Telegram
   integration test in which the service message of a new feed tab is delivered before the record
   is written, and the delivery still lands in the tab and no topic is deleted. The same for an
   object's tab.

## Files affected

- `internal/store/sqlite/tabs.go` and its tests
- `internal/telegram` integration tests, and the Telegram simulator only if it cannot yet order
  the service message before the record

## Validation

- Formatting, module, vet, build, race and non-race tests, and `scripts/check-plans`.
- The new tests fail without the upsert.

## Risks and open questions

- None open. The creation notice of a tab the assistant made fills neither the known nor the
  confirmed cache, the record fills the known one only after it succeeds, and the tab namer and a
  user's rename both leave a `fixed` row alone, so a taken-over tab is as a tab the assistant just
  made.
