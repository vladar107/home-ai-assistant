# 0170 — Placeholders for email addresses, phone numbers and addresses

Status: `in-progress`

## Context

Every model run sends the model provider what the turn holds: the user's message, the memory and
jobs sections of the system prompt, and every tool result. Tool results carry other people's
personal values: senders and recipients in mail, attendees and locations in calendar events,
phone numbers in pages and messages. The provider needs none of them to reason about the turn. A
tool needs them only when the model passes one back, to reply to a sender or to plan a trip to a
location.

An agent platform described at the Podlodka AI Engineers Club ("Агентская платформа в Авито")
replaces personal values in tool results with placeholders, and maps them back on the server.
The model works with the placeholders. This plan applies that to the assistant.

What exists:

- Every model run, chat, task, scheduled, tab name and rewrite, goes through `runtime.Run` from
  `internal/turn`. `RunRequest` carries the message, the memory and jobs sections and the tool
  set, and `RunResult` carries `Response` and `Final`.
- Every tool call goes through the broker's executor, which is passed to the runtime in the tool
  set. The broker records outside content (`Run.MarkExternal` scans raw text for URLs) and has
  the TypeSafe screener read third-party results.
- Write tools create their proposals inside the provider's `Execute`, from the call's arguments,
  so a card shows whatever arguments the provider received.
- Pi sessions persist on disk and resume. Closed conversations are kept 90 days (plan 0102).
  Principals can be revoked (plan 0168), and a user's data can be exported and restored (plan
  0129).
- The parcel connector already keeps addresses out of its results. Calendar events carry a
  structured `location`.

## Goal

The model provider never receives an email address or phone number that appears in model-bound
text, nor a calendar event's street address. It receives a placeholder instead. Users see real
values everywhere: replies, cards and saved records. Tools receive real values when the model
passes a placeholder back. Nothing a user can do works worse, apart from reasoning that needs a
hidden part of a value.

## Decisions

- **Advisor consult:** where the mapping lives and how far it reaches. Decision: a per-principal
  persistent vault with bracketed tokens, applied to everything model-bound, with tokens treated
  as capabilities. The deciding reason: a per-turn map leaves dangling tokens in resumed
  sessions, and values saved through a write tool would come back raw. Fake look-alike values
  could collide with real ones and reach a recipient.
- **Token format.** A token is `⟦` + kind + `-` + a random six-character base32 suffix, plus,
  for an email, a space and the domain, + `⟧`. Examples: `⟦email-k3f7qa bank.example⟧`,
  `⟦phone-7hm2xe⟧`, `⟦address-p4tq6n⟧`. The suffix is random and stored, so a token cannot be
  guessed or enumerated. The email body is not an addr-spec, so a token whose brackets the model
  dropped can never pass as a real address. The domain stays visible, because the routine-reply
  checks (plan 0167) and ordinary reasoning about a sender's organisation need it. This is a
  deliberate partial hide: a personal domain still names its owner. The operator confirms it at
  plan approval.
- **One vault per principal, in SQLite.** A row holds principal, kind, value, suffix, and first
  and last seen. Unique indexes cover (principal, kind, value) and (principal, suffix).
  Allocation runs in a transaction and retries on a suffix clash, so concurrent turns agree. The
  same value always gets the same token, so persisted sessions stay coherent and the prompt
  prefix stays cache-stable. Rows unseen for 120 days, past the 90-day conversation retention,
  are deleted, following the store's existing prune methods. A token whose row is gone renders as
  "(hidden address)", "(hidden number)" or "(hidden place)" and never resolves. Revoking a
  principal deletes its rows. Export and restore leave the vault out, on
  `wiring.ExcludedFromExport` with its reason. The exported sections hold no tokens: plan 0129
  already excludes conversation sessions, and every record a tool writes is stored with
  resolved, real values. Restored records are re-tokenized on their next read.
- **The vault is a new store of third-party contacts in plaintext.** It sits in the same SQLite
  database and backups as the mail-derived data the assistant already keeps, under the same
  protection. It holds no more than turns already handled, but in one place.
- **Everything model-bound gets the same pass,** with no exceptions by source. This covers the
  composed message (the user's own text included), the memory and jobs sections, the tools'
  guidance, and every tool result, error text included. Emails and phones are found by pattern. Values already in the
  vault are replaced by exact match on word boundaries, longest first. Declared values are added
  to the vault, then replaced. One rule everywhere means a value the user typed, saved through a
  tool, or read in mail is the same token in every place.
- **Addresses come only from declared values.** A provider's `Result` gains `Sensitive`, a list
  of values to hide. The calendar provider declares every event location, and the placeholder
  package keeps only those that look like a street address. A street address has at least
  eight characters, a digit, a letter and a space. It is not a URL, date or time, and has a
  comma or two words besides its numbers. "Home", "Zoom", "Platform 12" and meeting links stay
  readable. When an address has a comma, the part before it is registered too, as long as it
  holds a number, because trip updates quote only that part. The trip scan, which reads the
  calendar without the calendar tool, registers event places the same way. Free-text address
  detection is not attempted, so an address inside a mail body or a web page reaches the
  provider as text.
- **Saved places and map results are not declared.** Map and place search results are public
  business addresses. Saved places are the household's own records, which the model needs for
  distance and routing. Both reach the provider as they do today. This narrows "addresses from
  structured fields" to calendar locations, and the operator confirms it at plan approval,
  together with the visible email domain.
- **Strict patterns.** An email is a standard addr-spec with a dotted domain. A phone starts
  with `+` or `0`, has 9 to 15 digits with optional spaces, dashes, dots or one bracketed group,
  and is bounded by non-alphanumerics. Times, dates and number ranges are excluded. Tracking
  codes, IBANs and order numbers contain letters or exceed the length, so they do not match.
  Tests pin each false-positive class.
- **Tokens are capabilities.** Literal `⟦` and `⟧` in incoming text are replaced with ordinary
  brackets before tokenizing, so outside text cannot forge a token. A tool call's arguments are
  resolved against the principal's own vault, and an email token resolves only when its domain
  matches the stored value. A call fails, with an error the model can act on, when its
  arguments hold any of the following, which are never passed through:
  - an unknown token;
  - a malformed token;
  - a token body without brackets (`email-k3f7qa`);
  - a token in a JSON object key, or anything after the first JSON value.

  Arguments that are not one JSON value come back to the model as a tool error; they never fail
  the run. Text from one principal's run never resolves against another principal's vault.
- **Web searches never carry a token.** The provider runs web search itself, so the decorator
  also wraps the search check. A query holding a token, a malformed token or a bare body is
  denied with text telling the model to search without hidden values. No card is shown and no
  search runs.
- **Earlier conversations read back real values.** Pi session files hold tokens. The history
  service resolves session text before it searches, returns or shows it, so search by a real
  value works and titles show real values. The decorator then hides the text again, with the
  same tokens, when the model reads it.
- **One runtime decorator, nearest Pi.** It wraps the production runtime, and the eval
  harness's, as the layer Pi calls into. On the way in it tokenizes the message, memory, jobs
  and tool guidance, and wraps the tool executor. The wrapped executor resolves arguments
  before the inner executor runs, then tokenizes the result and error text it returns. On the
  way out it resolves `Response`, `Final`, every streamed text event, and the arguments of
  `tool_call` events. It buffers from `⟦` to `⟧`, up to the length of the longest well-formed
  token, so a token split across chunks is resolved whole and a stray bracket cannot hold back
  the stream.
- **Fail closed.** If the vault cannot be read or written, the turn fails with the ordinary
  failure reply and logs `placeholder.vault.failed` with its stage, never the value. Raw text
  is never sent instead.
- **Ordering with the broker and the eval recorder.** The broker's own checks run inside the
  wrapped executor, on resolved arguments and raw results. `MarkExternal` and the TypeSafe
  screener therefore see raw text, as today, and approval cards show real values. The eval
  recorder wraps the runtime and the executor outside the decorator. It records real values,
  so a case's argument and reply checks keep comparing real values. What the model received
  is seen in the Pi session files and the fixture's input.
- **The approval and card path needs no change.** Proposals are built inside `Execute` from
  resolved arguments.
- **Always on, with an operator escape.** `HOME_AGENT_PLACEHOLDERS` defaults to `true` and is
  mirrored into `.env` and `deploy/.env.nas`. `false` stops tokenizing for every principal but
  keeps resolving, so tokens already in resumed sessions still reach tools and users as real
  values. There is no per-user switch.
- **No backfill.** Session history written before rollout keeps the raw values it had. New turns
  in those sessions tokenize only what they send.
- **Eval baselines move.** The decorator changes what every case sends to the model, so the
  eval baseline is re-recorded after this lands, and earlier runs compare across the change
  only with that caveat.

## Approach

Wave 1: chunks 1, 2 and 3 run in parallel. Wave 2: chunk 4. Wave 3: chunk 5. Then chunk 6. The
migration number is assigned when chunk 1 lands, after whatever migrations landed before it.

1. **The vault** — Complex (Jev: complex, 0.98).
   - Task: the migration and the store methods. They allocate or reuse a token for (kind,
     value), resolve a token, list known values for exact matching, touch last seen, prune by
     age, and delete a principal's rows. Prune is wired where the other prune methods run, the
     delete is wired into principal revocation, and the table goes on
     `wiring.ExcludedFromExport`.
   - Acceptance, from integration tests on a temporary SQLite store:
     - the same value returns the same token;
     - concurrent allocations of different values get distinct suffixes, and of one value the
       same one;
     - a forced suffix clash retries;
     - principals never share rows;
     - prune removes only rows unseen past the cutoff, and a removed token resolves to "gone";
     - revoking a principal removes its rows.
2. **The placeholder package** — Complex (Jev: complex, 1.0).
   - Task: `internal/placeholder` with pattern detection, the address shape rule for declared
     values, bracket neutralisation, tokenize over a vault interface, resolve (strict for
     arguments, rendering markers for text), and a streaming resolver with its cap.
   - Acceptance, from table tests:
     - emails, and phones with and without a country code;
     - each false-positive class: dates, times, ranges, tracking codes, IBANs, order numbers;
     - declared "Home", "Zoom" and a meeting URL left alone, and a street address replaced;
     - longest-first, word-boundary replacement;
     - a forged token neutralised;
     - in strict mode, rejection of an unknown token, a bare token body, and an email token
       whose domain differs; in text mode, markers instead;
     - a token split across two and three chunks resolved whole;
     - a stray `⟦` released after the cap.
3. **Declared values from providers** — Complex (Jev: complex, 0.97).
   - Task: `Result.Sensitive` on the broker's provider result and a matching field on the
     runtime's tool result, passed through by the broker, with the calendar provider declaring
     event locations.
   - Acceptance: a broker integration test shows a calendar result's location carried as a
     declared value. Results from providers that declare nothing are unchanged.
4. **The runtime decorator, wiring and documentation** — Complex (Jev: complex, 0.82); depends
   on 1, 2, 3.
   - Task: the decorator, its setting, its place in the production wiring and in the eval world
     as the layer nearest Pi, a section in `docs/architecture.md` (scope, every limit below,
     the setting), and a line in `SECURITY.md`.
   - Acceptance: an integration test with the Pi fixture and a seeded mail fake, covering:
     - the fixture sees a token where the mail had an address, and sees memory and jobs
       tokenized;
     - a fixture reply echoing the token reaches the recording sender with the real value, and
       so does a streamed draft;
     - a fixture write call with the token creates a proposal whose card shows the real value;
     - a tool error naming an address reaches the fixture tokenized;
     - a forged token in the mail does not resolve, and a call with an unknown token or a bare
       token body fails;
     - a vault failure fails the turn and sends nothing raw;
     - with the setting off, the fixture sees exactly what it sees today, and a token from an
       earlier turn still resolves.
5. **End-to-end and eval coverage** — Complex (Jev: complex, 0.61, below threshold; taken as
   complex because it spans the conversation path and the harness); depends on 4.
   - Task: a Telegram-level integration test for a chat turn over mail and calendar, a
     scheduled run whose report later enters a chat turn, and a resumed session reusing a
     token. Two eval cases with seeded emails and a calendar street address, checked by real
     value.
   - Acceptance: the full suite passes with and without the race detector. Both eval cases'
     checks pass on a replay of fixture runs.
6. **Live check** — Orchestrator; depends on 5.
   - Task: run the two eval cases and a fresh baseline on the eval profile. After deploy, run
     one chat turn over the operator's own mail and calendar in Telegram, and read the session
     file for tokens.
   - Acceptance: the eval cases pass live. The session file shows tokens, and the reply and
     cards show real values.

## Files affected

- New: `internal/placeholder/`, a store migration and its methods in `internal/store/sqlite/`
- `internal/tools/broker.go`, `internal/runtime/runtime.go`, `internal/connectors/calendar/`
- `internal/wiring/wiring.go`, `internal/eval/world.go`, `internal/config/`, the revocation path
- `.env`, `deploy/.env.nas` (local, not committed)
- `docs/architecture.md`, `SECURITY.md`, `docs/plans/README.md`

## Validation

- Formatting, vet, build, module tidiness, the full test suite with and without the race
  detector, and `scripts/check-plans`.
- Chunk 6's live checks.

## Risks and open questions

- **Paths the layer cannot reach.** Images and photos are sent as pixels. The provider's own
  web search runs inside the provider. An address in free text is not detected. The
  documentation states each one.
- **The model loses what a token hides.** It cannot read a phone's country, an email's local
  part, or where an event is. Domain-level reasoning stays; a check that compared local parts,
  such as spotting `noreply@`, must compare tokens or move server-side. Chunk 5's eval cases
  watch for regressions in mail triage and trip planning.
- **Phone false positives** would hide a number the model needed to read, such as a tracking or
  order number. The pattern is strict and its tests pin each class; a new false positive
  becomes a test.
- **Reformatted addresses** (a location written differently elsewhere) are not matched by exact
  replacement. That residual leak is accepted.
- **TypeSafe still reads raw third-party text** for screening, as today. Tokenizing what the
  screener reads is a possible follow-up.
- **Plans 0164 and 0169 edit the Pi runtime and bridge.** The decorator wraps the runtime from
  outside and touches neither. Plan 0169 also edits `docs/architecture.md`; whichever lands
  second rebases its section.
