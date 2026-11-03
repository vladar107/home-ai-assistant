# 0131 — Movie tools follow the turn order

Status: `done`

## Context

The movie tools from plans 0044 and 0118 read the next film, record scores, and date films that
have no date. Adding films is left to the generic `notion_create_page` and
`notion_update_page`. The household also follows a rule that no tool knows about: the two pickers
take turns week by week, and when one of them has not chosen a film for their week yet, that week
is kept as an empty reserved evening and the other picker's next film moves a week later.

One morning session spent an hour and 24 turns adding a batch of films:

- `movies_schedule` gave every undated film the latest date plus a week, ignoring the turn order,
  so its proposal was rejected.
- The model then built the rows itself. Two creates sent the date both as the title and as the
  title-type property; the proposal was accepted, and Notion rejected the create only after
  confirmation, leaving the rest of the bundle applied.
- Rows landed in the wrong evenings and a later seven-row bundle shuffled dates, pickers, and
  films between rows to repair them, which is nearly impossible to check before confirming.
- One turn hit the per-turn call budget after nine single-row creates.
- The picker column is named `Кто`, but the tools only recognize a single-letter picker column,
  so `movies_next` never says who picked and nothing knows whose turn an evening is.
- A proposal that clears a text property shows the raw value `{"rich_text":[]}`.

## Goal

- "Add these films for me" places each film in the caller's next turns in one tool call: it fills
  the caller's empty reserved evenings first, then extends the schedule week by week, adding empty
  reserved evenings for the other picker's turns. The user confirms one batch whose every row is
  in its final place, and rejecting it changes nothing.
- `movies_schedule` places a film without a date in its picker's next turn instead of the latest
  date plus a week.
- A create that sets the title twice is refused before anything is proposed.
- A cleared property reads `(empty)` in a proposal.

## Decisions

### Pickers and turns

- **The picker column** is the rich_text property named `Кто`. A table without it keeps the
  existing single-letter picker detection, so the shape from plan 0044 still works.
- **One letter rule for both sides.** A picker value is identified by its first letter folded with
  `foldLookalike`. The caller, or a `picker` argument, matches a picker value when that folded
  letter is among `initialLetters` of the caller's display name or of the argument, so `V`, `B`,
  and `В` all match a `В` value. Matching neither picker is an error naming both; matching both
  is an error too.
- **The two pickers come from recent rows, not the whole history.** The latest dated row's picker
  is one; the other is the picker of the most recent dated row with a different picker. Older
  guest picks therefore never matter. No dated rows, a latest row with no picker, or no second
  picker are each errors that say what to fix, and nothing is recorded.
- **Turns alternate weekly from the latest evening.** The first new evening is the latest date
  plus 7 days and belongs to the other picker; each further evening adds 7 days and switches
  picker. A gap in past weeks does not matter, because only the latest row is read.
- **Never in the past.** When the latest date plus 7 days is before today in
  `HOME_AGENT_TIMEZONE`, the weeks keep alternating until the first one on or after today.
- **A new row writes `Кто` as that picker's latest row spells it.**
- **A reserved evening** is a row dated today or later, with a picker and no film.

### `movies_add`

- It takes `data_source_id` (required, as for the other writes), `films` (one or more non-blank
  titles, in order, with no title repeated in the call), and an optional `picker` initial that
  defaults to the caller.
- Its films fill that picker's reserved evenings in date order, then continue after the latest
  date. Each of the picker's turns gets a new row with its film, and each of the other picker's
  turns in between gets a new reserved row. A new row's title is the evening date; its properties
  are only the picker and film columns, never the title property.
- It refuses, with nothing recorded:
  - a film already in the table (case-insensitive, trimmed), naming that row's date; a rewatch
    is refused too, by the operator's decision;
  - a table that still holds undated films, naming them, so they are scheduled first and
    nothing lands ahead of them;
  - a truncated table, as the other writes do.

### `movies_schedule`

- Without `date`, a film without a date goes to its picker's next turn after the latest date,
  with reserved rows for the other picker's turns in between. Several undated films go in
  creation order, each continuing from the previous one. A film whose picker is empty is an
  error naming it.
- An already dated film given without `date` is moved to its picker's next turn after the latest
  other date, the same rule.
- It does not move a film into an existing reserved evening, because that would leave a second
  row for the same evening and the connector cannot trash pages. The operator chose this over
  trashing or blanking the extra row.
- An explicit `date` still sets that film's date as given and moves nothing else.

### Writes stay one confirmable batch

- New rows are recorded as ordinary `notion_create_page` calls and changed rows as
  `notion_update_page` calls, so schema checks, confirmation, and execution are unchanged.
- A call that creates any row proposes every item, its updates included, instead of letting the
  updates act at once in a clean turn. Rejecting the batch then leaves the table untouched. A call
  made only of updates keeps today's behavior.
- Every item is computed and validated before the first one is recorded. When the items exceed
  the bundle limit of 10, nothing is recorded and the error says how many films fit, so the model
  asks the user to confirm and continues with the rest.
- The approval service gains `ProposeAll`, which stores such a batch whole or not at all: it
  checks every summary and the room left in the turn's bundle, counting proposals the turn already
  made, before storing the first item. `Propose` becomes its one-item case and the bundle limit is
  exported, so the movie tools count against the same number.
- Rows a call proposes are not in the table until confirmed, so a second movie write in the same
  turn would plan the same evenings again. The guidance tells the model to call `movies_add` or
  `movies_schedule` at most once per turn.

### Other

- **Guidance** tells the model to add, schedule, and score films only through the movie tools and
  never to create or update rows of the movie table with the generic Notion tools.
- **`movies_next`** names the picker of the next evening, and whose turn it is when that evening
  has no film yet.
- **The title clash is refused at proposal time.** A `notion_create_page` in a data source whose
  `properties` set the title-type property fails with an error saying the title fills that
  property.
- **A cleared value reads `(empty)`.** A title or rich_text value with no text renders as empty,
  and the summary prints it as `(empty)`. Empty replacing empty still reads `(no change)`. Cleared
  select and date values (`null`) are out of scope.
- Out of scope: the intent warning that marked most of these proposals as unrequested (a separate
  change to a check every tool shares) and turning the date column into a date type.

## Approach

1. Writes (`writes.go`): refuse a create whose properties name the data source's title property;
   render empty title and rich_text values as described. Tests in `service_test.go`, with any new
   helper prefixed by its subject so it cannot collide with the movie chunk's helpers.
2. Movies (`movies.go`): recognize `Кто`; derive pickers and turns; add `movies_add`
   with its definition, guidance, and dispatch; change `movies_schedule` placement and make a
   batch with creates propose every item; validate the whole batch and the bundle limit before
   recording; name the picker and turn in `movies_next`. Tests in `movies_test.go` with a `Кто`
   table, and a recorder whose `Act` acts at once, to show that a batch with creates proposes its
   updates too.
3. Raise the tool count in `internal/telegram/approval_integration_test.go` for the new tool.
4. Approval (`approval.go`): add `ProposeAll`, rebuild `Propose` on it, export the bundle limit,
   and test it through the Telegram approval harness.
5. Plan index, checks, and live verification.

## Files affected

- `internal/connectors/notion/writes.go`, `service_test.go`
- `internal/connectors/notion/movies.go`, `movies_test.go`
- `internal/approval/approval.go`, `offer.go`
- `internal/telegram/approval_integration_test.go`, `approval_batch_integration_test.go`
- `docs/plans/0131-movie-turn-order.md`, `docs/plans/README.md`

## Validation

- Integration tests through the Notion simulator, one or more per decision:
  - `movies_add` fills the caller's reserved evening first, then alternates with reserved rows;
    create titles are dates and no create sets the title property; `Кто` copies the picker's
    spelling; the caller is matched across Latin and Cyrillic initials.
  - With an acting recorder, a `movies_add` that both fills and creates proposes every item and
    acts on none.
  - Refused with nothing recorded: a film already in the table, a repeated title in `films`, empty
    `films`, waiting undated films, a `picker` matching neither picker, no dated rows, a latest
    row without a picker, no second picker, a truncated table, and a batch over the bundle limit.
  - A latest date more than a week ago catches up to today with the alternation kept.
  - `movies_schedule` puts an undated film in its picker's next turn with a reserved row for the
    other picker when needed; an explicit date is used as given; the existing single-letter
    fixture still schedules its two undated films on 17.10 and 24.10.
  - `movies_next` names the picker, and the turn of an evening without a film.
  - A create that sets the title property is refused before anything is proposed.
  - A cleared rich_text renders as `(empty)`, and empty replacing empty as `(no change)`.
  - `ProposeAll` refuses a batch that the turn's partly filled bundle cannot hold and records
    none of it, and stores a batch that fits once a pending duplicate is counted.
- Formatting, module, vet, test (with and without `-race`), build, and plan checks.
- After deployment, ask the bot to add two films for the caller and confirm that the single batch
  fills the caller's reserved evening and extends the schedule with alternating turns.

## Risks and open questions

- The bundle limit caps one call at about five films when each needs a reserved row beside it;
  larger batches take a confirmation per part.
- A picker who leaves their reserved evenings unfilled keeps them reserved; `movies_add` for the
  other picker never takes them.
