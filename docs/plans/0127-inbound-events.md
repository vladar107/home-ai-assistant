# 0127 — Inbound events from the user's own systems

Status: `proposed`

## Context

The assistant learns about the outside world only by asking: polling watches,
scheduled jobs, and tool calls. Systems the user runs themselves cannot tell
it anything. Examples are a Home Assistant automation, a job on a self-hosted
runner, a backup script, and a NAS task.

Grafana alerts (plan 0069) are read on request, not pushed.

The service has one HTTP listener with health checks and the backlog API
(plan 0076). The backlog API has no authentication: reachability on the
private network is its boundary. That is acceptable for development tooling,
but not for an endpoint that makes the assistant message a user.

## Goal

- A user can create an **event source** in `/settings`, such as "Backups" or
  "Home automations". They get a token, shown once, to use from their own
  scripts.
- A `POST /api/events` with that token reaches only that user. Depending on
  how the source is set up, it is either:
  - forwarded as is to an Events feed tab; or
  - assessed by one model turn, which decides whether and how to tell the
    user, using their memory.
- Events can be referred to in conversation: "what did the backup say last
  night?"
- Sources can be paused, have their token rotated, and be deleted.

## Decisions

- **Per-source bearer tokens, stored hashed.** A new migration adds:
  - `event_sources`: principal, ID, name, mode (`forward` or `assess`), the
    SHA-256 of the token, enabled, and created, last-used, and rotated times.
    There are at most 10 per principal.
  - `inbound_events`: principal, source, ID, received time, title, text, severity,
    `dedupe_key`, and outcome (`delivered`, `suppressed`, `failed`,
    `dropped`). These are kept 30 days.

  The token is 32 random bytes in base64url, prefixed `hae_`. It is shown
  once, in a message that the bot deletes after 10 minutes. The pending
  deletion (chat, message ID, due time) is stored, so a restart still
  deletes the message. The comparison uses constant time on the hash. The
  backlog API is unchanged.
- **Endpoint.** `POST /api/events` on the existing listener, with
  `Authorization: Bearer <token>`. The JSON body is at most 16 KiB:
  - `title`: 1–200 runes;
  - `text`: up to 4000 runes;
  - `severity`: `info`, `warning`, or `critical`;
  - optional `dedupe_key`: 1–100 runes.

  The body is read through `http.MaxBytesReader`, with a 10-second
  per-request read deadline set through `http.ResponseController`. The
  server today sets only `ReadHeaderTimeout`.

  Responses:
  - `202`: accepted.
  - `400`: the body is invalid.
  - `401`: the token is missing or wrong, or its principal is no longer
    allowlisted or is disabled. That is checked on every request.
  - `403`: the source is paused. Nothing is recorded.
  - `429`: the source is over its rate limit. A source accepts at most 60
  events per hour, and above that returns `429` and records nothing. The
  same `dedupe_key` within one hour is counted but not delivered again.
  Requests are logged with the source ID only, never the token or the body.
- **Reachability is unchanged.** The endpoint is served wherever the
  listener already is: behind the home edge proxy on the private network.
  Exposing it to the internet is out of scope. If that is ever done, the
  token is the boundary. The operations guide states which proxy path must
  forward `/api/events`, without naming hosts.
- **Two modes.**
  - `forward` sends the title and text in code to the new `events` feed tab,
    with a source header and severity marker. It needs no model turn. Event
    text is escaped for the message's formatting mode, so it can never add
    links or formatting of its own.
  - `assess` runs one unwatched model turn on the owner's subscription,
    without tools but with the user's memory. No existing entry point fits:
    `RunCheck` offers read tools, and `RunReview` has no memory. So the turn
    engine gains `RunAssess`, which shares their setup. The turn gets the
    event and returns `{"notify": bool, "message": "..."}`. A `critical` event is always delivered. A failed
    assessment delivers the event as `forward` would.
- **Event text is outside content.** It comes from a script, but whatever
  produced it may contain anything. Delivered events reach the next turn as
  outside notices, as agent watch alerts do. The `events_recent` read tool
  is `ThirdParty`. A conversation that refers to an event is therefore
  exposed, and writes go to the card.
- **Telemetry.** Events are counted by mode and outcome, without source names
  in labels.

## Approach

1. Migration, store, and the `internal/events` package: sources, token
   minting and verification, rate limiting, dedupe, and pruning.
2. The HTTP handler in `internal/app/http.go`, with integration tests against
   a real listener.
3. Delivery: the `events` feed, the forward rendering, the `assess` turn, and
   outside-notice handoff.
4. The `/settings` page for sources: create, show the token once, rotate,
   pause, and delete. Add an `events_recent` read tool.
5. README, `docs/architecture.md`, and `docs/operations.md`, with a neutral
   `curl` example.

## Files affected

- `internal/store/sqlite/migrations/NNN_event_sources.sql`, a new store file
- `internal/events` (new), `internal/app/http.go`
- `internal/thread`, `internal/telegram` (settings page, feed),
  `internal/turn` and `internal/conversation` (`RunAssess`)
- `internal/wiring`, README, docs

## Validation

- HTTP integration tests with a temporary SQLite store:
  - a missing, wrong, or revoked token gets `401`, and nothing is recorded;
  - a valid event in `forward` mode reaches the owner's Events feed and no one
    else;
  - an oversized body gets `400`, and a body that trickles past the read
    deadline is cut off;
  - the 61st event in an hour gets `429`;
  - a duplicate `dedupe_key` is not delivered twice;
  - `assess` with the fake model suppresses an `info` event and always
    delivers a `critical` one;
  - a failed assessment falls back to forwarding;
  - a paused source gets `403` and records nothing;
  - a token whose principal was revoked gets `401`;
  - formatting characters in event text arrive escaped;
  - the token never appears in logs;
  - a token message whose deletion was due during a restart is deleted
    after start.
- A live check from the GoLand server: post a test event from the Mac with
  `/usr/bin/curl`.
- Formatting, module, test, vet, build, and plan checks.

## Risks and open questions

- A leaked token lets someone on the network message that user. Mitigations:
  per-source rotation, rate limits, and the listener staying private.
- `assess` costs a model turn per event. The rate limit bounds it, and
  `forward` is the default.
- Plan 0129 should export source names and modes, never token hashes. A
  restored source has no token until the user rotates it.
