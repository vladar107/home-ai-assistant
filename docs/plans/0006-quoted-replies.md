# 0006 — Quoted replies

Status: `proposed`

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

## Risks and open questions

- Reply parameters exist since Bot API 7.0; the client targets the current
  API, so no compatibility shim is planned.
- Quoting every reply adds visual weight to short exchanges; revisit if it
  proves noisy for single-message turns.
