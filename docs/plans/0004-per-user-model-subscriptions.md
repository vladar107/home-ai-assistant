# 0004 — Per-user model subscriptions

Status: `done`

## Context

The local conversation implementation in
[0003](0003-local-model-conversation.md) separates conversation sessions by
principal but creates one Pi runtime with the application's home directory.
All authorized users therefore reuse the same local Pi authentication profile.
Session isolation does not establish subscription ownership.

The product requirement is settled: each household user must connect and use
their own model subscription. This plan records that requirement and proposes
its implementation; the current application does not yet enforce it.

Verified against the pinned Pi 0.84.1 package on 2026-09-05:

- `PI_CODING_AGENT_DIR` selects the agent directory that holds `auth.json`,
  `settings.json`, model caches, trust state, and the debug log. No CLI flag
  sets it in any mode, and `--session-dir` redirects only session files.
- `auth.json` is a provider-keyed file written with mode 0600 under a lock
  file. For `openai-codex` it stores an OAuth record with access token,
  refresh token, expiry, and the ChatGPT account ID decoded from the token;
  no email or plan name. Pi refreshes tokens automatically and writes them
  back under the lock.
- Pi has no login command outside its interactive terminal UI. The
  `openai-codex` login is a PKCE flow with a browser callback on localhost
  port 1455, or a device-code flow against the OpenAI device endpoint with a
  fifteen-minute timeout.
- `pi auth check --provider openai-codex --json` reports credential status for
  the agent directory named in the environment.

## Goal

Resolve every model request to the authenticated principal's own subscription,
with independent connection, refresh, disconnection, and failure handling.
Never use the operator's or another household user's credentials as a fallback.

## Decisions

- Bind a model connection to an immutable principal ID before invoking Pi.
- Give each principal a private agent directory below the application data
  directory, for example `data/pi/<principal>/`, created with mode 0700 and
  passed to Pi as `PI_CODING_AGENT_DIR`, with that principal's session
  directory inside it. The name derives from the principal ID in core
  configuration, never from model output, Telegram text, or a user-supplied
  path.
- Do not rely on ambient authentication under the application's home
  directory. A principal without a provisioned `auth.json` receives connection
  guidance and no model invocation.
- Implement the device-code ceremony in the application: `/connect` requests
  a device code for `openai-codex`, replies with the verification address and
  user code, polls until approval or the fifteen-minute expiry, then writes the
  OAuth record into that principal's `auth.json` with mode 0600. Pending
  ceremonies live in memory keyed by principal, a new `/connect` replaces the
  previous one, and completion writes only to the initiating principal's
  directory. Tokens never appear in Telegram, logs, SQLite, or source control.
- After provisioning, Pi owns `auth.json`: it refreshes and rewrites tokens
  under its lock. The application reads only presence, expiry, and account ID.
- Store non-secret connection metadata in SQLite: principal, provider,
  ChatGPT account ID, and the connection timestamp. A unique constraint on
  provider and account ID rejects binding one ChatGPT account to two
  principals.
- `/connection` reports whether a connection exists, the last characters of
  the account ID, and expiry. `/disconnect` removes the auth file and the row,
  cancels the principal's active work, and prevents queued requests from
  reusing the credentials. Other users remain operational.
- Recheck connection ownership and enablement when queued work starts.
  Authentication failures during a turn produce the reconnect reply defined in
  [0005](0005-bot-commands-and-provider-failures.md).
- Treat different session IDs, separate processes, and filesystem permissions
  as separate controls. Profiles under one OS account do not constitute an OS
  sandbox against a compromised Pi process.
- Any future scheduled model invocation uses its owning principal's connection.
- Preserve deterministic operational commands when a user's model connection
  is unavailable.
- Do not copy the existing operator profile to household users. The operator
  connects through the same `/connect` ceremony as everyone else.
- Per-user auth files are secrets at rest. Exclude `data/pi/` from any copy
  that leaves the host, and document recovery as reconnecting rather than
  restoring tokens.

## Approach

1. Define the connection table and migration, the principal directory layout,
   and the missing-connection reply. Complexity: complex. Prerequisite for the
   rest.
2. After step 1, two parallel work areas. Complexity: complex.
   - Runtime: per-principal agent directory in the Pi adapter environment, a
     readiness check before each turn, and the authentication failure
     classification shared with plan 0005. Owns `internal/runtime/pi/`.
   - Connection lifecycle: device-code client, `/connect`, `/connection`,
     `/disconnect`, ceremony state with expiry, and integration tests against
     a loopback simulator of the device endpoint. Owns connection persistence
     and Telegram controls. Coordinate shared contracts before either changes
     the conversation coordinator.
3. Integrate dispatch, disconnect cancellation, and readiness reporting in the
   conversation coordinator and application wiring, with the lifecycle tests
   below. Complexity: complex.
4. Review and validate with two independently authenticated accounts, including
   refresh, revocation, restart, and concurrent requests. Record evidence before
   reporting multi-user subscription isolation complete.

## Files affected

- `internal/config/`, `internal/store/sqlite/`, and a migration
- `internal/runtime/pi/` and `internal/conversation/`
- a new connection package, `internal/telegram/`, and `internal/app/`
- `.env.example`, `.gitignore`, and deployment configuration as required by
  the per-principal data layout, plus the matching `README.md` configuration
  notes

## Testing

Integration suites over real components with loopback simulators, following
plan 0003; routine tests stay deterministic and credential-free.

### Connection ceremony

- Add a loopback simulator for the device endpoints: user-code request,
  polling that returns pending, approved, denied, or expired, and token
  exchange with a token whose account claim the test controls.
- Through the real Telegram simulator, identity service, connection package,
  and SQLite store, assert that `/connect` replies with the user code and
  verification address; approval writes `auth.json` with mode 0600 inside a
  0700 principal directory containing the OAuth record and stores the account
  ID row; denial and expiry reply accordingly and write nothing; a second
  `/connect` replaces the pending ceremony; a second principal approving the
  same account ID is rejected with nothing written; `/connection` reports
  presence, account suffix, and expiry; `/disconnect` removes the file and
  the row.
- Capture logger output and the simulator's sent messages and assert that the
  access and refresh tokens never appear there or in SQLite.
- Owns the connection package tests and `internal/telegram/*_test.go`.

### Runtime isolation

- Using the Pi fixture's environment report, assert that a turn runs with
  `PI_CODING_AGENT_DIR` set to the principal's directory with the session
  directory inside it, and that two principals run concurrently with distinct
  directories.
- Assert that a principal without `auth.json` receives the connection reply
  and no process starts, while another connected principal's turn proceeds.
- Owns `internal/runtime/pi/*_test.go` and `internal/telegram/*_test.go`.

### Lifecycle

- Disconnect during a running turn cancels it: the fixture process is gone,
  the request ends cancelled, and a queued request is not started with the
  removed credentials.
- Restart: connection rows and principal directories survive, a pending
  ceremony does not and a fresh `/connect` is required, and queued work
  resumes under its owning principal only.
- An authentication failure during a turn, using the fixture scenario from
  plan 0005, yields the reconnect reply and leaves the connection row for the
  user to inspect.
- Owns `internal/telegram/*_test.go`.

## Validation

- User A and User B invoke their distinct agent directories and cannot read,
  select, reset, disconnect, or refresh the other user's connection.
- An authorized user without a connection receives connection guidance, with
  no model invocation and no fallback to the application's home profile.
- A second principal cannot connect a ChatGPT account ID that is already bound.
- Expiry, revocation, refresh failure, or disconnect for one user does not
  prevent the other user's requests or deterministic commands.
- Restart preserves principal ownership, including queued requests and refreshed
  credential state; reconnect cannot resume a session under a different account.
- Verify profile selection and model-account identity without printing secrets
  or conversation content.
- Verify live the quota and reconnect replies introduced in
  [0005](0005-bot-commands-and-provider-failures.md), which were not
  exercised live there.
- All suites in the testing section pass under `go test ./...` and
  `go test -race ./...`, with the configured formatting, module, vet, build,
  and plan checks.
- Coordinate manual verification with the operator's GoLand process; do not
  start a second Telegram poller or restart that process automatically.

## Implementation results

Recorded on 2026-09-05.

- Each principal runs Pi with `PI_CODING_AGENT_DIR` set to its own directory
  under `data/pi/`, created with mode 0700 by the connection ceremony, with
  session files inside it. A principal without that directory, or without a
  refreshable credential and an account row, never starts a Pi process and
  receives the connect guidance instead.
- `/connect` runs the openai-codex device-code flow from the application:
  user-code request, polling with pending, slow-down, denial, and expiry
  handling, token exchange, account ID from the token, an account-uniqueness
  check, and an atomic 0600 write of Pi's `auth.json`. `/connection` and
  `/disconnect` are available to every authorized user; disconnect cancels
  that user's running turn and removes the credential and the row. Pending
  ceremonies are memory-only and do not survive a restart.
- Non-secret metadata lives in the new `model_connections` table with a unique
  provider and account constraint; tokens never appear in Telegram, logs, or
  SQLite.
- Found and fixed while wiring: with conversation disabled, the application
  passed a nil service pointer through the Telegram controller interface, so
  the first ordinary message would have panicked.
- Automated checks passed: formatting, `go mod tidy -diff`, `go test ./...`,
  `go test -race -count=2` for the Telegram, Pi, connection, and store
  packages, vet, build, `scripts/check-plans`, and whitespace. The suites run
  the real ceremony against a loopback simulator of the provider endpoints and
  the real Pi adapter against the Go fixture inside the principal directory.

### Live verification

Recorded on 2026-09-06 from the operator's GoLand run:

- `/connect` completed the device-code ceremony against the real provider
  endpoints for the operator's ChatGPT account, and conversation turns then
  ran through that principal's own Pi directory; confirmed by the operator
  alongside the live check of [0006](0006-quoted-replies.md).
- Not exercised live: a second household account, token refresh and
  revocation, a restart with a connected profile, and the quota and
  reconnect replies. All have automated coverage against the loopback
  provider simulator and the Go Pi fixture.

The operator accepted the plan as done on 2026-09-06.

## Risks and open questions

- The device-code flow depends on the OAuth client identity and endpoints
  embedded in Pi; a Pi or provider change breaks connection. Re-verify on
  every Pi upgrade.
- Pi refreshes tokens on its own schedule, so a revoked refresh token surfaces
  only at the next turn; `/connection` may report stale state until then.
- NAS custody of per-user auth files must be designed with the NAS packaging
  plan; do not infer NAS readiness from a local connection.
- Model availability must be checked for each connected subscription. A model
  listed in a client catalog does not establish access for a particular account.
- Photo input and model or effort selection are separate work from subscription
  ownership and are not implemented by this plan.
