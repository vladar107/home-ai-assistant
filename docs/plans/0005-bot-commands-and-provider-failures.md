# 0005 — Bot-registered commands and actionable provider failures

Status: `proposed`

## Context

The bot's command menu is maintained by hand in BotFather even though the
application already owns the list of commands it handles. Separately, the
live run recorded in [0003](0003-local-model-conversation.md) showed that when
the ChatGPT subscription reaches its usage limit, Pi retries internally and the
user either receives the generic unavailable reply or waits for the two-minute
turn timeout. Plan 0003 also closed with several live checks not exercised.

## Goal

Publish the command menu from the application itself, reply within seconds
with an actionable message when the model subscription is out of quota or not
authenticated, and record the live checks carried over from plan 0003.

## Decisions

- Keep the command list in code next to the handlers as the single source of
  truth and register it with `setMyCommands` once per start, after `getMe`.
  Registration failure is logged and does not stop polling; commands keep
  working without a menu.
- Register the default scope so the BotFather list is superseded. Reserve
  per-chat scopes for user-scoped integration commands in a later plan.
- Classify provider failures in the Pi adapter from the RPC `message_end`
  event whose assistant message has `stopReason` `error`, and from
  `auto_retry_start`, which carries the same error text before Pi waits.
  Quota and authentication categories send `abort_retry` and end the turn
  immediately; other categories keep Pi's own retry within the turn timeout.
- Match categories on stable substrings of the provider message and never log
  or persist the raw provider text.
- Keep the stored error kind within the existing schema; persist only the
  actionable reply text and report finer categories through telemetry.
- Replies stay content-free and name the next action: try again later for
  quota, reconnect the subscription for authentication. Plan
  [0004](0004-per-user-model-subscriptions.md) later makes both per user.

## Approach

1. Add the `setMyCommands` client call, derive the payload from the handled
   command list, and register at Telegram service start, with the command
   registration tests below.
2. Extend the Pi adapter with failure classification and `abort_retry`, map
   categories to replies in the conversation coordinator and to telemetry
   attributes, with the process-boundary and conversation-path tests below.
3. Run the live checks in the validation section and record the results.

## Files affected

- `internal/telegram/` client, commands, service, and integration tests
- `internal/runtime/pi/` and `internal/testutil/pifixture/`
- `internal/conversation/` and `internal/observability/`

## Testing

Integration suites over real components, following the strategy set in plan
0003: real temporary SQLite databases, the loopback Telegram simulator, and
the Go Pi fixture as a subprocess. Add no unit tests unless an isolated rule
cannot be reached through those boundaries.

### Command registration

- Extend the loopback Telegram simulator with `setMyCommands` and record every
  call. Through the real client, identity service, and Telegram service,
  assert that startup registers exactly once in the default scope with the
  handled command names and non-empty descriptions, and that later polling
  does not register again.
- Fail the registration call with a server error and assert that polling
  starts, `/status` still replies, and the failure is logged without stopping
  the service.
- Owns `internal/telegram/*_test.go`.

### Provider failures at the process boundary

- Extend the Go Pi fixture with scenarios that emit `message_end` whose
  assistant message has `stopReason` `error` with a usage-limit text, an
  authentication text, or an unrelated text, followed by `agent_settled`; and
  scenarios that emit `auto_retry_start` with the same three texts and then
  wait for an `abort_retry` command, recording in the fixture report whether
  it arrived before exit.
- Through the real adapter, assert the error category for quota and
  authentication, that the turn ends within a few seconds rather than at the
  turn timeout, that `abort_retry` was sent when Pi was retrying, that the
  process and its working directory are gone, and that no provider text
  appears in the returned error. Assert that an unrelated retried error is
  left to Pi and a later success still completes the turn.
- Owns `internal/runtime/pi/*_test.go` and `internal/testutil/pifixture/`.

### Conversation path

- With the fixture runtime wired through the existing constructors, send a
  message that triggers the quota scenario and assert that the quota reply
  reaches the chat, the request finishes with no stored provider text, the
  telemetry hook receives the quota category, and the next message runs
  normally. Repeat for authentication with the reconnect reply.
- Owns `internal/telegram/*_test.go`.

## Validation

- All suites above pass under `go test ./...` and `go test -race ./...`, with
  the configured formatting, module, vet, build, and plan checks.
- Live checks with the operator's GoLand process once the subscription quota
  has reset: the command menu visible in Telegram, `/cancel`, the busy reply,
  an unauthorized user with conversation enabled, a restart during a running
  turn, a quota failure reply, and `gpt-5.6-luna:low`. Record the results here
  before marking the plan done.

## Risks and open questions

- Verified on the pinned Pi 0.84.1: `message_end` carries `stopReason` and
  `errorMessage`; `auto_retry_start` carries `attempt`, `maxAttempts`,
  `delayMs`, and `errorMessage`. Pi's own non-retryable list does not include
  the observed usage-limit wording, so the adapter must classify that text.
- Telegram rate-limits `setMyCommands`; register once per start only.
- Provider error wording can change between Pi or provider releases; the
  fallback for an unrecognized category must remain the generic reply.
