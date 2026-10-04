# 0048 — Background sub-agents that do not block the chat

Status: `in-progress`

## Context

A chat turn is bounded twice. It must finish within
`HOME_AGENT_CONVERSATION_TURN_TIMEOUT` (2 minutes in both environments; the
configuration rejects anything above 10 minutes). The broker also stops it after
8 tool calls. That fits nearly every request: over the last 30 days, 95% of
turns finished within about 34 seconds. It does not fit requests that need tens
of minutes of reading and comparing, such as going through a month of mail and
summarising what needs action, or researching an appliance across many pages.
Today those requests time out or end with whatever 8 calls found.

The per-principal turn slot allows one Pi process per principal at a time. It
was added because two processes on one profile could race when refreshing
OAuth credentials. Pi 1.0.0 removes that race. Before a token expires, its
credential resolution takes a file lock on `auth.json`, re-checks expiry under
the lock, and skips the refresh when another process already did it
(`pi-ai/dist/auth/resolve.js`, "double-checked locking … refresh once
globally"). A second Pi process on the same profile can therefore run beside a
chat turn.

Scheduled runs already show the pieces a sub-agent needs:

- a durable run row that survives restarts
- a fresh Pi session per run
- read-only tools for a turn nobody watches
- delivery through the outbox
- a pending notice that tells the next chat turn what happened

## Goal

The model decides whether a request can be answered in this turn or needs a
sub-agent. For a long one it spawns a sub-agent and replies at once ("I'm on it,
I'll report back"). The sub-agent runs as its own Pi process beside the chat,
which stays fully usable. The user can see and cancel running sub-agents with
`/tasks`. A single status message shows progress. The result arrives as a reply
to the original request, survives a restart, and is known to the next chat
turn, so "what did you find?" works.

## Decisions

- **The model chooses foreground or background through a tool.** `task_start`
  takes a short title and a self-contained brief, since the sub-agent does not
  see the chat. The guidance says to use it when the request needs many lookups
  or extensive reading, and to answer directly otherwise. `task_start` is
  exempt from the per-turn call budget through a new `OutsideBudget` flag on a
  tool definition. When the budget runs out, its message suggests handing the
  rest to `task_start`, so a request that turns out larger than expected can
  still move to the background. `task_start` is a write-risk tool, like
  `reminder_set`, so it is absent from scheduled runs and from sub-agents, and a
  sub-agent cannot start another. It accepts only a chat turn's key, from which
  the originating chat and Telegram message are read.
- **Starting a sub-agent needs no confirmation.** Like `reminder_set`, it only
  spends the user's own subscription on read-only work for the user. The reply
  and the status message say that it started. `task_start` sends the status
  message itself, as a reply to the request: "Starting: <title>", or "Queued:
  <title>" behind an open task.
- **A sub-agent is a durable row with its own Pi session and process.** The
  `tasks` table holds:
  - the principal and chat
  - the originating Telegram message
  - the title and brief
  - the Pi session id
  - the status: queued, running, done, failed, or cancelled
  - the status message id, the deadline, the counters, and the result
  - the number of attempts, the delivery time, and whether a chat turn has been
    told about the report
  The Pi session is fresh and separate from the chat session, so the
  sub-agent's context does not grow the chat. The application spawns the
  process. Pi has no sub-agent feature of its own, and keeping spawning in Go
  keeps the tools, limits, and audit under the broker.
- **No Pi sub-agent extension.** Pi's example `subagent` extension and the
  community `pi-subagents` packages were considered and do not fit:
  - Their children live inside, or report to, a long-lived parent Pi session.
    Here each chat turn's Pi process exits when the turn settles. The
    `@tintinweb/pi-subagents` package loses running children when the parent
    exits, and `nicobailon/pi-subagents` reports into the Pi conversation, not
    to Telegram.
  - Children take their tools from agent definition files, which usually means
    Pi's built-in tools. The broker's tools reach Pi only through the per-turn
    bridge, and their budgets, grant rechecks, untrusted-result marking, and
    audit would be bypassed.
  - Durability, per-user limits, and telemetry would live outside SQLite and the
    existing metrics.
  Their useful ideas are adopted instead:
  - return an id at once, then notify on completion, with a separate full-result
    tool
  - cap the result preview
  Steering a running child, which the RPC process would allow, is left for later;
  nothing in this plan needs it.
- **Sub-agents run beside chat, outside the turn slot.** A sub-agent is one
  long Pi run with its own deadline and a larger call budget. It does not take
  the slot, so chat and scheduled runs are never delayed by it. The slot itself
  stays as it is for chat and scheduled runs; relaxing it is a separate
  decision.
- **Hard limits.** One running sub-agent per principal and at most three
  queued. A sub-agent's wall time is capped by `HOME_AGENT_TASK_TIMEOUT`
  (default 60 minutes, 1 minute to 6 hours), counted from its first start, so a
  restart does not extend it. Its broker tool calls are capped at 100 through a
  per-run budget the broker reads from the run's context, and its model calls at
  150. Web searches run inside Pi and outside the broker, so the model-call
  budget is what bounds them. Pi reports each finished assistant message to the
  run's event sink as a `model_call` event with its usage, which is how the
  model calls are counted while the run goes on.
- **A limit asks for a partial report with a second short run.** When the
  deadline or the model-call budget stops the run, the sub-agent's own session
  is run once more for at most 3 minutes with "report what you have". It keeps
  its tools so its history stays consistent, but its call budget is zero, so
  every call is refused. A run that spent its tool budget gets the
  budget-exhausted result and answers on its own. Either answer is delivered
  marked as partial. A report run that fails or times out fails the task with a
  short message.
- **Sub-agents are read-only.** They get the read-only tools and web search that
  scheduled runs get. Like a scheduled run, a sub-agent sees the user's memory
  ([0046](0046-per-user-memory.md)) in its prompt but cannot change it. A
  sub-agent that finds something to
  change says so in its report, and the user asks for it in chat, where
  approvals work as today.
- **Restarts resume, not abandon.** Unlike an interrupted chat turn, an
  interrupted sub-agent is started again on its own session with "You were
  interrupted; continue the task and report when it is complete." Its session
  file holds the progress, and read-only work is safe to repeat. Its counters
  are saved with each progress update and on shutdown, so the budgets carry
  over. A sub-agent whose run was started twice and is found running at startup
  fails with a message instead of starting a third time. A report that was not
  yet delivered is delivered after the restart.
- **Progress is one edited message.** When the run starts, the status message
  becomes "Working on: <title>" with a Cancel button, and it is edited at most
  once a minute with the elapsed time and the number of lookups, taken from the
  run's tool events. It never sends a message per step. On completion the status
  message shows the final state without buttons, and the report is sent as a
  reply to the originating message, split like chat replies. A card a
  sub-agent's tool staged goes out with the report under its own outbox turn
  kind, `task`, which `/new` does not discard.
- **The chat learns the result.** The next chat turn gets
  `[system] Background task "<title>" (id …) finished: <first lines>` through the
  existing pending notice. Unlike the scheduler's notices it is kept in SQLite,
  so it survives a restart; it is given once, for reports delivered in the last
  24 hours. A `task_result` read tool returns the full report by id, or lists
  the recent tasks without one, so follow-up questions can use it. Its result is
  marked third-party, since a report retells the mail and pages it read. For the
  same reason the chat turn that receives a task's notice, or a scheduled run's,
  counts as having read outside content, so a memory fact it writes needs
  Confirm ([0046](0046-per-user-memory.md)).
- **Only chat turns hear about `task_start`.** Its guidance is write guidance,
  so scheduled runs and sub-agents, which cannot call it, are never told to.
- **The report run cannot search.** The run that asks for a report once a limit
  is reached gets no broker calls and no web search, which no call budget would
  bound. Web searches are counted apart from broker calls, so a resumed run's
  broker budget is not used up by searches.
- **A finished report is kept through a shutdown.** Storing the result does not
  depend on the worker's context, so a deploy that lands as a run completes does
  not lose the report or spend another attempt.
- **Retention.** A task's brief and report are kept for 30 days after it
  finishes, then pruned at startup. The task's Pi session file, which holds what
  it read, stays with the user's other session history, which is never deleted
  today ([0047](0047-earlier-conversations.md)); `/privacy` says both.
- **`/tasks` lists running, queued, and recently finished sub-agents** (the
  last 7 days, at most 10) with Cancel buttons. Cancel stops the process at once
  and works only on the presser's own task. `/cancel` keeps cancelling the
  current chat turn only.

## Approach

1. Confirm the concurrent refresh on the NAS: run a sub-agent and a chat turn on
   one profile across a token expiry and check that both continue and that
   `auth.json` holds one valid credential. Complexity: simple.
2. Migration `016_tasks.sql` and store methods: create with the per-principal
   limits, claim, finish, cancel, recover. Complexity: simple.
3. `internal/task`: the service with one worker per principal, the isolated run
   (built like `RunScheduled`, without the turn slot, with its own deadline and
   budget), the partial-report request at a limit, recovery on start, and the
   tools `task_start` and `task_result`. The broker gains a per-run call budget.
   Complexity: complex.
4. Telegram: the status message and its edits, Cancel callbacks, `/tasks`, the
   report delivery as a reply, and the pending notice. Complexity: moderate.
5. Configuration: `HOME_AGENT_TASKS_ENABLED` and `HOME_AGENT_TASK_TIMEOUT`,
   mirrored into `.env`, `.env.example`, and the NAS env files.
6. Telemetry: sub-agents by outcome, duration, model calls, tool calls, and
   tokens; one trace per run of a sub-agent, so a resumed task has one trace per
   attempt. Outcomes are done, partial, failed, cancelled, and interrupted.
7. Update `README.md`, `docs/architecture.md`, `docs/operations.md`, `/help`, and
   `/privacy`.

## Files affected

- `internal/store/sqlite/migrations/016_tasks.sql`, `internal/store/sqlite/tasks.go`
- `internal/task/` (new: `task.go`, `service.go`, `tools.go`)
- `internal/conversation/task.go` (isolated run outside the turn slot)
- `internal/runtime/runtime.go`, `internal/runtime/pi/runtime.go` (`model_call`
  events), `internal/runtime/pi/usage_test.go`
- `internal/tools/broker.go`, new `internal/tools/budget.go` (per-run budget,
  budget-exhausted guidance)
- `internal/outbox/outbox.go` (the `task` turn kind)
- `internal/observability/telemetry.go`, new `internal/observability/tasks.go`
- `internal/telegram/` (new `tasks.go` for `/tasks`, the status message, and
  callbacks; `service.go` for the command, callback, `/help`, and `/privacy`)
- `internal/telegram/conversation_integration_test.go` (the harness adds tasks),
  new `internal/telegram/tasks_integration_test.go`
- `internal/config/config.go`, new `internal/config/tasks.go`, `internal/app/app.go`
- `.env.example`, `deploy/nas.env.example`, `deploy/compose.app.yaml`
- `README.md`, `docs/architecture.md`, `docs/operations.md`

## Validation

- `gofmt`, `go vet ./...`, `go test -race ./...`, `go build ./...`,
  `scripts/check-plans`.
- Integration tests with the Pi fixture:
  - a chat message sent while a sub-agent runs is answered without waiting
  - the report arrives as a reply to the originating message
  - Cancel stops the process
  - a restart mid-run resumes and delivers
  - hitting a limit yields a partial report
  - the queue and running limits are enforced
  - the next chat turn carries the notice
- Live: ask for a long mail or web research request. Check that:
  - the model starts a sub-agent by itself
  - chat answers while it runs
  - the status message updates
  - the report arrives
  - "what did you find?" uses it

## Risks and open questions

- The model may spawn sub-agents for requests it could answer at once, or may
  not spawn them when it should. Measure how often `task_start` is called and
  how often foreground turns exhaust their budget, then tune the guidance.
- Long runs spend the user's subscription quota quickly. A quota error ends the
  sub-agent with the quota reply, like a chat turn.
- Each Pi process is a Node process on the NAS. Measure its memory during a
  long run before raising the limit of one running sub-agent per principal.
- Running several sub-agents in parallel for one request (for example mail and
  web at once) is a later step once one at a time has been used for a while.
- A run stopped by its deadline, a restart, or the model-call budget can leave
  its session ending in a tool call without a result. The resumed run and the
  report run rely on Pi accepting such a session; check it live with a task
  that is interrupted during a lookup.
- Report delivery is at least once, like the scheduler's: a failure partway
  through a long report, or a crash before it is marked delivered, sends the
  sent chunks again.
- A task failed at startup because it was interrupted twice records no run
  outcome metric; its failure report is still delivered.
- The integration tests restart only the task service, which leaves the old
  service registered with the broker and the notices, so Cancel, `task_start`,
  and the notice are not exercised after a restart, nor is a report that was
  finished but not delivered when the process stopped.
- Tasks that wait on an outside job ("tell me when X is done") are a different
  shape, a polling job type in the scheduler, and are not part of this plan.
