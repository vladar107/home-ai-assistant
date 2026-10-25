# 0117 — Diagnosable polling failures and one turn for a burst

Status: `done`

## Context

Telegram's `getUpdates` twice answered HTTP 200 with a body the client could
not decode, each time for about two and a half minutes. The poller retried with
backoff as designed. Nothing the user sent in that time reached the bot. When
polling recovered, the waiting messages for one tab arrived in a single batch.
The first message started a turn, and the bot answered each of the next three
with the busy reply. To the user, the bot looked stuck in that tab.

The client discards the decoding error, so the log cannot tell a malformed
Telegram response apart from an update shape the client does not model. The
same queued updates decoded cleanly once polling recovered, which points at
Telegram. That cannot be proven from what was logged.

## Goal

- The next decoding failure says what failed: a syntax error or a value of
  the wrong type, the field, the offset, the body size, and the content type.
  It never logs body text.
- Plain text messages that reach the bot together in one batch, for the same
  tab, become one turn and not a turn plus busy replies.

## Decisions

### Decoding errors

- **The error names the failure, never the body.** For a `json.SyntaxError`
  it gives the offset. For a `json.UnmarshalTypeError` it gives the field path,
  the JSON kind, the Go type, and the offset. The path, such as
  `result.0.message.text`, also shows which update failed. The kind keeps only
  its first word, because its value can quote a number from the body. Neither
  error's `Error()` text is used, because a syntax error quotes a character of
  the body. Any other error is named only by its Go type. Both types are
  matched with `errors.As`.
- **The body length and `Content-Type` are added**, so a truncated response
  or an HTML page stands out. The server controls the content type, so it is
  cut to a short length.

### Bursts

- **Merging, not queueing.** The store allows one pending request per lane.
  Queueing the extra messages would mean changing the store, while merging
  them fits the existing model.
- **Coalescing happens per batch, in the poller.** A batch holds several
  messages after an outage or a restart. It also does whenever messages arrive
  while the previous batch is still being processed, as during a voice
  transcription, a media download, or a slow reply. Merging therefore depends
  on timing even when polling is healthy. That is harmless and accepted.
  Messages that arrive in separate batches keep today's behaviour, including
  the busy reply for a message sent while a turn runs.
- **A run is built after the existing checks, one run at a time.** Updates at
  or below the committed cursor are skipped, and invalid update IDs are
  rejected, as today, before a run starts. Each run is built inside the loop
  against the committed cursor as it advances, not all at once beforehand.
  Inside a run, a duplicate update ID is skipped and the run continues.
- **What can join a run is an allow-list.** A message is mergeable only if
  all of these hold:
  - it is a new `message`, not an edit, a press, a reaction, or a draft stop;
  - it passes `validatePrivateMessage`;
  - its text is not blank, and it has no caption;
  - it has no photo, document, location, venue, voice, audio, video, video
    note, sticker, or animation;
  - it is not forwarded and is not a topic service message;
  - it is not a command and does not start with `/`;
  - no secret guard intercepts it.

  The guard check uses `guardSecrets`, which never consumes a pending intake.
  A guarded message stands alone: it neither starts nor joins a run. The Notion
  guard only recognizes a token sent as the whole message, so a token merged
  into other text would reach the model unguarded.
- **Every member of a run has the same sender, chat, and tab.** After the
  first message, a member must also have `repliedTo(message) == 0`. Every
  message in a tab replies to the message that created the tab, so testing
  `ReplyToMessage != nil` instead would turn merging off in tabs, where the
  incident happened.
- **The run stops before the joined text would pass
  `conversation.MaximumMessageRunes`.**
- **The merged message is a copy of the last message.** It carries the
  joined text and the first message's `ReplyToMessage`. Texts are joined with
  a blank line, in the order the user sent them. The merged update carries the
  last update's ID. The reply quotes the last message, and the cursor advances
  past the last update.
- **A retry does not grow a run that was already enqueued.** The store
  deduplicates on the update ID alone. Suppose the cursor advance fails after
  a merged run was enqueued, and the poller fetches the batch again. If newer
  messages then joined, the run would get a new key. Once the first turn had
  finished, it would run messages that were already answered a second time.
  The poller therefore remembers a cap. Whenever a merged update was processed
  without failure, whatever the outcome, and the cursor advance then failed,
  the cap is set to that update's ID. A run may not cross the cap: a member at
  or below it, followed by one above it, ends the run. The cap is a local in
  `run`, passed into and returned from `processBatch` like the committed
  cursor. Every later update is above it, so it never needs clearing and never
  splits a later run. The cap holds only while the poller keeps running. A
  restart, graceful or not, starts a new `run` without it. If the cursor
  advance failed and the bot restarts before a later advance succeeds, a
  regrown run can be answered again. That needs a failed write and a restart
  together, and carrying the cap across restarts would need a store query, so
  it is accepted. The request is then still pending,
  or it was ended with the interrupted reply, so a regrown run gets the busy
  reply or one re-run. It is never dropped silently.
- **Every absorbed update is still counted.** Each absorbed update ends its
  own update span with the merged update's outcome and reason `merged`, so
  the update metric keeps one count per Telegram update and an ignored run
  stays ignored. These spans start and end only
  after the merged update's cursor advance succeeds, so a retried run is not
  counted twice. The merged update ends as usual. The absorbed spans' durations
  are near zero and pull the update duration p95 down slightly, which is
  accepted. A log line records how many messages the turn merged.

## Approach

1. In `internal/telegram/client.go`, describe the decoding error in
   `decodeResponse`.
2. In `internal/telegram/service.go`, add the mergeable check and the merged
   message builder, and group runs in `processBatch`. Carry the cap through
   `run` and `processBatch`.
3. In `docs/architecture.md`, extend step 2 of "A conversation turn" with
   merging. Reword its promise never to replay an accepted message to match
   the retry decision above.
4. Add the burst integration test described under Validation.

## Files affected

- `internal/telegram/client.go`
- `internal/telegram/service.go`
- `docs/architecture.md`
- `internal/telegram/burst_integration_test.go`
- `docs/plans/README.md`

## Validation

- The configured format, module, vet, race test, build, and plan checks.
- The existing mixed-batch integration tests pass unchanged, in particular
  the ones with ignored, malformed, and duplicate updates in one batch.
- A review checks each burst decision above against the code.
- One integration test with the existing Telegram harness. A batch of several
  texts in a tab becomes one turn with the joined text. A command and a
  guarded token in the same batch each stay on their own.

## Risks and open questions

- If a Notion token intake is pending when a burst arrives, the whole merged
  text is consumed as one intake reply and refused as not a token. None of the
  messages reaches the model. Today the first is consumed and the others get
  busy replies, so this is no worse.
- When a session is resumed into a tab, only the merged turn's last message
  is forwarded. Today the messages refused as busy are not stored either.
- Albums and pairs of a forward and a comment are bursts too. They still get
  busy replies and are out of scope.
- Telegram's read ticks in private-chat topics are outside the bot's control.
  The Bot API has no read method outside business connections.
