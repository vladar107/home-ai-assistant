# 0175 — CI reruns only the tests a change can affect, through Go's test cache

Status: `in-progress`

## Context

Plan 0173 split the CI race tests into four jobs (eval, Telegram, connectors, rest), each with a
build cache of its own, and runs every test on every push with `-count=1`. A push takes about 3.5
to 5 minutes whatever it changes, including documentation-only pushes, which are about a sixth of
recent commits; about a tenth more change only tests in a leaf package.

Go's test cache already reruns only what a change can affect: a package's result is reused when
its test binary, the environment variables it read and the files and directories it opened are
unchanged. For an opened file it compares size, mode and modification time, not content; for an
opened directory, its own stat and the name and stat of every entry. Three things keep that from
working in CI today:

- **The cache is saved once.** The race caches are keyed on the module files, so a key is saved
  by its first run and never again until a dependency changes.
- **A fresh checkout gives every file and directory a new modification time.** So the result of
  any package whose tests open a file or read a directory is thrown away on every run. Eval and
  Telegram, the two slowest packages, both do. In two clones at the same path, with every file's
  and directory's time pinned to a value derived from its content, every package but one came back
  cached. The exception, `internal/locale`, walks the whole module from its root to check its
  message catalog, so the root directory is one of its inputs, and that changes with every commit.
  Every Go package is under `cmd/` and `internal/`.
- **One test reads a file only through a child process.** The vendored web-search extension's test
  runs Node on the extension's TypeScript source, which the Go process never opens, so a change to
  the extension would not invalidate the test's cached result. No other test runs a child process
  on repository files.

Recent push volume is about 8 to 17 pushes on an ordinary day and about 60 on a busy one. The
race caches are about 205 to 260 MB per shard, and the repository uses about 8.3 of its 10 GB of
cache, mostly the image build's layer cache and CodeQL's. Eviction is by last access, and the
image's layer blobs are not read again on a layer hit, so some have not been accessed for a week.

## Goal

- A push reruns only the race tests whose inputs it changed; a documentation-only push finishes
  the CI workflow in about two minutes.
- Every race test still runs at least once a day, and on demand, and the race caches stay small
  enough not to push the image or CodeQL caches out.

## Decisions

- **File and directory times come from their content.** After checkout, a script sets each
  tracked file's modification time from its blob id and each tracked directory's from its tree
  id, mapped into a fixed range of years in the past at nanosecond resolution, so a changed file or
  directory gets a new time and an unchanged one keeps it, whatever the checkout, branch, merge
  commit or rewritten history, and two versions of a file of the same size practically never share
  one. A symbolic link gets its time from its blob id without following it. It needs no history,
  so the checkout stays shallow.
  Commit times were considered and declined: a pull request's merge commit can give a file new
  content under an old commit's time, and a rebase can give two versions of a file the same
  second, and either could reuse a wrong result. The script is written for the CI runner.
- **Tests open what they depend on, and nothing more.** The extension's test reads the source
  file it hands to Node before running it, so Go records it as an input. The catalog test walks
  `cmd/` and `internal/` instead of the module root, which holds every Go package and keeps the
  root directory, changed by every commit, out of its inputs. Neither test's assertions change.
- **Caches are saved only when they hold something new, and the old ones are removed.** The race
  jobs restore the newest cache for their shard by the prefix `go-race-<os>-<shard>-`, the prefix
  today's caches already have, so those serve as the first warm start. On a push to `main` a job
  saves a new cache, under a key with the run id and attempt, only if at least one package passed
  without `(cached)`: the test output is copied to a file, the exit status is taken from `go test` itself, and those
  lines are counted, and a failed package adds nothing, since Go caches no failed result. Caches are saved
  only for `refs/heads/main`; pull requests and other branches restore from `main` and save
  nothing. A job after the race jobs, outside `validate`'s needs, which runs no repository code
  and alone has the `actions: write` permission, lists `main`'s caches by creation time with an
  explicit limit and deletes every shard's cache except its newest; it runs even when a race job
  failed and is skipped on pull requests.
- **The full run starts empty.** A nightly scheduled run and a manual run on `main` skip the
  restore, so every race test runs (without `-count=1`, which would keep results out of the
  cache), and save the result as the shard's newest cache, also when a test failed, but not when
  the run was cancelled or timed out. That resets each cache's size daily and leaves only results
  proven that night: a package that failed has no result in it, so the next push reruns it and
  goes red too.
- **Pushes and pull requests reuse results.** `-count=1` comes off the race jobs. "Re-run jobs" on
  a push run reuses results like the push did; a run that must execute everything is started
  manually.
- **What the cache cannot see is a known gap.** A test whose outcome depends on something it
  neither opens nor reads from the environment through Go (the wall clock, the network, process
  limits, files or variables a child process reads) may pass from cache after it would have
  started failing. The nightly run is the net for that, as the operator chose over selecting
  packages from the dependency graph.
- **The checks job, the `validate` job's rule and the image workflow are unchanged.** The image
  keeps its `go test ./...` before publishing.

## Approach

Wave 1:

- **A. Content-derived file times.** Task: a script under `scripts/` that sets tracked files',
  symbolic links' and directories' modification times from their blob and tree ids, as decided. Files:
  `scripts/pin-mtimes` (new). Complexity: Complex (Jev `complex` at 1.0). Depends on: nothing. Accepted when, in two
  fresh clones made one after the other at the same path, with the script run after each clone,
  `go test ./...` in the second clone reports every package `(cached)` except `internal/locale`
  (until chunk B), changing one byte of one file at the same size makes that file's package
  rerun, and adding or removing a file under a directory a test lists, and changing a file nested
  below it, make that test's package rerun.
- **B. Tests open what they depend on.** Task: the extension test and the catalog test, as
  decided. Files: `internal/runtime/pi/searchcheck_test.go`, `internal/locale/catalog_test.go`.
  Complexity: Simple (Jev `simple` at 0.67, below the threshold; decided: two small test changes
  with a clear rule). Depends on: nothing. Accepted when neither test's assertions change, both
  pass, a same-size edit to the extension's source makes the Pi package rerun instead of reporting
  `(cached)`, and with chunk A a change outside `cmd/` and `internal/` leaves the locale package
  `(cached)`.

Wave 2:

- **C. Cache reuse in the race jobs.** Task: in `.github/workflows/ci.yml`, run the script after
  checkout in the race jobs; split the race cache into a restore and a save step as decided; drop
  `-count=1`; add a nightly `schedule` at an off-peak minute and `workflow_dispatch`, both
  skipping the restore; and add the pruning job. Files: `.github/workflows/ci.yml`. Complexity:
  Complex (Jev `complex` at 0.85). Depends on: A and B. Accepted when `actionlint` is clean and,
  on `main`, after one warming push: a documentation-only push reports every race package
  `(cached)`, saves no race cache, and takes at most two minutes; a test-only change in `internal/routes` reruns that package alone; a change to a
  `testdata` file reruns its package; a production change in `internal/store/sqlite` reruns eval
  and Telegram; a manual full run reruns every package and leaves one cache per shard; and the
  first nightly run does the same. A full run that fails is covered by reading the workflow, not by
  making one fail.

## Files affected

- `scripts/pin-mtimes`
- `internal/runtime/pi/searchcheck_test.go`, `internal/locale/catalog_test.go`
- `.github/workflows/ci.yml`

## Validation

- `actionlint` and `scripts/check-plans`; the affected packages pass with and without `-race`.
- The two-clone check and the one-byte change locally.
- On `main`: the pushes and runs in chunk C's acceptance, read from the job logs (`(cached)` counts
  per shard), the workflow durations and the cache listing.
- The repository's cache usage and each shard's cache size after a day of pushes, with the image
  and CodeQL caches still present.
- That a failed nightly run notifies the operator: GitHub notifies the last person to change the
  schedule, subject to their notification settings.

## Risks and open questions

- **Most pushes still run eval and Telegram.** Their tests depend on almost every package, so a
  production change anywhere reruns both, and those pushes stay at about today's time.
- **A push that overlaps the nightly run.** A push that restored the cache from before the nightly
  run and saves after it becomes the shard's newest cache, so the size reset and the nightly's
  proof are skipped for that shard until the next night. Skipping such a save would need the race
  jobs to read the cache listing; it is accepted instead.
- **Cache space during bursts.** Each push can add up to about 1 GB before its pruning job runs,
  and the repository starts near its 10 GB limit, so two overlapping pushes could still evict an
  image layer blob. Each run's pruning job brings the race caches back to one per shard.
- **Cache growth within a day.** Each saved cache is the restored one plus what the push rebuilt;
  the nightly run resets it. A busy day could grow a shard's cache before the reset; its size is
  measured after the first day.
- **Scheduled runs and repository activity.** GitHub disables scheduled workflows after 60 days
  without activity in the repository.
- **The image workflow may already reuse results.** Its `go test ./...` runs without `-count=1` on
  `setup-go`'s cache, so packages that open no files may already pass from cache there. It is left
  as it is, as decided.
