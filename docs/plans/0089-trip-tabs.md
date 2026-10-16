# 0089 — A tab for each trip, with its summary pinned

Status: `done`

## Context

Backlog item `e92ded72` asks for a separate chat for each trip, with the
trip's important details pinned in it.

After plan 0087:

- Trip messages go to the Reminders feed. These are suggested-trip cards,
  check-in reminders, start and end notices, and update lines.
- A conversation about a trip has no home. The trip line in every turn
  (plan 0082) names only the active or next trip.

In private chats, `pinChatMessage` pins any non-service message, and pins
there are always silent. The Bot API also has `unpinAllForumTopicMessages`
for private-chat tabs, which suggests that pins are kept per tab. The
documentation does not say how a pin is assigned to a tab.

## Goal

With tabs on, each planned trip has its own tab:

- The tab opens with a summary of the trip: destination, dates, each booking
  with its reference, and when each check-in opens, which is when its
  reminder fires. The summary is pinned and kept current.
- The trip's check-in reminders, start and end notices, and update lines
  arrive in the tab, with sound like every other message.
- Talking in the tab, or replying to a trip message there, is a conversation
  about that trip.

Suggested trips keep their Track and Ignore card in the Reminders feed. With
tabs off, nothing changes.

## Decisions

- **When the tab is made.**
  - A trip gets its tab when it becomes `planned`. That is Track on a card,
    or `trip_add` and the Confirm of a proposed add.
  - Trips that are already planned get their tab at start, once each. The trip
    loop does this before its first cycle (`trip.Service.Settle`), together
    with the reminder destinations below.
  - The tab is named after the trip's title, with the ✈️ icon when plan
    0088's icon set has it. As with feed tabs, a tab made before the icon set
    was read gets its icon with a later message.
  - Its registry row is `named_by = fixed`, with the subject `trip:<id>`
    (plan 0087's `telegram_tabs.subject`), so plan 0088 never renames it by
    topic. It is renamed only when the trip's title changes.
  - The registry is the only record of the trip's tab. When plan 0085's
    observer drops a gone row, the trip has no tab.
- **Migration 037.** Plan 0088's fix took 036, so this one is 037.
  - It adds a nullable `summary_message_id` to `trips`.
  - It adds a nullable `destination` to `scheduled_jobs`, such as
    `trip:<id>`, so `internal/schedule` never sees thread IDs.
  - It adds a nullable `destination` to `job_reports`, copied from the job
    when the report is saved, so a reply still resolves after the job is gone.
- **Destinations.**
  - The plan 0087 resolver takes an optional destination: `thread.Feeds`
    gains `Destination(ctx, principal, chat, destination, feed)`, which falls
    back to the feed. For `trip:<id>` it works like this:
    - A trip with a tab resolves to that tab, whatever its status, so the end
      notice of an ended or cancelled trip still lands there.
    - A planned trip that has not ended and has no tab gets its tab made,
      with a fresh summary sent and pinned, and resolves to it.
    - An ended or cancelled trip without a tab, a trip that no longer exists,
      a tab that cannot be made, and tabs off all resolve to the Reminders
      feed.
  - `TripNotifier` resolves the same way. `trip.Sender.Send` takes the trip's
    ID for this, so the start and end notices and the scan's update lines go
    to the trip. Suggested-trip cards stay in the Reminders feed.
  - `SetReminder` sets the destination explicitly each time, and `Update`
    and snooze keep it. `schedule.Service.Update` keeps the job's destination
    when the prepared job names none, as an edit through `schedule_update`
    does, and an undone delete restores it.
  - At start, existing check-in reminders get their destination. For each
    planned trip that has not ended, a derived reminder that exists and has
    no destination gets the trip's, and nothing else is created or changed.
    A reminder the user deleted or moved therefore stays as they left it on
    every restart, and one about to fire is untouched. The update leaves the
    job's `updated_at` alone, like the travel flag, so it races no edit or
    tick. Running it again changes nothing.
- **One conversation per trip.**
  - A new session in a trip tab, including after `/new`, gets the subject
    `trip:<id>`, unless that subject is already active in another tab, or the
    trip no longer exists because it was pruned. Then it is created without a
    subject.
  - A reply to a check-in reminder resolves to `trip:<id>`, not to `job:<id>`,
    because its report carries that destination. A trip tab is otherwise
    treated like a feed tab by plan 0087's routing, including the rule that
    only an explicit reply is refused as open elsewhere.
  - Each turn gets one line naming the trip, and saying that `trip_list` with
    its ID shows it in full. The model can then change the trip with the
    existing trip tools.
  - A trip's title can come from mail, so the line quotes it with Go's
    `strconv.Quote`, which its own text cannot close. The same quoting now
    applies to the job and watch names of plan 0087's subject line.
  - A report whose destination is a trip has that trip as its subject
    wherever it is replied to, so this holds in the Reminders feed too.
  - An answer in a trip tab gets no "About …" line for the tab's own trip,
    since the tab already names it. It still gets one when it moves to
    another subject.
- **The summary.**
  - Plain text, without markup or link previews, like the other trip
    messages. Times are shown in the trip's zone.
  - The summary message itself is sent with sound. Only the pin is silent,
    which Telegram does in private chats anyway.
  - Every trip write that changes what the summary shows edits it in place:
    dates, title, and bookings. The trip service tells a `trip.Tabs` hook
    (`TripChanged`) after each write, which `TripNotifier` implements. It does
    so after releasing its lock, so one trip's Telegram round trips never hold
    up another write. The Telegram side reads the trip again under a per-trip
    lock, so writes close together still leave the latest summary.
  - Each booking shows when its check-in opens. The summary does not claim a
    reminder, since one may not have been set, for example when the
    scheduler is full.
  - The summary is sent only to the trip's tab, never with a fallback to the
    main view, where it would be pinned.
  - If the summary message is gone, which is any permanent refusal of the
    edit, a new one is sent and pinned.
  - If a summary that was sent cannot be recorded, recording is tried once
    more, and each failure is logged. An unrecorded summary is sent again by
    the next write, so a second pinned summary stays rare and visible.
  - If the summary's send has its tab taken for gone, the resolver hands back
    no tab, and the message falls through to the Reminders feed.
  - When a trip ends, its summary gains "Trip ended". When it is cancelled,
    it gains "Cancelled". The mark is the summary's first line, which the
    pinned bar shows. The end is marked when the end notice is recorded, and
    an ended trip's summary is also brought up to date by any later write. The
    tab is kept until the user closes it with `/close` (plan 0085).
  - The tab is renamed only when the title changes. The registry records the
    new name on the fixed row.
  - If pinning fails, the summary stays as the tab's first message, and the
    failure is logged once per trip while the process runs.
- **Gone tabs.** When a trip tab is gone (plan 0085's observer, suspect rule
  included), its registry row is dropped and the trip's `summary_message_id`
  is cleared. The store's `DeleteTab` clears it in the same transaction, so
  `/close` forgets the summary too. The next trip message goes through the
  resolver above, which makes the tab again and sends and pins a fresh
  summary. A trip that has ended or was cancelled is not given a new tab.
- **Pruning.** When a trip is pruned (30 days after it ends), its tab is left
  alone, and the registry row stays for `/close`.
- **Privacy.** Logs carry trip, tab, and message IDs only, never titles,
  destinations, or booking references.

## Approach

### Chunks

The chunks were implemented in one pass, so chunk 1's stub for a planned trip
without a tab was never needed.

1. **Destinations.** Jev: complex (0.73).
   - Migration 037.
   - `destination` on jobs, set by `SetReminder` and kept by `Update` and
     snooze. `destination` on job reports, copied when saved.
   - The resolver's `trip:<id>` cases. Making the tab and summary is chunk
     2's, so this chunk stubs that step and resolves a planned trip with no
     tab to Reminders until chunk 2 lands.
   - The reminder destinations at start, set only on reminders that exist
     without one.
   - Acceptance, from integration tests:
     - A check-in reminder is delivered in its trip's tab.
     - It goes to Reminders for an ended or cancelled trip, and with tabs off.
     - A snoozed or edited reminder keeps its destination.
     - Setting destinations at start leaves a reminder whose check-in time has
       just passed untouched, does not revive a deleted reminder or undo a
       moved one, and changes nothing when run again.
     - A job without a destination uses its feed as before.
2. **Trip tab and summary.** Jev: complex (1.00), split from one larger
   chunk. Depends on chunk 1.
   - Creating the tab on `planned`, once at start for trips already planned,
     and from the resolver for a planned trip that has lost its tab.
   - The summary text, with send, pin, and edit on every trip write.
   - The ended and cancelled marks.
   - Acceptance, from integration tests against a fake Telegram boundary:
     - Track creates one tab and one pinned summary.
     - A booking change edits the summary.
     - A gone summary is sent and pinned again.
     - A cancelled trip's summary is marked.
3. **Notices, subject, and gone tabs.** Jev: complex, by the orchestrator,
   since this part was split from chunk 2. Depends on chunk 2.
   - Start, end, and update notices go to the trip tab.
   - The `trip:<id>` subject in the store's routing, including new sessions
     after `/new` and the elsewhere case, and the turn line.
   - Reply resolution through the report's destination.
   - Gone-tab handling.
   - Acceptance, from integration tests:
     - Start and end notices land in the tab.
     - A reply to a check-in reminder continues the trip's conversation, also
       after the reminder job is gone.
     - `/new` in a trip tab starts a new conversation about the same trip.
     - A deleted tab is made again with a fresh summary on the next notice.
     - No new tab is made for an ended trip.
4. **Docs.** Done by the orchestrator. The README covers trip tabs, and the
   architecture notes cover destinations.

### Waves

- Wave 1: chunk 1.
- Wave 2: chunk 2.
- Wave 3: chunk 3.
- Wave 4: chunk 4.

## Files affected

- `internal/store/sqlite/`: `migrations/037_trip_tabs.sql`, `trips.go`,
  `tabs.go`, `job_reports.go`, `schedules.go`, `conversation.go`
- `internal/schedule/`: `schedule.go`, `service.go`, `reports.go`, `undo.go`
- `internal/thread/thread.go`: `TripDestination`, `TripOf`, and
  `Feeds.Destination`
- `internal/trip/`: `trip.go`, `service.go`, `reminders.go`, `loop.go`,
  `scan.go`, the new `summary.go`, and the new `tabs_integration_test.go`
- `internal/telegram/`: `feeds.go`, `trips.go` (`SendPlainInTab` and
  `PinChatMessage` beside `sendPlain`), `service.go`, `tabs.go`, the new
  `triptabs.go` and `triptabs_integration_test.go`, and the harness in
  `conversation_integration_test.go` and `tabs_integration_test.go`
- `internal/conversation/types.go`: the trip subject and its turn line
- `internal/app/app.go`
- `README.md`, `docs/architecture.md`, `docs/plans/README.md`

## Validation

- `gofmt`, a `go mod tidy` diff, `go vet`, `go test -race ./...`,
  `go build`, and `scripts/check-plans`.
- A live check on the operator's bot with topics on:
  - Track a trip, and see its tab with the pinned summary.
  - Change a booking in chat, and see the summary update.
  - Check that the pin shows only in that tab and not in the main view. If
    pins turn out to be chat-wide, stop pinning and keep the summary as the
    tab's first message.

## Risks and open questions

- Pins may be chat-wide in private chats. The live check decides, and the
  fallback above keeps the feature without pins.
- Every planned trip adds a tab. Thirty unfinished trips is the existing cap,
  so the number of tabs stays bounded.
- This depends on plans 0085, 0087, and 0088 being in place first.
