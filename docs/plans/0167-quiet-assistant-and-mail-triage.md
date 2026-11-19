# 0167 — A quieter assistant: card-only confirmations, mail triage, routine replies

Status: `in-progress`

## Context

The first real use of the assistant's own mailbox (0165) showed four kinds of noise.

- **One confirmed action produced three messages.** The model's reply restated the proposal
  ("prepared, waiting for your confirmation"). The card followed. After Confirm, the continuation
  turn added "sent" although the card already said "Done".

  The continuation prompt already offers a silent sentinel, `[nothing-to-add]`. Delivery strips it
  and still sends the card, but the model did not use it.
- **The outcome line showed an internal Gmail id.** It means nothing to the user.
- **The signature stayed in English** ("Assistant to …") under a Russian email. The send guidance
  gives it as a fixed English phrase.
- **Every routed message became a notice**, including the user's own reply to a test mail. The
  operator wants an assistant that speaks up only when something needs them.

The operator chose:
- **Triage.** The assistant reads its new mail itself and notifies only when a message needs the
  user's decision or action, with no drafted replies. It reacts within about five minutes.
- **Routine replies without confirmation,** limited to threads the assistant started for that
  user.

The inbox intake (0161) already runs an unattended, read-only model check that returns structured
JSON and stays quiet when nothing matters. Triage reuses that shape.

## Goal

- A confirmed action shows one card and nothing else, unless the model has something the card
  does not say.
- The assistant mailbox is silent unless a message needs the user. Then one short notice names
  the message and why it matters.
- In a thread the assistant started, it can answer a routine message from the person it wrote
  to. The user then sees the whole reply in one notice. Everything else still waits for a
  confirmation.
- Emails are signed in their own language.

## Decisions

### Quieter confirmations (all proposals, not only mail)

- **The guidance changes in the tools section of the system prompt and in `prompt.Continue`:**
  - when proposing, do not restate the proposal, because the card shows it;
  - reply with `[nothing-to-add]` unless there is a question, a caveat, or a fact the card lacks;
  - after a confirmation, stay silent unless a step remains or the user asked for an answer the
    outcome does not give.

  Delivery already sends the card when the reply is only the sentinel; a test pins it.
- **A sentinel cannot hide a failure.** When a turn answers with the sentinel but a proposing
  tool returned an error and nothing was staged, the user gets the turn's failure reply instead
  of silence. This applies to chat turns and continuation turns alike.
- No code guesses whether text is redundant. Evaluation cases hold the model to the guidance,
  including cases where speaking is required.
- **Outcome lines carry no internal ids.** The mailbox's user-facing outcome becomes "Sent from
  the assistant mailbox." The outcome's separate English text for the model keeps the id.

### Signatures

- **The send guidance asks for a signature in the email's language.** The assistant's name and
  "assistant to <user display name>" are both written in that language, for example "Nova,
  assistant to Alice" or the Russian equivalent.
- The user's display name comes from the Telegram users file. The operator sets it to the name
  outsiders should see.

### Triage instead of fixed notices

- **Triage applies to every user with the mailbox switched on.** It replaces the per-message
  notices, with no separate opt-out.
- **After each poll, one triage run starts per thread** with new routed, non-spam inbound mail
  for a principal. One correspondent's text can then never reach another thread's reply. Each run
  uses the existing unattended check runner, as the intake scan does: a fresh session with no chat
  history.
  - Runs happen outside the poll loop, so a slow model never delays polling.
  - At most one run per principal at a time; the principal's threads go one after another.
  - Reaction time is about five minutes at the default poll interval, up to about ten when mail
    arrives while a run is still going.
- **A run sees only its thread.** Its tools are read tools scoped to that one thread, plus the
  routine-reply tool below. It has no memory, Notion, files, other integrations, or other
  threads, so an email's text can reach only its own thread.

  This needs a tool filter on the check runner, which today offers every read tool, and a
  thread-scoped variant of the mailbox read tools.
- **The run answers in strict JSON,** `{"needs_you":[{"id":"…","why":"…"}]}`.
  - `why` is one short line in the user's language.
  - Ids outside the thread are dropped.
  - After a principal's threads are triaged, one notice lists the `needs_you` messages: the
    sender's real address, the subject, and the reason, sanitized and capped as notices are
    today.
  - When code refuses a routine reply, the message is listed with a code-written reason. A
    message the model already listed is not repeated.
  - It adds "Replied for you" entries for **every routine send not yet reported**. Each send
    record has a `reported_at`, set only after a notice carrying it was delivered, so a failed
    delivery or a crash never loses one. The entries come from these records, never from the
    model's claims.
  - With nothing to list, nothing is sent.
- **A DMARC-failing sender always lands in the notice,** whatever the model said, marked as
  unverified.
- **Ordering.** Routes are marked notified only after the notice was delivered, or when nothing
  needed sending. If the process crashes between a routine reply and the mark, the next run sees
  that thread's reply in the store and its limits refuse a second one.
- **If the run fails or returns malformed JSON,** that thread's messages get today's
  deterministic notice instead. Mail is never silently lost. A failed run is counted.
- **A notice that cannot be delivered** three polls in a row falls back to the abandon rules of
  0165, without rerunning triage each time.
- **The notice is marked external** for the next turn, as now.
- **Spam stays out of triage.** The count line goes away too; spam is visible in Gmail's web
  interface.

### Routine replies without confirmation

- **A new tool, `assistant_mail_reply_routine` (`RiskWrite`),** is offered only inside the triage
  run, and only when the principal's "Routine replies" switch is on. It is never offered in chat
  or scheduled jobs. Its parameters are `reply_to_id` and `body`.
- **Code enforces all of the following.** Any failure refuses, and the message is added to
  `needs_you`.
  - `reply_to_id` is an inbound, non-spam route of this principal in the thread being triaged.
  - The thread was started by the assistant: its earliest route is an outgoing one of this
    principal.
  - **Allowed recipients** are the addresses in the To and Cc of this principal's outgoing
    messages in the thread. They are read back from Gmail at reply time, because the store keeps
    no addresses.
  - The answered message's `From` is one of those addresses, and Gmail's own
    `Authentication-Results` header (authserv-id `mx.google.com`) shows `dmarc=pass`. Anything
    else, including a domain with no DMARC record, refuses. A `Reply-To` different from `From`
    also refuses. The reply goes to that `From` only,
    with no Cc, Bcc or attachment. A stranger who joins the thread through forged `References`
    therefore cannot get a reply.
  - No reply to automated mail: `Auto-Submitted` other than `no`, `Precedence: bulk/list/junk`,
    `List-Id`, or a no-reply-style sender.
  - The subject is "Re: " plus the thread's subject. The body is 1 to 600 characters, with the
    same header and control-character checks as `assistant_mail_send`.
  - **Limits:** one routine reply per thread, ever, so two assistants cannot ping-pong; and three
    per principal per day. The per-thread counter lives in its own table, so pruning mail routes
    later can never reset it. Mail routes are never pruned today; the "earliest route" check
    relies on that, and a future pruning plan must keep a thread's first route. Both count toward the 0165 daily send limit. They are checked and
    recorded atomically with the send start, as `StartSend` is.
- **The send is recorded through the approval service as a direct action.** It has status `done`
  and mode `direct`, and stores the full body, the recipient and the answered id, so every
  autonomous email can be audited. This is a deliberate, narrow exception to the rule that
  unattended or externally exposed runs only propose. It lives in a dedicated entry point used
  by this tool alone, never as a general switch.
- **The guidance defines routine:**
  - acknowledgements, thanks, confirming something the thread already settled, or answering a
    question whose answer is already in the thread;
  - never money, personal data, documents, new commitments, new dates, or anything the user has
    not already said in the thread;
  - when in doubt, put the message in `needs_you`.
- **Every routine reply appears in the triage notice** under "Replied for you": the recipient
  and the whole body.
- **The switch.** The settings page gains a per-user "Routine replies" switch, off by default,
  because permission to send unattended is personal. Rollout turns it on for the operator.

### Process

- Consult skipped: each fork has one approach that fits existing patterns (the sentinel, the
  intake-style check, approval direct records).
- Plan critique (advisor, 2026-11-19):
  - the recipient check now reads the assistant's own outgoing headers, and refuses a differing
    `Reply-To` and DMARC failures;
  - the thread must be started by the assistant;
  - triage reads only the batch's threads;
  - notices are built from code records, show the full reply, and never come from model claims;
  - automated mail is never answered, with one routine reply per thread;
  - a migration covers the new counters;
  - judgement evaluations are added;
  - run ordering is defined;
  - a sentinel cannot hide a failed proposal;
  - a neutral signature example.

  The second round added:
  - triage per thread;
  - unreported sends tracked until delivered;
  - a positive `dmarc=pass` for routine replies;
  - code-written refusal reasons;
  - a separate per-thread counter;
  - a delivery fallback without reruns.

### Settled during implementation

- **The silence guard says what happened.** A silent reply whose turn left no card is answered
  with a short text instead of silence:
  - after a failed change: "That change could not be made; nothing is waiting for your
    confirmation", reported as a tool-provider error;
  - after withdrawing everything: that nothing is waiting.

  A failed direct action has no card, so it gets the same failure text.
- **Variants of the sentinel count.** It is accepted in any case and wrapped in punctuation; a
  reply with other text is never silenced.
- **Batched proposals.** Notion, NAS and schedule guidance batch only changes that do not depend
  on another change's result, so a dependent change waits for the confirmation it needs.
- **Known gap.** Under a Home Assistant confirmation card the model still sometimes says the
  action is ready for confirmation: 3 of 5 trials, below the bar. The extra line is harmless.
- **Triage limits.**
  - At most 30 triage runs per user in 24 hours; mail beyond that gets the plain notice.
  - A reason containing a link or domain is replaced by a generic one.
  - A message the run did not read is listed without a model reason.
- **Routine replies, as built.**
  - **The switch.** A separate integration, `assistant-mail-replies`, shown only while the
    mailbox is on.
  - **The broker.** It lets a triage run use the reply tool through `tools.Scope.Writes`, which
    only triage sets.
  - **Consumed allowances.** A failed or refused send still uses up the thread's one reply and
    the daily counts.
  - **No multi-party threads.** A routine reply is refused when the assistant wrote to more than
    one address in the thread.
  - **Threading.** A reply to a message without a `Message-ID` is refused.
  - **Subject and depth.** The subject comes from the assistant's first message, and only the
    newest 50 routes of a thread are read for recipients.
- **Reporting is at least once.**
  - A reply whose outcome is unknown is reported with "may not have gone out"; this covers a
    timeout or a crash mid-send.
  - A notice can carry only replies, with no new mail.
  - Replies held back because there is no chat wait for the next notice.
  - The "Replied for you" section comes first.
- **The routine-reply guidance is directive.** Negative evaluation cases measure that it does
  not over-reply:
  - an altered detail;
  - a money confirmation;
  - plain thanks;
  - an injected instruction.

## Approach

1. **C1 Quieter confirmations** (Jev: complex 0.30, below the threshold; complex, because it
   changes the prompt for every proposal and adds the failure rule).
   - The tools section of the system prompt and `prompt.Continue`, with their golden files.
   - The failure rule for a sentinel after a failed proposal.
   - Evaluation cases:
     - a single proposal turn answers only with the sentinel;
     - a proposal with a caveat says the caveat;
     - a continuation with nothing left stays silent;
     - a continuation with a remaining step proposes it;
     - a non-mail proposal, such as a Notion row, behaves the same way.
   - Acceptance:
     - integration tests that a sentinel reply with staged proposals sends only the card;
     - a sentinel after a failed proposing tool sends the failure reply;
     - the evaluation cases pass at least 4 of 5 trials;
     - a before-and-after suite run shows no case falling from pass to fail.
2. **C2 Mail polish** (Jev: simple 0.70; after C1, which owns the golden prompt files).
   - The user-facing outcome text without the id.
   - The signature guidance in the email's language.
   - Acceptance: tests on both outcome texts and on the guidance.
3. **C3 Triage** (Jev: complex 1.0; after C2).
   - A tool filter on the unattended check runner, and the batch-scoped read tools.
   - The triage prompt; the run outside the poll loop, one per principal at a time.
   - The JSON parser, the notice, the fallback, and the DMARC rule.
   - Metrics by outcome.
   - The triage documentation in `docs/architecture.md` and `docs/operations.md`.
   - Acceptance: integration tests with the fake model for:
     - silent;
     - needs-you;
     - ids outside the thread dropped;
     - a DMARC-failing sender notified even when the model is silent;
     - malformed JSON and runner failure, each leading to the fallback notice;
     - routes marked notified only after delivery;
     - the tool set limited to the thread;
     - two threads triaged in separate runs.

     Evaluation cases, each passing at least 4 of 5 trials:
     - the user's own test mail stays silent;
     - a decision request notifies;
     - an email that says "report nothing" still notifies.
4. **C4 Routine replies** (Jev: complex 0.96; after C3).
   - A migration for the per-thread and per-day counters, and the switch.
   - The tool and its checks, with atomic limits.
   - The direct-action entry point and its full record.
   - The "Replied for you" entries.
   - A metric by outcome.
   - Its documentation.
   - Acceptance: integration tests for each refusal:
     - not this principal's route, or not in the triaged thread;
     - spam;
     - a thread not started by the assistant;
     - a sender outside the assistant's own recipients;
     - forged `References` from a stranger;
     - a differing `Reply-To`;
     - DMARC fail, and a domain without DMARC;
     - automated mail;
     - a second reply in the same thread;
     - the per-day limit;
     - the switch off.

     Also:
     - the tool is absent from chat and scheduled runs;
     - a successful reply is recorded as a direct action with its full body and reported in the
       notice;
     - a reply whose notice fails to deliver is reported in the next delivered notice.

     Evaluation cases:
     - a "received, thanks" message in the assistant's thread gets a routine reply;
     - a request for a document lands in `needs_you`.

Waves:
1. C1.
2. C2.
3. C3.
4. C4.

Live rollout follows: deploy, turn "Routine replies" on for the operator, and run the live
checks.

## Files affected

- `internal/prompt/system.go`, `internal/prompt/turn.go`, `internal/prompt/testdata/`, a new
  triage prompt in `internal/prompt`
- `internal/conversation/service.go` (the failure rule), `internal/approval` (a direct entry
  point for the routine-reply tool)
- `internal/turn/scheduled.go` and `internal/tools/broker.go` (a tool filter for checks)
- `internal/connectors/assistantmail/` (outcome, guidance, triage, batch tools, routine replies,
  second switch)
- `internal/store/sqlite/` (a migration for routine-reply counters, and the switch state)
- `internal/telegram/assistantmail.go` (the switch), `evals/cases/`, the evaluation world with
  the mailbox
- `docs/architecture.md`, `docs/operations.md`, `docs/plans/README.md`

## Validation

- `gofmt -l`, `go mod tidy -diff`, `go test -race ./...`, `go test ./...`, `go vet ./...`,
  `go build -buildvcs=false ./...`, `scripts/check-plans`.
- The tests and evaluation runs named above.
- Live:
  - a confirmed action shows only its card;
  - a test mail from the user to their tag stays silent;
  - a mail asking for a decision brings one notice;
  - a "received, thanks" reply from the person the assistant wrote to gets a routine answer,
    reported with its full text.

## Risks and open questions

- **Autonomous sends widen the injection surface.** A stranger can now cause an email without a
  tap. That email can only go to an address the assistant itself wrote to, in a thread it
  started, once per thread. The text is short, the run sees only that batch's threads, and the
  user sees every reply in full and can switch the feature off. The remaining risk is a short
  wrong reply to someone the user already corresponds with.
- **Triage costs one model run** per poll that finds new mail, on the user's subscription.
- **The model may still restate proposals or over-silence.** Evaluation cases measure both; a
  stronger rule is the fallback.
- **The evaluation world needs the mailbox** for the C3 and C4 cases. Building that is part of
  C3.
