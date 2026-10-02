# 0034 — Make unfinished model calls and rare failures visible

Status: `done`

## Context

On 2026-10-02 two chat turns failed at 08:25:30 and 08:27:42 UTC after the 120-second turn
timeout (`defaultConversationTimeout`). The Home Agent dashboards showed almost nothing:

- **Model calls that never finish leave no record.** `RecordModelCall` runs only on an
  assistant `message_end` (`internal/runtime/pi/runtime.go`). When the turn deadline stops
  Pi, the in-flight call records no duration, no outcome, and no span. Logs held no tool
  calls and no model completions during either turn, so the model hung, but no signal says
  so.
- **The first error is invisible to `increase()`.** An OTel counter series first appears
  carrying its first increment. The `outcome="error", error_kind="timeout"` series started
  at `1`, so `increase()` saw only the step from 1 to 2: "Turn errors last 24h" showed
  1.0 for two errors, and a single error would show 0. This affects every rare-event series.
- **Traces cannot locate the stall.** A turn span has no children for model or tool calls.
  Tool calls already run under the turn's context: the Pi bridge's `BaseContext` derives
  from the turn context, so only the spans themselves are missing.

## Goal

When a turn fails or stalls, the dashboards and traces show which stage was responsible:

- every model call is counted with an outcome, including calls that time out, are
  cancelled, or end because Pi failed or exited;
- the first occurrence of a rare failure moves `increase()`-based panels;
- a turn's trace shows its model calls and tool calls as child spans, with Pi's own
  retries visible as span events.

## Decisions

- **New counter `home_agent.model.calls`** (Prometheus `home_agent_model_calls`), labelled
  `home_agent.principal` and `home_agent.outcome`:
  - `completed`: an assistant `message_end` with any stop reason other than `error` or
    `aborted`, including the success after Pi's own retries;
  - `failed`: an errored message that Pi will not retry (`agent_end` with `willRetry`
    false, or `auto_retry_end` with `success` false), a broken event stream, Pi exiting on
    its own, or a turn that ends in a runtime error before the model answers;
  - `timeout` / `cancelled`: the call was open when the turn context ended.
  Provider and model stay off the counter, so its series can exist from startup.
- **Outcome comes from the context first.** Every unfinished close, including Pi's own
  `agent_end` and `auto_retry_end` failures, reads the context. The outcome is decided from `ctx.Err()` before anything else: `DeadlineExceeded`
  means `timeout`, `Canceled` means `cancelled`, and only then `failed`. When the deadline
  passes, Pi is sent SIGINT, and its exit or closed stream can win the `select` against
  `ctx.Done()`.
- **The duration histogram keeps its meaning.** `model_call_duration` stays timed from
  assistant `message_start` to `message_end`, and records only finished calls, with its
  current labels. Unfinished calls go only to the counter and the span, so they do not add
  120-second `model="unknown"` points to "Model call p95 by model".
- **A model-call span covers the whole wait.** The Pi 0.84.1 source shows `turn_start`
  before every assistant request, after the prompt and after each tool batch. A retry
  follows `agent_end` with `willRetry` true, `auto_retry_start`, the backoff, and a new
  `agent_start` and `turn_start`. `home_agent.model.call` therefore opens when the prompt
  is sent and on `turn_start`, and as fallbacks on `auto_retry_start` and on an assistant
  `message_end` that no `turn_start` preceded (Pi's synthetic run-failure message). It
  stays open across Pi's retries and their backoff, so a recovered retry is one call.
  Each `auto_retry_start` adds a `pi.auto_retry` span event. Pi reports no retry category,
  so `home_agent.category` is derived from the error text. Spans for unfinished calls end
  with error status and the outcome. Counting `tool_execution_end` was rejected: a tool
  whose preparation fails ends before later tools of the same batch start.
- **Rare-outcome series start at zero**, created once at startup for each configured
  principal. Only counters are zeroed, never histograms. The label tuples must match the
  real increments exactly:
  - `home_agent_conversation_turns`: `outcome="error"`, `principal`, `safety_checks`
    ∈ {`on`, `off`, `unavailable`}, and `error_kind` ∈ {`timeout`, `runtime`,
    `tool_provider`, `attachments`, `not_connected`, `provider_quota`, `provider_auth`}.
    That is 21 series per principal. The conversation package owns the list of error kinds,
    so adding a kind is a one-place change. Cancelled turns are not zeroed, because they are
    user or shutdown actions.
  - `home_agent_model_calls`: `principal` and `outcome` ∈ {`timeout`, `cancelled`,
    `failed`}.
  This relies on cumulative temporality, the SDK default. An error in the first export
  interval after a restart is still only partly visible. Tool-call outcomes are not zeroed,
  because tools × outcomes × principals would add hundreds of idle series.
- **Pi auto-retry logging** is one `pi.model_call.retried` `WARN` per model call, with the
  category, retry count, and outcome, written when the call closes under the model-call
  span. A provider outage then does not flood the logs.
- **Tool spans** start in `turnExecutor.Execute`, as `home_agent.tool.call` with the tool
  and outcome. `tools.Telemetry` gains the span method.
- Dashboards live in `home-monitoring`, and the panel change is committed there separately.

## Approach

1. **Model-call outcomes, spans, and zeroed series**:
   - Read Pi 0.84.1's RPC event sequence for a turn with tools and a retry.
   - Track the open model call in `turnState`. Assistant messages and Pi's `agent_end` and
     `auto_retry_end` close it; a deferred close in `Run` covers calls still open at exit.
   - Add the counter, the span, the retry events, and the per-call retry `WARN`.
   - Extend the Pi runtime `Telemetry` interface and `observability.Telemetry`.
   - Zero the listed series from the configured principals during app startup.
2. **Tool spans**: start a `home_agent.tool.call` span in `turnExecutor.Execute` under the
   context the bridge already passes.
3. **Dashboards and docs**:
   - In `home-monitoring`, add "Model calls by outcome",
     `sum by (home_agent_outcome) (increase(home_agent_model_calls{service_name="home-agent", home_agent_principal=~"$principal"}[1h]))`,
     beside "Failed turns by error kind".
   - Note in `docs/operations.md` that failures are counted from zero at startup and how to
     read model-call outcomes.

## Files affected

- `internal/runtime/pi/runtime.go` (also a `Config.Logger` for the retry warning)
- `internal/testutil/pifixture/fixture.go` (tool and retry scenarios)
- `internal/observability/telemetry.go`
- `internal/tools/broker.go`
- `internal/conversation` (error-kind list)
- `internal/app/app.go` (zero-series initialization)
- tests beside the changed packages
- `docs/operations.md`
- `~/src/home-monitoring/grafana/dashboards/home-agent/home-agent.json`

## Validation

- A fake Pi that never answers, with a short turn timeout, records exactly one `timeout`
  model call. It also produces a `home_agent.model.call` span with error status, and a
  `timeout` turn.
- A fake Pi that exits on SIGINT before `ctx.Done()` is observed still records `timeout`,
  not `failed`.
- A normal turn records `completed`. A `message_end` with `stopReason` `error` that Pi does
  not retry records `failed`.
- A retry backoff falls inside one model-call span, which carries a `pi.auto_retry` event;
  a recovered retry counts one `completed` call and exhausted retries one `failed` call.
- Tool spans have the turn span as parent.
- After startup, the SDK reader holds zero-valued series for every listed label tuple.
- Run `gofmt`, `go vet`, `go test ./...`, the build, and `scripts/check-plans`.
- After deployment, check through Grafana MCP, before any failure, that
  `home_agent_model_calls` and the zeroed turn-error series exist in Prometheus with value
  0. Then a deliberately short turn timeout in local dev should produce a `timeout` model
  call and a trace with child spans.

## Risks and open questions

- A turn that fails before the model answers, for example a rejected prompt or an
  extension error, counts one `failed` model call although no request was made.
- Pi's local binary may differ from the pinned version (0.87.1 locally, 0.84.1 pinned);
  the event handling follows 0.84.1.
- All turns of a process share one long-lived trace rooted at
  `home_agent.application.conversation_start`, so Tempo shows the root as "not yet
  received". Out of scope here.
