# 0174 — Two tests that fail under load

Status: `in-progress`

## Context

A full local race run before plan 0173 failed two tests that pass alone and in CI:

- `TestCancelledDeliveryLetsTheNextMessageThroughAtOnce` (`internal/conversation`) failed with
  "bootstrap configured identities: begin configured principal bootstrap: context deadline
  exceeded". Its helper `newMetricsEnv` starts a 20-second context and only then opens its store
  with `testutil.OpenStore`. The first `OpenStore` in a test process also builds the migrated
  template, every migration under the race detector, and on a loaded machine that took the whole
  20 seconds before the test reached its first step. The conversation package has no parallel
  tests and no `TestMain`, and this test runs first, so it is the one that pays for the build.
  The build runs on `context.Background()` and the per-test `sqlite.Open` on `t.Context()`, so
  neither is bounded by the test's deadline once the deadline starts after them.
- `TestTurnWaitingBehindAnotherKeepsTyping` (`internal/telegram`) failed with "3 typing actions,
  want at least 5". While a turn waits for the lane's slot, the test sleeps a fixed 400 ms from
  the update and then counts the typing actions sent. Typing is a synchronous call on a 40 ms
  ticker that drops ticks when a call is slow, so a loaded machine sends fewer in that window. The
  same test's second check was already changed from a count to when typing last ran, for this
  reason; the first was not.

A search of the `*_test.go` files for a deadline context started in the same function before
`testutil.OpenStore`, `testutil.SeedStore` or `sqlite.Open` finds three helpers: `newMetricsEnv`
and the hint test's environment in `internal/conversation`, and
`newConversationHarnessWithOptions` in `internal/telegram`, whose 60-second deadline is long enough
that it has not failed. Other packages start their deadlines inside a test after the fixture is
built.

Plan 0173 makes the eval tests parallel, which adds load beside these tests in a full run.

## Goal

- Neither test fails under load for a reason other than the behaviour it checks, and both still
  verify what they verify today.

## Decisions

- **A test's deadline starts after its store is open.** The three helpers open the store first and
  start their deadline context after it, so the template's one-time build no longer counts
  against the test's steps. Building the template eagerly in every package's `TestMain` was
  considered and declined: it touches every package to fix an ordering mistake in three helpers.
- **The waiting turn's typing is waited for, not slept for.** The test waits, bounded by its
  context, until both the cadence's number of typing actions has been sent and typing has gone
  on for at least twice the draft delay since the first action, while the turn is still held. It
  then checks that typing never paused for more than the existing bound and that no draft was
  shown. The time condition keeps the draft check meaningful: a draft could first appear after
  one draft delay, and the count alone is reached before that on a fast machine.
- **The test also checks that the turn is held.** Before releasing the slot, it checks that the
  runtime has received no call for the waiting turn. Today this is only implied by the draft check.
- **The count in a fixed window gives way to the pause bound.** The old check also failed when
  typing came slower than the cadence but still within the pause bound, which is exactly what a
  loaded machine does. While the turn waits, the cadence is now checked only through the pause
  bound, as the test's second check already does after the release. No other assertion changes,
  and no test is made serial.

## Approach

Two `test:` commits; the first carries the plan and its index row.

1. Reorder the deadline and the store in `newMetricsEnv`, the hint test's environment and
   `newConversationHarnessWithOptions`.
2. In `TestTurnWaitingBehindAnotherKeepsTyping`, replace the fixed sleep with the bounded wait
   above and add the held-turn check, keeping the pause and draft checks.

Run `scripts/check-plans` before each commit.

## Files affected

- `internal/conversation/metrics_integration_test.go`, `internal/conversation/hint_integration_test.go`
- `internal/telegram/conversation_integration_test.go`
- `internal/telegram/draft_integration_test.go`
- `docs/plans/README.md`

## Validation

- Each changed package passes with and without `-race`.
- The deadline fix is shown deterministically: with a temporary, uncommitted sleep longer than 20
  seconds at the start of the template build, `TestCancelledDeliveryLetsTheNextMessageThroughAtOnce`
  fails with the same deadline error before the fix and passes after it.
- The typing fix is shown deterministically: with a temporary, uncommitted delay of about 100 ms
  in the fake Telegram server's chat action handler, the old test fails with "want at least 5"
  and the new one passes.
- As a secondary check, 20 separate single-count runs of each test beside a full
  `go test -race ./...`, all green.

## Risks and open questions

- **Real-time checks that stay.** The typing pause bound and the cancel test's one-second and
  two-second bounds are the behaviour those tests check and stay as they are; under extreme load
  they can still fail, and such a failure is not a failure of this plan.
- **Other load flakes.** `TestReminderSetInConversationNeedsNoConfirmation` timed out once in CI
  on 2026-11-18 and passed on rerun. It is not changed here; if it fails again, it gets its own
  plan.
