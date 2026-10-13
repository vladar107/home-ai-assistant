# 0072 — Scheduled jobs and their reports in the conversation's context

Status: `done`

## Context

Scheduled jobs (`model.turn` and reminders) carry prompts the user wrote with
care, such as their commute's stops and trains, and send reports, such as a
morning commute report. The user often refers to both later, and the
conversation knows neither unless it happens to call `schedule_list` or `schedule_get`.
The user often refers to a report later ("what about the second
train?", "move that reminder"), and the conversation does not know what the
report said:

- The schedule service keeps the last two delivered reports in memory and
  hands them to the principal's next chat turn only. After that turn, or after
  a restart, they are gone.
- `schedule_runs.reply` is cleared when a run is delivered, so no report text
  is kept at all.
- Replying to a job's Telegram message only switches sessions for replies the
  conversation sent; a job's message brings nothing.
- Background tasks and scheduled turns never see earlier reports.

## Goal

- Every chat turn and background task knows, briefly, what each of the user's
  jobs is for and the facts its prompt holds, and can read a job in full.
- Every chat turn knows, briefly, which job reports reached the user since its
  conversation last heard about them, and can read any recent report in full
  when it needs to.
- Replying to a job's message gives the turn that report.
- Reports survive restarts.

## Decisions

- **Reports are kept.** A new `job_reports` table keeps each delivered report
  of a `model.turn` or reminder run: principal, job ID and name at the time,
  text, whether it may retell private content, and delivery time. Its ID is
  the run's ID, so a retried delivery saves it once. A `job_report_messages`
  table keeps the Telegram message IDs it was sent as: each text chunk and the
  reminder message; a card's photo is not tracked. It cascades with the
  principal, but not with the job, so a report outlives a deleted or finished
  one-off job. Reports older than 14 days are pruned; at most 200 are kept per
  principal. The report is saved before the run is marked delivered; a failed
  save is logged and does not hold the run back, which would send it again.
- **The notice becomes an excerpt.** The in-memory notice is replaced by the
  stored reports not yet noticed. Each appears as one line: job name, local
  time, report ID, and the first 200 runes of the text, ending with "…" when
  cut. The notice says to call `job_reports` with the ID when the excerpt is
  not enough. A report is marked noticed when a chat turn takes it, so a
  restart neither loses nor repeats it. At most five are listed; older unseen
  ones are counted ("and 3 earlier reports").
- **A new conversation starts with a digest.** The first turn of a new
  session (after `/new`, or a fresh start) lists the reports from the last 24
  hours, so "the report this morning" still resolves after `/new`. Reports
  already noticed are listed by job, time, and ID only, without their text, so
  the digest does not count as outside content and a daily report does not make
  every new conversation of the day wait for Confirm; the model reads one with
  `job_reports` when it needs it. A turn opens a session when its request is
  the session's first.
- **Replying to a report quotes it.** When the user replies to one of a
  report's Telegram messages, the turn's message is prefixed with that report
  in full, capped at 2000 runes, and that report is left out of the excerpts.
  This is in addition to, not instead of, the usual session handling. The
  replied-to message ID is stored on the request
  (`conversation_requests.reply_to_message_id`, in migration 023), so a
  restart before the turn runs keeps it.
- **One read tool.** `job_reports` lists the principal's recent reports
  (optionally one job's, by job ID or name) with excerpts, or returns one
  report in full by report ID. It is a read, so chat turns, scheduled turns,
  and background tasks all get it, and it is offered only when the principal
  has at least one stored report.
- **The jobs themselves are in context.** A job's prompt often holds facts the
  user relies on in conversation: their stops, trains, routes, and what they
  care about. Each chat turn and background task gets a "Scheduled jobs"
  section next to Memory in the system prompt. It lists each enabled job and
  pending reminder as one entry: ID, name, schedule as `/jobs` shows it (the
  cron expression with its zone, or "once" and the time), and the first 300
  runes of its prompt on one line, ending with "…" when cut. Paused jobs are
  left out. The section changes only when a job changes, so the prompt stays
  cacheable. Guidance says that the user's job prompts hold facts they expect
  the assistant to know. When the user refers to something that fits a job,
  use it, and call `schedule_get` (the existing tool; there is no `job_get`)
  for the full prompt when the excerpt is not enough. The whole section is
  capped at 4000 runes, with the most recently created jobs first and a count
  of any left out: ordering by last change would reorder the section on every
  run, because each run moves a job's last change. Scheduled turns already
  carry their own prompt and do not get the section. The runtime request
  carries the section as `Jobs` beside `Memory`, and Pi appends it after
  memory.
- **Exposure follows the content.** Job reports may retell web or mail content,
  so the notice stays an outside notice, and reading a report through the tool
  or through a reply marks the turn as having read outside content. A report
  stored as private marks the turn private, as the notice does today.
- **Strictly per principal.** Every lookup is keyed by principal; a report ID
  or Telegram message ID of another principal finds nothing.

## Approach

1. Store: migration `023_job_reports.sql` and repository methods to save,
   list unnoticed, mark noticed, list recent, load one by ID or by Telegram
   message, and prune.
2. Schedule service: save the report when a run is delivered, with its message
   IDs, replacing `remember`; the notice reads from the store.
3. The "Scheduled jobs" section: rendered by the schedule service, passed to
   the runtime beside memory for chat turns and background tasks, plus the
   guidance.
4. Conversation: the reply-to lookup and the new-session digest go into the
   turn's message next to the existing notice.
5. Tool provider `job_reports` in `internal/schedule`, registered in
   `internal/app`.
6. README, `docs/architecture.md`.

## Files affected

- `internal/store/sqlite/migrations/023_job_reports.sql`, a new
  `internal/store/sqlite/job_reports.go`
- `internal/schedule/service.go`, `internal/schedule/tools.go` or a new file
- `internal/conversation/service.go`, `internal/conversation/types.go`,
  `internal/runtime` (the section beside memory)
- `internal/telegram/service.go` if the reply-to message must be passed
  through differently
- `internal/app/app.go`
- README, `docs/architecture.md`

## Validation

- Store tests on a temporary SQLite database: save, unnoticed and noticed,
  pruning by age and count, lookup by Telegram message, principal isolation,
  survival of a deleted job.
- Integration tests:
  - a delivered report appears as an excerpt in the next chat turn's message,
    and not in the one after;
  - the excerpt survives a service restart before the next turn;
  - a reply to a report's message prefixes that report in full;
  - `job_reports` lists and reads reports, and never another principal's;
  - a private report marks the turn private; reading any report marks it as
    having read outside content;
  - the first turn after `/new` gets the 24-hour digest;
  - a chat turn's system prompt lists the principal's jobs with prompt
    excerpts, never another principal's, and a changed job changes it;
  - a scheduled turn does not get the section.
- Formatting, module, test, vet, build, and plan checks.

## Risks and open questions

- Excerpts add a little text to turns after each report; five lines of 200
  runes is the ceiling.
- Watch alerts and background task reports keep their current notices; the
  same store could serve them later.
