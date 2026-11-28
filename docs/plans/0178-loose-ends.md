# 0178 — Loose ends: deadlines nobody asked about, and promises the user made

Status: `done`

## Context

Mail holds deadlines that need no action until the user changes their mind:
- a free-cancellation window on a booking;
- a return window on an order;
- a subscription that renews;
- a trial that turns into a paid plan;
- a document or contract that expires.

The user also makes promises, in chat ("I need to send the forms to the
accountant by Friday") and in sent mail ("I'll send it tomorrow"). Nothing
follows either kind up today.

- **The inbox intake scan cannot find them.** It reads only unread mail,
  enforced in code (plan 0161). Its prompt also forbids reporting receipts and
  confirmations of what the user did (`internal/intake/scan.go`). A booking
  confirmation is both: usually opened, and a confirmation. Those rules are
  right for notices that interrupt the user, so they stay.
- **The Trips scan does not record them.** It records travel bookings but no
  cancellation deadline (plan 0086).
- **Reminders do not fit.** They fire at a set time whatever happened. They
  cannot be closed as done, listed as "what is still open", or followed up
  depending on what happened since.
- **The operator wants a quiet assistant** (plan 0167). Tracking must cost
  the user nothing until a deadline is near.

The idea comes from a review of a commercial personal assistant. Its most
praised habit was reminding users of reservations they had forgotten to cancel.

## Goal

- **Mail and calendar.** With **Loose ends** on in `/settings`, the assistant
  finds these deadlines once a day in mail and in calendar events, read or
  unread, and tracks them silently.
- **Promises.** A dated promise the user states in chat is tracked, and so is
  a deadline in content they send. Each is acknowledged in one short line.
  Dated promises in the user's sent mail are tracked too.
- **Nudges.** On the morning a tracked item comes due for a nudge, one message
  lists every such item, each with **Done**, **Tomorrow**, and **Drop**
  buttons. A day with nothing due sends nothing.
- **Questions.** The user can ask what is still open and close an item in
  chat.

## Decisions

### Process

- **Advisor consult (2026-11-27).** The question was whether to extend inbox
  intake and reminders, or to add a loose-ends store with its own daily scan.
  The decision was the new store and a separate daily scan, with nudges as
  their own message. Widening the intake scan would need exceptions to plan
  0161's code-enforced rules. A card per deadline contradicts plan 0167.
  Done, list, and follow-up need state that reminders do not have.
- **Plan critique (advisor, 2026-11-27).** It led to these changes:
  - sent-mail promises carry their own eval gate;
  - the job, switch, and callbacks are owned by one chunk, and the scan by
    another;
  - due dates, time zones, and catch-up are defined;
  - the account is part of the key;
  - deadlines found twice are merged;
  - the scan can close stale items;
  - the counterpart is taken from mail headers in code;
  - a later message in a thread annotates a promise and does not close it;
  - the first run reads 30 days back;
  - nudges are sent before the scan;
  - the live check runs the scan itself.

  The second round:
  - defined the backfill;
  - merged only within one thread;
  - took the calendar counterpart from the organiser;
  - required DMARC for closes;
  - fixed the commit and status order.

### Store

- **Package and tables.** Package `internal/looseend`. Migration
  `072_loose_ends.sql` adds two tables:
  - `loose_ends` has these columns:
    - principal and ID;
    - `kind`: `cancel_window`, `return_window`, `renewal`, `trial_end`,
      `expiry`, or `promise`;
    - `title` and `counterpart`, at most 120 characters each;
    - `due_at`, an instant in UTC, and `due_all_day`;
    - `nudge_on` and `nudged_on`, local dates;
    - `source` (`mail`, `sent`, `calendar`, or `chat`), with `account`,
      `source_ref`, and `thread_ref`, all `NOT NULL DEFAULT ''`;
    - `outside`;
    - `status`: `open`, `done`, `dropped`, `expired`, or `closed`;
    - the created and updated times.
  - `loose_end_scans` holds `read_until`, `backfill_until`, `stalls`, and
    `calendar_digest` per principal.
    - It also holds `backfill_before`, the backfill's moving newest edge,
      which was settled during implementation.
    - Both backfill columns are NULL once the backfill is drained.
- **Keys.**
  - `UNIQUE(principal_id, source, account, source_ref, kind)` makes a second
    detection of the same message, event, or chat item a no-op.
  - A chat item's `source_ref` is its own ID.
- **Merging.** A new mail item is not stored when an item that is open or a
  tombstone already has the same principal, kind, account, Gmail thread, and
  local due date. A confirmation and a reminder in one thread are then one
  item, so inbox mail items keep their thread in `thread_ref` too. Two
  bookings from one platform in separate threads stay two items. A reminder in
  a new thread is an accepted duplicate, preferred to merging two real
  deadlines.
- **Tombstones.** An item marked done, dropped, or closed keeps its row, so
  the scan and the merge never bring it back. Rows that are no longer open are
  pruned 90 days after their due date.
- **Caps, enforced in code.**
  - At most 10 new items per scan.
  - At most 2 new items per sender domain per scan, so one sender cannot
    flood the list.
  - At most 100 open items per principal.

  Items past a cap are left out and counted in the log.
- **A switch,** `looseend.Switch` (plan 0106), is defined with the domain, so
  the tools and the job read the same state.
- **Export.** A `looseends` section covers `loose_ends`. `loose_end_scans` is
  not exported (plan 0129).

### Time

- **One zone.** Every rule uses the job's zone, `HOME_AGENT_TIMEZONE`.
  Settled during implementation:
  - a date-only due is stored as 23:59:59 local;
  - **Tomorrow** is offered while the next morning's run comes before the due
    time;
  - a nudge day whose run would come at or after the due time moves to the day
    before. The run's time of day is read from the job's cron expression.

- **What `due` means.**
  - A date without a time means the end of that local day in the job's zone
    (`HOME_AGENT_TIMEZONE` by default), stored as that instant, with
    `due_all_day` set.
  - A date-time with an offset is kept as that instant. One without an offset
    is read in the job's zone.
- **Nudge day by kind.** The local date is reckoned in the job's zone.

  | Kind | First nudge | Second nudge |
  | --- | --- | --- |
  | Cancellation | 2 days before the due date | — |
  | Return, renewal, trial | 3 days before | — |
  | Expiry | 30 days before | 7 days before |
  | Promise | On the due day | — |

  An item found inside its lead time takes today as its nudge day.
- **Selection.** A run nudges every open item whose `nudge_on` is today or
  earlier and whose `nudged_on` is before `nudge_on`. That catches up a missed
  day. After a nudge, `nudged_on` is set. An expiry item then moves `nudge_on`
  to its second nudge, and every other item is not nudged again unless the
  user presses **Tomorrow**.
- **The buttons.**
  - **Tomorrow** sets `nudge_on` to the next local day. It is not shown on
    the due day, or when the next day's run would come at or after the due
    time.
  - Once the due time passes, an open item becomes `expired` with no message.

### The daily job

- **Job.** Built-in job type `looseend.scan`, named "Loose ends", runs daily
  at 07:20 in the job's zone. It runs on the user's own subscription, as every
  scan does.
- **Each run, in order:**
  1. Expire items that are past their due time.
  2. **Nudges.** Code writes and sends them, before any model turn, so a slow
     or failed scan never delays them or lands after Heads-up.
  3. **The scan,** when Gmail or Calendar is on and connected.
  4. A second, short nudge message, only when the scan found items already
     inside their lead time. It may arrive after Heads-up, which then does not
     see it. That is accepted.
- **Skipping the model turn.** The turn is skipped when two code-side checks
  both find nothing:
  - a Gmail search finds no inbox or sent message after the cursor;
  - the calendar digest is unchanged. The digest is a hash of the event IDs
    and update times in the next 60 days.
- **Settings and the job.** The settings section and the job's lifecycle
  follow the inbox intake section:
  - the switch is off by default;
  - turning it on creates or enables the job;
  - turning it off disables the job and forgets the cursor and digest;
  - **Check now** runs the job at once.

  Tracked items are kept while the switch is off.
- **Heads-up.** The nudge message is the job's report, so Heads-up at 07:30
  sees it through `job_reports` and, by its existing rule, does not repeat it.
  The implementing chunk verifies two things:
  - that `job_reports` returns this report;
  - that reading an outside-marked report in Heads-up changes nothing beyond
    exposure, since its tools are read-only.

### The scan

- **One unattended `RunCheck` turn,** as the inbox intake scan is.
- **What it reads.**
  - Inbox mail since `read_until`, both read and unread (no `is:unread`), at
    most 20 messages per run.
  - Later runs read from the cursor and never more than a week back.
  - Leftover messages do not advance the cursor past the newest message
    opened, as in the inbox intake. Items dropped by a cap do advance it,
    because caps exist against flooding.
  - **Backfill.** Switching on sets `backfill_until` to 30 days back. Until
    that window is drained, each daily run also offers the model the next
    candidates from it.
    - Code finds them with a Gmail search for a fixed list of deadline words:
      cancel, refund, return, renew, trial, expire, and their equivalents in
      the supported interface languages.
    - They share the run's 20-message budget, newest first.
    - The one-week rule does not apply to the backfill window.
    - The per-domain cap does not drop backfill items; it leaves them for the
      next run.

    The cost stays one turn a day. With a lot of matching mail, the backfill
    only takes more days.
  - Calendar events in the next 60 days whose description states a
    cancellation term.
  - Sent mail (see "Promises in sent mail" below).
- **What it answers.** Strict JSON:
  `{"items":[{"account","message_id" or "event_id","kind","title","due"}],"closes":[{"item_id","account","message_id"}],"left":0}`.
- **Validation in code.**
  - A mail item counts only when its message was opened in full this run,
    checked in the Gmail call log (plan 0161).
  - `counterpart` and the sender domain for the caps come from that message's
    `From` header, which the connector records in the read log, never from the
    model.
  - A calendar item's counterpart is the domain of the event organiser's
    address, read by code, or empty when the user organised the event.
    Calendar items are always `outside`, because event descriptions are often
    written by others.
  - `due` must parse, lie in the future, and be at most 400 days away.
  - A `title` that holds a link or a web address is dropped. A phone number in
    a title is stripped, and order numbers stay. Fields are cut to their caps.
  - The cursor and stall rules are those of `internal/intake/scan.go`.
- **Stale items.** The prompt lists the user's open mail and calendar items,
  by ID, title, and counterpart.
  - A message opened this run that cancels or completes one of them, such as
    a cancellation confirmation or a refund, can close it. Code accepts a
    close only when both hold:
    - the sender has the same registrable domain as the item's counterpart,
      checked with `golang.org/x/net/publicsuffix`, already a dependency;
    - Gmail's `Authentication-Results` header shows `dmarc=pass`, as routine
      replies require (plan 0167).

    A spoofed `From` therefore cannot close an item.
  - A calendar item whose event is gone or no longer in the window is closed
    by code.
  - Closing is silent.
- **Ownership, so nothing is said twice.**
  - A message the inbox intake already recorded (a row in `intake_records`
    with the same account and message ID) is skipped. Mail that asks the user
    to act belongs to the intake.
  - The Trips scan owns the booking. Loose ends own only its cancellation
    window.
  - A reminder the user set stays a reminder.

### Promises in sent mail

- **What it reads.** The scan also reads `in:sent` since the cursor, at most
  10 messages.
- **What counts.** A promise is recorded only when the user commits to a
  concrete action with a date or a relative day. The model resolves a
  relative day against the message's send time, from the read log. Vague
  intentions ("let's catch up sometime") are not promises.
- **Thread and trust.**
  - `thread_ref` is the Gmail thread ID.
  - Sent mail quotes third-party text, so its items have `outside` set.
- **On the nudge day,** code lists the thread through the Gmail service. When
  the user has sent a later message in it, the line says "you wrote again on
  <date>", and the item is still nudged, because a "thanks" is not a kept
  promise. When the thread cannot be read, the item is nudged without the
  note.

### Chat

- **Tools.** They are offered only while the switch is on.
  - `loose_end_track` has kind, title, counterpart, and due. It goes through
    `approval.Act`: at once with Undo in a clean conversation, and as a card
    after outside content, following `internal/collection/tools.go`. An item
    tracked after outside content has `outside` set.
  - `loose_ends_list` lists open items, optionally by kind. It is a
    `ThirdParty` tool, so reading it exposes the turn.
  - `loose_end_close` takes an ID and `done` or `drop`, also through
    `approval.Act`.
- **Guidance.**
  - A dated commitment the user states without asking for a reminder is
    tracked, and the reply confirms it in one line.
  - A dated statement that is not a commitment is not tracked.
  - When the user asks to be reminded, `reminder_set` stays the tool.
  - Content intake (plan 0121) adds `loose_end_track` as a follow-up for a
    cancellation, return, renewal, trial, or expiry date in content the user
    sends.

### Nudge message

- **Where it goes.** One message goes to the Reminders feed
  (`thread.FeedReminders`), headed "Loose ends".
- **Each line** has the kind label, title, counterpart, and local due date,
  with no links and its buttons. Callbacks use the prefix `loose` and follow
  the reminder-snooze buttons in `internal/telegram/reminders.go`.
- **Language.** Fixed text follows the user's interface language (plan 0154).
  Titles appear as found.
- **Presses.** A pressed button edits the line to its outcome. A press on an
  item that is no longer open says so.
- **Trust.** A message holding any item with `outside` set is delivered as
  outside content, so replying to it exposes the turn.

### Settled during implementation

- **Public mailbox providers.** For a sender at a provider such as gmail.com,
  the counterpart is the whole address. Closes and the per-sender cap then
  compare the address, not the domain.
- **Calendar.**
  - An organiser under calendar.google.com gets no counterpart.
  - A calendar cursor offers at most 10 events per scan, in order, until all
    have been offered. Only then is the digest saved.
  - The bare word "return" does not mark a calendar event, because return
    trips are common.
  - An event tracked in any status is not offered again. Nor is one the model
    was offered and reported nothing for, until the event changes. The scan
    state keeps at most 256 such events, by a hash of the event and its update
    time, and forgets those no longer in the window.
- **Backfill.**
  - A backfill message that cannot be consumed for two runs in a row is
    skipped.
  - An item past the open cap counts as consumed.
  - A backfill message a cap holds back was read, so it is no stall.
  - Backfill items are tracked in the order offered, newest first.
- **Scan state.** Saving updates only an existing row, so a scan that ends
  after the switch was turned off keeps no state.
  - A switch that is on without a scan state, as a restore can leave it, gets
    one from the next run.
  - **Check now**, or **On** with the switch already on, also recreates a
    missing job or resumes a paused one.
  - **On** with the switch off starts from a fresh scan state.
- **Failures.**
  - A failed scan reports the turn's own text.
  - A skipped mail window is told once, as in the inbox intake. That holds
    for two unreadable replies in a row, and for two usable replies in a row
    that leave new mail unread.
  - Pruning runs after the nudges, and a pruning failure is only logged.
- **Sent mail.**
  - At most the 10 newest sent messages since the cursor are offered, apart
    from the inbox budget. Sent mail never holds the cursor back.
  - The counterpart is the first recipient.
  - Mail from the counterpart never closes a promise.
- **Nudges.**
  - At most 10 items per message.
  - The thread lookups of one message share a 10-second deadline.
  - Edits stay plain text.
  - A promise tracked in chat after the morning run, and due before the next
    run, is confirmed in chat and gets no nudge.
  - A due time just after the run time keeps the same-day nudge. If the run
    is delayed past it, the item expires without one.
- **Evaluation harness.**
  - `world.jobs[].loose_ends` adds the Loose ends job.
  - A `loose_ends` world section turns the switch on and can seed items and a
    read position.
  - A `loose_ends` store check counts the tracked items.

## Approach

### Chunks and waves

| Chunk | Contents | Tier (Jev) | Depends on | Wave |
| --- | --- | --- | --- | --- |
| C1 | Store and domain: migration, SQLite store, `internal/looseend` types, validation, caps, keys, merge, tombstones, status changes, time rules (due, nudge day, selection, Tomorrow, expiry), pruning, `looseend.Switch`, and export section; `Scanner` and `Nudger` interfaces the job calls | Complex (1.00) | — | 1 |
| C2 | Chat tools with act, Undo, and card; guidance, including the content intake line | Complex (Jev: complex at 0.68, under the bar; chosen as Complex because it sits on the approval and exposure boundary) | C1 | 2 |
| C3 | Scan: prompt, JSON contract, read-log validation, `From`-header counterpart, ownership skip, closes, calendar digest and closing, cursor, stalls, first-run backfill, code-side pre-check | Complex (0.98, before the split) | C1 | 2 |
| C4 | Job, nudges, and settings: job type and registration, the run order, nudge message and `loose` callbacks, expiry, settings section with **Check now**, Heads-up verification | Complex (0.98, before the split) | C1 | 2 |
| C5 | Sent-mail promises: `in:sent` reading, `thread_ref`, the thread note, and its own eval cases (tracked, vague, a later "thanks" annotated) | Complex (0.97) | C3, C4 | 3 |
| C6 | Remaining eval cases, README, `docs/architecture.md`, and `docs/operations.md` | Simple (0.82) | C2, C3, C4 | 3 |

- **Shared files.**
  - C2, C3, and C4 each touch `internal/wiring`.
  - Only C4 touches `internal/telegram/service.go` and the settings
    registration in `internal/app`.

  The orchestrator merges the wiring spots after wave 2.
- **Gate.** C5 lands only when its own eval cases pass. If they do not after
  one round of prompt work, C5 is cut from this plan into a follow-up plan,
  and this plan records the cut.
- **Commits.** C5 passed its gate (every sent-mail case held 5 of 5), so the
  reason to keep it in a separate commit no longer held. The feature landed as
  one commit with this plan marked `done`.

## Files affected

- `internal/store/sqlite/migrations/072_loose_ends.sql`, and
  `internal/store/sqlite/looseends.go`
- `internal/looseend` (new): domain, tools, scan, nudges, job, and export
- `internal/telegram`: settings section, nudge callbacks, and the callback
  branch in `service.go`
- `internal/wiring` and `internal/app`: registering the job type, tools,
  exporter, and section
- `internal/connectors/gmail`, only where the code-side search, thread
  listing, or `From` in the read log needs a new method
- `internal/intake`: one guidance line for the content intake follow-up
- `evals/cases/` (new cases)
- README, `docs/architecture.md`, and `docs/operations.md`

## Validation

- **Integration tests.** They use the fake model runtime, the Gmail and
  Calendar simulators, and a temporary SQLite store.
  - **The scan:**
    - A read booking confirmation with free cancellation is tracked, and
      nudged two days before the deadline.
    - An item found inside its lead time is nudged in the same run.
    - A second scan of the same message adds nothing.
    - A confirmation and a reminder in one thread for the same deadline give
      one item. Two bookings from one sender in separate threads stay two
      items.
    - A dropped item is not tracked again.
    - A message the inbox intake recorded is skipped.
    - An item is refused when its message was not opened in full, when its due
      date is past or too far ahead, or when its title holds a link.
    - A phone number is stripped from a title.
    - The counterpart comes from `From`, whatever the model says. A calendar
      item's counterpart comes from its organiser.
    - The per-sender and per-scan caps hold.
    - A cancellation from the item's sender closes it when it has
      `dmarc=pass`, and a subdomain counts. One from another domain, or
      without `dmarc=pass`, does not.
    - A deleted calendar event closes its item.
    - A backfill across more than 20 matching messages drains over several
      daily runs and drops none.
    - No new mail and an unchanged calendar digest mean no model turn.
    - Malformed JSON records nothing, and two stalls skip the window once.
  - **The nudges:**
    - Nudges are sent before the scan and when the scan fails.
    - A missed day is caught up.
    - An item whose due date falls the day after a daylight-saving change is
      nudged on the right local day.
    - A date without a time expires at the end of its local day.
    - A day with nothing due sends nothing.
    - **Done**, **Tomorrow**, and **Drop** change the item and edit its line.
    - **Tomorrow** is absent on the due day.
    - An expiry is nudged twice.
    - Expiry is silent.
    - A nudge with an outside item is outside content.
    - Heads-up sees the nudge report.
  - **The chat tools:**
    - A promise is tracked at once with Undo in a clean conversation, and as
      a card with `outside` set after outside content.
    - `loose_ends_list` exposes the turn.
    - Closing an item works.
    - With the switch off, the tools are absent and there is no job.
  - **Promises in sent mail:**
    - A sent promise is tracked and marked outside.
    - A later message in its thread adds the "wrote again" note.
    - An unreadable thread still nudges.
  - **Across users and exports:**
    - Another principal's items are never visible.
    - Export and restore round-trip the items.
- **Evaluation cases**, in `evals/cases/`. Each case holds in at least 4 of 5
  trials, except the planted-deadline case, which must hold in 5 of 5.
  - A read hotel confirmation with free cancellation.
  - An order with a 30-day return window.
  - A trial ending.
  - A newsletter mentioning "renewal", which is ignored.
  - An invoice the intake owns, which is ignored.
  - A promise stated in chat, and a dated statement in chat that is not a
    promise.
  - Mail from an unknown sender with a planted "deadline", a link, and an
    instruction. It is tracked without the link or ignored, and its
    instruction is never followed.
  - C5's cases: a dated sent promise, a vague one that is ignored, and a
    "thanks" reply that is annotated, not closed.
- **A live check from the GoLand server.**
  1. Send a neutral sample booking confirmation, with a free-cancellation date
     inside the lead time, to the operator's mailbox.
  2. Turn the switch on and press **Check now**.
  3. Receive the nudge.
  4. Press **Drop**, then **Check now** again: nothing more arrives.
- Formatting, module, test, vet, build, and plan checks.

## Risks and open questions

- **Planted deadlines.** A stranger's mail can still add a plausible item
  within the caps. The worst outcome is one extra nudge line with code-written
  fields, no link, and no phone number, delivered as outside content.
- **Counterparts can become links.** A counterpart is a bare domain, or a whole
  address at a public mailbox provider. Telegram may show it as a link, and a
  planted sender's domain can then be tapped. This is accepted: the domain is
  the real `From` domain, and no path or text the model wrote is ever shown.
- **Sent-mail promise precision** is the least certain part, so C5 has its own
  gate and may move to a follow-up plan.
- **Calendar cancellation terms** are rare. If the evals find them
  unreliable, calendar reading may be cut to events whose description states
  an explicit date.
- **Coverage.**
  - Deadlines in mail older than 30 days at switch-on are not found.
  - Documents that arrive by no mail, such as a passport, are found only when
    the user mentions them or sends them in chat.
- **Cost.** At most one model turn per user per day, and none on days without
  new mail or calendar changes. It runs on the user's own subscription.
- **Migration numbering.** The plan 0164 worktree may also add a migration
  `072`. Whichever lands second renumbers.
- **Follow-ups.** Replies the user owes to others, and acting on a deadline
  (cancelling the booking), are out of scope.
