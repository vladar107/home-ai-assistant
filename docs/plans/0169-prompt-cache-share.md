# 0169 — Prompt cache share by run kind, and two cache trials

Status: `in-progress`

## Context

Every model call resends the system prompt, the tool definitions and the session's history.
The provider serves the part of that prefix it still holds from its prompt cache. Cached input
is cheaper and faster, and on a subscription it weighs less against the usage windows. The
"Prompt cache share" panel on the Home Agent Overview dashboard shows the share of input served
from cache. Over the week before this plan it was about 83% for the task and Heads-up model and
about 66% for the chat model, which also runs scheduled jobs, tab names and checks.

The panel cannot say where the misses happen. `home_agent.model.tokens` carries the principal,
provider, model, effort and token type, but not the run kind, and not whether the call was the
first of its turn. Within a turn every call after the first extends a prefix the provider has
just seen. A miss on the first call means the cache expired while the user was away, or that
something early in the prefix changed.

A Pi developers' article on prompt caching lists what breaks the cache in an agent harness:
idling past retention, model and thinking-level switches, tool set changes, compaction, dynamic
system prompts, and edits to earlier messages. The bot already avoids most of these. Per-turn
facts go in the message (plans 0032 and 0098), the jobs section is ordered for stability, tool
loading is additive (plan 0164), and plan 0108 notes that a model switch misses once.

Two levers remain, and both are unreachable through Pi's own settings:

- **Retention.** Pi 1.1.0 sends the Codex backend `prompt_cache_key` and nothing about
  retention. `PI_CACHE_RETENTION=long` only feeds Pi's interactive cache warmer, which never
  runs in the bot's one-process-per-turn RPC mode. Whether the Codex backend honours
  `prompt_cache_retention: "24h"`, which the OpenAI API accepts for some models, is unknown.
- **The cache key of fresh sessions.** Pi sets `prompt_cache_key` to the session ID. Scheduled
  runs, tab names, rewrites and checks start a fresh session each time, so each one carries a
  new key. They share the system prompt and tools with earlier runs, but the key gives the
  provider no hint to route them where that prefix is cached.

Pi lets an extension replace the provider payload in a `before_provider_request` handler. The
application's own bridge extension is loaded on every run that has tools, and only on those.

## Goal

The dashboard shows the cache share by run kind, by whether the run had tools, and by first
versus later call, so the cause of a miss can be read from the panel. Only then, and only for
the misses the split points at, two trials measure whether a longer retention or a stable cache
key for fresh-session runs raises the share. A trial ships only when it measurably helps, and
is removed otherwise.

## Decisions

- **Measure before building the trials.** Chunks 1 and 2 ship and run for a week before
  chunks 3 and 4 start. A trial is built only when the split supports it. Retention is built
  only if the cache share of chat turns' first calls is at least ten points below that of their
  later calls. The stable key is built only if the share of background runs with tools is at
  least ten points below that of chat turns' later calls. If neither
  holds, the plan ends after chunk 2 and records that.
- **Three new attributes on the token counter.** `home_agent.kind` takes `chat`, `task` or
  `background` (the existing `RunKind`). `home_agent.call` takes `first` or `later` within one
  run, counting completed provider calls, so a retried first call stays `first`.
  `home_agent.tools` is `yes` when the run carries application tools. Only those runs load the
  bridge where the trials act; a run with web search alone is `no`. All three are bounded, so
  cardinality stays small. No new metric is added.
- **The trials live in the bridge extension.** A `before_provider_request` handler adds
  `prompt_cache_retention` when the retention setting names a value. It sets `prompt_cache_key`
  to the stable key when the runtime passes one. The runtime passes both to the bridge through
  its environment, as it passes the bridge's other settings.
- **Only the Codex provider's payload is touched.** The handler changes nothing for any other
  provider, and leaves the payload alone when it lacks the fields it expects.
- **The stable key carries no identity.** For background runs it is a truncated SHA-256 of the
  run kind, the model, and a fingerprint of the rendered system prompt and tool definitions.
  No principal goes into it. Each principal runs on their own subscription, so two
  principals' keys never share a cache. A key derived from the prompt itself only groups runs
  that really share a prefix. Chat and task runs keep Pi's session key, since their sessions
  resume.
- **Settings:** `HOME_AGENT_PI_CACHE_RETENTION` (empty or `24h`) and
  `HOME_AGENT_PI_STABLE_CACHE_KEY` (`false` or `true`), mirrored into `.env` and
  `deploy/.env.nas`.
- **Longer retention is also a privacy choice.** With `24h`, the provider keeps the cached
  prefix for up to a day instead of minutes. That prefix holds the system prompt, memory, jobs,
  and the session's history with its tool results. The operator turns it on knowingly, and the
  documentation says so.
- **Rollback is unsetting the setting.** If the backend starts rejecting the retention field,
  every run with tools fails. Clearing `HOME_AGENT_PI_CACHE_RETENTION` and redeploying restores
  today's payload. The operations guide names this step.
- **The probe runs on the eval profile only,** through the plan 0155 harness, never on a
  household principal. A live probe is an operator step, not a worker's.
- **Keep rule:** a trial stays on when it raises the cache share of the population it targets
  by at least ten points over a week. Retention targets first calls of chat turns; the stable
  key targets background runs with tools. It must also not raise failed model calls. A trial
  that misses this is removed with its setting, not left switched off.

## Approach

Wave 1: chunk 1, then chunk 2. Then deploy, and read a week of data (chunk 3). Wave 2, only
for supported trials: chunk 4, then chunk 5, then chunk 6.

1. **Kind, tools and call position on the token counter** — Simple (Jev: simple, 0.96).
   - Task: pass the run's kind and whether it has tools into the Pi runtime's turn state, and
     count the run's model calls. Then add `home_agent.kind`, `home_agent.tools` and
     `home_agent.call` to every `RecordModelTokens` point, copying how the existing attributes
     are set.
   - Acceptance: an integration test with the Pi fixture shows a two-call chat turn recording
     `first` then `later`, both with kind `chat` and tools `yes`. A scheduled run records kind
     `background`, and a toolless run records tools `no`.
2. **Dashboard split** — Simple (Jev: simple, 0.82); depends on 1.
   - Task: in the Home Agent Overview dashboard, which lives in the `home-nas` repository
     under `stacks/monitoring/grafana/dashboards/home-agent/`, split "Prompt cache share" into
     one series per kind, tools and call position, copying the existing panel's query.
   - Acceptance: the dashboard loads in Grafana with one series per combination that has
     data, and their weighted total matches the old panel.
3. **A week of data and the go/no-go** — Orchestrator; depends on 1, 2 and a deploy.
   - Task: read the split panel after a week and decide, by the rule under Decisions, which
     trials to build. Record the outcome in this plan.
   - Acceptance: this plan names the trials to build, or ends here with the reason.
4. **Cache options in the bridge** — Complex (Jev: complex, 0.55, below threshold; taken as
   complex because it rewrites the provider payload and derives the key); depends on 3. It
   edits the Pi runtime after chunk 1 and reuses chunk 1's kind and tools plumbing.
   - Task: the settings for the trials chunk 3 chose, the runtime passing the retention value
     and, for background runs, the stable key to the bridge, and the bridge's
     `before_provider_request` handler.
   - Acceptance: with the Pi fixture, the payload carries `prompt_cache_retention` only when
     set. `prompt_cache_key` equals the stable key only for a background run with the setting
     on. The key is the same for two runs with the same prompt, tools, kind and model, and
     differs when the jobs section changes. With both settings off, the payload is
     byte-identical to today's.
5. **Cache probe** — Complex (Jev: complex, 0.33, below threshold; taken as complex because it
   drives sessions and timing through the harness); depends on 4. It edits
   `cmd/home-agent-eval/main.go`, so it lands serialised with plan 0155's chunks that edit the
   same file.
   - Task: a `home-agent-eval cache-probe` command. It runs two turns of a fixed synthetic case
     in one session with a gap (15 minutes by default, past the provider's default
     retention), and two background-kind runs with fresh sessions. The case's prefix is kept
     above the provider's minimum cacheable length. Each measurement repeats three times and
     reports each run's cached share of input, with each setting on and off. The result is
     conclusive only when the off run misses at that gap. Otherwise the command says the gap
     was too short.
   - Acceptance: with the Pi fixture, the command reports the counts it was fed, marks an
     off run that hit as inconclusive, and reports a payload rejection as such.
6. **Live probe and decision** — Orchestrator; depends on 5.
   - Task: run the probe on the eval profile, and turn on in production the settings it
     favours. After a week, keep or remove each trial by the keep rule.
   - Acceptance: each trial is either on with its setting and the privacy note documented, or
     removed with its code and setting.

## Files affected

- `internal/runtime/pi/runtime.go`, `internal/runtime/pi/bridge.go`,
  `internal/runtime/pi/bridge.ts`, `internal/observability/telemetry.go`
- `internal/config/conversation.go`, `.env`, `deploy/.env.nas` (local, not committed)
- `internal/eval/` and `cmd/home-agent-eval/main.go` (the probe)
- The `home-nas` repository: the Home Agent Overview dashboard
- `docs/architecture.md` (settings, privacy note, rollback), `docs/plans/README.md`

## Validation

- Formatting, vet, build, module tidiness, the full test suite with and without the race
  detector, and `scripts/check-plans`.
- The split panel in Grafana after deploy, and the live probe on the eval profile.

## Risks and open questions

- **The Codex backend may reject or ignore `prompt_cache_retention`.** Rejection shows in the
  probe; an ignored field shows as no gain, and the trial is removed.
- **A stable key concentrates requests.** OpenAI advises keeping one key's rate modest.
  Background runs with one prefix are a few an hour, well inside that.
- **Plans 0164 and 0170 touch nearby code.** Plan 0164 edits the bridge, so chunk 4 rebases
  onto whatever 0164 has landed and keeps its deferred-tool handling intact. Plan 0170 also
  edits `docs/architecture.md`; whichever lands second rebases its section.
- **A Pi upgrade may change the payload shape.** The handler leaves an unexpected payload
  untouched, and the fixture test pins the expected fields, so an upgrade that changes them
  fails the test instead of silently dropping the trial.
