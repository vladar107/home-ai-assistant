# 0095 — Telegram intake fixes: oversized voice turns, secrets in captions, and help

Status: `done`

## Context

An audit of the Telegram intake found three defects:

1. A voice or audio message with a caption can wedge the poller. A transcript may be up to 3800
   characters, and the turn's message adds the voice marker and the caption (up to 1024
   characters), so it can pass the conversation's 4096-character limit. `Enqueue` rejects it with a
   plain error, the poller treats that as a transient `enqueue_conversation` failure and retries the
   batch without advancing the cursor. Telegram redelivers the update forever, the recording is
   downloaded and transcribed on the user's own subscription each time, and every later update for
   every user is blocked behind it.
2. Message interceptors, among them the Notion token guard that keeps any token-looking message
   away from the model, run only for plain text. A photo whose caption is a token, or a voice note
   transcribed into one, reaches the model and is not deleted. The Google sign-in interceptor has
   the same gap for a caption carrying a sign-in address.
3. `/help` and the reply to an unknown command are a hard-coded list that omits `/jobs` and has
   drifted from the commands registered with Telegram.

## Goal

- A recording whose turn would not fit is answered once with a refusal, and the poller moves on.
- No input-validation failure from `Enqueue` can block the poller: it is answered and the update's
  cursor is committed.
- A caption or transcript that looks like a Notion token or a Google sign-in address is deleted and
  answered, and never reaches the model.
- `/help` lists exactly the commands the deployment registers, plus those answered only when typed.

## Decisions

- The transcript is budgeted against the composed message instead of a fixed rune cap: the
  marker, transcript and caption together must fit the conversation's message limit, which the
  conversation package now exports as `MaximumMessageRunes`. Without a caption the old
  too-long reply is used; with one, a separate reply says the caption made it too long and
  suggests sending the caption on its own.
- `Enqueue` wraps every input-validation failure in a new sentinel, `conversation.ErrInvalidRequest`.
  A stopped service and storage failures stay plain errors, since retrying them is right.
  `processUpdate` answers the sentinel with a refusal reply quoting the message, which commits the
  cursor like any other answered update.
- Interceptors gain a separate `guard` step that only protects secrets: it never consumes a pending
  intake and never completes a sign-in. Plain text keeps the full `intercept`. A message carrying
  media (supported or not) or a transcript runs only the guard, before anything is downloaded, so a
  photo cannot complete a pending Notion intake or Google sign-in. The Notion guard keeps its
  existing reply; the Google guard gets its own reply asking for the address to be pasted as text.
- The Notion token check matches a whole message, so a voice turn is guarded on the transcript and
  the caption separately rather than on the composed message with its marker. Transcription
  therefore returns the bare transcript, and the turn's message is composed after the guard.
- `/help` is built from `botCommands()` plus a shared list of commands answered only when typed
  (`/status`, `/privacy`), which the model's command help topic now uses too. The per-command
  help lines for `/memory`, `/tasks` and `/close` are dropped as the list covers them; the
  `/backlog export` hint remains as a separate line because the menu cannot name an argument.

## Approach

1. Export the message limit and add `ErrInvalidRequest` in `internal/conversation`, wrapping the
   validation errors in `Enqueue`.
2. Budget the voice turn in `internal/telegram/voice.go` and add the caption-specific reply.
3. Add the guard step to the interceptor interface (`settings.go`, `google.go`) and run it for
   media and transcripts in `processUpdate`; answer `ErrInvalidRequest` there.
4. Derive the help reply from the registered commands (`commands.go`, `service.go`, `help.go`).
5. Add integration tests for the voice budget, the validation refusal, the guard on captions and
   transcripts, and the help list.

## Files affected

- `internal/conversation/service.go`, `internal/conversation/types.go`
- `internal/telegram/voice.go`, `service.go`, `settings.go`, `google.go`, `commands.go`,
  `help.go`, `memory.go`, `tasks.go`, `tabs.go`, `backlog.go`
- Telegram integration tests

## Validation

- Formatting, module, vet, build and plan checks; race tests for the changed packages and the full
  suite.

## Risks and open questions

- A spoken token is deleted together with the voice note, which is the intended protection.
- A caption that happens to contain a local address with a `code` parameter is deleted, as the
  same text sent alone already is.
