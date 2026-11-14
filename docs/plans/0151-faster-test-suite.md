# 0151 — Faster tests: one migration per process everywhere, no fixed waits

Status: `done`

## Context

Plan 0110 brought the race suite to a few minutes with a migrated template database and parallel
Telegram tests. Since then the "Test with race detection" step in CI has grown to about 18
minutes, and the backlog asks why the tests wait and lock so much and whether that can go.

A measurement of the current suite, on a 12-core Mac that other work was loading at the same time,
and of the latest CI run, found two separate causes.

**Under the race detector, the time is SQLite migrations.** Sixteen packages still open their test
store with `sqlite.Open`, which runs all 57 migrations for every test, and none of their tests is
parallel. A CPU profile of trip tests under the race detector puts about two thirds of their time
in `sqlite.migrate`, about 3 seconds of CPU per test. The SQLite driver is C translated to Go, so
its internal mutexes and memory accesses are all instrumented; these are the "system mutexes" a
profile shows. In the latest CI run, the packages that migrate per test take about 2,760 of the
3,340 package-seconds: trip 477, store 366, Home Assistant 361, web 359, Notion 268, NS 216,
Grafana 206, NAS 199, bus 155, location 95 and routes 60. Telegram, on the template, takes 264. CI
has four cores, so its wall time follows the sum. Locally the race run took about 9 minutes, and
trip (464 s) was the last package to finish.

**Without the race detector, the time is fixed delays that tests wait out.** The suite took about
85 seconds, of which `internal/telegram` was about 76: a serial set of about 42 seconds, mostly
waiting with an idle CPU, then a parallel set of about 35 seconds whose tail is one test.

- **The default approval sweep.** `TestInboxFollowUpExpiresOnThreeCardsAndIsDropped` back-dates
  its approvals in the database and then waits for the sweep, which runs every 30 seconds by
  default. It takes 30 seconds and is the last Telegram test to finish.
- **Real approval lifetimes.** Six Telegram tests wait out a 2 or 3 second approval lifetime with a
  100 ms sweep: `TestWidgetShowsExpiry`, `TestChangesWidgetShowsExpiry`,
  `TestUnflaggedWidgetIsEditedThroughTheRefusedTextEdit` (two offers),
  `TestLongBatchExpiryEditsTheButtonMessageOnly`, `TestApprovalLifecycleIsRecordedOncePerResolution`
  and `TestNotionProposalsExpireFailSafelyAndDoNotSurviveRestart`. A short real lifetime can run
  out under load before the test acts, so they are serial, about 16 seconds together.
- **The conversation delivery retry.** A failed delivery is retried after a fixed 5 seconds
  (`deliveryRetryDelay`), which no configuration changes.
  `TestCardTextFailureAfterThePhotoIsRetriedWithoutASecondPhoto` waits it out twice in the serial
  set (10 seconds); `TestCardPhotoFailureIsRetriedWithTheCardStillStaged` and the `500` case of
  `TestConversationDeliveryFailuresAndUnicodeChunks` wait it out once each.
- **The device code poll.** The connection ceremony polls at the RFC 8628 floor of one second and
  adds five seconds on `slow_down`. The subtests of `TestConnectOutcomes` take 1 to 7 seconds, and
  since their parent is serial, its `slow_down` subtest holds the serial set up for 7 seconds.
- **The Pi stop grace.** Stopping a Pi process waits 2 seconds after closing its input before it
  interrupts it. The fixture's hanging scenarios ignore a closed input, so each stop of one costs 2
  seconds: `TestSilentTurnIsRetriedOnceWithoutRepeatingTheUserMessage` (7 s),
  `TestCancellationAndTimeoutStopRealProcess` (4 s),
  `TestSilenceAfterAToolCallWaitsForTheTurnDeadline` (4 s) and `TestUnansweredModelCallTimesOut`
  (3 s). None of the 45 Pi tests is parallel.
- **The HTTP retry backoff.** Connector clients build their `httpretry` transport with only a
  service name, so their failure tests sleep through real backoffs up to the 5 second window: two
  web weather tests (5 s each), the bus OVapi fallback (5 s), Grafana's error classes (4 s), and
  two Home Assistant outage tests (2 s each).

What is not a cost: `t.Setenv` appears only in the config package and in one Pi test, no test
holds a package-level mutex (the mutexes in the tests are fields of per-test simulators), each
test has its own database so there is no lock contention between tests, and the 5 ms polling
loops (`waitState`, `waitMessageRecorded`) return as soon as their condition holds.

The savings below are estimates from this measurement and are upper bounds: a test that opens
no store saves nothing from the template, and a test that waits for something other than the
delay removed saves less than the delay.

## Goal

- The CI race step drops from about 18 minutes to under 8, and the local suite without the race
  detector from about 85 seconds to about 30, every test verifying what it verifies today.
- No test waits out a delay longer than about a second unless the delay is what it asserts.
- No new flake: nothing depends on a real lifetime running out before or after the test acts.

## Decisions

- **Every package outside the store opens its test store from the template.**
  `testutil.OpenStore` replaces `sqlite.Open` in the test helpers of trip, web, Home Assistant,
  bus, Grafana, NAS, Notion, NS, location, routes, tools, app, approval, attachment, connection
  and wiring. A test that is about opening the database (permissions, symlinks, bad
  configuration) keeps `sqlite.Open`. All of these test packages can import `testutil` without a
  cycle; the internal ones (`app`, `wiring`, the NAS and tools internal files) are checked when
  switched.
- **The store package gets a template of its own for the tests that are not about migrations.**
  Plan 0110 left the store migrating from empty because its tests cannot import `testutil`. Only
  15 of its 59 tests are about migrations; the other 44 go through `openTestStore`. A helper in
  the store's own test files builds the template once per process the same way `testutil` does,
  and `openTestStore` copies it. The `TestMigration…` tests and any test that calls `Open` on
  purpose keep migrating from empty.
- **Expiry is made to happen, not waited for.** A harness helper back-dates the pending approvals
  of the test's database, as `TestInboxFollowUpExpiresOnThreeCardsAndIsDropped` already does, and
  the sweep stays at the proven 100 ms. Their approvals keep the default lifetime until then, so
  nothing expires while the test is still acting, and the expiry tests become parallel. The
  helper runs only after `waitMessageRecorded` for the proposal's last message, because a sweep
  that finds a record without its message cannot edit it. A fake approval clock was considered:
  it needs a new `approval.Config` field and moves the approval service off the real time the
  rest of the harness uses; back-dating changes no production code.
- **The delivery retry delay is configuration, shortened only where a test asks for it.** The
  conversation service takes `DeliveryRetryDelay` in its `Config` (zero means 5 seconds), passed
  through `wiring` like `TurnTimeout`. The same delay covers load, deliver and store-response
  errors, each retried without bound, so the harness keeps the production value by default and
  only the retry tests set about 100 ms through a per-test harness option. The two card tests
  read the outbox between the failed attempt and the retry, so the fake Telegram server holds
  their retried send until the test has read the outbox and released it; with that gate the short
  delay cannot let the retry overtake the test.
- **The device poll's floor and slow-down step are configuration.** The connection service takes
  both in its `Config`; zero means the RFC 8628 values (one second and five seconds), a negative
  value is refused by `NewService`, and the fields' comments say so. `wiring` already passes that
  config through whole. The floor alone is not enough: the codexauth simulator answers with an
  interval of one second and the poll takes the larger of that and the floor, so the simulator
  gains an option for a zero or sub-second interval.
- **The Pi stop grace is configuration, and only its first step is shortened in tests.** The Pi
  runtime takes `StopGrace` in its `Config` (zero means 2 seconds) for the wait between closing
  Pi's input and interrupting it. The waits after the interrupt and after the kill, and
  `cmd.WaitDelay`, keep their production margins, so reaping stays safe under the race detector.
  Tests set about 500 ms.
- **The fixture's hanging scenarios keep ignoring a closed input.** Exiting on a closed input would
  skip the escalation from close to interrupt to kill that those tests cover.
- **The remaining serial Telegram tests stay serial.** Typing and draft indicators, the 300 ms
  turn, task and intake timeouts, the 2 s connection code timeout, and the order-sensitive tests
  from plan 0110 assert wall-clock behaviour and cost about 8 seconds together.
  `TestCardTextFailureAfterThePhotoIsRetriedWithoutASecondPhoto` stays serial for its own reason
  (it reads the outbox before load lets clean-up finish) but no longer waits.
- **A package's tests become parallel only where nothing is shared.** Before marking a package,
  a search shows no `t.Setenv`, `os.Chdir`, package variables written by tests, process-wide
  defaults, or fixed ports. Every test and subtest calls `t.Parallel()` first, before any server,
  store or temporary directory is built, as in plan 0110. A test that asserts wall-clock time, or
  uses `t.Setenv`, stays serial with a one-line reason; a test that flakes under load moves to
  the serial set rather than having its assertion loosened. The app and wiring tests stay serial:
  `app.New` sets `httpretry.SetDefaults` and the OpenTelemetry globals.
- **Decided while implementing.**
  - `expirePendingApprovals` waits for the given message to be recorded itself, so a caller cannot
    back-date first. The harness's approval lifetime option is gone, since no test sets one.
  - The approval service's `SetLifetime` is gone too. The restart test used it only to undo a
    short lifetime, and nothing else calls it.
  - The inbox expiry test waits for each expiry edit before its next check. With a 100 ms sweep,
    that edit could otherwise come between the press and the settings page edit the press waits
    for.
  - The slow-down subtest also checks that approval came at least one slow-down step after
    `slow_down`. The harness polls at 50 ms with a 300 ms step.
  - More tests stay serial, each with its reason:
    - Pi's provider-failure test asserts a 3 s bound, its reader-release test counts goroutines
      across the process, and its bridge test dials a closed port that another test may reuse.
    - Two web weather tests assert durations, and the tools deadline test reads the wall clock.
    - The subtests of the Pi model-pair test share one principal's first turn.
    - Subtests that share their parent's fixture (several web refusal and geocoding tests, an NS
      trip-status test) run in order.
  - Pi's early-exit test stays serial. Under load, Pi's exit can close its output before the
    runtime reads it, and the turn then reports an invalid event stream instead of the exit code.
    That is a production race, outside this plan.
- **No change to `-p`, `-parallel`, GOMAXPROCS or CI sharding.** Locally the suite is bounded by
  waiting, in CI by CPU; neither improves by changing how many tests run at once. Sharding is the
  follow-up only if CI stays above about eight minutes.

## Approach

Each chunk is a commit of its own, ends with the touched packages passing with and without
`-race`, and leaves production behaviour unchanged. The chunks of a wave touch disjoint files and
can run side by side. Times in the acceptance criteria are read from the package `Elapsed` of
`go test -count=1 -json` on an otherwise idle machine, or, for CI, from the package lines and the
race step's duration in `gh run view`.

Wave 1:

- **A. Template store outside the store package.** Task: switch the store helper of web, Home
  Assistant, bus, Grafana, NAS, Notion, NS, trip, location, routes, tools, app, approval,
  attachment and wiring to `testutil.OpenStore`, keeping `sqlite.Open` only where a test is about
  opening. The connection package's helper is left to chunk D. Files: the `*_test.go` store
  helpers of `internal/connectors/{web,homeassistant,bus,grafana,nas,notion,ns}` and
  `internal/{trip,location,routes,tools,app,approval,attachment,wiring}`. Complexity: Simple.
  Depends on: nothing. Accepted when no test there calls `sqlite.Open` except to test opening,
  every assertion is unchanged, and in CI under the race detector trip takes at most 200 s, Home
  Assistant and web at most 150 s each, Notion at most 110 s, NS, Grafana and NAS at most 90 s
  each, bus at most 70 s, location at most 40 s and routes at most 30 s. Expected saving: up to
  about 1,700 of CI's package-seconds.
- **B. A template inside the store package.** Task: an in-package helper that migrates once per
  process and copies, used by `openTestStore` and the other non-migration helpers; the migration
  tests keep migrating from empty. Files: `internal/store/sqlite/telegram_test.go` (where
  `openTestStore` lives) and the store test files that call `Open` directly. Complexity: Complex:
  each direct `Open` has to be sorted into "about migrations or opening" and "just needs a store",
  and a migration test must never get a pre-migrated file. Depends on: nothing. Accepted when the
  15 migration tests still start from an empty file and the store package takes at most 180 s in
  CI under the race detector. Expected saving: up to about 250 of CI's package-seconds.
- **C. Approval expiry without waiting.** Task: give
  `TestInboxFollowUpExpiresOnThreeCardsAndIsDropped` a 100 ms approval sweep; add an
  `expirePendingApprovals` harness helper beside `waitMessageRecorded`; move the six expiry tests
  and `export_confirm_integration_test.go`'s 1 s lifetime to the default lifetime, back-date once
  the proposal's message is recorded, keep the 100 ms sweep, and make the six parallel without
  their serial comments. Files: `internal/telegram/content_intake_integration_test.go`,
  `approval_batch_integration_test.go` (helper), `widget_integration_test.go`,
  `changes_widget_integration_test.go`, `metrics_integration_test.go`,
  `approval_integration_test.go`, `export_confirm_integration_test.go`. Complexity: Complex: each
  test orders propose, record, back-date and expect differently, the restart test must back-date
  before the restart it checks, and `TestUnflaggedWidgetIsEditedThroughTheRefusedTextEdit`
  expires two offers one after the other. Depends on: nothing. Accepted when every assertion is
  unchanged, no Telegram test sets an approval lifetime below the default, the inbox test and each
  of the six take at most 2 s without the race detector, and the six pass in parallel. Expected
  saving: up to about 30 seconds of the Telegram package's tail and 16 seconds of its serial set.
- **D. Configurable delivery retry and device poll.** Task: `DeliveryRetryDelay` in
  `conversation.Config`, through `wiring.Inputs.Core`, with a per-test harness option that only
  the three retry tests set to about 100 ms; a gate in the fake Telegram server that holds the two
  card tests' retried send until the test has read the outbox; the poll floor and slow-down step
  in `connection.Config`, zero meaning the RFC 8628 values and negatives refused; a zero or
  sub-second interval option in the codexauth simulator; the Telegram harness and the connection
  tests using them; and the connection tests' store helper switched to `testutil.OpenStore`.
  Files: `internal/conversation/types.go`, `internal/conversation/service.go`,
  `internal/wiring/wiring.go`, `internal/connection/device.go`, `internal/connection/service.go`,
  `internal/connection/service_test.go`, `internal/testutil/codexauth/simulator.go`,
  `internal/telegram/conversation_integration_test.go`,
  `internal/telegram/card_integration_test.go`,
  `internal/telegram/connection_integration_test.go`. Complexity: Complex: the card tests' gate
  must hold the retry and not the first attempt, and `slow_down` must still be seen to lengthen
  the interval. Depends on: nothing. Accepted when production configuration is unchanged, the
  three retry tests take at most 1 s each, `TestConnectOutcomes` takes at most 3 s, every
  connection subtest still checks what it checks, and, with chunk C, `internal/telegram` takes at
  most 35 s without the race detector. Expected saving: up to about 15 seconds of the Telegram
  serial set and 10 seconds of parallel tests.
- **E. Pi stop grace and parallel Pi tests.** Task: `StopGrace` in `pi.Config` for the
  close-to-interrupt step only, set to about 500 ms in the Pi tests' config and in the Telegram
  tests' Pi configs where one stops a hanging fixture; then mark the Pi tests parallel except the
  `t.Setenv` test and the stall and slot tests (`stall_test.go`, `limit_test.go`), each with a
  reason. Files: `internal/runtime/pi/runtime.go`, `internal/runtime/pi/*_test.go`,
  `internal/telegram/conversation_provider_test.go`, `internal/telegram/web_integration_test.go`.
  Complexity: Complex: the reap margins must stay safe under the race detector, and with a 1.5 s
  test stall timeout a parallel test must not be slowed into a false stall by fixture start-up.
  Depends on: nothing. Accepted when the stubborn fixture is still killed after the interrupt, the
  package passes five times in a row with `-race -cpu 2`, and it takes at most 12 s without the
  race detector. Expected saving: up to about 15 seconds of the Pi package.

Wave 2:

- **F. Parallel connector tests.** Task: mark the tests of web, Home Assistant, bus, Grafana, NAS,
  Notion and NS parallel under the rule above, one commit per package. Files: those packages'
  `*_test.go`. Complexity: Complex: some share a simulator, a clock or a metrics reader across
  subtests, and wall-clock assertions must be found. Depends on: A. Accepted when each package
  passes `go test -race -cpu 2 -count=3` and takes, without the race detector, no more than
  twice its slowest test. Expected saving: little CPU, but locally each package finishes in about
  its slowest test instead of the sum of its tests.
- **G. Parallel tests elsewhere.** Task: the same for trip, location, routes, tools, approval,
  attachment and the store package; app and wiring stay serial. Files: those packages'
  `*_test.go`. Complexity: Complex, as F; the store's migration tests may share nothing but must
  each keep their own empty file. Depends on: A and B. Accepted as F, and the CI race step takes
  at most 8 minutes with all chunks in.

## Files affected

- The `*_test.go` store helpers and tests of the packages in chunks A, F and G
- `internal/store/sqlite/*_test.go`
- `internal/telegram/*_test.go` (harness options, the expiry helper, the expiry, card, connection
  and Pi-config tests)
- `internal/conversation/types.go`, `internal/conversation/service.go`
- `internal/connection/device.go`, `internal/connection/service.go`,
  `internal/connection/service_test.go`
- `internal/testutil/codexauth/simulator.go`
- `internal/runtime/pi/runtime.go`, `internal/runtime/pi/*_test.go`
- `internal/wiring/wiring.go` (the delivery retry only)

## Validation

- The full suite passes with and without `-race`, and in CI.
- Flake run before the plan is done: `go test -count=15 -timeout 30m ./internal/...` and
  `go test -race -cpu 2 -count=2 ./internal/...` together, both green.
- Before and after wall time for the suite, the CI race step, and the slowest packages, measured
  with no other suite running, reported in chat.

## Risks and open questions

- **HTTP retry backoff in connector tests.** The transport already takes `Sleep` and `Jitter`
  options, but no connector exposes them. Either each connector's configuration grows a retry
  option, or `httpretry` gains a process default beside `SetDefaults` that a package's `TestMain`
  sets. Recommendation: neither for now; after chunk F these tests run in parallel and the backoff
  costs only the slowest test's time, about 20 seconds of connector time in all. Revisit if a
  connector package stays on the critical path.
- **The store's own template.** It duplicates about 30 lines of `testutil` in the store's test
  files. Recommendation: accept the duplication; moving the builder into the store package would
  put test support in production code.
- **Default lifetime in back-dated tests.** A test that shows the lifetime to the user would now
  show the default's text. Recommendation: assert that text rather than keep a short real
  lifetime.
- **CPU-bound CI.** Parallel tests do not reduce CPU, so CI's gain comes from chunks A and B
  alone. If CI stays above about eight minutes, sharding the race step is the follow-up.
- **Smaller fixed timeouts that stay.** The 2 s ChatGPT usage timeout, the 3 s status usage
  timeout and the 2 s model catalog lock wait are what their tests assert, run in parallel, and
  are left alone.
