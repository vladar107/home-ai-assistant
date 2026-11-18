# 0162 — Heads-up: one morning run instead of two

Status: `in-progress`

## Context

Heads-up runs at 07:30 and 18:30 (`HeadsUpCron = "30 7,18 * * *"`,
`internal/schedule/headsup.go`). The user asked for a morning report only: the evening run is to
go, and everything else about Heads-up stays as it is (backlog item `d4ab646c`).

A Heads-up job is an ordinary model-turn job with a fixed ID. A user may move it in conversation,
and `MigrateHeadsUp` already moves jobs still on an older built-in schedule (`0 8,19 * * *`) while
leaving schedules the user chose alone.

## Goal

- A new Heads-up job runs once a day at 07:30.
- An existing Heads-up job still on either earlier built-in schedule (`30 7,18 * * *` or
  `0 8,19 * * *`) moves to 07:30 once at startup, keeping its notes, model, travel pause, and
  enabled state. A schedule the user chose stays untouched.
- `/settings` → Heads-up, the README, and the docs say 07:30 only.

## Decisions

- The built-in method is unchanged. Its rule "from 15:00, include tomorrow" still serves a user
  who moves Heads-up to the evening, so the method keeps working on any schedule.
- `MigrateHeadsUp` takes a list of earlier built-in schedules instead of one. Each runs through
  the existing path (`Prepare` + `Update`, which recomputes the next run).
- The evening eval case `h7-evening-heads-up` stays: it exercises the method on a run at 18:30,
  which a user-moved job still gets.

## Approach

1. Set `HeadsUpCron` to `30 7 * * *`. Replace `legacyHeadsUpCron` with a list of the earlier
   built-in schedules, and let `MigrateHeadsUp` move jobs on any of them.
2. Change the English and Russian status lines on the Heads-up settings page to name 07:30 only,
   and fix the section's "twice-daily" doc comment.
3. Update the README's Heads-up paragraph and `docs/architecture.md`, and add an upgrade note to
   `docs/operations.md`: at startup a job on 07:30 and 18:30 moves to 07:30.

## Files affected

- `internal/schedule/headsup.go`, `internal/schedule/travel_test.go`
- `internal/telegram/headsup.go` and the settings-page integration tests that name the times
- `README.md`, `docs/architecture.md`, `docs/operations.md`

## Validation

- Integration tests with a real temporary SQLite store: a Heads-up job on `30 7,18 * * *` and
  one on `0 8,19 * * *` both move to `30 7 * * *` with the next 07:30 as their next run, and a
  second migration changes nothing. A job with the Heads-up ID on a schedule the user chose, and
  a job with another ID on the old schedule, are left alone.
- The configured format, module, test, vet, build, and plan checks.
- After deploy, `/jobs` shows Heads-up at 07:30 only, and no Heads-up arrives at 18:30.

## Risks and open questions

- A user who wants the evening run back moves the job in conversation; a schedule the user chose
  is never migrated.
