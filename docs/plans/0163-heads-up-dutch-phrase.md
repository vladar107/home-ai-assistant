# 0163 — Heads-up: a Dutch phrase of the day

Status: `done`

## Context

The user wants to practise Dutch at about A2 level (backlog item `64968d86`). They want one
useful, natural, everyday phrase a day, never repeated, with the translation hidden behind a
spoiler so they can try to understand it first. The phrase belongs to the morning Heads-up
(plan 0162), not to a job of its own.

Heads-up today replies `[nothing-to-add]` and stays silent when nothing needs the user. Job
reports are pruned after 14 days, so past reports cannot keep phrases from repeating over months.
The Telegram renderer (`internal/telegram/markup.go`) knows code, bold, links, and quotes, but
not spoilers.

## Goal

- On the Heads-up page in `/settings`, a user turns on "Dutch phrase". It is off by default and
  can be on only while Heads-up is on.
- With it on, the first Heads-up run of each local day ends with a phrase block. On a morning
  when Heads-up has nothing to report, the block is sent alone.
- The block holds a natural, everyday Dutch phrase at about A2 level and one short line on the
  situation it fits, in simple Dutch. Behind a Telegram spoiler come the translation of both into
  the user's interface language, and a note on a tricky word when there is one.
- A phrase the user has received before is never sent again, however long ago it was.
- The user can change the level or topics by adding notes to Heads-up in conversation.

## Decisions

- Consulted the advisor (Fable) on producing the phrase inside the Heads-up turn or beside it.
  Decision: beside it, in code. Non-repetition and "send even when quiet" must hold, and only
  code can guarantee them. The Heads-up prompt and tools stay untouched.
- The phrase comes from a separate tool-less turn on the user's own subscription, like the
  weekly review: no tools, no memory, a fresh session, on the background task model. The turn
  answers with JSON holding up to five ranked candidates.
- A new principal-scoped table `dutch_phrases` keeps every phrase sent. It holds the local date,
  the phrase, its normalized form, the usage line, both translations, the note, and when it was
  created. Two unique keys: one row per (principal, local date), and one per (principal,
  normalized phrase).
- The prompt carries the 100 most recent phrases as a hint. Code checks candidates against the
  whole table and takes the first one never sent. When every candidate collides or the reply
  cannot be read, that day gets no phrase and the miss is logged.
- Normalization is lowercase with punctuation removed and whitespace collapsed. Near-duplicates
  are left to the recent-phrases hint.
- The row is stored before the block goes out, and the block is rendered from the stored row. A
  failed delivery loses that day's phrase but never causes a repeat.
- Once per day: only the run that creates the day's row adds the block. A later run the same day
  (a retry, or a second run on a schedule the user chose) does not. A manual run shows the day's
  phrase again, creating it if the day has none yet.
- The block goes out only when the Heads-up turn succeeded, quiet or not. A failed turn reports
  its failure as today and adds no phrase. A phrase failure leaves the Heads-up result as it was.
- The switch is a principal switch, like Widgets: `integration.Switch{Integration:
  "dutch-phrase", Name: "Dutch phrase"}`, off by default. The existing switches export
  already carries it.
- Turning the phrase off keeps the history, so turning it on again does not repeat phrases.
  `/export` includes the history, `/privacy` says it is kept, and restore brings it back.
- The translation language is the user's interface language (`locale.ForPrincipal`). The block's
  labels are localized in English and Russian.
- The Heads-up notes go to the phrase turn with an instruction to follow only what concerns the
  Dutch phrase, such as level and topics. The Heads-up method gains one sentence: notes about the
  Dutch phrase are handled elsewhere, so the Heads-up turn never writes a phrase of its own.
- Candidate validation rejects a field that is empty, over its length bound, or that holds a
  newline, a backtick, `**`, or `||`. Model text then cannot break the block's markup.
- The phrase turn is bounded by the engine's turn timeout, like every tool-less turn. A slow,
  failed, or timed-out phrase turn leaves the Heads-up result as the turn produced it.
- Two runs that race for the day's row: the insert yields on conflict and re-reads the row. The
  loser adds no block on a scheduled run and shows the stored row on a manual one.
- Turning Heads-up off deletes its job but keeps the phrase switch. The switch has no effect
  without the job, and it shows again once Heads-up is back on. A paused or skipped Heads-up run
  sends no phrase that day. A moved Heads-up brings the phrase at its new time.
- The block is part of the run's result, so it is stored in the job report like the rest of the
  Heads-up.
- The renderer gains `||text||` → `<tg-spoiler>text</tg-spoiler>` as an inline rule beside bold,
  for every message. To keep ordinary text such as `a || b` literal, the character right after
  the opening `||` and the one right before the closing `||` must not be whitespace. Code spans
  are handled first, so `||` inside code stays literal. An unclosed `||` stays literal, and bold
  works inside a spoiler.

## Approach

1. Spoiler markup in `internal/telegram/markup.go`, with table tests.
2. A new package `internal/dutchphrase` (built in parallel with step 1 against the spoiler
   contract above):
   - migration `065_dutch_phrases.sql` and store methods in `internal/store/sqlite/dutchphrases.go`;
   - the phrase prompt, built in `internal/prompt` beside the weekly review prompt;
   - a `Checker` interface for the tool-less turn, the JSON decoding, candidate validation (length
     bounds, non-empty fields), normalization, the pick, the store, and the block rendering;
   - the service entry point: a principal, the scheduled local date, the notes, and whether the
     run is manual, returning the block or "";
   - an export and restore section copying `internal/location/export.go`.
3. Heads-up wiring:
   - `schedule.Service` gets an optional Heads-up phrase source, set at wiring like `SetParcels`;
   - the Heads-up branch of `modelTurn.Run` asks it when the switch is on and the turn
     succeeded, appends the block after a blank line, or sends it alone instead of staying quiet;
   - the one-sentence addition to the Heads-up method;
   - the Heads-up settings page gets the toggle and a status line in English and Russian, shown
     while Heads-up is on;
   - a `/privacy` line, and startup wiring in `internal/wiring` and `internal/app`;
   - the README, `docs/architecture.md` (Heads-up section), and `docs/operations.md`.

## Files affected

- `internal/telegram/markup.go`, `internal/telegram/markup_test.go`
- `internal/dutchphrase/` (new, with its tests and export section), `internal/prompt/`
- `internal/store/sqlite/migrations/065_dutch_phrases.sql`, `internal/store/sqlite/dutchphrases.go`
- `internal/schedule/modelturn.go`, `internal/schedule/service.go`, `internal/schedule/headsup.go`,
  and their tests
- `internal/telegram/headsup.go`, `internal/telegram/privacy.go`, and their integration tests
- `internal/wiring/` (including the export registry), `internal/app/app.go`
- `README.md`, `docs/architecture.md`, `docs/operations.md`

## Validation

- Markup tests cover a spoiler, an unclosed `||`, `a || b || c` staying literal, `||` inside a
  code span, bold inside a spoiler, and escaping inside a spoiler.
- Integration tests with a real temporary SQLite store and a fake checker at the model boundary:
  - the first unsent candidate is chosen and a sent one skipped, across normalization;
  - all candidates sent before, or an unreadable reply, give no phrase and no row;
  - a candidate holding a newline, a backtick, `**`, or `||` is skipped;
  - the phrase turn runs as the job's own principal;
  - a second scheduled call the same day returns "", while a manual call returns the day's phrase;
  - export and restore round-trip the history.
- Integration tests of the Heads-up run, with a fake runner:
  - with the switch on, a report gets the block appended and a quiet run sends the block alone;
  - a failed turn gets no block, and a phrase failure or timeout leaves the report unchanged;
  - with the switch off, nothing changes.
- An integration test of the Heads-up settings page: the toggle appears only while Heads-up is
  on, turning it on and off is saved, and the switch survives turning Heads-up off and on.
- An end-to-end test: a scheduled Heads-up run with the phrase on reaches the fake Telegram API
  with a `<tg-spoiler>` element.
- The configured format, module, test, vet, build, and plan checks.
- After deploy, with the phrase on, a manual Heads-up run in Telegram shows the block with a
  working spoiler.

## Risks and open questions

- Model cost: one extra small turn on the user's subscription each morning while the phrase is on.
- The history grows by one row a day. The 100-phrase hint keeps the prompt bounded, and the
  unique key keeps the check exact.
- Near-duplicates, such as one phrase with and without "alstublieft", pass the exact key. Only
  the recent-phrases hint guards against them.
- Replies are split before rendering, so a split inside the spoiler would show `||` literally. A
  Heads-up is a few short lines, far under Telegram's limit, so this is accepted.
- The migration number (065) is checked again at landing.
- Evals: the phrase turn sits outside the Heads-up turn the eval harness runs, so this plan adds
  no eval case. Phrase quality is checked by hand after deploy.
