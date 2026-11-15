# 0155 — Regular agent evaluations and prompt experiments

Status: `in-progress`

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

- `conversation.Service.runTurn` owns a chat turn around the engine: it picks the tool set from
  the user's switches, records the user's own message so proposals are judged against it, takes
  the turn's notice and subject context, starts the Jev tool hint, builds the `tools.Run` the
  broker and the safety checks read, and turns a timeout or a failure into a reply.
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

Success for the first stage: about forty cases covering every failure kind the history review
found and the flows that work today, chat and scheduled turns alike, with every connector the
assistant uses served by a local fake; deterministic checks and the operator's review; three
trials each; a Markdown report, run by hand from the operator's Mac.

## Decisions

- **Build a small Go harness; adopt no eval framework.** It reuses the turn engine, the Pi
  runtime, the broker and the SQLite store as they run in production, adds no dependency, and
  sends nothing anywhere production does not: the model provider and TypeSafe. Cases and
  results use JSON, which the standard library reads. Revisit if the judge or report work grows
  beyond this plan.
- **An eval case** is a JSON file with:
  - `id`, `title`, `tags` (such as `regression`, `happy`, `capability`, `time`, `proposal`,
    `injection`, `scheduled`) and the failure kind it covers, when it covers one;
  - `clock`: the fixed local time and the IANA zone the turn sees;
  - `user`: a synthetic display name, the switches that are on, memory text, and a model and
    effort when the case pins one;
  - `world`: the state the tools see. Internal tools (schedule, memory, watches, collections, trips,
    projects, routes, backlog) run for real on a temporary SQLite store seeded from the case,
    including scheduled jobs and saved routes. Every connector reaches a loopback fake seeded from
    the case, so a mailbox, a timetable, a forecast or a device is case data, not a scripted tool
    answer: the existing `googlesim`, `calendarsim` and `notionsim`; new fakes for the train and bus
    planners, the weather service, page fetch, places, flights, the monitoring connector and the NAS
    file search; a fake home server whose synthetic devices, sensors and history the case lists;
    and `mcpsim` serving the case's catalogue MCP tools and their answers. Web search is the
    model provider's own search inside Pi, not a connector, so it cannot be faked: it runs live,
    cases check the arguments it was called with, and anything that depends on its results is
    left to the rubric and the operator's review;
  - `turns`: a sequence of steps in one session. A step is a user message; a Confirm or Reject of
    the proposals the previous turn made; a 👎 on the previous reply, which starts the reconsider
    turn the way a reaction in Telegram does; or a run of one of the world's scheduled jobs at
    the case clock;
  - `expect`: deterministic checks: tools that must be called, with argument predicates (equal,
    contains, matches, within a time range); tools that must not be called; a maximum number of
    calls; proposals that must exist and what they write, or that nothing was written; the final
    state of the internal store (a job exists with this schedule, no recurring job was created);
    and reply checks (contains, does not contain, matches, language);
  - `rubric`: optional yes/no questions for the judge, each about one dimension;
  - `trials`: how many times to run it, default three.
- **Cases are synthetic and public.** They live in `evals/cases/` in this repository, use
  neutral names, places and data, and never copy a real message, trace, memory or device. A case
  inspired by a real failure is rewritten from scratch by the operator. Fake home devices and
  catalogue MCP tools are invented for the case and never copied from the household's. A Go test
  loads every case and checks its shape and its unique ID. A second test builds each case's eval
  world with every connector served by its fakes and every switch on, reads the tool names it
  offers, and checks that every tool the case names is one of them. Both run in CI with no
  model.
- **Scheduled turns run through the scheduler's own step.** A scheduled step seeds nothing new:
  it asks the schedule service to run one of the world's jobs now (`RunNow`), so the job's
  prompt, the Heads-up task and its call budget, conditions and project lines are production's
  own, and the turn runs unattended on the case clock. The reply is captured where the
  scheduler would deliver it, including a quiet run that sends nothing.
- **Happy paths guard what works.** Beside the failure cases, the suite holds cases for flows
  that work today, drawn from replies the operator marked 👍 and from frequent flows in the
  history review that went well, rewritten like every case. They carry the `happy` tag. The
  report shows their pass rate on its own line, and a change that lowers it is a regression even
  when failure cases improve.
- **The first cases come from the operator's own history, read locally.** The operator copies
  their own conversations (the Pi session files of their principal) and a snapshot of the store
  from the NAS into a private folder on the Mac, outside the repository. No other household
  member's conversations are read. The review starts from the turns marked 👎, reconsidered,
  failed or with a tool error, then reads the rest, and groups what went wrong into failure kinds
  with counts and examples. That analysis stays in the private folder and is never committed.
  The operator picks the kinds that matter; each becomes one or more synthetic cases written from
  scratch, and the operator approves every case before it is added.
- **A local privacy scan backs the operator's review.** Before a run, and as a command of its own
  the operator runs before committing a case, the harness reads the household values from the
  ignored `.env` (such as places, names and addresses its settings hold) and refuses any case or
  variant that contains one. The scan runs only on the operator's machines; CI has no `.env`, and
  the values are never printed, only the file and field that matched.
- **The operator reviews every trial until the checks earn trust.** A `review` command in the
  terminal steps through a run's trials, showing the user message, the tool calls, the proposals,
  the reply and the automatic verdict, and records the operator's own verdict (good, bad or
  unsure) with an optional note beside the run's results; the local results store of stage 2
  imports them. Where the operator and the checks
  disagree, the case is fixed: a check that missed a real failure is tightened, and one that
  failed an acceptable answer is loosened. The report shows the agreement between the checks and
  the operator per case; full review stops only when it is consistently high, after which the
  operator reviews the disagreements and a sample. The same labels later calibrate the judge.
- **Checks in order: code first, judge second.** A trial passes when every deterministic check
  passes and, when a rubric exists and the judge is on, every judged question is "yes". Grading
  prefers the final state over the exact path: an ordered sequence of calls is checked only when
  the case says the order matters.
- **The runner plays real turns through the conversation service.** A trial builds the wiring the
  production binary builds, with the store in a temporary directory and the static connectors
  pointed at the seeded simulators, and enqueues each user message on `conversation.Service` as
  a chat turn. The reply is captured at the service's delivery boundary instead of reaching
  Telegram. The harness never calls `turn.Engine` itself, so tool selection, the recorded user
  message, notices, the `tools.Run`, and timeout and failure replies are production's own. The
  broker's own executor runs every call; there is no scripted layer in front of it. The message
  is composed with the case's clock, so the time line, the user line and the tool guidance are
  what production would send. Every tool definition the model sees is the real one.
- **The eval world supplies what the wiring needs from Telegram.** `wiring.Build` requires the
  chat, reminder, alert, task-message and tab senders, and the chat side alone is several
  interfaces (the approval deliverer, documents, reactions). The harness implements them in one
  recording stub, so proposals, cards and replies are captured for the checks and nothing is
  sent. The `eval` principal is bootstrapped through `identity` with a synthetic Telegram user ID
  and chat, as a household user would be. Only the services a chat turn needs are started: the
  model connection, the conversation service and approvals. The scheduler's steps are not
  started, so no job runs on the eval profile behind the run's back. A runtime decorator that
  records each run for the cassette forwards the optional interfaces the wiring type-asserts
  (`conversation.Transcripts`, `history.Directories`).
- **Safety checks and the tool hint run as in production.** The `eval` principal's switches turn
  Jev's judgments and tool hint on, using the TypeSafe key from the operator's ignored `.env`, so
  a trial exercises the layer that withdraws a proposal or a search. Only synthetic case text
  reaches TypeSafe. A case may turn them off through its `user` switches; the report lists
  which trials ran with them, since Jev adds variance of its own.
- **One clock for the whole world.** The case's clock must reach every seam that reads the time,
  or an approval, a reminder or a task would be dated by the wall clock while the turn is told
  another day. The seams are: `wiring.CoreConfig.Now`, which feeds the schedule service and its
  reminder job, collections, intake, search checks, trips, projects, watches, events and routes;
  `turn.Config.Now`; `conversation.Config.Now`; the connectors' own `Now` fields; the approval
  service, which today fixes its clock to `time.Now` and gains a config field for proposal
  creation, expiry and confirmation; the user-memory and backlog services, whose `Now` the wiring
  does not pass today; and the direct `time.Now` calls in `store/sqlite/approvals.go` and
  `store/sqlite/schedules.go`, which gain a clock. `tools/broker.go` and the conversation
  service's own `time.Now` calls only time a call or a turn stage for metrics, and SQL
  `strftime('now')` stamps on exposure flags, job reports, conversations and tabs are never
  shown to a turn; those stay on the wall clock. Background tasks are not wired into the eval
  world in stage 1, so the task service keeps its clock until a case needs it.
- **The runner uses the Pi release the image pins.** It installs Pi from
  `third_party/pi-runtime` as the Dockerfile does and checks at startup, through the runtime's
  existing version check (`ExpectedVersion`), that Pi reports the version in
  `HOME_AGENT_PI_VERSION`'s default, which the config package exports for it. A different Pi
  refuses to run, so a result always names the Pi production runs.
- **The model is live; replay is for checks, not prompts.** Every trial records a cassette: the
  composed message, the system prompt's fingerprint, the runtime's event stream
  (`runtime.Event`), each tool call with its arguments and the answer given, the proposals and
  cards the recording stub captured, the reply, the tokens and model calls from the run's usage,
  and the model ID. Replay feeds a cassette
  back through the checks, so a new or fixed check is tried on past runs at no cost. A replay
  cannot judge a prompt change, since the model would answer differently; the report says which
  trials were replayed.
- **Who pays: the operator's own subscription, through a dedicated eval profile.** The harness
  runs as a synthetic principal, `eval`. Pi, the connection service, the model catalog and the
  usage reader all resolve a profile as `<profiles root>/<principal>`, so the command line names
  an eval profiles root (`--profiles <root>`) and the profile is `<root>/eval`. The root may not
  be, or lie inside, the application's data directory (`HOME_AGENT_DATA_DIR`, whose `pi`
  directory holds the household's profiles). The harness never runs as the operator's
  application principal and never on another user's login, wherever it runs.
- **The eval profile's binding lives in a persistent eval store.** A credential is usable only
  when the store binds it: readiness and the credential readers check a `model_connections` row
  for the principal against the account in `auth.json`. The root therefore holds `eval.sqlite`
  beside the profile, and login binds the account there. Each trial's temporary store is seeded
  with that binding, and the `usage` command reads through the eval store.
- **The eval profile is logged in by the harness itself.** `home-agent-eval login --profiles
  <root>` runs the device-code flow of `internal/connection`, printing the verification address
  and code in the terminal instead of sending them to Telegram. It creates `<root>/eval` as a real
  directory, writes the credential into it with the same private permissions, and binds the
  account in the eval store. Copying `auth.json` from a production profile
  is forbidden and the documentation says why: the provider rotates the refresh token on use, so
  two copies refreshing apart would leave one of them, possibly the operator's own chat, logged
  out.
- **A run checks the quota before it starts and stops at its budget.** The pre-flight reads the
  eval profile's usage through the reader `/status` uses (`chatgpt.Client.Usage`) and refuses to
  start when any window is above a set percentage used (default 50%), so an eval never eats the
  quota the operator's own chat needs. The usage reader never refreshes and a weekly profile
  almost always holds an expired access token, so the pre-flight first takes the credential
  through `connection.Service.Credential`, which refreshes under the profile's file lock. During
  the run a
  budget bounds it, by default 300 model calls and raised per run on the command line, and the
  first quota or authentication failure stops it, reporting the trials it did not run.
- **Suite size.** Stage 1 starts with about forty cases: two or three for each failure kind the
  history review found and about ten happy paths. A regression run plays three trials per case,
  and a variant comparison five.
- **Cassettes stay local.** Run cassettes are written to the ignored results directory. Only the
  few cassettes the harness's own tests need are committed.
- **Prompt variants are named overrides of the fixed sections.** `prompt.System`'s constants
  become fields of a `prompt.Sections` value with a `Default`; production uses `Default` and its
  rendered bytes do not change, which the existing golden files prove. A variant file
  `evals/variants/<name>.json` has a `description`, and any of `sections` (section name to
  replacement text, names checked against `prompt.SectionNames`; the closing secrecy sentence is
  not replaceable), `tool_guidance` (the name of a tool to the text that replaces the guidance of
  the connector offering it, write guidance included; names checked against the tools the planned
  cases' worlds offer, before any model call), and `model` and `effort` (the run's defaults; a case
  that pins its own keeps it). Most failures the first baseline found sit in tool guidance, so a
  variant may change it as well as the fixed sections. Overrides reach the runtime only through the
  harness: `RunRequest` gains an optional `PromptSections` field, which the eval recorder sets and
  the Pi runtime reads through `RunRequest.SystemPrompt`, and the tool broker gains a guidance
  override map that only the eval world sets. Production sets neither, so its bytes do not change.
  A run records the variant's name, the SHA-256 of its file and the fingerprint.
- **Variants do not break prompt caching.** The system prompt stays free of per-turn facts in
  every variant, and a run executes all trials of one variant before the next, so each variant
  warms its own cached prefix and production's prefix is never touched. A winning variant ships
  as an ordinary change to the defaults with its golden files, never as a runtime switch, and is
  never tried live on household users: their turns are too few to compare and are private.
- **A prompt's identity is its fingerprint.** The harness records the SHA-256 of the rendered
  system prompt without memory and jobs (the tools' guidance text is part of it, so a
  guidance-only variant has its own fingerprint), the commit, the Pi version, the model and the effort with
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
acceptance criteria. Chunks 4, 5, 6, 8, 9, 10 and 11 all edit `cmd/home-agent-eval/main.go`, so
they run one at a time in that order, each on top of the one before.

### Stage 1: a regression suite run by hand

0. **History review** — Operator with the orchestrator; no code; no dependency.
   - Task: the operator copies their own Pi session files and a store snapshot into a private
     folder outside the repository; the review groups what went wrong into failure kinds with
     counts and examples, and the operator picks the kinds the first cases cover; it also lists
     the flows that work, from 👍 replies and frequent successful turns, as happy-path candidates.
   - Acceptance: the operator has approved the failure kinds and the happy paths; nothing from
     the history is in the repository.
1. **Case format and the first cases** — Complex; depends on 0 for the cases, not the loader.
   - Task: the case types and loader, the CI shape and ID test, and about forty synthetic cases,
     chat and scheduled, written from scratch for every failure kind and happy path the operator
     approved, each case approved one by one. Kinds expected beside them: answering a date question
     from the time line; creating a one-off reminder at the right local time; not creating a
     recurring job unless asked; proposing a write instead of claiming it was done; calling a tool
     before saying something is unavailable; ignoring an instruction inside a seeded mail; replying
     in the language of the message; taking the likely reading of an ambiguous request and stating
     it; asking first when a guess would cost something.
   - Files: `internal/eval/case.go`, `internal/eval/case_test.go`, `evals/cases/*.json`.
   - Acceptance: every case loads; a malformed case and a duplicate ID each fail the test with
     the file named; the test runs in CI with no model and no network. Tool names are checked in
     chunk 3, once the eval world serves every connector.
2. **The eval world and its clock** — Complex; depends on 1.
   - Task: build the production wiring for the `eval` principal on a temporary store, seed the
     internal tools and the `googlesim`, `calendarsim` and `notionsim` simulators from the case,
     and leave background tasks unconfigured. Run chat, Confirm and Reject, 👎 and scheduled
     steps: a 👎 enqueues the reconsider turn as the Telegram reaction handler does, and a
     scheduled step calls the schedule service's `RunNow` for the job. Implement the
     recording stub for the chat, reminder, alert, task-message and tab senders, bootstrap the
     `eval` principal through `identity`, seed the model-connection binding from the eval store,
     and start only the model connection, the conversation service and approvals. Give the case's
     clock and zone to every seam listed under "One clock for the whole world", adding a clock to
     the approval service and to the store's approval and schedule writes, and passing it to the
     user-memory and backlog services.
   - Files: `internal/eval/world.go`, `internal/eval/chat.go`, `internal/eval/world_test.go`,
     `internal/eval/case_test.go`, `internal/wiring/wiring.go`, `internal/approval/approval.go`,
     `internal/store/sqlite/approvals.go`, `internal/store/sqlite/schedules.go`.
   - Acceptance, with the Pi fixture: a message enqueued on the conversation service runs a turn
     whose reply and proposals reach the recording stub and nothing else; no scheduler step runs
     on its own; a scheduled step runs the job's prompt with the Heads-up budget where it applies
     and captures a quiet run as quiet; a 👎 step produces a reconsider turn; the safety checks
     and the tool
     hint reach a TypeSafe stand-in when the case's switches turn them on and nothing when they
     are off; the composed message starts with the case's time line; a
     seeded mail, event or page is what the connector's tool returns; an internal tool writes to
     the temporary store; a proposal's creation and expiry, a reminder's next run, a schedule
     run's record and a memory proposal are all on the case's clock, as are the trip,
     project, watch and collection lines a turn reads; nothing outside loopback is contacted; the
     production wiring passes `time.Now` and its tests are unchanged.
3. **Fakes for every connector** — Complex; depends on 2.
   - Task: loopback fakes seeded from the case for the train and bus planners and disruptions,
     the weather service (its existing forecast and geocoding URL overrides), page fetch
     (through the web connector's existing resolver and dialer, so the guard still vets every
     hop), places, flights, the monitoring connector and the NAS file search; a fake home
     server whose synthetic devices, sensors, history and
     vacuum schedule come from the case; and `mcpsim` serving the case's catalogue MCP tools.
     Where a connector has no endpoint override yet, add one that production leaves empty. Add
     the CI test that checks every tool a case names against the tools its world offers.
   - Files: `internal/testutil/` (new fakes beside the existing simulators),
     `internal/eval/world.go`, the connectors that gain an endpoint override,
     `internal/eval/case_test.go`.
   - Acceptance: with the Pi fixture, each fake's seeded answer is what its tool returns; a
     home or MCP tool appears only when the case's world defines it; an unknown tool name in a
     case fails the tool-name test with the file named; nothing outside loopback is contacted;
     every connector's own tests are unchanged.
4. **Operator commands: login, quota pre-flight and privacy scan** — Complex; no dependency.
   - Task: the eval store; `home-agent-eval login --profiles <root>` running the
     `internal/connection` device-code flow with a terminal deliverer and binding the account in
     the eval store; `home-agent-eval usage --profiles <root>` refreshing the credential through
     `connection.Service.Credential` and reading usage through `chatgpt.Client.Usage` over the
     eval store; `home-agent-eval scan` reading
     the household values from the ignored `.env` and refusing cases or variants that contain one.
   - Files: `cmd/home-agent-eval/main.go`, `internal/eval/login.go`, `internal/eval/preflight.go`,
     `internal/eval/scan.go`, `internal/eval/profile.go`, `internal/config/conversation.go` (the
     exported Pi version default), and in `internal/connection` only what lets a caller outside
     the Telegram flow start and finish a ceremony for one principal.
   - Acceptance: against the `codexauth` simulator, login creates `<root>/eval`, writes a
     credential readable by Pi there with private permissions, binds it in the eval store, and
     refuses a root that is or lies inside `HOME_AGENT_DATA_DIR`; with an expired access token the
     usage command refreshes and then reads; against a usage stub, a window above the threshold
     refuses with the window named; the scan finds a planted value in a case and prints the file
     and field, never the value; without `.env` the scan says it found nothing to check.
5. **Runner, cassettes and checks** — Complex; depends on 2, 3 and 4.
   - Task: check the Pi version against the pinned one, run the quota pre-flight and the scan,
     then run each case's trials in a fresh session on the eval profile, playing every step
     through the world and waiting for its captured reply, record the cassette,
     apply the deterministic checks, enforce the budget, stop on quota or authentication failure,
     and write the JSON result and a Markdown report (pass rate, `pass^k`, the happy-path pass
     rate on its own line, pass rate per failure kind, failing checks per case, tokens, Pi
     version, model, prompt fingerprint).
   - Files: `internal/eval/runner.go`, `internal/eval/checks.go`, `internal/eval/report.go`,
     `cmd/home-agent-eval/main.go`, `.gitignore`.
   - Acceptance: an integration test with the Pi fixture covers a passing and a failing trial, a
     budget stop, a quota stop, a Pi of another version refused at startup, and a run refused by
     the pre-flight; the command refuses to start without an explicit profiles root and never
     opens the application's data directory; the budget counts model calls from the run's usage.
6. **Operator review** — Complex; depends on 5.
   - Task: the `review` command over a run's trials, the operator's verdicts and notes stored
     beside the trials, and the agreement between checks and operator in the report.
   - Files: `internal/eval/review.go`, `internal/eval/report.go`, `cmd/home-agent-eval/main.go`.
   - Acceptance: with recorded trials, a review stores a verdict per trial and resumes where it
     stopped; the report counts agreements and lists every disagreement with its case; a trial
     the operator marks bad that the checks passed shows as a missed failure.
7. **Operator documentation** — Simple; depends on 6.
   - Task: how to install the pinned Pi, log in the eval profile (and why `auth.json` is never
     copied), run a suite, read the report, scan and write a new case.
   - Files: `docs/architecture.md` (a section on evals).
   - Acceptance: the steps run as written on the operator's Mac.

### Stage 2: variants, history and comparison

8. **Prompt sections and variants** — Complex; depends on 5.
   - Task: `prompt.Sections` with `Default`, the optional sections field on `RunRequest` read by
     the Pi runtime, a guidance override map on the tool broker that only the eval world sets,
     variant files (sections, tool guidance, model and effort), the prompt fingerprint, a
     `--variant` flag on the run command that records the variant's name, file hash and
     fingerprint in `result.json` and the report, and one example variant,
     `look-before-asking`, for the assistant asking for details it could find with its tools.
   - Files: `internal/prompt/system.go`, `internal/runtime/runtime.go`,
     `internal/runtime/pi/runtime.go`, `internal/tools/broker.go`, `internal/eval/variant.go`,
     `internal/eval/world.go`, `internal/eval/recorder.go`, `internal/eval/runner.go`,
     `evals/variants/`, `cmd/home-agent-eval/main.go`.
   - Acceptance: every existing golden file is unchanged; the Pi fixture receives the variant's
     prompt only when the harness sets one; the fingerprint changes with a section or a tool's
     guidance and not with memory or jobs; a variant naming a tool no planned world offers is
     refused before a model call; trials of one variant all run before the next starts.
9. **Local results store, trend and compare** — Complex; depends on 8.
   - Task: the SQLite file with runs, trials, checks and the operator's verdicts; `trend` (pass rate
     and `pass^k` per run, split by fingerprint, model and Pi version) and `compare` (the paired
     report above).
   - Files: `internal/eval/store.go`, `internal/eval/stats.go`, `internal/eval/compare.go`,
     `cmd/home-agent-eval/main.go`.
   - Acceptance: the statistics are tested on hand-computed examples; comparing a run with itself
     reports no difference; runs over different case sets compare only their shared cases and
     say so.
10. **Replay** — Simple; depends on 9.
   - Task: re-score stored cassettes with the current checks, as a `replay` command.
   - Files: `internal/eval/replay.go`, `cmd/home-agent-eval/main.go`.
   - Acceptance: changing a check and replaying a run changes its verdicts without starting Pi,
     and the report marks the run as replayed.

### Stage 3: judge and weekly trend

11. **Rubric and pairwise judge** — Complex; depends on 10.
   - Task: the judge run, its prompt, `unknown`, order-swapped pairwise verdicts, and a labelling
     command that records the operator's own verdicts and reports agreement per question.
   - Files: `internal/eval/judge.go`, `cmd/home-agent-eval/main.go`.
   - Acceptance: with the Pi fixture, a swapped pair that disagrees counts as no verdict; a
     question below the agreement threshold is reported but does not decide a trial.
12. **Weekly run and Grafana** — Complex; depends on 9 and on the weekly-run location question
    below.
    - Task: run the regression suite weekly on the dedicated eval profile, export pass rate,
      `pass^k`, tokens and the fingerprint's short form as metrics, and add a panel to the
      monitoring repository's dashboard.
    - Files: depends on the answer; `internal/observability/telemetry.go` for the metrics either
      way, and the dashboard in the monitoring repository. On the NAS it also needs the harness
      in the image or a container of its own, the deploy files, and an interactive device login
      of the eval profile there; those files are named once the question is answered.
    - Acceptance: two runs a week apart show as two points in the panel, and a fingerprint change
      is visible on it; the run refuses to start on any profile but the eval profile.

## Files affected

- New: `internal/eval/`, `cmd/home-agent-eval/`, `evals/cases/`, `evals/variants/`
- `internal/testutil/` (new fakes for every connector) and an endpoint override in any connector
  that lacks one (stage 1)
- `internal/wiring/wiring.go`, `internal/approval/approval.go`,
  `internal/store/sqlite/approvals.go`, `internal/store/sqlite/schedules.go` (the clock, stage 1)
- `internal/connection` (a ceremony outside Telegram, stage 1), `internal/config/conversation.go`
  (the exported Pi version default, stage 1)
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
- **Fakes are not the real services.** Cases test the agent's choices, not the connectors, which
  keep their own integration tests. Home and catalogue MCP tools are built from the fake's
  invented devices and tool lists, so their descriptions can drift from the live ones; a case
  that depends on a description is updated when the connector changes.
- **Stage 1 is larger than first planned.** Covering every failure kind means fakes for every
  connector and scheduled turns before the first run. The chunks stay independent enough that
  the runner can start on the chat cases while the remaining fakes land.
- **Threading a clock into the store and the task service touches production code.** Production
  passes `time.Now`, so behaviour does not change, and the existing tests prove it.
- **Jev adds variance of its own.** With the safety checks and the tool hint on, a trial's
  outcome also depends on TypeSafe's answers, which may change without a commit here. The report
  marks the trials that ran with them, and a comparison of prompt variants can turn them off by
  case when it needs to isolate the prompt.
- Open questions for the operator, decided after stage 2:
  1. Where does the weekly run happen: on the NAS, as a separate run of the harness with its own
     eval profile directory beside the application's data, or on the Mac on a schedule, pushing
     metrics? Either way it uses the dedicated eval profile only. Recommendation: the NAS, which
     is always on and already has the pinned Pi and the OTLP collector.
  2. Which model judges, and may Jev judge simple yes/no questions? Recommendation: a different
     model on the same subscription through Pi; keep Jev out of judging, so the judge does not
     share a model with the safety checks it may be grading.
