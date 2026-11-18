# 0161 — Inbox intake: unread mail only, priority notifications only, and notes the user can edit

Status: `done`

## Context

Inbox intake (plan 0121) checks new mail every three hours. It reports each message the model
judges to need something, and proposes follow-ups on one card. In use it has two problems:

- It reads every new message, including mail the user already opened on another device. It then
  reports mail the user has already dealt with.
- "Needs something" is read loosely. Receipts, delivery confirmations, and informational mail with
  nothing to do arrive as notifications.

The user asked the assistant in chat to narrow the check, and the change failed. `schedule_update`
refuses every job whose type is not `model.turn` with "Reminders cannot be changed"
(`internal/schedule/tools.go`). The inbox job is an `intake.scan` job, so the reason given was
wrong. Its rules also live in code, so there was nothing the user could change. The tempting
workaround is a second, hand-written mail-check job next to the built-in one. That would duplicate
the check, so it must not be offered.

Gmail's `IMPORTANT` label is Google's own guess at importance. It says nothing about what matters
to this user.

## Goal

- The check reads only mail that is still unread when it runs. Mail the user has opened is never
  reported.
- A message is reported only when it asks the user to do something, or brings an important new
  update about something already in motion. Receipts, acknowledgements, confirmations of what the
  user did, newsletters, adverts, and informational mail produce nothing.
- The user can tell the assistant in chat what matters to them in mail. The assistant edits notes
  on the Inbox intake job, and every check follows them.
- The check runs every hour between 08:00 and 22:00 instead of every three hours.
- Asked to change the check, the assistant explains what it can change (the notes) and what lives
  in `/settings` (on, off, check now). It never calls the job a reminder and never proposes a
  parallel mail-check job.

## Decisions

- **Unread only, checked by code.** Every scan query carries `is:unread`. When `gmail_read_message`
  opens a message, the Gmail connector records in the run's `tools.CallLog` the account, the
  message ID, and whether the message carried `UNREAD` at that moment. Code drops a reported message
  that was not opened in full during this run, or was not unread when opened. That also enforces in
  code the existing rule that nothing is reported from a search snippet. The scan scope is
  read-only, so the check never marks mail as read.
- **Gmail's `IMPORTANT` label plays no part.** It is neither a filter nor a hint, and the prompt
  says to ignore it.
- **Priority per message.** The JSON gains `priority`:
  - `action`: the user must do something. It needs `action_required`, and follow-ups as today.
  - `update`: important news about something already in motion, such as a decision, a changed
    date, or a problem, with nothing to do. It needs a new `update` text and carries no follow-ups.
  - `parcel`, only while Parcels is on: a shipping notice reported only to start tracking. It needs
    a `parcel` follow-up.

  A message is accepted only with the text its priority needs, and an empty or unknown priority is
  never defaulted. Text that belongs to the other priority is cleared rather than rejected, which
  keeps the existing per-field leniency. Any other message is dropped quietly. A message that is not important is not an error, so it is never counted in the
  "could not read" notes. It is logged with a reason.

  A notice that also asks for something, such as a customs payment or a pickup deadline, is an
  `action`. It may still carry a `parcel` follow-up, and that follow-up is dropped at offer time
  when the number is already tracked, as today.
- **Drops that are not about importance are still surfaced.** Two drops count in the existing
  "could not read" note: a message that was never opened in full this run, and an account and
  message ID that match nothing in the read log. The cursor rules are unchanged, so these messages
  are not retried. A message that was opened but was no longer unread is dropped quietly, because
  the user has seen it.
- **Parcel notices that change nothing are silent.** At record time, code asks
  `ParcelSupport.Proposal` for each `parcel` message's number. When the number is already tracked,
  tombstoned, pending, or declined, the message is dropped with no record and no line in the
  summary. For a number that no integration can track, a read-only `ParcelSupport.Following` asks
  whether mail already follows it: a new one keeps its follow-up and is news, one already followed
  is silent. The follow itself still happens in the offer step, after the record is stored, so a run
  that fails before then leaves the next run to report it. With Parcels off, a delivery notice is reported only when it asks for something, such
  as a customs payment or a pickup deadline.
- **Stored, not just shown.** An additive migration adds `priority TEXT NOT NULL DEFAULT 'action'`
  and `update_text TEXT NOT NULL DEFAULT ''` to `intake_records`. Existing rows and chat records
  read as `action`. An export from before this change still imports, with the defaults. The
  previous image ignores the new columns, so a rollback stays possible. The summary is still written
  from validated records. `intake_get`, `intake_list`, and the export include both fields. Consult:
  store or not → store, so later questions can be answered and updates stay distinct from open
  actions.
- **The summary has two groups:** "Needs something from you" lists `action` messages, and "Worth
  knowing" lists `update` and `parcel` messages. An update never puts anything on the card; a
  parcel's tracking proposal does. A run with only updates sends the summary and no card.
- **Follow-ups already waiting are still offered.** Records from earlier runs, including those from
  before this change, keep their `pending_actions`. Their mail was reported while it was unread, and
  the card is the follow-up to that report. A run with only carried follow-ups behaves as today: it
  offers them under the "mail I told you about earlier" line.
- **Notes on the Inbox intake job, edited through `schedule_update`.** Consult: generalise
  `schedule_update`, or add a dedicated intake tool → generalise it, through an early dispatch
  rather than a second branch through the `model.turn` flow. The model already reaches for
  `schedule_update` on this job, and a dedicated tool would need a broader API for replacing a job's
  config from outside `schedule`.
  - `schedule.JobType` gains `EditableNotes`. `schedule` owns a `notes` field in the job config, with
    `Notes(config)` and `WithNotes(config, notes)`. The intake scan type sets the flag, and its
    `Validate` accepts `{"notes": "..."}` with the same length cap as job instructions.
  - `proposeUpdate` dispatches at once on the job's type:
    - A `model.turn` job takes the existing path, unchanged.
    - A type with `EditableNotes` goes to `proposeNotesUpdate`. It takes only `prompt` or
      `prompt_edits`, applied to the notes. Every other field, including cron, name, model, anchor,
      condition, project, and travel pause, is refused by name, without being ignored.
    - Any other type gets a correct refusal: it is a built-in check, controlled from `/settings`.
      Only a real reminder keeps the reminder message.
  - The notes edit has its own approval action and executor, with the same semantics as a job
    update: done at once and undoable, a card after outside content, and refused when the job
    changed since it was proposed. `confirmedJob`, `executeUpdate`, and the `model.turn` path stay
    as they are.
  - `schedule_list`, `schedule_get`, and the prompt's job section show the notes under a label like
    Heads-up's: "Notes (the user's preferences; how Inbox intake checks mail is built in)".
  - The scan prompt puts the notes after the rule that mail text is data. It frames them as the
    user's preferences about what to report. They cannot widen the tools or the JSON contract. They
    cannot relax the unread rule or the parcel rule either, because code enforces both.
  - Turning the switch off deletes the job, as it does today and as Heads-up's switch does, so the
    notes go with it. The `/settings` line says so. Backup and restore carry the notes, because they
    are part of the job's stored config.
- **Hourly.** `InboxCron` becomes `0 8-22 * * *`, which is the scan type's own minimum interval.
  An existing inbox job still on the three-hour schedule is moved to it at startup by
  `MigrateInbox`, as `MigrateHeadsUp` moves Heads-up jobs. The occurrence still reads at most ten
  messages, and the cursor rules are unchanged.
- **Guidance.** The scheduler guidance says Inbox intake is built in, its notes are edited with
  `schedule_update`, and it is turned on and off in `/settings`. It also says never to create a
  separate job that checks mail for the same purpose.

## Approach

1. **Schedule notes path** (`internal/schedule`): `EditableNotes`, the notes config helpers, the
   early dispatch in `proposeUpdate`, `proposeNotesUpdate` with its approval action, executor, and
   undo, the corrected refusals, the listing and prompt labels, and the guidance line. It is tested
   with a test job type inside `schedule`. It also covers the hourly `InboxCron` and
   `MigrateInbox`.
2. **Read log** (`internal/tools/calllog.go`, `internal/connectors/gmail/tools.go`): record each
   opened message's account, ID, and unread flag beside the existing received time, and expose a
   lookup.
3. **Record fields** (`internal/intake` record type, store, and export; a migration in
   `internal/store/sqlite`): `priority` and `update_text` through the store, `intake_get`,
   `intake_list`, and the export and import.
4. **Scan** (`internal/intake/scan.go`): the prompt (`is:unread`, priority, `update`, notes, the
   `IMPORTANT` rule), the scan JSON, the unread and opened checks, the priority validation, the
   parcel pre-check, the two-group summary, and the rule that only actions and parcels get a card.
   It sets `EditableNotes` on the scan type, reads the notes from the occurrence's config, and owns
   the end-to-end tests on the inbox job, including the `schedule_update` ones.
5. **Texts and docs**: the `/settings` Inbox intake lines in English and Russian, including that
   turning the switch off forgets the notes and the hourly cadence; README, `docs/architecture.md`, and
   `docs/operations.md`.

## Files affected

- `internal/schedule/schedule.go`, `tools.go`, `prompt.go`, and a new notes helper file
- `internal/tools/calllog.go`, `internal/connectors/gmail/tools.go`
- `internal/intake/scan.go`, `intake.go`, `export.go`, and the store record type
- `internal/store/sqlite/intake.go`, `internal/store/sqlite/migrations/NNN_intake_priority.sql`
- `internal/telegram/inbox.go`
- README, `docs/architecture.md`, `docs/operations.md`

## Validation

- Integration tests with the fake model runtime, a fake Gmail at its HTTP boundary, and a temporary
  SQLite store:
  - every scan search carries `is:unread`;
  - a reported message that was opened but no longer unread is dropped;
  - a reported message that was never opened in full is dropped;
  - an `action` without `action_required`, or an `update` without `update`, is dropped quietly and
    not counted as unreadable;
  - a message that was never opened, or whose ID matches nothing, is counted in the "could not
    read" note, and the next cursor is what it is today;
  - a carried follow-up from an earlier record is still offered;
  - pre-existing rows read as `action`, and an export without the new fields imports;
  - a run with only updates sends the "Worth knowing" summary and no card;
  - a `parcel` notice for a number already tracked sends nothing, and a new number gives a card;
  - the notes reach the scan prompt after the outside-content rule;
  - priority and update text are stored, returned by `intake_get`, and exported;
  - `schedule_update` on the inbox job with `prompt_edits` changes the notes at once and is
    undoable;
  - after outside content, the same edit is a card;
  - a stale confirm is refused;
  - cron, name, or model on the inbox job is refused by name;
  - the Trips scan job gets the built-in-check refusal, and a reminder keeps the reminder message;
  - turning the switch off and on starts with empty notes;
  - `MigrateInbox` moves an inbox job on the three-hour schedule to the hourly one, and leaves other
    jobs on that cron alone.
- `gofmt`, module, vet, test (with and without `-race`), build, and `scripts/check-plans`.
- The fake model cannot judge importance, so the tests cover only what code enforces. Whether the
  model classifies real mail well is checked after deploy, in the Telegram bot chat:
  - "only tell me about mail I haven't read and that needs me" edits the notes, and creates no new
    job;
  - `/jobs` shows no duplicate mail check;
  - "Check now" is run over a read message, an unread receipt, and an unread message that asks for
    something. Only the last one is reported.

## Risks and open questions

- **Notes are a persistent injection target.** Mail read in chat could steer the model into writing
  them. The card after outside content and the length cap apply. The validated JSON contract and
  the read-only scan stay the real boundary.
- **Matching depends on IDs.** Code matches a reported message to the read log by account and
  message ID, exactly as `gmail_search` printed them. A model that reformats either drops the
  message. The test covers the exact form, and the drop is logged with a reason.
- **Some mail is never seen.** Mail the user opens before the next check is never seen by Inbox
  intake. That is the intent, but an opened bill is not reported either. This is accepted, and the
  `/settings` text says so.
- **Notes stay with the job.** If the user later wants these preferences to apply to an on-demand
  mail question in chat as well, they belong in intake's own store per principal. That is out of
  scope here.
