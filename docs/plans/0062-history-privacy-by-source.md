# 0062 — History search keeps privacy by source

Status: `done`

## Context

Plan 0061 flags `conversation_search` and `conversation_read` as `Private`, because an earlier
conversation may retell Notion page content or NAS file contents. Any call to either tool
therefore marks the current conversation private, and its replies stop reaching TypeSafe until
`/new`. In the first session after deployment, the second message led to a history search, and
from then on the intent check and the hint saw only the user's own messages again.

Each conversation already records whether it read such content, in `conversation_sessions`
`private_at`. The blanket flag is stricter than the promise needs. This plan replaces plan 0061's
decision to count conversation history among the always-private tools.

## Goal

- A history search or read marks the current conversation private only when it returns text from
  a conversation that is itself private.
- The privacy promise holds: Notion page contents and NAS file contents never reach TypeSafe, and
  no reply is sent from a conversation that has read them, until `/new`.

## Decisions

- **The result says whether it is private.** `tools.Result` gains `Private`. The broker treats a
  successful or error result as private when its definition is flagged or the result says so.
  The definition flag stays for tools whose results are always private.
- **The history tools decide per result.** `conversation.SessionRecord` gains `Private`, read from
  `private_at` in the session query. Read reports a private result when the conversation it reads
  is private. No matches, and refusals, are not private. The two tools lose their definition flag.
- **Search withholds private excerpts.** Every conversation from before plan 0061 is private, so
  nearly any search would match one and lock the current conversation. While the current
  conversation is not private, a match from a private conversation is listed with its id, date,
  and message number, and with a note in place of its excerpt that reading it keeps this
  conversation's replies from the safety checks. Only a `conversation_read` of it then marks the
  current conversation private. Once the current conversation is private, search shows every
  excerpt and reports a private result as before.
- **The current conversation counts like any other.** A match from the active conversation
  retells nothing new, and its privacy is already marked or not.
- **No migration.** Conversations from before plan 0061 are already marked private by migration
  020, so their history keeps protecting what it may hold.
- **Task reports stay private.** `task_result` keeps its definition flag, since tasks record
  nothing about what they read.

## Approach

1. **C1 History privacy by source** (`internal/tools/broker.go`, `internal/conversation/types.go`,
   `internal/store/sqlite/conversation_history.go`, `internal/conversation/history/tools.go`):
   - Add `Result.Private` and use it in the broker's private marking.
   - Add `SessionRecord.Private` as the last of the session columns and scan targets. The
     session query orders by column 4.
   - Withhold private excerpts in search as decided, set the result flag in read, and drop the
     definition flag.
   - Update the affected existing tests. Add sibling cases:
     - in `TestSuccessfulPrivateCallRecordsItsTurn`, a `Result{Private: true}` from an
       unflagged tool, on success and on an error result;
     - in the sessions integration test, `ConversationPrivate` after searching and reading a
       private earlier conversation, a conversation that is not private, and a search with no
       match.
   - *Accepted when:*
     - A read of a private conversation marks the current conversation private.
     - A search from a conversation that is not private shows no excerpt from a private one, and
       marks nothing.
     - A search or read of conversations that are not private, or a search that finds nothing,
       marks nothing.
     - `task_result` still marks it private.

| Chunk | Tier (Jev) | Depends on |
| --- | --- | --- |
| C1 | complex (my call: Jev 0.35 is below the bar; the excerpt rule needs judgment) | — |

## Files affected

- `internal/tools/broker.go`
- `internal/conversation/types.go`
- `internal/store/sqlite/conversation_history.go`
- `internal/conversation/history/tools.go`
- Existing tests that cover these paths

## Validation

- `gofmt`, the module check, `go vet`, `go test ./...` including `-race`, `go build`,
  `scripts/check-plans`.
- After deployment, in a new conversation, a history search leaves the replies recorded unless
  the model reads a private conversation.

## Risks and open questions

- The model sees fewer excerpts from older conversations, and needs one more call to read one.
  Reading it ends the safety checks' view of the replies until `/new`.
- A withheld match still shows that the searched words occur in a private conversation. The words
  are the user's or the model's own, so nothing private is revealed.
- A conversation becomes private only once it reads private content, so an earlier non-private
  conversation's own replies were already eligible for TypeSafe. Reading them again sends nothing
  that was not sendable before.
