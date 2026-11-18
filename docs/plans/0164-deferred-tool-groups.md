# 0164 — Deferred tool groups

Status: `in-progress`

## Context

Every chat turn declares every tool the principal has switched on, in full, and the broker joins
each provider's guidance into the system prompt (`internal/tools/broker.go`,
`sections.System`). The Jev tool hint adds one line naming the likeliest tool; it removes nothing,
and in production it named the tool the model then used in about a third of tool-using turns, so
it cannot preselect tools.

In the largest eval world the set is 39 tools: about 46 KB of definitions and 30 KB of guidance,
roughly 19k tokens. Production adds the MCP connectors, whose size is not measured yet. Most chat
turns call at least one tool, so deferring everything would add a model call to most turns. Usage
is skewed instead: the largest definitions belong to tools called a handful of times a month or
never (watch writes, intake, collection, project, chart, image), while schedule, routes, calendar,
mail and home tools are called daily.

Pi 1.1 can register a tool with `exposure: "deferred"`: it is not declared until activated with
`pi.setActiveTools()`. Each tool change is recorded in the session. The household's Codex models
support anchored `additional_tools`, so an additive load keeps the cached prefix; a non-additive
tool change makes Pi resend the full list. Pi fires `session_compact` after
compaction. Only chat runs resume a chat session: tasks, scheduled turns and agent-watch checks
each start their own session.

Backlog item 7c73d61d asked whether loading only the needed tools is already done and, if not,
what it would gain.

## Goal

- The six largest tool definitions are shorter, with identical schemas.
- Offered tool size per chat turn is visible in Grafana by source, so production's baseline and
  its MCP share are measured before groups are chosen.
- On chat turns, rarely used tool groups are declared only after the model loads them. A group's
  guidance leaves the system prompt and arrives with the load. Unattended turns behave as today.
- The eval suite passes as well with deferral as without it, by the thresholds below.

## Decisions

- Consult (advisor, 2026-11-18): own loader vs Pi's `tool_search` → own `load_tools`. A group name
  works for requests in any language. A loader result can carry the group's guidance. BM25 over
  English text gives neither.
- The grouping unit is a static `DeferGroup` string on each `tools.Definition`, so a provider can
  defer only its write tools (watch keeps `watch_list` direct) or all of them.
- Deferral applies to `RunChat` only. That run kind alone resumes chat sessions, so a session never
  alternates between a deferred and a flattened set. Other kinds receive the flattened set: all
  tools direct, group guidance back in the system prompt.
- The broker stays the authority: deferral changes only what is declared. Every call is still
  rechecked against the principal's grants.
- A static flag in code, not configuration. Rolling it back is a new tagged deploy, which is
  accepted. The eval harness can switch deferral off for an A/B, as it already overrides guidance.
- Initial groups: watch writes, intake, collection, project, chart, image, revisited against the
  C2 baseline. MCP connectors stay direct unless that baseline shows they dominate, which would
  call for a follow-up plan.
- Pi does not carry loads across processes. A new process starts from the registered direct
  tools and records the deferred ones as removed, which resends the full list. So at session
  start the bridge restores the deferred tools that the session's recorded tool changes show as
  active after its last compaction. It skips any that are no longer registered. Loads then hold
  for the conversation, and the tool set stays additive.
- On `session_compact`, the bridge deactivates the deferred tools. The summary may have lost their
  guidance, and compaction rewrites the cached prefix anyway.
- A session written before deferral restores its tools that are now deferred, because they were
  active in it, so it keeps its full set. The saving starts with new sessions and after
  compaction. This keeps old conversations working without a migration.
- The model calls only declared tools, so a call to an unloaded deferred tool needs no handling.
  The risk is that it answers from an older result in history instead of loading the group. The
  system prompt line and the eval cases address that.
- Two landings. Landing 1 (C1, C2) deploys first to collect a production baseline. Landing 2
  (C3a, C3b, C4) follows the A/B.

## Approach

1. **C0 Pi spike** (orchestrator). Drive `pi --mode rpc` 1.1 directly with a scratch agent and
   session directory and a test bridge extension, against the operator's subscription, without
   touching the GoLand server. Confirm:
   - a deferred tool activated from inside another tool's `execute` is declared and callable in
     the next model call of the same turn;
   - the request carries `additional_tools` and `cacheRead` stays high across the load;
   - activation survives resume in a new process that registers the tools again;
   - a stale activated name that is no longer registered is ignored without a non-additive change;
   - a direct tool registered after an activation, as from a newly enabled integration, is active
     on resume;
   - a session with recorded activations still resumes under a build that registers no deferred
     tools (rollback);
   - `session_compact` reaches the extension and `setActiveTools` works there, and what the
     deactivation then costs in cache reads;
   - an extension can intercept a call to a registered but inactive tool, through Pi's
     `tool_call` event or its unknown-tool path, to load the group and return the error above;
   - a session written before deferral, whose history calls a tool that is now deferred, resumes
     cleanly (the forward direction of the rollback check).
   If any of these fails, stop and revise the design before C3b.
2. **C1 Shorter definitions** (Jev: simple 0.73; overridden to complex, because choosing which
   repeated text can go changes model behaviour). First the orchestrator records a baseline: the
   eval cases that use these tools, three trials each. Then, in `internal/watch/tools.go`,
   `internal/schedule/tools.go` and `internal/intake/service.go`, cut `watch_propose`,
   `watch_update`, `schedule_update`, `schedule_propose`, `reminder_set` and `intake_record`.
   Remove property descriptions that repeat the connector guidance, and keep each fact in one
   place. Property names, types, enums, bounds and required lists stay identical. Acceptance:
   together the six shrink by at least 30% in bytes, and a schema comparison test shows the same
   structure with descriptions ignored.
3. **C2 Offered-size metric** (Jev: simple 0.94). Add a histogram,
   `home_agent.tool.offered_bytes`, recorded on every chat turn with `part` (`definitions`,
   `guidance`) and `source` (the integration name of a switched provider, else `builtin`, so the
   set is bounded). Follow the existing telemetry pattern and its tests.
4. **Landing 1** (orchestrator): rerun the C1 baseline cases; land C1 and C2 as separate commits,
   with this plan as `in-progress`; deploy with the operator; collect at least three days of
   production data.
5. **C3a Deferred groups contract** (Jev: complex 0.97; after C2).
   - `tools.Definition.DeferGroup`; `tools.Contribution.Groups`, each with a name, a one-line
     summary and guidance; `ToolDefinition.Group` and `ToolSet.Groups` in the runtime contract.
   - The broker rejects a `DeferGroup` its provider does not describe. `ToolSet.Flatten()` returns
     everything direct, with group guidance joined into `Guidance`.
   - A broker setting that turns deferral off, which only the harness calls, and a variant field
     that sets it.
   - A `home_agent.tool.group_loads` counter by group, and a `deferred` part for the size
     histogram.
6. **C3b Runtime and bridge** (Jev: complex 0.98; after C0 and C3a).
   - The Pi runtime flattens for every kind except `RunChat`. For chat, `tools.json` carries the
     groups, and `bridge.ts` registers deferred tools with `exposure: "deferred"` and one
     `load_tools` tool, whose `group` argument is an enum of this turn's groups.
     `load_tools` activates the group, returns its guidance, and answers again when called for a
     group that is already loaded.
   - At session start, the bridge restores loads from the session's tool changes after its last
     compaction, keeping only tools that are still registered. On `session_compact` it
     deactivates the deferred tools. Both follow Decisions.
   - The system prompt lists each group with its summary in one line. It tells the model to load
     a group before using its tools, to load again rather than rely on an older result, and not to
     load a group whose tools it already has. When the hint's tool is deferred, the hint line names
     the group to load.
   - `load_tools` is left out of the call budget, calls per turn, hint follow-through and activity
     labels.
   - The Pi fixture reproduces the behaviour C0 recorded, so the tests check Pi's real behaviour
     rather than a guess.
7. **Group list** (orchestrator, after landing 1's data and before C4). Fix the groups from the
   production data by source and the 30-day call counts, and record the list here. C3a and C3b
   do not depend on it and may proceed while the data collects.
8. **C4 Initial groups and evals** (Jev: simple 0.61, below the threshold; complex, because the
   summaries decide whether the model loads a group; after C3b and the group list).
   - Mark the groups and move their guidance into group guidance. For watch, only the write tools
     and the write guidance move. Write summaries that name what the user would ask for.
   - Make each read tool that stays direct mention its group's write tools, so the model never
     says it cannot do something it only needs to load.
   - Add eval cases:
     - one per group whose request needs the group's tool, across English and Russian;
     - one ordinary request that must not load any group;
     - one two-step case that loads a group in the first step and uses it again in the second.
9. **C5 A/B and landing 2** (orchestrator). Run the full suite on the branch twice, with the
   deferral-off variant and without it, three trials per case. Keep C3a, C3b and C4 only if:
   - the overall pass rate is within 3 points of the deferral-off run;
   - no case loses more than one of its three trials;
   - every new case passes at least two of three;
   - prompt tokens per model call (input plus cache reads) drop by at least 15%;
   - model calls per turn rise by at most 10%.
   Then update `docs/operations.md`, run the final review, land, deploy with the operator, and
   compare offered bytes, cache reads and turn latency in Grafana with the landing 1 baseline.

Waves: C0, C1 and C2 in parallel; landing 1; C3a, then C3b, while the data collects; the group
list; C4; then C5.

Commits: landing 1 carries C1, C2 and this plan as `in-progress`. If C5 keeps deferral, landing 2
carries C3a, C3b and C4, and the last commit marks this plan `done`. If it does not, those
commits are dropped, and one docs commit marks this plan `done`, recording in Decisions only that
deferral was not adopted. C1 and C2 shipped, so `abandoned` does not fit.

## Files affected

- `internal/watch/tools.go`, `internal/schedule/tools.go`, `internal/intake/service.go`,
  `internal/collection/tools.go`, `internal/project/tools.go`, `internal/chart/tool.go`,
  `internal/imagegen/`
- `internal/tools/broker.go`, `internal/runtime/runtime.go`, `internal/runtime/pi/bridge.go`,
  `internal/runtime/pi/bridge.ts`, `internal/runtime/pi/runtime.go`, `internal/prompt/`
- `internal/conversation/service.go`, `internal/observability/telemetry.go`
- `internal/testutil/pifixture/`, `internal/eval/variant.go`, `evals/cases/`, `evals/variants/`
- `docs/operations.md`, `docs/plans/README.md`, this plan

## Validation

- `gofmt`, the module check, `go vet`, `go test ./...` (and `-race`), `go build`,
  `scripts/check-plans`.
- Integration tests through the Pi fixture: a deferred tool is not declared before the load and
  is callable after it in the same turn; a load holds on the next turn of the session and is
  dropped after compaction; unattended turns declare everything; a call to a tool the principal
  is no longer granted is refused.
- C1: the baseline cases pass at least as often as before, and the schema comparison test holds.
- C5: the thresholds above, then the Grafana comparison after deploy.

## Risks and open questions

- **The model answers without loading a group it needed.** Summaries, the direct read tools'
  descriptions, the hint line and the eval cases cover this, and the A/B decides.
- **Activation is sticky** for a conversation, so savings shrink as a conversation goes on.
  Unloading would be a non-additive change and resend the full list, so groups stay loaded until
  compaction or `/new`.
- **The saving may be small** (about 6k tokens in the eval world, mostly cache reads). The C5
  thresholds decide whether C3a, C3b and C4 ship.
- **Pi behaviour differs from its docs.** C0 checks it first, the fixture records it, and a Pi
  upgrade must recheck it.
- **Compaction is untested end to end in evals,** because the eval worlds never reach the context
  window. C0 and the fixture cover it.
