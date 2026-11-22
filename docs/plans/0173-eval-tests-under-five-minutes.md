# 0173 — Race suite under five minutes: template store and parallel eval tests

Status: `in-progress`

## Context

Plans 0110 and 0151 brought the race suite down with a migrated template database and parallel
tests. The eval package has grown since and uses neither, and it is now the long pole again: the
CI "Test with race detection" step takes about 14 minutes of a 15-minute job, and
`internal/eval` alone takes 693 seconds of it. The other 72 packages take about 980
package-seconds together; eval runs on alone after them. A later run took about 25 minutes, but
every package in it was about 1.6 times slower (eval 1,160 s, Telegram 396 s, store 164 s), so
that was a slower runner, not growth. Locally, on twelve cores, the race run took about 16
minutes, of which eval took 902 seconds; the next slowest packages were Telegram (263 seconds in
the full run, 166 alone) and the store (136).

Every eval test builds a full world through `NewWorld`, which opens its store with `sqlite.Open`
and so runs every migration. A CPU profile of one world-building test under the race detector puts
65 percent of its CPU in `sqlite.migrate`. Locally a world costs about 5.5 seconds under the race
detector, and none of the eval tests is parallel. `TestCasesNameOnlyToolsTheirWorldOffers` builds
one world per repository and test-data case, 69 in all, and takes 384 seconds; the other tests
take about 210 seconds together, mostly one world each.

The internal tests build their worlds through one helper, `newVariantWorld` in `world_test.go`,
which can choose the world's directory. The external tests (`runner_test.go`,
`runner_variant_test.go`, `replay_test.go`) and `TestProbesFindLooseChecksWithoutAModel` build
theirs inside `Run` and `RunProbes`, in production code.

A first search of the eval package and its test files found no `t.Setenv`, `os.Chdir`, fixed
ports, or package variables written by tests. `TestMain` replaces the default transport's dialer
once, before any test runs; `app.New`'s process defaults are not reached from `NewWorld`.

**CI budget after the eval change.** Eval's CPU should drop to about a third, about 245
package-seconds, plus what the worlds built inside `Run` and `RunProbes` still migrate. With the
other packages' 980 that is at least 1,300 package-seconds, about 5.4 minutes on four cores with
perfect packing.
The race step also spends about 2.5 minutes compiling and linking, and the job about a minute on
set-up and vet. The CI workflow is therefore expected to land at six to seven minutes after the
eval change, so the CI split (chunk B) is the expected case. It still runs only once measurement
confirms it.

The branch ruleset requires the `validate` status check, which is the CI job's name.

## Goal

- The CI workflow finishes in under five minutes, and the local race run
  (`go test -race ./...`) in under five minutes, with every test verifying what it verifies today.

## Decisions

- **The test helper seeds the world's store from the template; production code is unchanged.**
  `newVariantWorld` passes `WorldOptions.Dir` as a per-test temporary directory and writes the
  migrated template to the store path `NewWorld` opens. `NewWorld`'s `sqlite.Open` then finds
  every migration applied and checks their checksums, as it does in production. `testutil` gains
  a function that writes the template to a path, creating its directory and file with the modes
  `sqlite.Open` requires, and `OpenStore` uses it. A `WorldOptions` hook was considered and
  declined: it changes the production API for a test's sake.
- **A test fails if a world migrated anyway.** The seeding helper checks, after `NewWorld`, that
  the seeded file holds the principal `NewWorld` writes through its store and the same migration
  records as a fresh copy of the template, so neither a moved store path nor a skipped seed can
  silently bring the migrations back. Both are compared as stored, without reading the clock.
- **Worlds built inside `Run` and `RunProbes` keep migrating.** `Run` builds one world per case
  for its offered-tool check besides one per trial, and opens the profile store once per run.
  Migrating once per eval run is a production speed-up for
  `home-agent-eval run`, outside this plan.
- **The eval tests become parallel under plan 0151's rule.** Every test and subtest calls
  `t.Parallel()` first, before any world, server or temporary directory is built. Before marking a
  test, a search shows no `t.Setenv`, `os.Chdir`, package variables written by tests, files
  written under `testdata` or a shared output directory, golden-update flags, `slog.SetDefault`,
  default metric registries, `time.Local`, shared `httptest` servers, or fixed ports. A test that
  asserts wall-clock time, or shares a fixture across subtests that must run in order, stays serial
  with a one-line reason. A test that flakes under load moves to the serial set rather than having
  its assertion loosened.
- **The two load flakes outside eval are reported, not fixed here.** If
  `TestTurnWaitingBehindAnotherKeepsTyping` or `TestCancelledDeliveryLetsTheNextMessageThroughAtOnce`
  fails in a validation run, the output goes to the operator, who decides the fix.
- **Decided while implementing.**
  - The seeding check reads the seeded file through its own handle rather than the world's
    second handle, which builds the path separately and could still see the seeded file after
    the store had moved.
  - Chunk A2's 60-second bound for eval is dropped. The migrations still run inside `Run` and
    `RunProbes`, and in parallel they queue on the SQLite driver's process-wide allocator lock, so
    they, not the test order, bound the package. The goal is the whole suite's time, which the
    validation measures.
  - Every eval test is parallel; none needed to stay serial.
- **The CI split keeps the required check.** The race tests run in three parallel jobs: eval,
  Telegram, and `go list ./...` minus those two, so the last list is complete by construction.
  Eval and Telegram are the two slowest packages by far and both bound by CPU under the race
  detector, so each gets a runner of its own. The format, module, plan, vet and build checks run
  in a job of their own beside them. `setup-go`'s cache key is shared with the image workflow,
  which saves it without race builds, so the race jobs keep their own cache of builds and modules
  per shard, keyed on the module files. A final job named `validate` needs every
  other job, runs even when one fails, and passes only when all of them passed, so the ruleset
  needs no change and a skipped job cannot pass the required check.

## Approach

Each chunk is one commit and ends with the package passing with and without `-race`. Times are
read from the package `Elapsed` of `go test -count=1 -json` on an otherwise idle Mac, or, for CI,
from the CI workflow's total duration in `gh run view`, excluding the image and CodeQL workflows.

Wave 1:

- **A1. Seed eval worlds from the template.** Task: add a `testutil` function that writes the
  migrated template to a path, used by `OpenStore`; have `newVariantWorld` pass a per-test `Dir`,
  seed the store `NewWorld` opens, and check afterwards that the world did not migrate. Files:
  `internal/testutil/store.go`, `internal/eval/world_test.go`. Complexity: Complex (Jev `complex`
  at 0.60 for the whole of A, below the threshold; decided: `sqlite.Open`'s permission and
  migration checks need care). Depends on: nothing. Accepted when no file outside
  `internal/testutil` and `*_test.go` changes, the check fails if the seed is skipped, and the
  package passes under `-race`; the measured eval time is reported.

Wave 2:

- **A2. Parallel eval tests.** Task: mark the eval tests and their subtests parallel under the
  rule above, keeping any unsafe test serial with a one-line reason. Files: `internal/eval/*_test.go`.
  Complexity: Complex (as A1; the parallel-safety review of each test needs judgment). Depends on:
  A1, since both touch `world_test.go`. Accepted when the diff adds only `t.Parallel()` calls,
  serial-reason comments and per-test fixture changes that parallelism needs; the package passes
  `go test -race -cpu 2 -count 3 ./internal/eval` and `go test -count 10 ./internal/eval`; and
  the local race run takes under five minutes.

Wave 3, only if either of the first two `main` runs of the CI workflow after A2 takes five minutes
or more:

- **B. Split the CI race step.** Task: as decided above. Files: `.github/workflows/ci.yml`.
  Complexity: Orchestrator (Jev `complex` at 0.49, below the threshold; decided: small, and tuned
  against live CI timings). Depends on: A2. Accepted when every package is tested exactly once,
  the required `validate` check still reports, and three consecutive `main` runs of the CI
  workflow take under five minutes.

If the local race run still takes five minutes or more after A2, work stops there: the slowest
packages are reported to the operator with their measurements, and the next step is decided then.

## Files affected

- `internal/testutil/store.go`
- `internal/eval/*_test.go`
- `.github/workflows/ci.yml` (chunk B only)

## Validation

- The full suite passes with and without `-race`, locally and in CI.
- Flake run: `go test -race -cpu 2 -count 3 ./internal/eval` and
  `go test -count 10 ./internal/eval`, both green.
- The local race run, run twice on an otherwise idle Mac, under five minutes and green.
- Before and after wall time of the local race run, the CI workflow and the eval package,
  reported in chat.

## Risks and open questions

- **CPU-bound CI.** Parallel tests spread eval over the four cores but do not reduce its CPU; the
  budget above expects chunk B to be needed.
- **Sharding compiles three times.** Each shard compiles and links its race test binaries, and
  eval and Telegram each compile most of the dependency graph. Each shard's own race cache keeps
  this to a link after the first run on new module files; the measurement in B's acceptance
  includes it.
- **Cache space.** The repository's caches are shared with the image workflow's build layers
  under one size limit. Three race caches add to it, and the oldest entries are evicted first.
- **Runner variance.** One runner took 1.6 times as long as another on the same suite. B's
  acceptance asks for three consecutive runs under five minutes rather than one.
- **Wall-clock tests elsewhere under more load.** The local baseline run failed two tests that
  pass alone: `TestTurnWaitingBehindAnotherKeepsTyping` (fewer typing actions than it wants) and
  `TestCancelledDeliveryLetsTheNextMessageThroughAtOnce` (a context deadline during bootstrap).
  Parallel eval tests add load while they run; see the decision above.
- **A world's directory chosen by the test.** `NewWorld` removes only a directory it made itself;
  the test's temporary directory is removed by `t.TempDir`. A world that writes outside `state/`
  writes into the same directory as before.
