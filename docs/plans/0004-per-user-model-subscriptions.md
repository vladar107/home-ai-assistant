# 0004 — Per-user model subscriptions

Status: `proposed`

## Context

The local conversation implementation in
[0003](0003-local-model-conversation.md) separates conversation sessions by
principal but creates one Pi runtime with the application's home directory.
All authorized users therefore reuse the same local Pi authentication profile.
Session isolation does not establish subscription ownership.

The product requirement is settled: each household user must connect and use
their own model subscription. This plan records that requirement and proposes
its implementation; the current application does not yet enforce it.

## Goal

Resolve every model request to the authenticated principal's own subscription,
with independent connection, refresh, disconnection, and failure handling.
Never use the operator's or another household user's credentials as a fallback.

## Decisions

- Bind a model connection to an immutable principal ID before invoking Pi.
- Use a distinct protected Pi authentication profile and session directory per
  principal. Select profiles through core-owned configuration, never model
  output, Telegram text, or a user-supplied filesystem path.
- Pass an explicit principal-specific Pi configuration directory; do not rely
  on ambient authentication under the application's shared home directory.
- Let every user connect, inspect, and disconnect their own model connection.
  Missing, expired, or revoked credentials produce an actionable response for
  that user without invoking a different subscription.
- Bind the connection ceremony to its initiating principal, with bounded expiry
  and replay protection. Tokens remain outside Telegram, application logs,
  SQLite preferences, and source control.
- Keep credentials and refresh state within that principal's protected runtime
  profile. Follow the existing local-secret and NAS Keychain custody rules;
  document how mutable refreshed credentials are persisted and recovered.
- Treat different session IDs, separate processes, and filesystem permissions
  as separate controls. Profiles under one OS account do not constitute an OS
  sandbox against a compromised Pi process.
- Recheck connection ownership and enablement when queued work starts. A
  disconnect cancels that principal's active work and prevents queued requests
  from reusing disconnected credentials; other users remain operational.
- Any future scheduled model invocation uses its owning principal's connection.
- Preserve deterministic operational commands when a user's model connection
  is unavailable.
- Do not automatically copy the existing operator profile to household users.
  Explicitly associate or reconnect it only for its verified owner.

## Approach

1. Define the principal-to-connection mapping, protected profile layout, and
   missing-connection behavior in configuration and persistence. Complexity:
   complex. This is the prerequisite for runtime routing and connection flows.
2. After the mapping is agreed, implement two parallel work areas: runtime
   profile selection and per-user connection lifecycle. Complexity: complex.
   Give the runtime work ownership of `internal/runtime/pi/` and the connection
   work ownership of connection persistence and Telegram controls; coordinate
   shared contracts before either changes the conversation coordinator.
3. Integrate dispatch, disconnect cancellation, and readiness reporting in the
   conversation coordinator and application wiring. Complexity: complex.
   Depends on both preceding work areas.
4. Update architecture, security, integrations, roadmap, and operator guidance
   to describe the implemented boundary. Complexity: simple. Preserve completed
   plans and historical experiment results.
5. Review and validate with two independently authenticated accounts, including
   refresh, revocation, restart, and concurrent requests. Record evidence before
   reporting multi-user subscription isolation complete.

## Files affected

- `internal/config/`, `internal/store/sqlite/`, and a migration if needed
- `internal/runtime/pi/` and `internal/conversation/`
- `internal/telegram/` and `internal/app/`
- `.env.example` and deployment configuration as required by profile delivery
- `README.md`, `docs/03-system-architecture.md`, `docs/04-integrations.md`,
  `docs/05-security-and-privacy.md`, `docs/10-delivery-roadmap.md`,
  `docs/11-risks-and-open-questions.md`, and `docs/13-operator-runbook.md`

## Validation

- User A and User B invoke their distinct authenticated profiles and cannot
  read, select, reset, disconnect, or refresh the other user's connection.
- An authorized user without a connection receives connection guidance, with
  no model invocation and no fallback to the application's home profile.
- Expiry, revocation, refresh failure, or disconnect for one user does not
  prevent the other user's requests or deterministic commands.
- Restart preserves principal ownership, including queued requests and refreshed
  credential state; reconnect cannot resume a session under a different account.
- Verify profile selection and model-account identity without printing secrets
  or conversation content.
- Run configured formatting, module, test, vet, build, and plan checks. Add no
  new test files unless explicitly requested.
- Coordinate manual verification with the operator's GoLand process; do not
  start a second Telegram poller or restart that process automatically.

## Risks and open questions

- Verify the pinned Pi version's profile override, independent refresh, and
  supported account-identity metadata before finalizing storage interfaces.
- Decide the concrete supported connection ceremony and local-to-NAS credential
  provisioning procedure. Do not infer NAS readiness from a local login.
- Decide how to detect and reject accidentally binding one authenticated model
  account to two household principals using non-secret account metadata.
- Model availability must be checked for each connected subscription. A model
  listed in a client catalog does not establish access for a particular account.
- Photo input and model/effort selection are separate work from subscription
  ownership and are not implemented by this plan.
