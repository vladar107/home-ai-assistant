# 0096 — Resume interrupted deliveries instead of sending them again

Status: `done`

## Context

A long chat reply, task report, or scheduled run's report is split into Telegram-sized chunks
and sent one by one. It is marked delivered only after the last chunk. A transient failure on a
later chunk, such as a 429 or a 5xx, leaves the delivery to be retried, and the retry starts from
the first chunk again. The Telegram client does not retry on its own, so the user sees every chunk
before the failed one twice. Chat replies, background task reports
([0048](0048-background-tasks.md)), and scheduled deliveries all work this way.

A restart can also send a whole reply again. On shutdown the services cancel their context. If
that happens after the last chunk is sent but before the delivered update, the update fails with
`context canceled`. The row stays ready, so the next process sends the whole reply, report, or
watch alert again. Approvals already avoid this: they store a finished action's outcome on a
context that ignores cancellation.

Plan 0048 called task report delivery at least once, like the scheduler's, and said a failure
partway through a long report sends the sent chunks again. Plan 0003 documented duplicate replies
or chunks as possible. Both are still true when Telegram's answer to a send is genuinely lost.
They are not true when the send is known to have succeeded.

## Goal

A delivery that is retried after a transient failure or a restart sends only the chunks the user
has not received. A reply, report, or alert whose last send succeeded is recorded as delivered even
when shutdown begins at that moment. A staged card is neither lost nor sent twice. Delivery stays
at least once only where the result of a send is unknown, because the request was cut off or its
response was lost.

## Decisions

- **Each delivery counts the chunks already in the chat.** Chat requests, tasks, and schedule runs
  get a `delivered_chunks` column. After each chunk is sent, its position in the full list of
  chunks is saved, and a retry continues from there. The count is over the full list of chunks.
  A card's caption that carries the first chunk counts as that chunk, so the count means the same
  whether or not a card went out.
- **The chunks must come out the same on every attempt.** They do, because a retry splits the same
  stored text. The text depends only on the stored reply and on fields that do not change between
  attempts, such as the subject line, or the resume line, which is built from a stored time. A job
  renamed between two attempts of one scheduled delivery changes its heading. The retry then skips
  the same number of chunks of the new text, which is acceptable for such a rare case.
- **A card goes first, so a delivery with chunks in the chat never sends it again.** When the count
  is above zero, the card step is skipped and the next chunk goes without a quote. When it is zero,
  the card step works as before: the outbox's sent mark already keeps a photo from being uploaded
  twice, and a captioned card still drops the first chunk. The card stays in the outbox until the
  delivery is recorded as done, then it is removed as before.
- **A scheduled run keeps the messages it was sent as.** Its report is saved with every message ID
  once the run is delivered, so a reply to any chunk finds the report. A resumed run would
  otherwise know only the chunks of its last attempt. The run also stores the messages of the
  chunks sent so far as a JSON array, `delivered_messages`, and saves them with the count.
- **Recording what was sent outlives shutdown.** Saving a chunk's progress, the delivered state,
  the chat reply's message ID, a scheduled run's report, and a watch alert's message all use
  `context.WithoutCancel` with a five-second timeout, as approvals do. A send that was already made
  is then recorded, and shutdown waits at most that long for it.
- **At least once remains the guarantee where a send's result is unknown.** When the request to
  Telegram is cut off, or its response is lost, the chunk may or may not be in the chat, and it is
  sent again. Exactly once cannot be had without Telegram's help, and losing a reply would be worse
  than repeating one chunk. The repeat is now one chunk at most, not the whole reply.
- **No new settings.** The timeout is a constant beside the existing delivery constants.

## Approach

1. Add migration `038_delivery_progress.sql` with `delivered_chunks` on `conversation_requests`,
   `tasks`, and `schedule_runs`, and `delivered_messages` on `schedule_runs`.
2. Load the count with each request, task, and run. Add store methods that save it while the
   delivery is still open: `delivering` for a chat request, finished and undelivered for a task,
   and `ready` for a scheduled run.
3. In each delivery loop, skip the delivered chunks and the card step when the count is above zero.
   Save the count after each chunk on a context that ignores cancellation, then mark the delivery
   done on one too.
4. The scheduler rereads a ready run before delivering it, so it uses that fresh row, with its
   progress, instead of the tick's snapshot.
5. Watch alerts are single messages, so only the delivered update and the kept message move to a
   context that ignores cancellation.

## Files affected

- `internal/store/sqlite/migrations/038_delivery_progress.sql`
- `internal/store/sqlite/conversation.go`, `tasks.go`, `schedules.go`
- `internal/conversation/types.go`, `service.go`
- `internal/task/task.go`, `service.go`
- `internal/schedule/schedule.go`, `service.go`
- `internal/watch/service.go`
- Integration tests for resumed and shutdown-interrupted deliveries.

## Validation

- A chat reply whose second chunk fails transiently is resumed with that chunk, and the first is
  not sent again, also across a restart.
- A reply whose last chunk is sent as shutdown begins is recorded as delivered and is not sent
  again by the next process.
- A task report and a scheduled report resume after a failed chunk without repeating earlier ones,
  and a scheduled report keeps the messages of every chunk.
- Card delivery keeps its existing tests: a photo is not sent twice, and a captioned card still
  carries the first chunk.
- `gofmt`, `go mod tidy -diff`, `go vet`, `go build`, `go test -race ./...`, and
  `scripts/check-plans`.

## Risks and open questions

- A delivery that loses a send's response still repeats that one chunk.
- If saving a chunk's progress fails, a retry sends that chunk again. The failure is logged.
- Shutdown can take up to five seconds longer while an update finishes.
