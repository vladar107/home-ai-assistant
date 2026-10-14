# 0079 — Act and tell: fewer confirmations, with Undo

Status: `in-progress`

## Context

In the 30 days to 2026-10-14 the assistant sent about 180 Confirm cards. Most
came from tools that propose every time, even in a conversation that has read
nothing from outside and for a change the user asked for in so many words,
though the change touches only the user's own data:

- scheduled jobs: 61 cards, 26 rejected;
- watches: 14;
- saved routes and memory instructions;
- Notion property updates: 30, mostly movie scores and dates.

The user wants the assistant to have more agency without losing safety.

Other tools already act directly in a clean chat and propose only after
outside content (plan 0051): reminders, background tasks, the backlog, memory
facts, and ordinary Home Assistant actions. That rule holds up, and this plan
extends it.

## Goal

- In a clean chat turn (defined below), the tools listed below act at once.
  The reply is followed by a "Done" message with an Undo button per action.
- In any other turn they propose, as today.
- Every direct action can be undone by its owner for 24 hours, exactly once,
  unless the object has changed since.
- Fewer cards: the share of `done` among `done` + `proposed` approvals is the
  measure.

## Decisions

### Policy

- **One fixed policy** for every user, with no setting (the operator's
  choice).
- **Act directly** in a clean turn:
  - `schedule_propose`, `schedule_update`, `schedule_remove`;
  - `watch_propose`, `watch_update`, `watch_remove`;
  - `route_save`, `route_update`, `route_remove`;
  - `memory_add` and `memory_replace` of an instruction;
  - `notion_update_page`, including the movies tools that build one.
- **Always confirm:**
  - `notion_create_page` and `notion_append_blocks`;
  - `nas_save_attachment`. Undoing it would need a delete permission on the
    NAS, which the connector deliberately lacks, and it sent no cards in the
    last 30 days.
  - Home Assistant devices labelled as needing confirmation;
  - lessons the assistant infers itself (plan 0078).
- **Clean turn.** `Act` acts only when every condition below holds, and it
  fails closed when any piece is missing:
  - the turn is a chat turn, `outbox.ChatRequest(tools.TurnFrom(ctx))`;
  - the run exists and was started by the user's own message,
    `tools.RunFrom(ctx).UserMessage()`, which excludes continue, reconsider,
    and Confirm turns;
  - `exposure.External` is false. For memory instructions, the stricter
    `ConversationExposed` must also be false: that is memory's existing rule,
    and it counts household content too.
- **Attachments count as outside content.** A forwarded file or photo can
  carry instructions, and it stays readable in the conversation after its
  turn. So `read_attachment` becomes a third-party, non-household read, and
  a turn that carries an attachment marks its conversation exposed when it
  starts, which also covers images kept in the session history. Sending a
  photo therefore turns later writes in that conversation into proposals,
  until `/new`.
- **Notion and NAS reads stay "private", not outside content,** as plan 0051
  accepted for reminders. A turn that read a Notion page can still update a
  Notion property directly. Memory instructions are protected by their
  stricter rule.
- **Parallel calls.** Today the broker marks a run external only after a
  third-party read returns, so in a batch with `fetch_page` and
  `schedule_propose` the write could be evaluated first.
  - The broker now also marks the *run* external when a third-party,
    non-household call starts: after the grant recheck, just before the
    provider runs.
  - The post-success marks stay as they are: `MarkExternal` with the result
    text, which web fetch's address memory needs, and the conversation mark.
  - A failed outside call therefore makes the rest of its run confirm, and
    leaves the next turn clean.
  - `Act` checks `External` after taking its per-principal lock.
  - The window is narrowed, not closed: calls arrive as separate bridge
    requests, so a write evaluated before the outside call even starts still
    acts. That is accepted as residual risk.
- **Duplicates in one turn.** A repeated identical direct call (same tool and
  argument hash, same turn) returns the first record's result instead of
  acting twice, as proposals are deduplicated today. Records carry their
  turn's request ID for this, and for the "Done" message.

### Records and Undo

- **Direct actions are approval records in the `direct` mode.**
  - A new store method inserts the record already `confirmed`, with
    `started_at` set. It skips the bundle replacement in `CreateApproval`, so
    a direct action never expires a bundle the same turn is building. A
    restart still marks an interrupted record `failed`.
  - `execute` then runs the tool's existing `ExecuteApproved`, and the record
    ends `done` or `failed`.
  - Pending-bundle queries ignore `direct` records.
- **Migration 027 rebuilds `approvals`.** The table is `STRICT`, and its
  status checks cannot be altered in place. The rebuild adds:
  - the `mode` column (`proposed`, `direct`), and `request_id`, the chat
    turn of a direct record;
  - the `done` and `undone` statuses, in both the status check and the
    `finished_at` check;
  - an uncapped `undo` JSON column and `undo_until`. The `outcome` column is
    capped at 1,000 characters, too small for a job definition.
- **`Outcome` gains `Undo`**, the payload `ExecuteApproved` returns, which is
  kept apart from `Arguments` (whose hash stays canonical). The executor
  fills it when it writes, because a create's ID and Notion's previous
  values are known only then. `Act` stores it when the record finishes, and
  an optional `Undoer` interface on the executor applies it.
- **Wiring.** `approval.Service` gets the exposure check through a setter,
  like `SetIntentChecker`, because approvals are built before exposure in
  `internal/app`. There is no import cycle.
- **Execution deadline.** A write inside a tool call runs under the tool's
  deadline, 15 seconds by default, not the 45 seconds `execute` gives after
  Confirm. The direct-acting tools' definitions set `Timeout: 45s`. Notion
  needs a read and a write with a retry.
- **Locks.** No provider mutex may be held across `Act`. Memory already
  proposes without holding `s.mu`.
- **What each executor captures, and what Undo does:**

  | Action | Captured | Undo |
  |---|---|---|
  | Create a job, watch, or route | its ID | delete it |
  | Update a job, watch, or route | the previous definition, `Enabled`, and `UpdatedAt` | restore them |
  | Remove a job | the whole row, loaded before the delete | recreate it with the same ID; run history is gone, reports remain. A one-time job whose time has passed cannot be restored, and Undo says so |
  | Remove a watch | the whole definition with `Enabled` | recreate it; its state and alert history start fresh |
  | Remove a route | the whole route | recreate it |
  | Memory instruction added | the new entry ID | remove it |
  | Memory instruction replaced | the old text and `UpdatedAt` | restore the old text |
  | Notion update | the previous values, converted from the API response shape to the request shape, for the property types `checkRowProperties` already accepts | write them back, after reading the page and finding the values the action wrote unchanged; other property types get no Undo |

- **Undo is refused**, with a short reason on the "Done" line, when:
  - another principal presses it;
  - it is pressed after `undo_until`, or a second time;
  - the object changed since (`UpdatedAt`, or the Notion values);
  - the object is gone.

### What the user and the model see

- **The "Done" message.**
  - After the reply, `approval.AfterDelivery` sends one message per turn
    listing its direct actions, found by the turn's request ID, one line each, from the summary a card would
    have shown.
  - A new deliverer method sends an inline keyboard with one Undo button per
    action, row by row, and edits it as buttons are pressed.
  - The callback data is `undo:<record id>`, parsed by its own parser in the
    Telegram callback dispatch.
  - When a turn also has proposals, the "Done" message comes first, then the
    bundle.
  - Delivery is best-effort. The action stands even if the message is lost,
    and `/jobs`, `/memory`, and the others still show it. Conversations and
    callbacks are private-chat only, so groups do not arise.
- **The model.**
  - A direct tool result says the action is done and the user can undo it.
  - Each affected tool's description and guidance stop saying "the user must
    confirm" and instead say a change made after outside content needs
    Confirm.
  - Notices gain `done` and `undone` branches. The next turn learns that the
    user pressed Undo, the way it learns of a Reject.
  - `proposal_withdraw` on a direct record answers that it is already done
    and the user can undo it.
- **The intent check, after the fact.** It still runs after delivery and is
  advisory. For a direct record, the store method that sets the warning is
  allowed to write to `done` records. A flagged action's "Done" line carries
  the warning beside its Undo button. Its metric is counted when the record
  finishes, without the warning.
- **Metrics.** `home_agent.approvals` gains the statuses `done` and `undone`.
  The home-monitoring dashboard's confirm and reject panels exclude them, and
  a new panel shows direct actions and the undo rate per tool.

## Approach

Waves and chunks:

1. **Wave 1, approval core** (`internal/approval`, `internal/store/sqlite`,
   `internal/tools/broker.go`, `internal/attachment`, `internal/conversation`,
   `internal/app`):
   - migration `027` (after plan 0080's `026`) and the direct store path;
   - `Act`, with the clean-turn gate (`approval.Service` gets the exposure
     service) and deduplication;
   - `Undoer` and `Proposal.Undo`;
   - undo execution, with ownership, deadline, single use, and staleness
     delegated to the Undoer;
   - notices and `proposal_withdraw` text;
   - the after-the-fact intent warning;
   - the early external mark in the broker;
   - `read_attachment` as third-party, and the exposure mark for turns that
     carry attachments;
   - composing the "Done" message in `AfterDelivery`, and the new
     `Deliverer` method's signature, with a fake deliverer in tests;
   - integration tests with a fake executor.
2. **Wave 2, in parallel:**
   - **Telegram:** the client implementation of the new `Deliverer` method,
     with keyboard edits; the `undo:` callback; `help.go` text about
     confirmation. End-to-end tests in a new
     `internal/telegram/undo_integration_test.go`, using a fake executor.
   - **Own-data tools:** schedule, watch, and routes switch to `Act`, with
     undo capture, Undoers, explicit timeouts, and updated descriptions and
     guidance. Tests in `internal/telegram/act_own_data_integration_test.go`
     assert on stored state and approval records, not on the "Done" message,
     which the Telegram chunk covers.
   - **Memory and Notion:** memory instructions with the stricter rule, and
     Notion updates with value conversion and Undo. Updated descriptions and
     guidance. Tests in
     `internal/telegram/act_memory_notion_integration_test.go`, likewise on
     state and records.
3. **Wave 3, docs** (orchestrator):
   - README, `docs/architecture.md` (approval section, tiers),
     `docs/operations.md` (routes and the approval table);
   - the home-monitoring dashboard panels.

## Files affected

- `internal/approval/approval.go`, `internal/approval/withdraw.go`, migration
  `027`, `internal/store/sqlite/approvals.go`, `internal/tools/broker.go`
- `internal/telegram` (client keyboard method, callback dispatch, the "Done"
  message)
- `internal/schedule/tools.go`, `internal/watch/tools.go`,
  `internal/routes/tools.go`, `internal/memory/tools.go`,
  `internal/connectors/notion/writes.go` and `movies.go`
- README, `docs/architecture.md`, `docs/operations.md`
- `~/src/home-monitoring`: the home-agent dashboard

## Validation

- **Act's gate**, tested directly with a fake executor. `Act` acts only for
  a chat turn with a user message that is not external; anything else, or
  anything missing, proposes. A turn carrying an attachment, and a later
  turn after `read_attachment`, propose. A batch that runs a third-party read
  and a write concurrently proposes. An identical repeated call acts once.
- **Each tool family** in a clean turn: the change is applied at once, the
  "Done" message follows the reply and precedes any bundle, and Undo restores
  the previous state. After outside content, the same call produces a Confirm
  card.
- **Refusals.** Undo by another principal, after 24 hours, a second time, or
  after the object changed is refused, with its reason on the "Done" line.
- **Turn kinds.** Reconsider and Confirm-continuation turns propose. Memory
  instructions propose after household content.
- **Notion,** through the fake Notion server: Undo restores the previous
  values, and a property changed in between refuses Undo.
- **Metrics.** `home_agent.approvals` is emitted with `done` and `undone`.
- Formatting, module, test, vet, build, and plan checks.

## Risks and open questions

- **Undo is weaker than not doing it.** A deleted watch loses its alert
  history, a job's past runs are not restored, and a passed one-time job
  cannot come back. This is accepted for the user's own data. Actions with
  effects outside stay behind Confirm.
- **The model acts on misunderstandings.** A wrong job or watch exists until
  the user notices. The "Done" message, the intent warning, and Undo are the
  mitigation, and undo rates per tool show where it fails.
- **Notion is shared with whoever else edits it.** Undo works only while
  nobody else has changed the property.
- **The parallel window.** A direct write evaluated before a concurrent
  outside read has started still acts.
- **Private reads can steer Notion updates.** A Notion page a guest edited
  can influence a direct Notion property update in the same turn, as it
  already can a reminder.
