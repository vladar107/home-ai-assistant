# 0030 — Reply latency, model, and business metrics

Status: `in-progress`

## Context

Plan 0029 added TypeSafe safety checks. Before users turn them on, the household needs a
baseline to judge whether they help. Today's metrics cannot give one:

- `home_agent_conversation_turn_duration` runs from the worker claiming a request to the model
  finishing. It leaves out queue wait, turn setup (including the tool-hint wait, up to 800 ms),
  and reply delivery.
- Nothing measures model calls, individual tool calls, day-card rendering, approval outcomes,
  or token usage.
- The home-agent Grafana dashboard has Service, Telegram, Conversation, and Store rows only.

## Goal

Measure what a user experiences and what the assistant does, per user, so that turning on
safety checks can be compared against a baseline:

- how long a reply takes end to end, and which stage the time goes to;
- how the model behaves: call latency, model calls and tokens per turn, prompt-cache reuse;
- how tools, approvals, scheduled runs, and the safety checks are used and how they end.

## Decisions

- **Principal label:** metrics about a user's activity carry `home_agent.principal`, approved
  on 2026-09-30. There are two users, so series stay few. Principal IDs are configuration,
  not secrets.
- **Safety-checks label:** reply and turn metrics carry `home_agent.safety_checks` with the
  value `on`, `off`, or `unavailable` (no key). The conversation service reads it once per turn;
  it is one indexed SQLite read. Tool and approval metrics do not carry it: approvals carry
  `warned` instead, and dashboards compare tools per principal and period. Because users opt
  in individually, users with and without the checks can be compared over the same period.
- **Existing patterns:**
  - Instruments and recording methods live in `internal/observability/telemetry.go` in its
    `Start…`/`Record…` shape; a `Start…` method also opens a span for Tempo.
  - Packages that have no telemetry today get it through a setter at wiring time, like
    `SetExposure`, so constructor signatures and test fakes stay unchanged.
  - Prometheus names follow `add_metric_suffixes = false`: dots become underscores, with no
    unit or `_total` suffix.
- **No content in metrics:** labels never carry message text, tool arguments, page titles, or
  model output. Tool names are labelled only when bound to the turn; anything else becomes
  `unknown`.
- **End-to-end latency** runs from the request's stored `created_at`, set when the Telegram
  message is queued, to the last reply chunk Telegram accepts. The store starts loading
  `created_at` into `Request`. Telegram's own delay before the update reaches the bot is
  outside this; it already shows in polling and update-processing time.
- **Stages of a chat reply** are `queue` (queued → claimed), `setup` (gate, tools, web-search
  switch, attachments, and the hint wait), `model` (the Pi run, including tool calls and
  day-card rendering inside tools), and `delivery` (card upload and reply chunks). Their means
  stay within the mean reply time; the short hand-off between the model finishing and delivery
  starting belongs to no stage.
- **Recovered requests:** a request created before this process started is labelled outcome
  `recovered`, and one whose reply could not be delivered `undeliverable`. Dashboards
  exclude both from latency panels.
- **Model call time is per assistant message:** from Pi's `message_start` to `message_end` on
  the assistant role. Usage, provider, model, and stop reason come from that message (pi-ai's
  `AssistantMessage` has a required `usage` with `input`, `output`, `cacheRead`, `cacheWrite`,
  and a stop reason from a fixed set). The shape is verified against the pinned Pi 0.84.1
  before relying on it; missing fields leave the metric unrecorded, never estimated.
- **Tool calls per turn and hint follow-through** come from the runtime event sink, which
  already sees every tool call by name, including Pi's built-in `web_search`. This replaces
  `discardSink`. The broker cannot see a turn end.
- **Baseline runs with the key set and nobody opted in.** Nothing is sent to TypeSafe, and
  every user is labelled `off` rather than `unavailable`.
- **Dashboards** change in the separate `home-monitoring` repository, in its own commit that
  follows that repository's checks.

## Metrics

OTel name → Prometheus name. Label keys are prefixed `home_agent.` (`home_agent_` in
Prometheus).

| Name | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `home_agent.reply.duration` | histogram (s), turn buckets | principal, kind (`chat`, `scheduled`), outcome (`delivered`, `failed`, `recovered`, `undeliverable`), safety_checks | Queued to delivered |
| `home_agent.reply.stage.duration` | histogram (s), turn buckets | principal, stage (`queue`, `setup`, `model`, `delivery`) | Time in one stage of one chat reply |
| `home_agent.model.call.duration` | histogram (s), turn buckets | principal, provider, model, stop_reason | One assistant message |
| `home_agent.model.tokens` | counter | principal, provider, model, type (`input`, `output`, `cache_read`, `cache_write`) | Tokens as Pi reports them |
| `home_agent.model.calls_per_turn` | histogram, buckets 1–10 | principal, safety_checks | Assistant messages in one turn |
| `home_agent.model.turn_tokens` | histogram, token buckets | principal, safety_checks, direction (`prompt` = input + cache read + cache write, `output`) | Tokens in one turn |
| `home_agent.tool.calls` | counter | principal, tool, outcome | Broker calls: success, tool_error, timeout, failed, oversized, budget_exhausted, not_granted, grant_check_failed, unknown_tool |
| `home_agent.tool.duration` | histogram (s), request buckets | tool, outcome | Broker call time, including result screening for screened tools |
| `home_agent.tool.calls_per_turn` | histogram, buckets 0–10 | principal, safety_checks | Tool calls in one chat turn, `web_search` included |
| `home_agent.daycard.render.duration` | histogram (s), request buckets | outcome | Rendering one day-card image |
| `home_agent.approvals` | counter | principal, tool, status (`proposed`, `executed`, `rejected`, `expired`, `replaced`, `failed`), warned | Approval lifecycle |
| `home_agent.judgment.decisions` | counter | principal, purpose, result | `intent`: warned, clean, or suspicious (warned without a request); `screen`: flagged or clean; `hint`: hinted or none |
| `home_agent.hint.follow_through` | counter | principal, result (`followed`, `other_tool`, `no_tool`) | Whether the model called the hinted tool in that turn |
| `home_agent.schedule.runs` | counter | principal, outcome (`delivered`, `failed`, `recovered`, `undeliverable`, `skipped`) | Scheduled runs as the scheduler delivers them |
| `home_agent.conversation.turns` (existing) | counter | + principal, safety_checks | Existing counter, gaining two labels |

`home_agent.judgment.requests` and `home_agent.judgment.duration` from plan 0029 stay. They
also cover the intent check that runs after the reply, before the approval card is sent.

## Approach

1. **C1 Telemetry** (`internal/observability/telemetry.go`): the instruments and recording
   methods above, all covered by `Noop()`. Add the two labels to `StartConversationTurn`.
   - Add a test seam, `NewWithReader(sdkmetric.Reader)`, so tests read real recorded values
     through `sdkmetric.NewManualReader()`. There is no new module.
   - Define each consuming package's small telemetry interface only if a shared type is
     clearer; otherwise each package declares its own.
2. **C2 Conversation and store** (`internal/conversation`, `internal/store/sqlite/conversation.go`):
   - Load `created_at`, and detect recovered requests against the service start time.
   - Record reply duration and the four stages; delivery covers `SendCard` and the reply chunks.
   - Read the safety-checks label once per turn through an optional opt-in status lookup.
   - Add a counting event sink in place of `discardSink`. It records tool calls per turn, and
     hint follow-through against the hint the turn received.
3. **C3 Pi runtime** (`internal/runtime/pi/runtime.go`):
   - Extend `rpcMessage` with `usage`, `provider`, `model`, and the stop reason, reading
     assistant messages only.
   - Time assistant `message_start` → `message_end`.
   - Record model call duration and tokens, and per-turn model calls and tokens.
   - Takes telemetry through a setter.
4. **C4 Broker, day card, approvals, judgments, scheduler**:
   - `internal/tools/broker.go`: per-tool calls and duration, with unbound names labelled
     `unknown`.
   - `internal/connectors/web/card.go`: render timing.
   - `internal/approval/approval.go`: proposed and each resolution, with `warned`.
   - `internal/judgment`: decisions.
   - `internal/schedule/service.go`: `schedule.runs`, and `reply.duration` with kind
     `scheduled`.
   - Each takes telemetry through a setter.
5. **C5 Dashboards** (`home-monitoring`, `grafana/dashboards/home-agent/home-agent.json`).
   Copy the existing panel style and datasource, and add these rows:
   - **Reply latency:** p50 and p95 by kind, excluding recovered and undeliverable. The mean
     stage breakdown is stacked:
     `sum by (home_agent_stage)(increase(home_agent_reply_stage_duration_sum[$__range])) / scalar(sum(increase(home_agent_reply_duration_count{home_agent_kind="chat"}[$__range])))`.
   - **Model:** call p95 by model; tokens by type; cache share,
     `cache_read / (input + cache_read + cache_write)`; model calls and prompt tokens per turn.
   - **Tools:** calls by tool and outcome; p95 by tool (screening included); calls per turn;
     budget exhaustion.
   - **Day card:** renders and p95.
   - **Approvals:** by status; confirm and reject rates by `warned`.
   - **Safety checks:** judgment p95 and errors; warned and flagged rates; hint
     follow-through.
   - **Usage:** turns per user per day; failed turns by reason; scheduled runs by outcome.
   - Use `increase` over `$__range` or 1d for stats. At household volume, per-second rates are
     mostly gaps.
   - Add a `principal` variable: `label_values(home_agent_conversation_turns, home_agent_principal)`,
     with All.
6. **C6 Wiring and docs** (orchestrator): call the setters in `internal/app/app.go`, and pass
   the opt-in lookup to the conversation service. Document the metric list in
   `docs/08-observability-and-operations.md`, and the baseline procedure in
   `docs/13-operator-runbook.md`.

Waves:
1. C1 alone.
2. C2, C3, C4 and C5 in parallel. They edit disjoint packages, and C5 needs only the names
   above.
3. C6.

## Baseline and comparison procedure

1. Deploy this plan with `HOME_AGENT_TYPESAFE_ENABLED=true` and nobody opted in. Nothing is
   sent to TypeSafe, and every user is labelled `off`.
2. Collect at least seven days. Record the baseline in this plan's results:
   - reply p50 and p95, and the stage split;
   - model calls, tool calls, and prompt tokens per turn;
   - tool error and budget-exhaustion rates;
   - approval reject and expire rates;
   - cache share.
3. Users opt in. The comparison itself is a separate plan, so this plan can be completed once
   the baseline is recorded.

## Files affected

- `internal/observability/telemetry.go`
- `internal/conversation/service.go`, `internal/conversation/types.go`,
  `internal/store/sqlite/conversation.go`
- `internal/runtime/pi/runtime.go`
- `internal/tools/broker.go`
- `internal/connectors/web/card.go`, `internal/connectors/web/service.go`
- `internal/approval/approval.go`
- `internal/judgment/intent.go`, `screen.go`, `hint.go`
- `internal/schedule/service.go`
- `internal/app/app.go`
- `docs/08-observability-and-operations.md`, `docs/13-operator-runbook.md`
- `~/src/home-monitoring/grafana/dashboards/home-agent/home-agent.json`

## Validation

- **Tests assert recorded values through `NewWithReader` and a manual reader**, at the
  existing integration boundaries: the telegram harness with a fake runtime, the Pi fixture
  emitting assistant messages with `usage`, and the broker, approval, and schedule flows.
  - A chat turn records reply, stage, and turn metrics with the right principal and
    safety-check labels, and the stage sums stay within the reply duration.
  - A request created before start is labelled `recovered`.
  - Pi usage becomes token counters and per-turn histograms, and a message without usage
    records none.
  - Tool outcomes, including budget exhaustion, are counted per tool, and an unbound name is
    labelled `unknown`.
  - An approval records proposed and its resolution with `warned`.
  - Followed, other-tool, and no-tool hints are told apart.
  - No label value contains message or argument text.
- `gofmt`, the module check, `go vet`, `go test -race ./...`, `go build`,
  `scripts/check-plans`.
- In `home-monitoring`: `bash -n scripts/*`, `shellcheck scripts/*`, `docker compose config`,
  and valid dashboard JSON.
- After deployment:
  - query each new metric through Grafana's data source proxy;
  - check that every panel renders with real turns;
  - check that the mean stage breakdown matches the mean reply time.

## Risks and open questions

- Pi's `usage` shape may differ between providers or versions. The parser tolerates missing
  fields, and a Pi upgrade re-checks it.
- Principal labels multiply series by the number of users. That is fine for a household.
  Revisit if the allowlist grows well beyond a handful of people.
- Two users give small samples. Compare over one to two weeks per phase, read p95 with care,
  and prefer means and per-turn distributions.
- The time between the model finishing and delivery is kept in memory, so a restart in that
  window loses the delivery stage for that request. Its reply is still counted.
