# 0110 — Faster integration tests: shared migrated database, no race exit sleep, parallel Telegram tests

Status: `in-progress`

## Context

The full race suite takes about 25 minutes in CI, where the slowest test binary runs close to
its 30-minute `go test -timeout`, and about 20 minutes locally. `internal/telegram` alone takes about 1,000 seconds: 276 top-level tests, none parallel,
each building a full harness. Under the race detector the tests are CPU-bound, about 3.3 seconds
of CPU each. A CPU profile shows about 59 percent of that in `sqlite.Open` running the 42
migrations from scratch for every test. About ten tests also start the Pi fixture, a re-executed
race test binary, which sleeps a second at exit because of the race detector's default
`atexit_sleep_ms`; the same cost hits `internal/runtime/pi`. Other packages that migrate SQLite per
test are slow for the same reason (watch, store, conversation, schedule, task).

Nothing structural blocks parallel tests: no environment or directory changes, no fixed ports,
per-harness clocks and servers, and no mutated package state. A set of tests asserts wall-clock
behaviour (typing indicators, short timeouts and lifetimes) and would flake under contention.

## Goal

- The full race suite runs in a few minutes locally and in about four to six minutes in CI, with
  every test still verifying what it verifies today.

## Decisions

- **Migrate once, copy per test.** A test helper builds a template once per test process: it
  migrates a database in its own temporary directory through the real `sqlite.Open`, closes it,
  requires that no WAL or shared-memory file is left, keeps the bytes in memory, and removes the
  directory. Each test writes the bytes into its own private directory (0700, file 0600) and opens
  it with the real `sqlite.Open`, which still checks permissions, symlinks, and migration
  checksums. A failure to build the template is returned to every caller, never reported through
  the first test. Migrations seed no data, so a copy matches a fresh migration apart from its
  applied times.
- **Scope of the copy.** The Telegram harness and the slow packages that open a fresh store per
  test (watch, conversation, schedule, task). The store package keeps migrating from empty: its
  tests exercise the migrations, and they cannot import a helper that imports the store.
- **No exit sleep in the Pi fixture.** The fixture exits through `syscall.Exit` with the same exit
  codes, after flushing its output, so the race detector's one-second exit sleep is skipped.
  Passing `GORACE` through is not done, because the runtime's restricted child environment is a
  production boundary. The trade-off: a data race inside a fixture child no longer changes its
  exit status; the fixture is test code and its output is still read.
- **Parallel by default, serial where time is asserted.** Every Telegram test and subtest calls
  `t.Parallel()` as its first statement, before any harness, server, or temporary directory is
  built, so no deadline starts counting while it waits. The tests that assert wall-clock time stay
  serial, each with a one-line reason: typing indicators, short turn and task timeouts, Notion
  intake expiry, approval lifetimes and sweeps, the connection code timeout, and the known
  order-sensitive tests. A test that only has an upper bound (shutdown, process stop grace, the
  integration timeout) gets a wider bound if load makes it flake, not a serial slot. Parallel tests
  start only after every serial test finishes, so the serial set sets a floor of roughly a minute
  on the package.
- **No package split and no CI sharding** unless CI still exceeds about five minutes afterwards.

## Approach

1. Template database helper in `internal/testutil`, used by the Telegram harness; then by the other
   packages that open a fresh migrated store per test.
2. Remove the race exit sleep from Pi fixture children.
3. Mark the Telegram tests parallel, keeping the timing-sensitive set serial.
4. Measure locally and in CI; tune `-parallel` in CI only if needed.

## Files affected

- `internal/testutil/` (new template-database helper), `internal/testutil/pifixture/`
- `internal/telegram/*_test.go`, and the per-test store setup in `internal/watch`,
  `internal/conversation`, `internal/schedule`, `internal/task`
- `.github/workflows/ci.yml` only if `-parallel` needs setting

## Validation

- The full race suite passes three times in a row locally, and in CI.
- Before/after wall time for `internal/telegram` and the whole suite, measured with no other suite
  running, reported in chat.

## Risks and open questions

- Parallel load can expose latent timing assumptions beyond the known set; such tests move to the
  serial set with a reason rather than having their assertions loosened.
- CI runners have four cores shared across four packages at a time, so CI time is roughly the
  suite's race CPU divided by four; the template copy matters more there than parallelism. If CI
  stays above about five minutes, sharding is the follow-up.
