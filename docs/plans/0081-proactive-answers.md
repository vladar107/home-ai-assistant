# 0081 — Proactive answers

Status: `done`

## Context

A review of the operator's own chats from 9 to 14 October showed the assistant
doing less than a short message asked for, waiting to be pushed, and never
speaking first unless a fixed job told it to:

- "How's my weight?" got the current value and a false "no history is visible",
  although `ha_history` was offered and the body-fat sensor was in the same live
  context. Each later step (history, body fat, the verdict) needed another
  message, and the assistant asked for age and sex instead of answering with
  what it had.
- "When do I leave for the train?" and "Will I get to work on Wednesday?" were
  answered with questions, although the calendar and the user's scheduled jobs
  held the answer.
- "I can't watch websites" was said while agent watches existed, and "checked
  again" was said without calling a tool.
- A reply ended with "I'll look up the route" and no tool call, so the user had
  to say "go on". Offers such as "I can set up an alert, shall I?" needed a
  "yes" for what the request already implied.
- After Confirm on a memory fact, the continuation turn replied
  `[nothing-to-add]` while the user's question was still unanswered.
- The fact needed Confirm at all only because the conversation had started with
  a morning report, and every injected job report marks the conversation as
  having read outside content, whatever its run read.
- An hourly mail check sent "No new emails were found" every hour, because a
  scheduled run always delivers its reply.

Causes in the code:

- The system prompt ends with "Ask a brief clarifying question when the request
  is ambiguous" and says nothing about gathering related data, assuming
  sensibly, taking the implied next step, or trying a tool before claiming
  something is impossible.
- `ContinueMessage` asks only whether a *step* of the request remains, so a
  question left open beside a confirmed action counts as done.
- A Confirm pressed while another turn is queued or running gets no
  continuation (`ErrBusy`, logged and dropped). Loki shows none dropped in the
  last 14 days, but nothing prevents it.
- `ha_history` returns only mean, min, and max for one fixed period: no
  direction, rate, or points, and nothing longer than a year or a rolling 30 days.
- A sensor or device name is resolved against a snapshot cached for 5 minutes,
  so a sensor exposed a minute ago is reported as missing.
- `job_reports` rows keep `private` but not whether the run read outside content.
- A scheduled run cannot decide that it has nothing to say.
- The assistant only speaks first through jobs the user wrote.

## Goal

A short message gets a complete, useful answer in one reply: the assistant
collects what its tools already offer, states any assumption instead of asking,
takes the next step the request implies, finishes what it starts in the same
turn, and finishes the user's question after a Confirm. Scheduled checks speak
only when there is something to say, and a user who turns on Heads-up hears
from the assistant, unprompted, when something in their day needs attention.

## Decisions

- **Behaviour rules in the prompt replace the clarifying-question line.** The
  assistant:
  - answers the intent, not only the literal question;
  - uses the data its tools already return (related sensors, history, calendar,
    jobs, memory) before replying;
  - picks the most likely reading and states the assumption in one short
    clause, asking only when a wrong guess would cost something (money, a
    message to someone, something hard to undo) or nothing sensible can be
    assumed;
  - takes the next step the request implies instead of offering it: the lookup
    the answer depends on, a fact the user just stated, a reminder, an alert for
    the one train the user worries about today. Act-and-tell writes run at once
    with Undo, others are proposed as usual. A new recurring job or watch, which
    keeps spending the user's subscription, is created only when asked and
    otherwise mentioned in one line; a step the request does not imply is
    mentioned in one line at most, never asked as a question;
  - never says something is unavailable or impossible without having called the
    tool that would show it in this turn;
  - never ends a reply by announcing work it has not done ("I'll check…"): it
    does it now or says what it needs.

  The rules stay in the cacheable system prompt. Those about tools go only into
  the variant with tools; the clarifying line of the variants without tools
  stays.
- **The continuation finishes the request, not only its steps.** `ContinueMessage`
  asks the model to do any remaining step and to answer any question from the
  user's earlier messages that is still unanswered; `[nothing-to-add]` only when
  the outcome already answers everything. It stays an exact constant, since own
  messages are recognized by equality.
- **A Confirm pressed during a turn still continues.** When `Enqueue` reports
  busy, the principal is remembered. After the in-flight turn is delivered, the
  continuation is queued only if the approval outcome notice is still pending,
  so the outcome is answered once. The approval service keeps the deferred
  turn, since its after-delivery hook already runs after every chat reply. It stays in memory, best effort like today: a
  restart loses only the automatic step, and the outcome still reaches the next
  turn as a notice.
- **`ha_history` gives trends.** An optional `points` argument (`day`, `week`,
  `month`) returns the period as points (mean, or change for counters) from
  `recorder/statistics_during_period`, which takes explicit start and end times:
  the named periods are converted to times in Home Assistant's own time zone,
  read from `/api/config` with the snapshot. Above 60 points the next coarser
  bucket is used; at `month`, the latest 60 months are returned and the answer
  says so. A points answer ends with first, last, and the change between them.
  `last_90_days`, `last_365_days`, and `all` (from the first statistic) join the
  periods. Without `points` the answer is unchanged.
- **A name miss refreshes once.** When a history or watch lookup finds no
  matching exposed entity, the snapshot is refreshed once, under the snapshot
  lock and unless it is under 30 seconds old, and the lookup retried, so a newly
  exposed sensor works without waiting 5 minutes and concurrent misses refresh
  once. A failed refresh keeps the stale snapshot and the lookup still misses.
- **A report is outside content only when its run was.** `job_reports` gains
  `external`, set from the run's own record (`tools.Run.External()`), carried
  through `ScheduledReply`, `schedule.Result`, and `schedule.Run` like
  `Private`. A run recovered after a restart has lost its record and counts as
  external unless it is a reminder, the same rule `Private` follows; reminders
  are never external, since their text is the user's own. Existing rows are
  external. Injected excerpts and replies to a report mark the conversation per
  report, only when it is external. The `job_reports` read tool keeps its
  third-party marking: reading reports on demand is unchanged.
- **A scheduled run may stay quiet.** The model-turn handler returns a quiet
  result when a successful reply is only `[nothing-to-add]`; the marker is
  stripped from any other reply. Other job types are unaffected. A quiet run
  ends as `skipped` with reason `quiet`, which the schema already allows:
  nothing is sent, no report is stored, a staged card is removed, and
  `home_agent.schedule.runs` counts it under its own outcome. A manual Run now
  that ends quiet sends "Nothing to report right now" instead, so the button
  never looks broken. The scheduled turn's message (`scheduledMessage`, not the
  path agent watches share) says it may stay quiet when its instructions ask to
  report only on a condition and the condition does not hold; a failed run
  still reports its failure. The schedule tools' guidance says to write "only
  tell me when…" jobs this way.
- **Heads-up: the assistant speaks first.** A per-user Heads-up switch in
  `/settings` (Everyday tools), off by default, so each user opts in to spending
  their own subscription. Turning it on creates an ordinary model-turn job named
  "Heads-up", at 08:00 and 19:00 in the default zone, with a built-in prompt
  of at most 1000 runes (the job limit); turning it off deletes it. Its ID is
  derived from the principal (the first 8 bytes of a SHA-256 of a fixed label
  and the principal ID, hex), so a double press creates it once, a rename or
  reword in chat keeps it, and the switch shows on exactly while that job
  exists and is enabled; "on" for a paused one resumes it, and the job limit is
  shown as a note. Because it is an ordinary job, it shows in `/jobs`, and the
  user can move, pause, or reword it in conversation; the prompt is stored with
  the job, so later tuning of the built-in text reaches new jobs only. The
  reply is written in the language of the user's memory and other jobs. The
  prompt asks the run to
  look at today's and tomorrow's calendar, the user's jobs, watches, and saved
  routes, transit disruptions on the trips those imply, the weather where the
  user will be, Home Assistant devices needing attention (open doors,
  errors, low batteries, a full bin), health and energy trends, and pending
  reminders; to read the reports already sent today with `job_reports` and
  repeat none of them; and to send a few short lines only when something needs
  the user's attention or action, otherwise `[nothing-to-add]`. It is a
  scheduled turn: read-only tools, its own Pi session, the owner's subscription,
  and nothing written without the user, with a budget of 20 tool calls instead
  of a turn's usual 8, since its prompt asks for about a dozen lookups. The
  switch shows on when the job exists and is enabled.

## Approach

1. Prompt behaviour rules and `ContinueMessage`.
2. Busy-safe continuation in `telegram`, `conversation`, and `approval`.
3. `ha_history` points and periods; snapshot refresh on a name miss.
4. `external` on job reports: migration `028_job_report_external.sql`, the
   scheduled reply carries `External`, `saveReport` stores it, `TurnReports`
   and the reply-to-report path mark outside content only for external reports.
5. Quiet scheduled runs, then Heads-up: the settings switch, the built-in job
   prompt, and its creation, resumption, and deletion through the schedule
   service.
6. Add integration tests for the new behaviour and align existing ones.
7. Update `docs/architecture.md` and `README.md`; check live in chat.

## Files affected

- `internal/runtime/pi/runtime.go`, `internal/conversation/service.go`
- `internal/telegram/service.go`, `internal/approval/approval.go`
- `internal/connectors/homeassistant/` (tools, snapshot, watch, tests)
- `internal/schedule/`, `internal/conversation/scheduled.go`,
  `internal/store/sqlite/` (migration, job reports)
- `internal/telegram/settings.go` and a Heads-up section
- `docs/architecture.md`, `README.md`, `docs/plans/README.md`

## Validation

- `gofmt`, `go mod tidy` check, `go vet ./...`, `go test -race ./...`,
  `go build ./...`, `scripts/check-plans`.
- Integration tests: Confirm during a running turn gives one continuation; a
  report from a run that read only household content leaves the conversation
  clean, and one from a run that read a web page marks it; history points over
  the simulator, with coarsening and refresh on a miss; a quiet run sends
  nothing and stores no report; the Heads-up switch creates and deletes its job.
- Live in chat on the operator's server: "how's my weight?" answers with the
  trend and body fat in one reply; a request that needs a lookup gets it in the
  same turn rather than "I'll check"; a fact saved in a conversation that only
  saw a morning report is written at once; Confirm during a running turn still
  continues; the hourly mail check stays quiet without new mail; Heads-up sends
  a message only when something needs attention.

## Risks and open questions

- Fewer questions and implied steps mean more wrong guesses; the stated
  assumption and Undo keep them visible and cheap to correct.
- An implied write in a conversation that has read outside content is still
  proposed, so the rule never bypasses Confirm.
- A report from a run that read only household content no longer marks the
  conversation; the run's own record is the same signal chat turns already use.
- Heads-up costs two model turns a day per user who turns it on, and its value
  depends on its prompt; the prompt is tuned after first use like other
  features. Its runs almost always read outside content (transit, web, mail),
  so a conversation that takes its report waits for Confirm on writes, as with
  any such report.
- The "never end with undone work" and "take the implied step" rules are prompt
  rules; they are checked live, not enforced in code.
