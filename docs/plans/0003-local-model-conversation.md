# 0003 — Local model conversation

Status: `done`

## Context

The Telegram walking skeleton authorizes private chats and answers `/status`
and `/privacy` without a model, but ordinary text is ignored. The application
already defines an `AgentRuntime` boundary and the isolated Go-to-Pi spike has
verified ChatGPT subscription access. The next useful slice is natural
conversation that can be tested from GoLand without prematurely solving NAS
runtime credentials or enabling integrations.

## Goal

Make ordinary Telegram text invoke a locked-down Pi runtime with durable,
principal-isolated conversation sessions while preserving deterministic
commands, cancellation, safe restart behavior, and content-free telemetry.

## Decisions

- Deliver a local-first, opt-in conversation runtime; the existing deterministic bot remains usable when conversation is disabled, and turn-level runtime failures do not affect commands.
- Keep commands as deterministic operational controls and route only ordinary text to the model.
- Let Pi persist conversation history in protected per-session files; keep ownership and request lifecycle metadata in SQLite.
- Persist accepted conversation work before advancing the Telegram cursor so a restart cannot silently lose an accepted message.
- Do not replay a turn that was running during a restart because Pi may already have persisted part of it; deliver a deterministic interruption reply and start the next turn in a fresh session.
- Allow at most one active turn per principal while allowing different principals to run concurrently.
- Invoke Pi with no tools, extensions, skills, templates, themes, context files, or approval capability.
- Give Pi only the home directory needed for its authentication profile, the executable directory, and an explicit telemetry opt-out; do not pass application secrets through the subprocess environment.
- Resolve possible runtime tools per principal on every turn, but return no tools until a later user-integration plan.
- Keep integration preferences non-secret and principal-scoped; keep credentials outside those preferences.
- Defer Pi packaging and production credential delivery to a later NAS plan.
- Prioritize integration and end-to-end validation across real components; add unit tests only where they provide distinct behavioral value. The user's authorization to introduce tests replaces the original no-new-test-files constraint.
- Validate principal-isolated sessions with simulated model accounts in routine tests. The current shared Pi authentication profile is an owner-only prototype limitation; per-user subscription ownership is required by [0004](0004-per-user-model-subscriptions.md) and must not be claimed as delivered by this plan.

## Approach

1. Add conversation configuration and SQLite migrations for active sessions and durable requests.
2. Promote the verified Pi JSONL RPC logic into a bounded `AgentRuntime` adapter with persistent session IDs and deterministic cancellation.
3. Add a conversation coordinator that recovers queued work, serializes turns per principal, and exposes principal-scoped cancel and new-session operations.
4. Route authorized ordinary Telegram text to the coordinator while keeping `/status`, `/privacy`, `/cancel`, and `/new` deterministic.
5. Add safe runtime metrics and traces without logging prompts, replies, credentials, or raw runtime output.
6. Document GoLand configuration, stored data, commands, limitations, and the real Telegram verification procedure.
7. Add the automated integration and local end-to-end validation described below, repair defects demonstrated by those scenarios, and record the remaining live verification evidence before completing this plan.

## Files affected

- `internal/config/`
- `internal/store/sqlite/` and a new migration
- `internal/runtime/pi/`
- `internal/conversation/`
- `internal/telegram/`
- `internal/app/` and `cmd/home-agent/`
- `.env.example`, `README.md`, and relevant documentation
- Grafana dashboard and deployment configuration where runtime state must be exposed

## Validation implementation plan

The conversation implementation is already in progress. This section plans the
new testing work before test implementation; it is not a reconstruction of
earlier validation. The user approved this testing approach before execution.

### Wave 1 — Independent integration suites

- Conversation flow and persistence (complex): exercise the real Telegram
  client, authorization service, conversation coordinator, and migrated SQLite
  store against a loopback Telegram API simulator and a controllable runtime.
  Own `internal/telegram/*_test.go` and `internal/conversation/*_test.go` as
  needed. Cover ordinary text, contextual session reuse, `/new`, busy handling,
  responsive commands, cancellation, unauthorized users, private-chat-only
  ingress, independent principal sessions, duplicate updates, and restart
  recovery. Assert observable delivery and durable state, not internal call
  sequences. Use channels and bounded deadlines to coordinate concurrency.
- Pi process boundary (complex): exercise the real adapter against a small Go
  JSONL RPC fixture launched as a subprocess. Own `internal/runtime/pi/*_test.go`
  and the reusable Pi fixture. Cover version compatibility, exact configured
  provider/model forwarding, session reuse, tool lockdown, minimized environment,
  isolated working directory, timeout/cancellation and process exit, malformed
  or oversized output, and safe runtime failures. The fixture must work without
  Node, Pi installation, credentials, or network access.

### Wave 2 — Complete conversation path and delivery failures

- After both fixtures are usable, exercise the real Telegram, identity,
  conversation, SQLite, and Pi components together through their existing
  constructors. Extend `internal/telegram/*_test.go` with minimal shared test
  helpers if needed. This provides local end-to-end coverage of the conversation
  path through the external process boundary. It does not claim coverage of the
  executable's configuration loading or health-listener startup.
- Avoid reserving then reopening a free port for an `App.Run` test: the current
  configuration rejects port zero, and that workaround introduces a race.
  A separate app-listener change is not a prerequisite for this validation slice.
- Verify recovery of queued, running, and ready-to-deliver requests across
  restarts; interrupted model work is not replayed. Verify transient Telegram
  delivery failures retain work without reinvoking the model and permanent
  failures do not leave a principal permanently busy.
- Cover successful sends followed by uncertain delivery acknowledgement as an
  at-least-once boundary. Do not assert an exactly-once Telegram guarantee the
  implementation cannot provide; document possible duplicate replies or chunks.
- Use explicit configuration, synthetic tokens, private temporary directories,
  and loopback endpoints. Routine tests must not load the repository `.env`,
  access real Pi credentials/history, or contact the operator's bot.

### Wave 3 — Verification and completion evidence

- Run formatting, `go mod tidy -diff`, `go test ./...`, race-enabled integration
  tests, vet, build, `scripts/check-plans`, and whitespace checks. Use the existing
  CI test job and add a race check if supported by the configured runner.
- Review the tests and any directly related fixes. Add no dependencies or
  unrelated refactoring; do not add production abstractions merely to mock them.
- Update the runbook and this plan with actual commands, results, and remaining
  limits. Automated tests with simulated services do not prove real provider
  authentication, model quality, or Telegram service compatibility.
- Coordinate the final live owner-account smoke check with GoLand: plain text,
  contextual follow-up, `/new`, responsive commands, cancellation, and restart.
  Never start a competing poller or restart the operator's process automatically.
- Keep status `in-progress` until automated checks pass and the live acceptance
  evidence is recorded. Per-user subscription implementation, photo input, and
  a model change remain separate from this validation scope.

## Validation results

Recorded on 2026-09-05 after the Wave 1 and Wave 2 suites were completed.

### Defects demonstrated by the new suites and fixed

- `internal/runtime/pi`: when the adapter rejected an event while the child
  process kept writing, the stdout scanner stayed blocked on the full event
  channel after the process had been stopped, leaking one goroutine per failed
  turn (`TestRejectingQueuedEventsReleasesReader`). The scanner now stops as
  soon as `Run` returns.
- `internal/store/sqlite`: a request in the `delivering` state did not count as
  pending, so a message that arrived while a reply was being sent was enqueued
  instead of receiving the busy reply, contradicting the one-active-turn rule
  (`TestBusyWhileResponseIsBeingDelivered`). The busy check now includes
  `delivering`.
- Test-only: the restart-recovery scenario used identifiers shorter than the
  36-character schema check and could not create its fixture rows.
- `internal/store/sqlite`: `EnqueueConversation` ran as a deferred WAL
  transaction that read before writing. When another principal's worker
  committed during that window, SQLite returned `SQLITE_BUSY` immediately
  instead of honouring the busy timeout, the poll batch failed, and the batch
  was retried after backoff (`TestConversationSessionsAndUserIsolation` under
  `-race`). The store now begins every transaction with an immediate lock; all
  of its transactions write, so writers queue on the busy timeout instead.

### Automated checks

All of the following passed on 2026-09-05 from the repository root:

```sh
gofmt -l .
go mod tidy -diff
go test ./...
go test -race ./...
go vet ./...
go build -buildvcs=false ./...
scripts/check-plans
git diff --check
```

### Live verification

Observed from the local GoLand run on 2026-09-05 through the application's
own SQLite state and Pi session metadata, without reading conversation text:

- 12:02 to 12:11 CEST with `gpt-5.5`: nine ordinary-text turns completed and
  were delivered, and `/new` closed the session with reason `user_reset`.
- 16:31 to 16:49 CEST with `gpt-5.6-luna:low`, after a second `/new`: Pi
  accepted the model, one turn completed, and seven failed because the
  ChatGPT subscription reported that its usage limit had been reached. Five
  produced the unavailable reply; two reached the two-minute turn timeout
  because Pi kept retrying.
- `/status` and `/privacy` had already passed a real Telegram test before
  conversation was enabled.

Automated suites use simulated Telegram and Pi services and do not prove
subscription access, model quality, or Telegram compatibility. The operator
accepted the plan as done on 2026-09-05 with the gaps below.

### Not verified and carried forward

- `/cancel`, the busy reply, an unauthorized user while conversation is
  enabled, and a restart during a running turn were not exercised live.
- A quota failure should produce an actionable reply within seconds instead
  of the generic unavailable reply or a turn timeout.
- Re-verification with `gpt-5.6-luna:low` once the subscription quota resets.

These items move to [0005](0005-bot-commands-and-provider-failures.md) and,
for per-user subscription state, to [0004](0004-per-user-model-subscriptions.md).

## Risks and open questions

- The Pi authentication profile and session files contain sensitive runtime state and require protected local permissions.
- Pi is process-isolated by configuration but not OS-sandboxed; the pinned local package runs as the application user and is a trusted dependency.
- A successful Telegram send followed by a crash before delivery state is committed may cause a duplicate reply; silent loss is not accepted.
- Pi credential refresh, NAS packaging, resource usage, and intended multi-user subscription use remain production gates.
- Per-user integration discovery and enable/disable controls will be implemented only when the first real integration is introduced.
