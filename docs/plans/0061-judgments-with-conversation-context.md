# 0061 — Judgments with conversation context

Status: `done`

## Context

Plan 0029 added TypeSafe judgments. In the first days with safety checks on, the approval intent
check warned on four of the six actions the user approved, and on none of the actions the user
rejected. Every warning so far was a false alarm, which teaches the user to ignore it.

A synthetic replay against `jev-1.13.0` separated requested and unrequested actions cleanly in
English and in Russian, so language is not the cause. Two causes are:

- **The check sees only the user's side.** It reads the user's last two messages, never the
  assistant's question they answer. "Yes, add it" after the assistant offered something looks
  like agreeing to nothing. Adding the assistant's question moved such cases from 0.58–0.84 to
  0.80–0.98, while a "no thanks" stayed at 0.02.
- **Background runs raise the bar for chat.** The broker marks the principal exposed for 30
  minutes after any third-party tool succeeds, including in a scheduled job. The user's own hourly
  Gmail job therefore keeps the stricter 0.5 threshold active for half of every hour. Two warned
  watch changes (0.41 and 0.42) came 30 seconds after such a run.

The tool hint has the same blind spot. On standalone requests it chose correctly 23 of 24 times
in both languages, and a two-level choice did no better. In production, though, it answered
"none" at 0.99–1.00 on follow-ups that led to schedule and watch proposals, because it sees only
the latest message. It also runs on the automatic continuation turn after Confirm, whose message
is ours.

## Goal

- A user who agrees to an action the assistant offered gets an unwarned card. An action the user
  did not ask for, or declined, is still warned.
- Third-party content read by a scheduled job or background task no longer makes the intent check
  stricter for the user's chat, unless screening flagged it as an injection.
- The tool hint judges a follow-up in the context of the previous exchange. It is not requested on
  continuation turns, and its misses become measurable.

## Decisions

- **The registry keeps the assistant's latest chat reply** next to the user's last two messages,
  within the same 30-minute window. It is rendered oldest first as `user:` and `assistant:` lines.
- **The reply is recorded after delivery and its follow-up.** It is recorded at the end of
  `deliver`, after `AfterDelivery` has run the intent check. So the reply of the turn whose action
  is being judged is never sent, and it cannot vouch for its own proposal. The next turn's hint,
  which runs before that turn's message is recorded, sees the question its message answers. A
  continuation turn's proposals are judged with the reply that preceded them.
- **Only a plain, successful chat reply is recorded.** Error, timeout, scheduled, and task replies
  are not, and neither is `NothingToAdd`.
- **A reply after a Notion or NAS read is not recorded** (decided 2026-10-09). The settings promise
  that Notion page and NAS file contents never reach TypeSafe holds, and the assistant keeps
  reading them as before. A new `Private` flag on `tools.Definition` marks tools whose result
  carries that content, or can replay it:
  - the Notion read tools and the movie table tools;
  - the Notion write tools, whose errors list a table's property names and options;
  - the NAS file reads;
  - conversation history;
  - background task reports.

  A Pi session keeps its history across turns, so a later reply can quote what an earlier turn
  read. The mark therefore belongs to the conversation, like outside-content exposure. When a
  private call succeeds or returns an error text in a chat turn, the broker has the exposure service set a new
  `private_at` column on the session. The conversation service records no reply from a private
  session until `/new`, and a failed lookup counts as private. The migration marks existing
  sessions private, since their history is unknown.
- **Background reports carry the mark.** A scheduled run or background task may read Notion or
  NAS content, and its report reaches the next chat turn as a notice. The run record keeps a
  private mark next to its external one. When the chat turn takes a private report, it marks
  its conversation private before the model runs. The mark lives in memory only.
  - A scheduled run reloaded from the store after a failed delivery or a restart counts as
    private, unless it is a reminder.
  - Every background task report counts as private. Tasks keep no record of what their runs read,
    and they are rare and started by the user.
  - An approval outcome from an executor that declares its outcomes private makes its notice
    private. The Notion executor does, because a rejected write quotes the table's property names
    and options. The recorded text is the model's response without the
  memory notes. It is scrubbed first and then cut to its last 600 runes, so a cut never splits a
  path or token that the scrubber would have matched. The question the user answers is almost
  always at the end.
- **Only the intent check and the hint read the exchange.** The web search check keeps the
  user-only messages. It guards a conversation that has read outside content, and there the
  assistant's own text is what an injection steers.
- **The intent question names the roles.** Assistant lines are context only. The user must have
  asked for the action, or agreed to the assistant's offer of it.
- **Exposure marks only chat turns.** The broker calls `MarkExposed` only for a chat turn key, the
  same `outbox.ChatRequest` test the per-conversation exposure service applies.
- **Suspicion marks stay principal-wide.** A scheduled run whose content screening flagged as an
  injection still makes every approval warn for 30 minutes. It is rare, and its alerts reach the
  same user, who may act on them.
- **The hint gets the earlier exchange as a separate state field.** `message` stays the turn's
  message, and `earlier` holds the exchange recorded before this turn. The flat choice over all
  tools stays: the two-level variant was no more accurate and doubled the latency.
- **A missed hint is counted.** When the hint was asked and answered `none` or fell below the
  confidence threshold, and the model then called a tool, `home_agent.hint.follow_through`
  records `missed`. A hint that was not asked, failed, or timed out records nothing, so
  `startToolHint` reports "answered, no hint" apart from "no answer". With `followed`, `other_tool`, and
  `no_tool`, this tells whether the hint earns its place. Removing it is a later decision based on
  that data.
- **The opt-in text says assistant replies are sent.** The settings description and the
  `/privacy` line add the assistant's replies to what is scrubbed and sent. They also say that a
  reply given after reading Notion or NAS content is not sent, so the existing promise stands.

## Approach

1. **C2 Chat-only exposure and private conversations** (`internal/tools`, `internal/exposure`,
   `internal/store/sqlite` with a new migration, and the tool definitions of the private tools):
   - Guard the `MarkExposed` call with `outbox.ChatRequest(e.turn)`.
   - Add the `Private` definition flag and set it on the private tools.
   - Have the broker mark a chat session private after a successful private call. Copy
     `MarkTurnExposed`, `MarkConversationExposed`, `ConversationExternal`, and migration 017.
   - Add a lookup the conversation service can ask by request ID.
   - Update the affected existing tests.
   - *Accepted when:*
     - A successful third-party call marks the principal exposed under a chat turn key, and not
       under a scheduled or task turn key.
     - A successful private call in a chat turn marks its session private. A failed call, or one
       in a scheduled or task turn, does not.
     - `/new` starts a session that is not private.
2. **C1 Exchange and intent check** (`internal/turnstate`, `internal/conversation`,
   `internal/judgment/intent.go`, `internal/app`):
   - Add reply recording and an exchange method to the registry.
   - Widen the conversation service's `Recent` dependency to record messages, record replies,
     and read the exchange, so C3 needs no new wiring.
   - Keep the plain reply with the turn until delivery. Record it at the end of `deliver` unless
     the session is private.
   - Switch the intent check to the exchange and the new question.
   - Update the affected existing tests.
   - *Accepted when:* against the fake TypeSafe server, an intent request after an assistant
     question carries `user:` and `assistant:` lines. It never carries the current turn's reply.
     Error replies, `NothingToAdd`, memory notes, and the reply of a turn that read Notion or NAS
     content are absent. The search check's request is unchanged.
3. **C3 Hint context** (`internal/judgment/hint.go`, `internal/conversation`): after C1.
   - Skip `startToolHint` for `ContinueMessage`.
   - Pass the exchange as `earlier`, through the `ToolHinter` interface, and mention it in the
     question.
   - Record `missed` in `recordTurnMetrics`, as defined above.
   - Update the affected tests, including the fake hinter's signature.
   - *Accepted when:* a continuation turn makes no hint request, and a hint request carries
     `earlier` with the previous turn's reply. `missed` is recorded only for an answered hint
     with no suggestion followed by a tool call.
4. **C4 Wording and docs** (orchestrator):
   - Update the settings and privacy texts in `internal/telegram/typesafe.go`.
   - Update what `docs/architecture.md` says is sent to TypeSafe.

Waves: C2 first, then C1, then C3 and C4.

| Chunk | Tier (Jev) | Depends on |
| --- | --- | --- |
| C2 | complex (my call: Jev's 0.61 "simple" was for a narrower version; this one adds a migration and a store method) | — |
| C1 | complex (0.77) | C2 |
| C3 | complex (0.94) | C1 |
| C4 | orchestrator | C1, C3 |

## Files affected

- `internal/turnstate/turnstate.go`
- `internal/conversation/service.go`, `internal/conversation/types.go`
- `internal/judgment/intent.go`, `internal/judgment/hint.go`
- `internal/tools/broker.go`, `internal/exposure/exposure.go`
- `internal/store/sqlite/memory.go`, a new `internal/store/sqlite/migrations/020_private_exposure.sql`
- The private tools' definitions (Notion, NAS, movies, conversation history, background tasks)
- `internal/app/app.go`
- `internal/telegram/typesafe.go`
- `docs/architecture.md`
- Existing tests that cover these paths

## Validation

- `gofmt`, the module check, `go vet`, `go test ./...`, `go build`, `scripts/check-plans`.
- Existing integration tests against the fake TypeSafe server still pass. Their assertions are
  updated where the request now carries the exchange.
- A live synthetic replay through the real checker and hinter code, with no household data, run
  explicitly with the key and deleted afterwards. It covers:
  - agreeing to an assistant offer, and declining one;
  - a wrong target;
  - a follow-up after the assistant's question;
  - a request in English and in Russian.

  Pass bar: no requested action is warned, and every unrequested or declined one is.
- After deployment, compare `home_agent.judgment.decisions` for intent (warned against clean),
  and `home_agent.hint.follow_through` including `missed`, through Grafana.

## Risks and open questions

- Existing conversations are private after the migration, so the exchange only takes effect
  after each user's next `/new`.
- An offer made right after a Notion or NAS read, such as "add this to the page?", is judged
  without that reply, so a plain "yes" to it may still be warned.
- An assistant reply from an earlier turn that read outside content can be steered into offering
  an action, and a plain "yes" then passes the check. The user still sees the card, and the
  exposed threshold applies when the conversation itself read outside content. A suspicious mark
  always warns.
- Two production warnings (0.05 and 0.10) are not explained by either cause. Without message
  content in the logs they may persist; the post-deployment metrics will show it.
- Assistant replies may contain names and other free text that the scrubber cannot remove. The
  opt-in text says so before the user turns the checks on.
