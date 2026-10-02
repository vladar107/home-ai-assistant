# 0024 — Durable scheduler and scheduled model turns

Status: `done`

## Context

Everything the assistant does today starts with a message from the user. The
conversation worker, the approval sweep, and the Telegram poller are the only
loops in the process, and none of them acts at a time of its own choosing.

[06 — Scheduling and notifications](../06-scheduling-and-notifications.md)
already chose the shape: an in-process scheduler backed by SQLite, with the
database as the source of truth for what is due, what already ran, and what was
delivered. It also lists the proactive work waiting on that core — the morning
briefing, reminders, reconciliation, backup-age checks.

The first use the operator wants is a recurring question asked in plain
language: "every Monday at 9, look at my Notion tasks and tell me what is
overdue". Deterministic job types, such as a briefing assembled without the
model, are the other priority, but today the only data sources are Notion and
the NAS roots, so none is worth building yet. This plan builds the core so that
such a type is one registration away.

## Goal

A user asks in conversation for something to happen on a schedule, sees the
exact job — its cron expression, time zone, the next three local run times, and
the prompt — and confirms it with the existing approval buttons. At each
occurrence the assistant runs that prompt against the user's own model
subscription with read-only tools and sends the result to their chat, once,
even across restarts. `/jobs` lists the user's jobs and pauses, resumes, runs,
or deletes each one. A report the user just received is known to their next
chat turn, so "tell me more about the second one" works.

## Decisions

- **Durability is SQLite plus an in-process loop.** System cron and the NAS task
  scheduler know nothing about principals, occurrences, or delivery; a workflow
  engine is a cluster; in-memory schedulers lose state on restart; SQLite queue
  libraries are queues, not schedules. The database is already backed up,
  migrated, and opened with `_txlock=immediate`.
- **Schedules are standard five-field cron expressions**, parsed with
  `github.com/robfig/cron/v3` (`cron.ParseStandard`) and nothing else from that
  library: its runner, `@every`, seconds, and the `CRON_TZ=` prefix are not
  used. The time zone is a separate column, not part of the expression. The
  library has no transitive dependencies. It has not been released since 2020,
  which is acceptable for a parser of a frozen syntax.
- Each job stores an IANA time zone, defaulting to a new
  `HOME_AGENT_TIMEZONE` setting (`Europe/Amsterdam`). Instants are stored in UTC.
  The binary embeds `time/tzdata` so the container does not depend on a system
  zone database.
- **One occurrence runs at most once.** `schedule_runs` has
  `UNIQUE(job_id, scheduled_for)`, and a tick claims due jobs, inserts their run,
  and advances `next_run_at` in one transaction. A second tick, a restart, or a
  retry cannot insert the same occurrence twice.
- **There is no lease column.** The application is one process with one writer,
  so recovery happens at startup, as approvals already do: a run left `running`
  by a previous process is marked `interrupted`, and the user is told it was not
  retried. A model turn is never replayed silently, for the same reason the
  conversation does not replay one.
- **Catch-up runs the latest missed occurrence once**, if it is less than six
  hours old, and otherwise skips to the next future occurrence. The grace window
  is a property of the job type, not something the user sets.
- **The run is the outbox.** A run stores its reply and moves through
  `queued → running → ready → delivered`, like a conversation request, and a
  `ready` run is delivered with the conversation's retry and permanent-error
  handling. As with a conversation request, the reply is cleared once it has
  been delivered. A separate notification outbox waits until a second producer
  needs it.
- **Job types are a registry**, keyed by a namespaced type such as
  `model.turn`, each with a typed config, a validator, and a handler that returns
  the text to deliver. `model.turn` is the only type in this plan.
- **A scheduled model turn has read-only tools.** The broker gains
  `ReadOnlyToolsFor`, which offers only `tools.RiskRead` definitions and binds
  the executor to them; grant rechecks are unchanged. No write tool is offered,
  so an unattended run cannot propose a Notion write, file to the NAS, or create
  another job. `nas_send_file` is a read and stays available, so "on the first
  of each month send me my payslip" works — to the owner's own chat only.
  Guidance about write tools moves to a separate `Contribution.WriteGuidance`,
  so a read-only turn is not told about tools it does not have.
- **Each run is an isolated Pi session.** A fresh runtime session ID per run
  keeps chat history out of the prompt and keeps repeated runs from piling up in
  the chat session. The prompt is wrapped in a system line naming the job and
  saying the user is not watching and only read-only tools exist.
- **The delivered report joins the chat.** After a report is delivered, its
  job name and text (bounded to 2,000 characters) become a pending notice for
  the principal's next conversation turn, through the same `Notice` mechanism
  approvals use. The notice is held in memory, so a restart forgets it; the
  report itself is already in the chat.
- **A principal runs one model turn at a time.** Two Pi processes on one
  principal's profile directory would race on token refresh, so the conversation
  service owns a per-principal turn lock that both the chat worker and scheduled
  runs take. A scheduled run waits behind a chat turn; a chat message sent during
  a scheduled run waits too, and shows the typing indicator while it waits.
- **Jobs are created in conversation and confirmed.** The scheduler contributes
  `schedule_list` (read), `schedule_propose` and `schedule_remove` (write, through
  `approval.Propose`). The confirmation text is rendered by Go, not the model:
  name, cron, zone, the next three local run times, the prompt verbatim, and the
  read-only rule. The approved record's ID becomes the job ID, so a repeated
  execution cannot create a second job.
- **Limits:** at least one hour between any two of the next 50 occurrences of a
  `model.turn` schedule; at most ten jobs per principal; a prompt of at most
  1,000 characters; a name of at most 60. At most two runs execute at once
  across the household.
- A run whose principal is disabled, revoked, or no longer allowlisted is
  skipped without delivery. A run whose principal has no connected subscription
  is delivered as the conversation's `NotConnectedReply`, prefixed with the job
  name, rather than silently skipped.
- `/jobs` is one command with inline controls per job — Pause or Resume,
  Run now, Delete — in the style of `/settings`, instead of the separate
  `/pause`, `/resume`, and `/run` commands 06 sketched. Deleting one's own job
  needs no confirmation. "Run now" inserts an out-of-schedule run and leaves
  `next_run_at` alone.
- Logs carry the principal, job ID, job type, and outcome, never the prompt or
  the reply. Metrics are left for when a dashboard needs them.
- On the spring-forward day a job whose local time does not exist is skipped,
  which is the parser's behaviour; on the autumn day the repeated local time
  runs once, because the wrapper passes over an occurrence that repeats the
  previous one's wall-clock time.

## Approach

1. `go.mod`: add `github.com/robfig/cron/v3`. `cmd/home-agent`: import
   `time/tzdata`. `internal/config`: `HOME_AGENT_TIMEZONE`, validated with
   `time.LoadLocation`. Complexity: simple.
2. `internal/store/sqlite/migrations/009_schedules.sql`: `scheduled_jobs` (ID,
   principal, name, type, cron, zone, config JSON, enabled, `next_run_at`,
   timestamps) and `schedule_runs` (ID, job, principal, `scheduled_for`, manual
   flag, status, error kind, reply, timestamps, the unique occurrence key).
   `internal/store/sqlite/schedules.go`: create, list, set enabled, delete,
   advance a due job and queue its occurrence in one transaction, move a run
   between statuses, recover interrupted runs, and prune runs finished more
   than 30 days ago. Complexity: moderate.
3. New `internal/schedule`: the cron wrapper (parse, validate the minimum
   interval, next occurrence in a zone, the latest missed occurrence within the
   grace window); the job-type registry; `Service` with a 30-second tick, the
   concurrency bound, startup recovery, and delivery through the conversation
   `Deliverer` split into Telegram-sized chunks. The clock is injectable.
   Complexity: complex.
4. `internal/conversation`: the per-principal turn lock shared by `runTurn` and a
   new `RunScheduled(ctx, principalID, message)` that applies the
   connection gate, read-only tools, a fresh session, the turn timeout, and the
   same quota, auth, and timeout replies as a chat turn; a notice combinator so
   approval and schedule notices can both reach a turn. Complexity: moderate.
5. `internal/tools/broker.go`: `ReadOnlyToolsFor` and
   `Contribution.WriteGuidance`, which the Notion and NAS connectors now use for
   their write guidance. Complexity: simple.
6. `internal/schedule/tools.go`: the `tools.Provider` for the three tools with
   their guidance (how to write a five-field cron in the user's zone, that runs
   are unattended and read-only, that the prompt must say what to report), and
   the `approval.Executor` that creates or removes a job. Complexity: moderate.
7. `internal/telegram/jobs.go`: `/jobs` with its inline controls and callbacks;
   register the command in `commands.go`; add a `/privacy` sentence saying a
   scheduled job's prompt and its latest reports are stored until it is deleted.
   Complexity: moderate.
8. `internal/app/app.go`: build the store, registry, and service; register the
   provider and executor; start and stop the scheduler after the conversation
   service. Complexity: simple.
9. Documentation: update 06 with the decisions that differ from its sketch (no
   lease column, the run as outbox, `/jobs` controls, stored prompts limited to
   read-only tools), the capability rows in `docs/02-scope-and-capabilities.md`,
   the scheduled reports in `docs/04-integrations.md`, the new setting in
   `deploy/compose.app.yaml`, `deploy/nas.env.example`, and the runbook, and this plan
   with its index row. Complexity: simple.

## Files affected

- `go.mod`, `go.sum`, `cmd/home-agent/main.go`, `internal/config/`
- new `internal/store/sqlite/migrations/009_schedules.sql`,
  `internal/store/sqlite/schedules.go`
- new `internal/schedule/` (`schedule.go`, `cron.go`, `service.go`,
  `modelturn.go`, `tools.go`, `cron_test.go`), new
  `internal/store/sqlite/schedules_test.go`
- `internal/conversation/service.go`, `internal/conversation/types.go`, new
  `internal/conversation/scheduled.go`
- `internal/tools/broker.go`, `internal/connectors/notion/tools.go`,
  `internal/connectors/nas/tools.go`, `internal/connectors/nas/service_test.go`
- new `internal/telegram/jobs.go`, `internal/telegram/commands.go`,
  `internal/telegram/service.go`
- `internal/app/app.go`
- `internal/telegram/conversation_integration_test.go` (an optional scheduler on
  an injected clock), new `internal/telegram/schedule_integration_test.go`
- `deploy/compose.app.yaml`
- `docs/02-scope-and-capabilities.md`, `docs/04-integrations.md`,
  `docs/06-scheduling-and-notifications.md`,
  `docs/13-operator-runbook.md`, `deploy/nas.env.example`,
  `docs/plans/README.md`

## Validation

- `gofmt -l`, `go mod tidy -diff`, `go test -race ./...`, `go vet ./...`,
  `go build -buildvcs=false ./...`, `scripts/check-plans`.
- Cron, as focused unit tests: next occurrences in `Europe/Amsterdam` across
  both daylight-saving transitions, pinning what happens to a job at 02:30 on
  the spring-forward day and that the repeated hour in autumn produces one run;
  the one-hour minimum refuses `*/30 * * * *` and accepts `0 * * * *`; `@every`,
  seconds, and `CRON_TZ=` are refused.
- Store, against a real temporary database: two claims of the same occurrence
  queue one run; the same job ID is created once; the per-principal limit is
  enforced; another principal's job cannot be read, paused, or deleted.
- Integration, through the fake Telegram server and a controlled runtime, with
  an injected clock: a model turn proposes a job, the confirm button creates it
  exactly once, the clock passes its time, the runtime receives the wrapped
  prompt in a fresh session with read tools only and no write guidance, a write
  call from that turn is refused, and the reply arrives in the owner's chat and
  is cleared from the run; the occurrence does not run twice; the next chat
  turn carries the report notice; a chat message during a scheduled run waits
  for it; "Run now" runs without moving `next_run_at`; a paused job does not
  run and a resumed one continues from now; another user's button cannot touch
  the job; a disconnected owner gets the not-connected reply and a revoked one
  gets nothing; after downtime a two-hour-old occurrence runs once and a
  seven-hour-old one is skipped; a run left `running` is reported as
  interrupted at startup and never run again.
- Live, in the operator's chat: a job every hour at minute 5, observed firing
  twice and surviving one deployment restart between them, then deleted from
  `/jobs`.

## Risks and open questions

- The stored prompt runs unattended. Read-only tools cap what it can do, but a
  prompt injection in a Notion page or a NAS file it reads could make it send
  the owner one of their own NAS files. That is the same boundary `nas_send_file`
  already accepts in a live turn.
- A scheduled run holds the principal's turn lock for up to the turn timeout,
  so a chat message sent then waits. A briefing-style job at a time the user is
  likely to be chatting makes that visible.
- A job set inside the missing spring hour, such as 02:30, does not run on that
  one day. Running it at 03:00 instead would need a wrapper that searches for
  the gap; it is left out because nobody schedules a report at 02:30.
- The report notice is lost on restart. If follow-ups after a restart turn out
  to matter, the latest delivered run per principal can serve as the notice
  instead.
- Deterministic job types and a notification outbox shared by several
  producers are deliberately left to the plan that introduces the first such
  type.
