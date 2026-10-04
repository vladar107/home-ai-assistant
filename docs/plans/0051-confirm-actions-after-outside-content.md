# 0051 — Confirm actions after outside content

Status: `done`

## Context

The 2026-10-04 security audit found that text from outside the household can
steer tools that act at once. A conversation that has read a web page, an
email, a calendar invite, or log lines can, in the same or a later turn:

- run a Home Assistant action;
- set a reminder that later arrives as a trusted message;
- start a background task;
- carry data out in a URL through `fetch_page`, or through the `urls` argument
  of the runtime's `web_search`, which the search provider fetches.

The broker records which conversation read third-party content (`exposed_at`
on `conversation_sessions`). Only memory writes act on it: they become
proposals (plan 0046).

Home Assistant's own tools are marked third-party too, and the guidance asks
the model to read the live context before acting. So every conversation that
uses the home is already exposed in the memory sense. Gating actions on that
flag would make every home action need confirmation.

## Goal

Once a turn or its conversation has read content written outside the
household, nothing it does afterwards acts on the outside world without the
user's say-so:

- Home actions, reminders, and background tasks become proposals with Confirm
  and Reject buttons.
- `fetch_page` only opens addresses the turn has actually seen.
- `web_search` no longer fetches model-chosen URLs.

Conversations that only read the household's own devices keep today's
behaviour, and so do background tasks that read the web.

## Decisions

- **Two exposure levels.** Third-party tools stay as they are for memory. A
  new `Household` flag on a tool definition marks third-party text from the
  household's own devices: the Home Assistant tools.
  - These mark the conversation externally exposed, in a new `external_at`
    column: every other third-party result, the runtime's `web_search`, and
    an outside notice injected at turn start.
  - New third-party tools count as external unless marked otherwise.
- **Exposure is tracked at two scopes.**
  - Each run gets a mutable run state on its context, attached beside the turn
    key before tools are collected. It holds an external mark, the URLs seen,
    and the run's instruction text.
  - The broker sets the mark and records URLs from external results.
  - The conversation sink does the same for `web_search`, from the URLs in the
    runtime's `tool_execution_end` events.
  - The instruction text is the raw user message, the confirmed task brief, or
    the job's prompt. It never includes an injected outside notice.
  - Across a chat conversation, the mark persists in `external_at` until
    `/new`, because the content stays in the session's history.
  - A chat turn counts as exposed if either mark is set. A scheduled or task
    run uses its run state only, which matters only for `fetch_page`, since
    those runs get read tools alone.
  - A resumed task run and a task's report run start as exposed, because
    their session may already hold outside content. They keep the brief as
    their instruction.
  - Pending notices are collected before the turn decides about search, so an
    outside notice turns search off in the turn that carries it.
  - Seen addresses match whole. A closing bracket stays part of an address
    that opened it.
  - A failed database lookup counts as exposed.
- **Exposure has its own package.** Marking moves from the memory service into
  `internal/exposure`, which records both levels and answers the gate
  question. Memory keeps reading its own flag.
- **The migration backfills.** Migration `017` adds `external_at` and copies
  `exposed_at` into it, so sessions exposed before the deploy stay gated until
  `/new`.
- **Home actions become proposals.** When the turn is exposed, an immediate
  Home Assistant write tool proposes `ha_action` with the tool name and its
  arguments, rendered exactly in the summary.
  - On confirmation the executor checks again: the switch is on, the snapshot
    is fresh, actions are still allowed, and the tool still exists as a write
    tool. Only then does it run the tool.
- **Reminders and tasks become proposals.**
  - `reminder_set` proposes the reminder, and the time is validated again on
    confirmation.
  - `task_start` proposes the task with the chat request it answers. The task
    service becomes an approval executor: it resolves the request's origin
    under the record's principal and starts the task on confirmation. A full
    queue at that point fails the action with its own message.
  - `reminder_cancel` stays immediate. Injected content can cancel a reminder,
    which costs a reminder but acts on nothing outside.
- **`fetch_page` only opens seen addresses after exposure.** A fetch result has
  to return within the turn, which an approval cannot do. So once the turn is
  exposed, the URL must appear verbatim in one of:
  - the turn's instruction: the user's message, the confirmed task brief, or
    the scheduled job's prompt;
  - an earlier tool result of the same run, including `web_search` results.

  Otherwise the call is refused, and the refusal tells the model to ask the
  user to send the link. The broker carries the instruction text to tools the
  same way it carries the turn key.
- **`web_search` takes no URLs.** The vendored extension drops the `urls`
  argument of `web_search` and stops registering the separate `url_context`
  tool, so the model cannot hand addresses to the provider. `fetch_page`, with
  its guard, is the one way to read a given address. The change is recorded in
  `VENDORED.md`.
- **No `web_search` in an exposed conversation.** With `openai-codex`, the
  provider's search agent can open a page named in the query, so a query can
  carry data to an attacker's address. The runtime's tool cannot be vetoed
  mid-turn. So a chat turn that starts in an externally exposed conversation
  runs without `web_search`, and the guidance tells the model that `/new`
  restores search. Only the turn that first reads outside content keeps the
  gap.
- **The interrupted-action notice is generic.** The text shown for an action
  interrupted by a restart no longer says "check Notion".

## Approach

1. Exposure foundation: the `Household` flag, the external bit on
   `TurnExposureRecorder`, the run state on the context, migration `017` with
   backfill, store methods, `internal/exposure`, `Household` on the Home
   Assistant tools, external marks for `web_search` and the outside notice,
   and the `web_search` result URLs from the runtime's events.
2. Home Assistant gate and the `ha_action` executor.
3. Reminder and task gates and executors.
4. The `fetch_page` gate and the vendored extension change.
5. Wiring in `app.go`, the generic interrupted text, `docs/architecture.md`,
   and guidance text.

## Files affected

- `internal/tools/broker.go`, new `internal/exposure`
- `internal/store/sqlite/` (migration `017`, exposure methods)
- `internal/memory/tools.go` (marking moves out)
- `internal/conversation/` (web search, outside notice, instruction text)
- `internal/telegram/` integration tests, `internal/testutil/pifixture`
- `internal/runtime/pi/` (web search result URLs)
- `internal/connectors/homeassistant/`
- `internal/schedule/tools.go`, `internal/task/`
- `internal/connectors/web/`, `third_party/pi-web-search/`
- `internal/approval/approval.go`, `internal/app/app.go`,
  `docs/architecture.md`

## Validation

- Formatting, module, vet, test, build, and plan checks.
- Integration tests over real SQLite:
  - Household-only exposure leaves home actions immediate.
  - External exposure creates a pending record with the exact arguments, and
    confirming runs it.
  - A switch turned off, or a full task queue, at confirmation fails the
    action with its message.
  - Backfilled sessions read as external.
  - `fetch_page` opens seen addresses and refuses unseen ones with the
    refusal text, and a task that reads the web still fetches before its
    first external result.
- Live checks:
  - Summarise a web page, then ask to turn on a light: the reply carries
    Confirm and Reject. After `/new` the same request runs at once.
  - Search, then open a result: it works.

## Risks and open questions

- The Home Assistant live context also carries text written outside the
  household: media titles, calendar event titles, and to-do items. Under the
  `Household` flag these do not gate actions. This is an accepted trade-off;
  the alternative is to confirm every home action.
- An attacker page can list many links and let the model choose among them,
  which leaks a few bits per fetch within the call budget. A per-host egress
  allowlist would close this and is out of scope.
- Links from an earlier turn need the user to paste them once the
  conversation is exposed.
