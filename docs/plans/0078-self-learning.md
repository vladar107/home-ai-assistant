# 0078 — Self-learning from feedback and outcomes

Status: `done`

## Context

The assistant changes its behaviour only when the user says so in the moment:
memory keeps facts at once and instructions after Confirm (plan 0046). Other
signals are dropped:

- **Reactions.** `reply_feedback` keeps the latest reaction class per reply,
  but nothing reads it back. A 👎 queues one reconsider turn that repairs that
  reply only (plan 0064).
- **Corrections.** "No, I meant…" fixes the current answer. Nothing detects
  that the same mistake will recur.
- **Tool failures.** The broker reports each outcome (`failed`, `timeout`,
  `tool_error`, …) to telemetry alone. No per-principal record exists.
- **Know-how.** When a multi-step task finally works (which tools, which
  arguments, which order), the next request starts from scratch. Pi runs with
  `--no-skills`, and no procedures store exists.

This plan was added from the backlog ("think through agent self-learning:
how agents can learn from feedback and outcomes while keeping user control,
transparency of changes, and safety"). It is the design; plans 0080, 0113,
and 0114 implement its three steps.

## Goal

- Mistakes the user points out turn into lasting behaviour, when the user
  agrees, so they do not recur.
- Know-how from tasks that succeeded is reused on later requests.
- Every learned change is proposed and confirmed, shown where it came from,
  and removable in `/memory`. Nothing changes behaviour silently.

## Principles

These apply to all three mechanisms below.

- **Learning writes only through proposals.** A learned instruction or
  procedure changes every later turn, so it is always a Confirm card, like a
  memory instruction today. Facts keep their current rules.
- **Learned entries say where they came from.** A learned entry has the
  memory source `learned` and refers to its trigger: the reply that got 👎,
  the correction, or the weekly review. `/memory` shows it as
  "learned from …".
- **Outside content cannot teach.** A lesson proposed in an exposed
  conversation, or drawn from a reply that read outside content, is screened
  like any exposed memory write. It is also labelled in the card, so a web
  page cannot plant behaviour.
- **Lessons are bounded.** Learned instructions share the existing 1,500
  character instruction cap, which forces consolidation instead of growth.
  Procedures get their own small cap.
- **Measurable.** Proposals and their outcomes (confirmed, rejected, expired)
  are counted by mechanism. A high reject rate means a mechanism is noise and
  should be tuned or turned off.
- **Per user and switchable.** Each mechanism is a `/settings` toggle under
  memory. Turning memory off turns learning off.

## Mechanisms

### 1. Learn on correction

The smallest change, with no new storage.

- **The reconsider turn asks for the lesson.** The fixed reconsider message
  after a 👎 gains a second step. After the reply is repaired, if the mistake
  comes from a preference or rule that would recur, the model proposes one
  memory instruction for it, and otherwise proposes nothing.
- **Guidance covers explicit corrections.** When the user corrects the
  assistant ("no, use metric", "I said the 07:42, not the 07:12") and the
  correction states a lasting preference, the model offers to remember it as
  an instruction. A one-off fix is not a lesson.
- **The card names its trigger.** "Learned from your 👎 on <date>" or "from
  your correction", with the instruction text, and Confirm or Reject.
- **Open:** the system cannot verify the model's judgment of "would recur".
  The reject rate shows whether it over-proposes.

### 2. Weekly reflection

A review of the week's signals, run by the application, not by a free
scheduled turn.

- **Inputs are gathered by code.** The review collects:
  - every 👎 reply in the past 7 days, with the user message before it and
    the reply text, read from the Pi session files via `request_id` (new
    reader code; the history package only exposes titles today);
  - whether its reconsider turn repaired it;
  - tool failures from a new per-principal `tool_outcomes` table: tool name,
    outcome, and time, never arguments or results, pruned after 30 days;
  - which learned entries were used, and how often they were rejected;
  - the current instructions, so it can spot ones that overlap or contradict.
- **A special turn kind.** Scheduled turns offer no write tools and cannot
  create Confirm cards, by design. Reflection is its own turn kind. It
  receives the gathered inputs and only two tools: `lesson_propose`, which
  creates an instruction or procedure proposal, and `backlog_add`, for
  problems that need code, such as a tool that keeps failing. Both are
  Confirm cards.
- **Consolidation.** Besides new lessons, the review proposes merging
  instructions that overlap or contradict one another into one, and removing
  the ones replaced. A merge is a single Confirm card that shows the old
  entries and the new text.
- **Output.** One message titled "Weekly review" with at most three proposals
  and a line per backlog suggestion. A week with nothing worth proposing
  sends nothing.
- **Cost and control.** One model turn per week on the user's own
  subscription, off by default, and switched on in `/settings`. The schedule
  is a fixed weekly slot in `HOME_AGENT_TIMEZONE`.
- **Exposure.** Reply texts can contain outside content, so the review counts
  as exposed and every proposal is screened.

### 3. Learned procedures

Know-how kept apart from the always-on prompt.

- **A new memory kind, `procedure`.** Each procedure has a title of up to 80
  characters and steps of up to 1,000 characters: which tools, which
  arguments, which order, and what to avoid. A principal keeps at most 20.
- **Not in the prompt body.** The system prompt lists only procedure titles
  with their IDs. The model reads one with `procedure_read` when a request
  matches it. The prompt changes only when titles change, so caching holds.
- **Written after success.** After a chat turn or background task that used
  several tool calls and recovered from failures to succeed, guidance lets
  the model propose a procedure. It is a Confirm card that shows the steps.
  Reflection can also propose procedures, or propose pruning ones never read.
- **Stale procedures.** A procedure records when it was last read. Reflection
  proposes removing one unused for 60 days, or one whose tools no longer
  exist.
- **`/memory` shows procedures** in their own section, each with its source
  and a Delete button.

## Suggested order

1. **Learn on correction.** Guidance, the reconsider message, the `learned`
   source, and the card label. Small, and it shows quickly whether proposed
   lessons are welcome.
2. **Learned procedures.** The new kind, `procedure_read`, the prompt titles,
   and `/memory`.
3. **Weekly reflection.** The `tool_outcomes` table, the feedback and session
   readers, the reflection turn kind, and the weekly slot. This is the
   largest piece. It depends on the other two for its outputs.

Each step gets its own implementation plan when it starts. This plan stays
the design they refer to, and each records where it departs from it: learn
on correction in 0080, learned procedures in 0113, and the weekly review in
0114.

## Files affected (expected)

- `internal/memory` (the `learned` source and the `procedure` kind),
  `internal/conversation` (reconsider message, reflection turn kind),
  `internal/tools` (the reflection tool set), `internal/telegram` (`/memory`,
  `/settings`, card labels)
- `internal/store/sqlite`: migrations for the memory source and kind, and
  `tool_outcomes`
- `internal/conversation/history` (a reader of replies by request)
- README, `docs/architecture.md`, `docs/operations.md`

## Validation

Validation is per step, in its implementation plan. Each step needs:

- integration tests with the fake Telegram server and a temporary database
  showing that a learned proposal needs Confirm, carries its source, and
  appears in `/memory`;
- tests showing that exposed content is screened, and that turning the
  toggle off removes the mechanism;
- metrics for proposals and their outcomes by mechanism.

## Risks and open questions

- **Prompt drift.** Many small confirmed instructions can contradict one
  another. The shared cap and reflection's merge proposals are the
  mitigation.
- **Proposal fatigue.** Too many cards would make the user reject everything.
  Limits: one lesson per reconsider, three per review, and the reject-rate
  metric.
- **Reading Pi session files** couples reflection to Pi's file format, which
  only the history package depends on today. Accepted: the reader lives in
  the history package beside the one that already parses it.
- **Privacy.** `tool_outcomes` adds a new per-user record. It holds tool
  names and outcomes only, is pruned after 30 days, and must be listed in
  `/privacy`.
- **Lessons stay per user.** Every lesson stays with its principal; nothing
  learned is shared across the household. Changing what everyone gets (the
  system prompt, skills, or tools) is a later step and out of scope here.
  Until then, a lesson that needs such a change becomes a backlog item for a
  development session.
