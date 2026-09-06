# 0006 — Quoted replies

Status: `done`

## Context

Replies arrive as plain messages. When a model turn is running and the user
sends commands or further messages in the meantime, later replies are not
visibly tied to their requests. The operator observed this on 2026-09-05 when
`/status` and `/privacy` replies landed before the model's answer to an
earlier question.

## Goal

Every bot reply visibly quotes the Telegram message it answers, using
Telegram's native reply feature, so a chat with interleaved requests stays
readable.

## Decisions

- Send each reply with reply parameters naming the request's message ID, and
  allow sending without the quote when the original message no longer
  exists, so a deleted request never becomes a permanent delivery failure.
- Persist the request's Telegram message ID with the durable conversation
  request so a reply delivered after a restart still quotes it. Add a
  migration for the column; the value is a non-secret identifier and is not
  cleared with the message text.
- Quote only the first chunk of a long reply; later chunks follow without a
  quote.
- Apply the same to command replies, the busy reply, and error replies, since
  those are the ones most likely to interleave with a running turn.

## Approach

1. Extend the durable request with the message ID and thread it from the
   Telegram update through enqueue to delivery, with the persistence test
   below.
2. Add reply parameters to the client's send call and use them for command
   and conversation replies, with the delivery tests below.

## Files affected

- `internal/store/sqlite/` and a migration
- `internal/conversation/` and `internal/telegram/`, including tests

## Testing

Integration suites over the loopback Telegram simulator, real SQLite, and
the controllable runtime, following plan 0003.

- Extend the simulator to record the reply parameters of every send. Assert
  that command replies, the busy reply, and a model reply each quote the
  message ID of the update that caused them, that a multi-chunk reply quotes
  only its first chunk, and that every send allows delivery without the
  quoted message.
- Restart with a ready but undelivered reply and assert that it is delivered
  after the restart still quoting the original message ID.
- Owns `internal/telegram/*_test.go`.

## Validation

- All suites pass under `go test ./...` and `go test -race ./...`, with the
  configured formatting, module, vet, build, and plan checks.
- Live: send a slow question, then `/status`, and confirm both replies quote
  their own request in Telegram.

## Implementation results

Recorded on 2026-09-06.

- Every reply from the Telegram service and the conversation coordinator
  carries `reply_parameters` naming the request's message ID with
  `allow_sending_without_reply` set, so a deleted request never blocks
  delivery. Command replies, the busy reply, the not-connected, quota,
  reconnect, timeout, and interrupted replies, and model replies all quote;
  only the first chunk of a long reply does.
- The request's Telegram message ID is persisted in the new
  `telegram_message_id` column (migration 005, nullable with a positive
  check) and survives the clearing of the message text, so a reply delivered
  after a restart still quotes its request. Rows created before the
  migration have no ID and deliver without a quote; verified by upgrading a
  version-004 database that held a ready request.
- An update whose message has no positive `message_id` is ignored with the
  `malformed_message` reason next to the existing malformed-sender and
  malformed-chat cases, so no reply is ever sent without a request to quote.
- Connection ceremony outcomes are notifications delivered later by the
  connection service and stay unquoted; the client keeps its plain send for
  them next to the new reply variant.
- The loopback simulator records the reply parameters of every send and
  fails any quote that would not survive a deleted request. Every reply
  assertion in the Telegram suites names the update it answers, and
  notification assertions require the absence of a quote.
- Automated checks passed: formatting, `go mod tidy -diff`, `go test ./...`,
  `go test -race ./...`, vet, build, `scripts/check-plans`, and whitespace.

### Live verification

Recorded on 2026-09-06 from the operator's GoLand run after restarting on
this build: a slow question followed by `/status` produced two replies, each
quoting its own request in Telegram; confirmed by the operator.

The operator accepted the plan as done on 2026-09-06.

## Risks and open questions

- Reply parameters exist since Bot API 7.0; the client targets the current
  API, so no compatibility shim is planned.
- Quoting every reply adds visual weight to short exchanges; revisit if it
  proves noisy for single-message turns.
