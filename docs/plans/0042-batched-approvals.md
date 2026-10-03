# 0042 — Batched approvals

Status: `in-progress`

## Context

The approval core keeps at most one pending proposal per principal:
`CreateApproval` expires the previous pending record with reason `replaced`,
and the `approvals_one_pending_principal_idx` unique index enforces it. The
Notion, NAS, and schedule write guidance therefore tells the model to propose
a single change per turn. A request like "move both dates" becomes a chain of
turns: the model proposes one change, waits for the user to confirm it, and
only then proposes the next. That is slow and the user does not expect it.

Scheduled turns are read-only, so proposals only come from chat turns, and a
turn's proposals reach Telegram together through `AfterDelivery` after the
reply.

## Goal

Every proposal made during one chat turn, by any connector, is shown in one
Telegram message with one Confirm and one Reject button. Confirm executes the
items in the order they were proposed, and the message then shows each item's
outcome. A proposal made in a later turn still replaces a batch that is
waiting.

## Decisions

- Batching lives in the approval core, not in per-connector batch tools, so
  Notion, NAS, Home Assistant, and schedule writes all batch the same way.
- A bundle is the principal's pending records whose message has not been
  delivered yet (`chat_id IS NULL`). A new proposal joins that bundle while
  the bundle is younger than half the proposal lifetime; otherwise, or when
  the principal's pending records were already delivered, they are all
  replaced and the proposal starts a new bundle. This keeps a turn whose
  reply was never delivered from leaking stale items, or a near-expired
  deadline, into a later turn.
- Joined records take the bundle's `created_at` and `expires_at`, so a bundle
  expires as one unit.
- A proposal whose arguments hash equals an item already in the bundle is not
  added again. A proposal cannot be withdrawn within the turn, and the
  guidance says so: propose each change once, after reading the target.
- Confirm moves every pending item of the bundle to `confirmed` in one
  statement, then executes them in order and continues past a failed item.
  Each item keeps its own terminal status and outcome; Notion has no
  transactions, so there is no rollback. Each item records `started_at`
  before it runs, so a restart marks confirmed items that never started as
  `expired` with reason `restart` (nothing was written) and only a started
  one as `failed` with reason `interrupted`. A record confirmed before the
  upgrade counts as started.
- Records gain `bundle_id` (the first record's ID), `position`, and
  `started_at`. Existing rows get `bundle_id = id` and `position = 0`, so the
  `approval:<action>:<id>` callback shape is unchanged and an old proposal
  message keeps working. `Press` resolves the bundle from the record it
  names.
- A one-item bundle renders exactly as today. A larger bundle numbers its
  items under a short header; each item shows its own status line, or one
  footer when every item's status line is the same (waiting, rejected,
  expired, replaced, or all done with the same outcome).
- A bundle holds at most 10 items. Its summaries, with room for an intent
  warning on every item, must fit 2500 UTF-16 units; item outcomes are cut
  to 120 runes in a multi-item rendering. That keeps every rendering under
  Telegram's 4096-unit limit. A proposal that does not fit returns
  `approval.ErrBundleFull`, which every connector passes to the model as a
  tool failure telling it to have the user resolve the waiting batch first.
- `Propose` is serialized with its own mutex, separate from the service's
  state mutex, so parallel tool calls in one turn cannot race when joining a
  bundle. With parallel calls, item order is arrival order. Delivery holds
  the same mutex while it sends and records the message, so no proposal can
  join a bundle after it was rendered.
- The intent check runs per item, concurrently, and prefixes the warning to
  the mismatched item's summary.
- Expiry, restart, and undelivered resend group records by bundle and edit
  each bundle's message once with the full bundle rendering. The next-turn
  notice is one notice per principal covering the whole bundle, one sentence
  per item.
- Telemetry stays per record; logs gain `bundle_id`.

## Approach

1. Migration `012_approval_bundles.sql`: rebuild `approvals` (precedent
   `011_one_time_jobs.sql`) with `bundle_id` (`length = 16`), `position`
   (`>= 0`), and `started_at`, copy rows with `bundle_id = id`, drop the
   one-pending-per-principal index, and index `(bundle_id, position)`.
2. Approval core: `Record` gains `BundleID`, `Position`, and `StartedAt`.
   Repository methods move to bundle granularity: `PendingApprovals`,
   `LoadBundle`, `ConfirmBundle`, `StartApproval`, and `SetApprovalMessage`
   by bundle; `CreateApproval` either appends to the given bundle or
   replaces every pending record. `Propose`, `AfterDelivery`, `Press`,
   `report`, the sweep, notices, and rendering work on bundles.
3. SQLite store: implement the repository changes, including the restart
   split on `started_at`.
4. Connectors and model text: change the Notion, NAS, and schedule write
   guidance ("Only one proposal can wait at a time…") to "propose every
   change the request needs in this turn, once each; they are shown and
   confirmed together", and make `ProposedText` speak of the proposal being
   shown with the turn's other proposals. Add an `errors.Is(err,
   approval.ErrBundleFull)` branch in Notion, NAS, schedule, and Home
   Assistant, which today mask every proposer error.
5. Tests: update the existing approval tests to the new semantics, keeping
   the cross-turn replacement case, and add an integration test in
   `internal/telegram/approval_integration_test.go`: two proposals in one
   turn arrive as one message, Confirm executes both with one failing and
   the other still running, the message shows per-item outcomes, and the
   next turn's notice covers both.

## Files affected

- `internal/store/sqlite/migrations/012_approval_bundles.sql`
- `internal/approval/approval.go`
- `internal/store/sqlite/approvals.go`
- `internal/connectors/notion/writes.go`, `internal/connectors/nas/writes.go`,
  `internal/schedule/tools.go`, `internal/connectors/homeassistant/confirmed.go`
- `internal/telegram/approval_integration_test.go` and other existing
  approval tests affected by changed signatures or behavior

## Validation

- Formatting, module, vet, test, build, and plan checks.
- Live check on the operator's GoLand server: ask to move two Notion dates
  in one message and confirm a single message lists both changes and
  executes both.

## Risks and open questions

- Telegram updates are processed one at a time, so executing a bundle in the
  callback handler stalls the bot for every user until it finishes; ten
  items at the 45-second per-item timeout is the worst case. Typical Notion
  writes take about a second each.
- The join window is time-based, not turn-based: a turn whose proposals are
  more than half the lifetime apart replaces its own earlier ones, and a
  turn whose reply was cancelled can pass its proposals to a turn that
  follows within that window. Both are rare at the default 10 minutes.
- A wrong proposal cannot be dropped from a bundle before the user sees it;
  the user rejects the whole bundle and asks again.
