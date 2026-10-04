# 0054 — Continue after Confirm, and runtime hygiene

Status: `done`

## Context

A request that needs several writes in sequence stalls after the first one.
The model proposes a write, the turn ends, and the user presses Confirm. The
action runs and its outcome is kept as a notice, but the model only sees that
notice on the user's *next* message. So the next step happens only after the
user nudges the bot ("go on"), once per step. Writes the model can propose
together already share one bundle (plan 0042), but a step that depends on an
earlier one, or one the model only decides on after seeing the result, cannot
be proposed in the same turn.

A concurrency and performance audit also found issues worth fixing:

- Shutdown can take longer than Docker's default 10 s stop grace period. The
  services stop one after another. Then the deferred telemetry flush gets a
  second full `ShutdownTimeout` (`internal/app/app.go:120-126`). With Alloy
  unreachable, the total is about 16 s, Docker kills the container, and the
  last metrics and traces are lost.
- The `/jobs` "Run now" button is not idempotent. When the edit or reply after
  the press fails briefly, the poller processes the same update again and
  `RunNow` queues a second run. Its `scheduled_for` is a fresh `now()`, so the
  `UNIQUE (job_id, scheduled_for)` guard does not catch it.
- `approval.Service.proposing` is one mutex for everyone, and `AfterDelivery`
  holds it while sending the bundle to Telegram. That send can take up to
  35 s, so another user's write tool can block past its 15 s tool deadline.
- Pi session files are never deleted. Every scheduled run and every
  background task writes a new one, and chat sessions with photos grow to
  tens of MB.
- Smaller items:
  - The comment on `conversation.Service.turns` still says the per-principal
    slot exists because of the `auth.json` refresh race. Pi 1.0.0 locks that
    file (plan 0048), and `RunTask` deliberately skips the slot.
  - `Enqueue` calls `Attachments.Discard` without checking that `Attachments`
    is set.
  - On SIGTERM, chat turns stop as soon as the signal arrives. Scheduled runs
    and tasks stop only when their `Shutdown` runs.
  - A process killed during a turn leaves `bridge-*` temp directories under
    the principal's profile directory.

## Goal

- After the user confirms a bundle, the model continues the original request
  on its own. If steps remain, it carries on, which may mean proposing the
  next write. If nothing remains, it stays silent. One Confirm moves the work
  forward one step, with no nudge.
- A deploy's stop finishes within Docker's grace period, with telemetry
  flushed or abandoned quickly.
- A replayed "Run now" press queues at most one run.
- One user's slow approval send never delays another user's proposal.
- Pi session files of finished scheduled runs and tasks are removed after the
  run retention period.
- The smaller items above are fixed.

## Decisions

- **Continuation is a normal conversation request.** When a Confirm press
  runs the bundle, the Telegram service queues a conversation request for the
  principal. It uses the callback's `update_id` as `telegram_update_id`, the
  proposal message as the message to reply to, and a fixed continuation
  instruction (`conversation.ContinueMessage`) as the message. This reuses the
  whole turn pipeline: the worker, the per-principal single pending request,
  delivery, cancel, and recovery.
- The request is queued before the proposal message is edited. A press the
  poller retries finds the bundle already resolved, so `Press` reports that
  nothing ran and no second continuation is queued; the update ID's unique
  key backs this up. Failing to queue is logged and costs only the automatic
  step, since the outcome still reaches the next turn as a notice.
- A continuation's message is not recorded as the user's message for the
  intent check, so actions stay judged against what the user asked.
- The continuation turn receives the approval outcome through the existing
  notice (`PendingNotice`), so the model sees exactly what ran and what it
  returned.
- Continuation runs when the press confirmed the bundle and ran its items,
  whether they ended `executed` or `failed`. A rejection or expiry means the
  user stopped, so no continuation is queued; the notice waits for their next
  message as it does today.
- If the principal already has a pending request (the user typed something
  after the proposal), the continuation is skipped. The notice reaches the
  user's own turn instead, and nothing is queued behind it.
- **No-op continuations stay silent.** The continuation instruction tells the
  model to reply with exactly `[nothing-to-add]` when every step of the
  request is done and the bundle message already says everything. Delivery
  removes the token from any reply; when nothing else is left, nothing is
  sent and the request is marked delivered. An empty reply without the token
  still becomes the usual failure text.
- The guidance lives in the tool-turn system prompt rather than in each write
  tool's description: after a confirmation the model is asked to continue,
  so it proposes an earlier step now and takes the dependent one then.
- No loop guard beyond the user: every further write still needs its own
  Confirm, and a continuation without a new proposal ends the chain.
- Shutdown: set `stop_grace_period: 30s` in `deploy/compose.app.yaml`. Give the
  telemetry shutdown a fixed 3 s budget instead of a second
  `ShutdownTimeout`.
- `Enqueue` discards staged attachments only when there are some, so a
  deployment without attachment support cannot reach a nil `Attachments`.
- "Run now" idempotency: `RunNow` takes a trigger naming the press (bot ID
  and update ID). A manual run with a trigger takes its 16-character ID from
  a hash of the job ID and the trigger, and the insert ignores a conflict on
  `id` as well as on `(job_id, scheduled_for)`, so a retried press finds the
  run already queued. No migration is needed.
- `proposing` becomes one mutex per principal (a mutex map guarded by `mu`).
  Bundles belong to a single principal, so joining a bundle stays serialized
  where it matters.
- Session pruning goes into the scheduler's hourly prune through an optional
  `schedule.SessionPruner`, implemented by the Pi runtime. A session file is
  removed when no `conversation_sessions` row and no kept task refers to it
  and it was last modified more than `runRetention` (30 days) ago. Chat
  sessions stay available to `/sessions`, and a file still being written is
  never old enough.
- Conversation workers start from `context.WithoutCancel(ctx)` and are stopped
  by `Shutdown`, like the scheduler and tasks. That gives all three the same
  shutdown order and budget.
- At start-up, the Pi runtime removes leftover `bridge-*` directories under
  the profile directories. No turn is running yet at that point, so none of
  them is live.
- Not changed, by design: the Telegram poller processes updates one at a time
  and in order, and a scheduled run holds its scheduler slot while it waits
  for the principal's chat turn. Both are acceptable for a household with two
  users, and changing them would cost update ordering or add a second
  scheduling layer.

## Approach

1. Approval: `Press` returns `approval.Pressed` with the message text, whether
   the press was the principal's own, and whether it ran the bundle.
2. Telegram: callbacks carry the bot and update ID. A press that ran the
   bundle queues the continuation through `conversation.Service.Enqueue`;
   `ErrBusy` is logged as skipped.
3. Conversation: `ContinueMessage` and `NothingToAdd`, the silent delivery,
   and no intent recording for a continuation. The Pi tool prompt says what
   happens after a confirmation.
4. Approval: per-principal `proposing` locks.
5. Schedule and store: `RunNow` takes a trigger; the run insert ignores an ID
   conflict.
6. Pi runtime and store: `SessionPruner` and `ReferencedSessions`, wired into
   the scheduler in `app`.
7. App and deploy: telemetry shutdown budget, `stop_grace_period`,
   conversation worker context, and the start-up `bridge-*` sweep in
   `pi.New`.
8. Fix the `turns` comment and the `Attachments` guard.
9. `docs/architecture.md` describes the continuation and session pruning.

## Files affected

- `internal/approval/approval.go`
- `internal/telegram/service.go`, `internal/telegram/jobs.go`
- `internal/conversation/service.go`
- `internal/runtime/pi/runtime.go`
- `internal/schedule/service.go`, `internal/store/sqlite/schedules.go`,
  `internal/store/sqlite/conversation_history.go`
- `internal/app/app.go`, `deploy/compose.app.yaml`, `docs/architecture.md`
- Integration tests in `internal/telegram`, a pruning test in
  `internal/runtime/pi`, and the `Press` callers in
  `internal/connectors/homeassistant`

## Validation

- Integration tests:
  - A Confirm queues one continuation turn, and a later press on the
    resolved proposal queues none.
  - A Reject queues no continuation.
  - A continuation replying `[nothing-to-add]` sends nothing.
  - A busy principal skips the continuation and keeps the notice.
  - A replayed "Run now" update creates one run.
  - Session pruning removes only unreferenced, old files.
- `go test -race ./...`, `go vet ./...`, formatting, build, and
  `scripts/check-plans`.
- Live check on the operator's server: ask for a two-step write that depends
  on the first result, confirm once, and see the second proposal arrive
  without a nudge.

## Risks and open questions

- Each Confirm now costs one more model turn (several seconds of Pi start-up
  plus tokens), even when nothing remains to do.
- The model may ignore the silence token and send a short "Done" anyway. The
  guidance and the live check should show whether the wording holds.
