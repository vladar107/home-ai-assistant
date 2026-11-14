# 0155 — Regular agent evaluations and prompt experiments

Status: `proposed`

## Context

The assistant's behaviour depends on text that changes often: the fixed sections of the system
prompt (`internal/prompt/system.go`), the guidance and descriptions each connector gives its
tools (`internal/tools/broker.go` joins them into `ToolSet.Guidance`), the `[system]` lines a
turn's message starts with (`internal/prompt/turn.go`), and the model and reasoning effort a user
chose. Golden files under `internal/prompt/testdata` pin the exact bytes of that text, and the
Telegram integration tests drive whole turns, but every one of those tests runs on a scripted
runtime (`controlledRuntime`, or the Pi stand-in in `internal/testutil/pifixture`). Nothing
measures what a real model does with the text. A wording change is judged by reading a few live
replies, and a regression shows up later as a 👎 or a 👎-driven reconsider turn.

How a turn runs today:

- `turn.Engine.Compose` builds the message: the current local time line, the user's name, place,
  trip, collections, a notice, an optional tool hint from Jev, then the user's text.
- `turn.Engine.Run` passes a `runtime.RunRequest` to the Pi runtime (`internal/runtime/pi`),
  which writes the system prompt (`prompt.System` plus memory and jobs via
  `prompt.WithSections`) to a per-turn file and starts one Pi process in RPC mode with
  `--provider openai-codex`, `--model` and `--thinking`. The prompt carries nothing that changes
  per turn, so the provider's prefix cache holds across turns.
- The broker's tools reach Pi through a per-turn bridge extension (`bridge.ts`) that forwards
  every call to the application's executor over loopback; Pi's built-in tools are off.
- Each principal has their own Pi profile directory (`runtime.ProfileDirectoryName`), logged in
  to their own ChatGPT subscription. No profile is shared and none falls back to another.
- The system prompt has no version: its identity is its bytes at a commit.

The backlog asks for regular checks of the agent's quality with a trend over time, and for a way
to try several prompt variants on the same criteria so changes are chosen by results.

### What current practice says

- Start with 20 to 50 tasks drawn from real failures, with positive and negative cases; grade
  the outcome and the final state rather than an exact sequence of steps; read transcripts; keep
  a regression suite apart from capability experiments
  ([Anthropic, Demystifying evals for AI agents](https://www.anthropic.com/engineering/demystifying-evals-for-ai-agents)).
- tau-bench grades an agent by comparing the world's final state with the goal state and reports
  `pass^k`, the chance that all of k trials succeed, because a household assistant must be
  reliable, not lucky once ([tau-bench](https://arxiv.org/abs/2406.12045)).
- Prefer code-based checks; use a model judge only where code cannot decide. Judges show position,
  verbosity and self-preference bias ([Zheng et al.](https://arxiv.org/abs/2306.05685)); pairwise
  or pass/fail judgments are more reliable than open scores, the judge should reason before it
  answers, and it must be calibrated against human labels before it is trusted
  ([OpenAI eval best practices](https://developers.openai.com/api/docs/guides/evaluation-best-practices)).
  A judge should have an "unknown" answer, one question per rubric dimension, and be a different
  model from the one under test
  ([Anthropic docs](https://platform.claude.com/docs/en/test-and-evaluate/develop-tests)).
- Run several trials per case and average per case first; compare two variants by the per-case
  differences, with a standard error and a 95% interval, not by two pooled rates
  ([Miller, Adding error bars to evals](https://arxiv.org/html/2411.00640)). A suite of a few dozen
  cases can only show large effects; small differences need hundreds of cases.

### Tools surveyed

- promptfoo: MIT, Node.js, runs locally; OpenAI announced its acquisition on 2026-03-09 and it
  says it stays open source ([announcement](https://openai.com/index/openai-to-acquire-promptfoo/)).
- Inspect AI: MIT, Python, local ([repository](https://github.com/UKGovernmentBEIS/inspect_ai)).
- OpenAI Evals: the open repository now points users to the hosted dashboard
  ([repository](https://github.com/openai/evals)).
- Braintrust: closed source and hosted; self-hosting is an enterprise option.
- Langfuse: MIT and self-hostable, acquired by ClickHouse in 2026
  ([post](https://langfuse.com/blog/joining-clickhouse)); it is a trace and experiment store that
  needs its own database stack.
- DeepEval: Apache-2.0, Python, with an optional hosted account
  ([repository](https://github.com/confident-ai/deepeval)).
- A few small Go eval libraries exist; none is established enough to depend on.

Each of them evaluates a prompt sent to a provider API. What needs evaluating here is the whole
turn: the composed message, the Pi process with its subscription login, the broker, the approval
path, and the tool results. Driving that from Node or Python means a shim around the Go code, a
new language toolchain, and in the hosted cases household-shaped transcripts on a third party.
What they would add over a small harness is a report viewer and judge templates; the statistics
are a few dozen lines.

## Goal

The operator runs one command that plays a suite of synthetic cases through the real turn path
on a real model and gets a report: which cases passed, how reliably, and how this run compares
with the last one or with another prompt variant, with an honest interval. Later the same suite
runs weekly on its own, and its pass rates show as a trend in Grafana. Choosing between prompt
wordings becomes reading a comparison, not a few live replies.

Success for the first stage: about fifteen cases, deterministic checks only, three trials each,
a Markdown report, run by hand from the operator's Mac.

## Decisions

- **Build a small Go harness; adopt no eval framework.** It reuses the turn engine, the Pi
  runtime, the broker and the SQLite store as they run in production, adds no dependency, and
  sends nothing anywhere but the model provider already in use. Cases and results use JSON, which
  the standard library reads. Revisit if the judge or report work grows beyond this plan.
- **An eval case** is a JSON file with:
  - `id`, `title`, `tags` (such as `regression`, `capability`, `time`, `proposal`, `injection`);
  - `clock`: the fixed local time and the IANA zone the turn sees;
  - `user`: a synthetic display name, the switches that are on, memory text, and a model and
    effort when the case pins one;
  - `world`: the state the tools see. Internal tools (schedule, memory, watches, collections,
    trips, projects) run for real on a temporary SQLite store seeded from the case. Connectors
    whose tool definitions are fixed reach the existing loopback simulators (`googlesim`,
    `calendarsim`, `notionsim`), seeded from the case, so a mailbox, a calendar or a page is
    case data, not a scripted tool answer. Connectors whose tools depend on outside state, the
    home connector and catalogue MCP servers, are left out until a later plan;
  - `turns`: one or more user messages in one session, optionally with a Confirm or Reject of the
    proposals the previous turn made;
  - `expect`: deterministic checks: tools that must be called, with argument predicates (equal,
    contains, matches, within a time range); tools that must not be called; a maximum number of
    calls; proposals that must exist and what they write, or that nothing was written; the final
    state of the internal store (a job exists with this schedule, no recurring job was created);
    and reply checks (contains, does not contain, matches, language);
  - `rubric`: optional yes/no questions for the judge, each about one dimension;
  - `trials`: how many times to run it, default three.
- **Cases are synthetic and public.** They live in `evals/cases/` in this repository, use
  neutral names, places and data, and never copy a real message, trace, memory or device. A case
  inspired by a real failure is rewritten from scratch by the operator. A Go test loads every case
  and checks its shape, its unique ID, and that every tool it names is a static tool: an internal
  tool or one of a connector with fixed definitions. Home and catalogue MCP tools are built from
  outside state, so the test cannot know them and a case may not name them. It runs in CI with no
  model.
- **A local privacy scan backs the operator's review.** Before a run, and as a command of its own
  the operator runs before committing a case, the harness reads the household values from the
  ignored `.env` (such as places, names and addresses its settings hold) and refuses any case or
  variant that contains one. The scan runs only on the operator's machines; CI has no `.env`, and
  the values are never printed, only the file and field that matched.
- **Checks in order: code first, judge second.** A trial passes when every deterministic check
  passes and, when a rubric exists and the judge is on, every judged question is "yes". Grading
  prefers the final state over the exact path: an ordered sequence of calls is checked only when
  the case says the order matters.
- **The runner plays real turns.** A trial builds the wiring the production binary builds, with
  the store in a temporary directory and the static connectors pointed at the seeded simulators.
  The broker's own executor runs every call; there is no scripted layer in front of it. The
  message is composed by `turn.Engine.Compose` with the case's clock, so the time line, the user
  line and the tool guidance are what production would send. The Jev tool hint is off, so no
  text goes to TypeSafe. Every tool definition the model sees is the real one.
- **One clock for the whole world.** The case's clock must reach every seam that reads the time,
  or an approval, a reminder or a task would be dated by the wall clock while the turn is told
  another day. The seams are: `wiring.CoreConfig.Now`, which feeds the schedule service and its
  reminder job, collections, intake, search checks, trips, projects, watches, events and routes;
  `turn.Config.Now`; the memory service's `Now`; the connectors' own `Now` fields; and the direct
  `time.Now` calls in `store/sqlite/approvals.go`, `store/sqlite/schedules.go` and
  `task/service.go`, which gain a clock. `tools/broker.go` reads `time.Now` only to time a call,
  which the case clock need not reach.
- **The runner uses the Pi release the image pins.** It installs Pi from
  `third_party/pi-runtime` as the Dockerfile does and checks at startup, through the runtime's
  existing version check, that Pi reports the version in `HOME_AGENT_PI_VERSION`'s default. A
  different Pi refuses to run, so a result always names the Pi production runs.
- **The model is live; replay is for checks, not prompts.** Every trial records a cassette: the
  composed message, the system prompt's fingerprint, Pi's event stream, each tool call with its
  arguments and the answer given, the reply, the tokens and the model ID. Replay feeds a cassette
  back through the checks, so a new or fixed check is tried on past runs at no cost. A replay
  cannot judge a prompt change, since the model would answer differently; the report says which
  trials were replayed.
- **Who pays: the operator's own subscription, through a dedicated eval profile.** The harness
  runs as a synthetic principal, `eval`, whose Pi profile directory is given on the command line.
  It never reads the application's profiles directory, never runs as the operator's application
  principal, and never runs on another user's login, wherever it runs.
- **The eval profile is logged in by the harness itself.** `home-agent-eval login --profile-dir
  <dir>` runs the device-code flow of `internal/connection`, printing the verification address
  and code in the terminal instead of sending them to Telegram, and writes the credential into
  that directory with the same private permissions. Copying `auth.json` from a production profile
  is forbidden and the documentation says why: the provider rotates the refresh token on use, so
  two copies refreshing apart would leave one of them, possibly the operator's own chat, logged
  out.
- **A run checks the quota before it starts and stops at its budget.** The pre-flight reads the
  eval profile's usage through the reader `/status` uses (`chatgpt.Client.Usage`) and refuses to
  start when any window is above a set percentage used (default 50%), so an eval never eats the
  quota the operator's own chat needs. During the run a budget (model calls and tokens) bounds it,
  and the first quota or authentication failure stops it, reporting the trials it did not run.
- **Prompt variants are named overrides of the fixed sections.** `prompt.System`'s constants
  become fields of a `prompt.Sections` value with a `Default`; production uses `Default` and its
  rendered bytes do not change, which the existing golden files prove. A variant file in
  `evals/variants/` replaces named sections and may also set a model and effort; a later stage may
  replace one connector's guidance by tool name. `RunRequest` gains an optional sections field
  that only the harness sets.
- **Variants do not break prompt caching.** The system prompt stays free of per-turn facts in
  every variant, and a run executes all trials of one variant before the next, so each variant
  warms its own cached prefix and production's prefix is never touched. A winning variant ships
  as an ordinary change to the defaults with its golden files, never as a runtime switch, and is
  never tried live on household users: their turns are too few to compare and are private.
- **A prompt's identity is its fingerprint.** The harness records the SHA-256 of the rendered
  system prompt without memory and jobs, the commit, the Pi version, the model and the effort with
  every run, so a trend line can be split where the prompt changed.
- **Results stay local first.** A run writes a JSON result and a Markdown report to an ignored
  directory, and its rows to a local SQLite file (`runs`, `trials`, `checks`) on the machine that
  ran it. The trend and comparison commands read that file.
- **The comparison is paired.** For two runs over the same cases the report shows, per case, the
  pass rate of each and the difference; overall, each pass rate, `pass^k`, the mean per-case
  difference with its standard error and 95% interval, and the cases that got better or worse. When
  the interval includes zero the report says no difference was detected, and how large a difference
  the suite could have detected. Tokens and model calls per trial are compared too.
- **The judge is a no-tools Pi run on the same eval profile,** on a model different from the one
  under test when the subscription offers one. It answers each rubric question with reasoning,
  then `yes`, `no` or `unknown`. A pairwise comparison of two variants' replies asks twice, with
  the order swapped, and counts only agreeing verdicts. The judge is trusted for a question only
  after the operator labels a sample of replies and the agreement report clears a threshold.
- **The weekly run comes last,** after the suite is stable. It runs the regression suite against
  the default prompt on the current release, on the dedicated eval profile only, and exports each
  run's pass rate, `pass^k` and cost as metrics through the existing OTLP exporter, with a panel
  in the monitoring repository's dashboard.

## Approach

Three stages, each useful on its own. Each chunk names its complexity, dependencies and
acceptance criteria. Chunks 3, 4, 6, 7, 8 and 9 all edit `cmd/home-agent-eval/main.go`, so they
run one at a time in that order, each on top of the one before.

### Stage 1: a regression suite run by hand

1. **Case format and the first cases** — Complex; no dependency.
   - Task: the case types and loader, the CI shape test, and about fifteen synthetic cases over
     internal tools and static connectors, covering: answering a date question from the time
     line; creating a one-off reminder at the right local time; not creating a recurring job
     unless asked; proposing a write instead of claiming it was done; calling a tool before saying
     something is unavailable; ignoring an instruction inside a seeded mail; replying in the
     language of the message; taking the likely reading of an ambiguous request and stating it;
     asking first when a guess would cost something.
   - Files: `internal/eval/case.go`, `internal/eval/case_test.go`, `evals/cases/*.json`.
   - Acceptance: every case loads; a malformed case, a duplicate ID, an unknown tool name and a
     home or catalogue MCP tool name each fail the test with the file named; the test runs in CI
     with no model and no network.
2. **The eval world and its clock** — Complex; depends on 1.
   - Task: build the production wiring for the `eval` principal on a temporary store, seed the
     internal tools and the `googlesim`, `calendarsim` and `notionsim` simulators from the case,
     and leave the home connector and MCP servers unconfigured. Give the case's clock and zone to
     every seam listed under "One clock for the whole world", threading a clock into the store's
     approval and schedule writes and the task service where they call `time.Now` today.
   - Files: `internal/eval/world.go`, `internal/eval/world_test.go`, `internal/wiring/wiring.go`,
     `internal/store/sqlite/approvals.go`, `internal/store/sqlite/schedules.go`,
     `internal/task/service.go`.
   - Acceptance, with the Pi fixture: the composed message starts with the case's time line; a
     seeded mail, event or page is what the connector's tool returns; an internal tool writes to
     the temporary store; a proposal's creation and expiry, a reminder's next run, a schedule
     run's record and a task's creation time are all on the case's clock, as are the trip,
     project, watch and collection lines a turn reads; nothing outside loopback is contacted; the
     production wiring passes `time.Now` and its tests are unchanged.
3. **Operator commands: login, quota pre-flight and privacy scan** — Complex; no dependency.
   - Task: `home-agent-eval login --profile-dir <dir>` running the `internal/connection`
     device-code flow with a terminal deliverer; `home-agent-eval usage --profile-dir <dir>`
     reading the profile's usage through `chatgpt.Client.Usage`; `home-agent-eval scan` reading
     the household values from the ignored `.env` and refusing cases or variants that contain one.
   - Files: `cmd/home-agent-eval/main.go`, `internal/eval/login.go`, `internal/eval/preflight.go`,
     `internal/eval/scan.go`, and in `internal/connection` only what lets a caller outside the
     Telegram flow start and finish a ceremony for one profile directory.
   - Acceptance: against the `codexauth` simulator, login writes a credential readable by Pi in
     the given directory with private permissions, and refuses a directory inside the
     application's profiles directory; against a usage stub, a window above the threshold
     refuses with the window named; the scan finds a planted value in a case and prints the file
     and field, never the value; without `.env` the scan says it found nothing to check.
4. **Runner, cassettes and checks** — Complex; depends on 2 and 3.
   - Task: check the Pi version against the pinned one, run the quota pre-flight and the scan,
     then run each case's trials in a fresh session on the eval profile, record the cassette,
     apply the deterministic checks, enforce the budget, stop on quota or authentication failure,
     and write the JSON result and a Markdown report (pass rate, `pass^k`, failing checks per
     case, tokens, Pi version, model, prompt fingerprint).
   - Files: `internal/eval/runner.go`, `internal/eval/checks.go`, `internal/eval/report.go`,
     `cmd/home-agent-eval/main.go`, `.gitignore`.
   - Acceptance: an integration test with the Pi fixture covers a passing and a failing trial, a
     budget stop, a quota stop, a Pi of another version refused at startup, and a run refused by
     the pre-flight; the command refuses to start without an explicit profile directory and never
     opens the application's profiles directory.
5. **Operator documentation** — Simple; depends on 4.
   - Task: how to install the pinned Pi, log in the eval profile (and why `auth.json` is never
     copied), run a suite, read the report, scan and write a new case.
   - Files: `docs/architecture.md` (a section on evals).
   - Acceptance: the steps run as written on the operator's Mac.

### Stage 2: variants, history and comparison

6. **Prompt sections and variants** — Complex; depends on 4.
   - Task: `prompt.Sections` with `Default`, the optional sections field on `RunRequest` read by
     the Pi runtime, variant files, the prompt fingerprint, and a `--variant` flag on the run
     command.
   - Files: `internal/prompt/system.go`, `internal/runtime/runtime.go`,
     `internal/runtime/pi/runtime.go`, `internal/eval/variant.go`, `evals/variants/`,
     `cmd/home-agent-eval/main.go`.
   - Acceptance: every existing golden file is unchanged; the Pi fixture receives the variant's
     prompt only when the harness sets one; the fingerprint changes with a section and not with
     memory or jobs; trials of one variant all run before the next starts.
7. **Local results store, trend and compare** — Complex; depends on 6.
   - Task: the SQLite file with runs, trials and checks; `trend` (pass rate and `pass^k` per run,
     split by fingerprint, model and Pi version) and `compare` (the paired report above).
   - Files: `internal/eval/store.go`, `internal/eval/stats.go`, `internal/eval/compare.go`,
     `cmd/home-agent-eval/main.go`.
   - Acceptance: the statistics are tested on hand-computed examples; comparing a run with itself
     reports no difference; runs over different case sets compare only their shared cases and
     say so.
8. **Replay** — Simple; depends on 7.
   - Task: re-score stored cassettes with the current checks, as a `replay` command.
   - Files: `internal/eval/replay.go`, `cmd/home-agent-eval/main.go`.
   - Acceptance: changing a check and replaying a run changes its verdicts without starting Pi,
     and the report marks the run as replayed.

### Stage 3: judge and weekly trend

9. **Rubric and pairwise judge** — Complex; depends on 8.
   - Task: the judge run, its prompt, `unknown`, order-swapped pairwise verdicts, and a labelling
     command that records the operator's own verdicts and reports agreement per question.
   - Files: `internal/eval/judge.go`, `cmd/home-agent-eval/main.go`.
   - Acceptance: with the Pi fixture, a swapped pair that disagrees counts as no verdict; a
     question below the agreement threshold is reported but does not decide a trial.
10. **Weekly run and Grafana** — Complex; depends on 7 and on the cadence question below.
    - Task: run the regression suite weekly on the dedicated eval profile, export pass rate,
      `pass^k`, tokens and the fingerprint's short form as metrics, and add a panel to the
      monitoring repository's dashboard.
    - Files: depends on the answer; `internal/observability/telemetry.go` for the metrics either
      way, and the dashboard in the monitoring repository.
    - Acceptance: two runs a week apart show as two points in the panel, and a fingerprint change
      is visible on it; the run refuses to start on any profile but the eval profile.

## Files affected

- New: `internal/eval/`, `cmd/home-agent-eval/`, `evals/cases/`, `evals/variants/`
- `internal/wiring/wiring.go`, `internal/store/sqlite/approvals.go`,
  `internal/store/sqlite/schedules.go`, `internal/task/service.go` (the clock, stage 1)
- `internal/connection` (a ceremony outside Telegram, stage 1)
- `internal/prompt/system.go`, `internal/runtime/runtime.go`, `internal/runtime/pi/runtime.go`
  (stage 2)
- `internal/observability/telemetry.go` and the monitoring repository's dashboard (stage 3)
- `.gitignore`, `docs/architecture.md`, `docs/plans/README.md`

## Validation

- Formatting, vet, build, module tidiness, the full test suite with and without the race
  detector, and `scripts/check-plans`. No test in CI starts a real model or needs a credential.
- Stage 1, by the operator: a run of the suite on the eval profile finishes within its budget,
  and every failing case is read in its cassette to tell a real failure from a broken case.
- Stage 2: the default prompt's golden files are byte-identical; one variant compared with the
  default yields a paired report.
- Stage 3: the judge's agreement report on the operator's labels; two weekly points in Grafana.

## Risks and open questions

- **Small suites detect only large effects.** Thirty cases with three trials each tell a clear
  improvement from a clear regression, not a few points. The report says how large a difference
  it could have detected, so a non-result is not read as "equal".
- **Live runs cost the operator's subscription quota,** which their own chat shares. The budget
  and the quota stop bound a run; the weekly run's size is chosen with that in mind.
- **The provider changes models under the same ID.** The trend is split by fingerprint, model ID
  and Pi version, and a shift with none of them changed is read as a provider change.
- **Synthetic cases drift from real use.** New cases come from failures the operator sees,
  rewritten without private data; that review is the operator's, since no check can find private
  data in free text.
- **Simulators are not the real services, and stage 1 leaves dynamic tools out.** Cases test the
  agent's choices, not the connectors, which keep their own integration tests. Home and
  catalogue MCP tools are built from outside state; evaluating them needs a recorded snapshot of
  their definitions and is left to a later plan.
- **Threading a clock into the store and the task service touches production code.** Production
  passes `time.Now`, so behaviour does not change, and the existing tests prove it.
- Open questions for the operator:
  1. Is the operator's own subscription, logged into a dedicated eval profile, the right payer?
     The plan never uses the operator's application principal or the application's profiles
     directory. Recommendation: yes; a second subscription only if eval runs start to crowd the
     operator's own chat out of its quota.
  2. Where does the weekly run happen: on the NAS, as a separate run of the harness with its own
     eval profile directory beside the application's data, or on the Mac on a schedule, pushing
     metrics? Either way it uses the dedicated eval profile only. Recommendation: the NAS, which
     is always on and already has the pinned Pi and the OTLP collector; decide after stage 2.
  3. Which model judges, and may Jev judge simple yes/no questions? Recommendation: a different
     model on the same subscription through Pi; keep Jev out, so the harness sends nothing to a
     second provider even though the cases are synthetic.
  4. How many cases and trials? Recommendation: 20 to 40 cases, three trials for the regression
     run, five for a variant comparison.
  5. What budget caps a run, and above what usage does the pre-flight refuse? Recommendation:
     300 model calls by default, raised per run on the command line, and a refusal above 50% of
     any usage window.
  6. Should cassettes of synthetic cases be committed? Recommendation: commit only the few the
     harness's own tests need; keep run cassettes local.
