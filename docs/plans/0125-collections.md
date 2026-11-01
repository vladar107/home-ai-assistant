# 0125 — Collections: typed records the assistant keeps for the user

Status: `proposed`

## Context

Memory (plan 0046) holds free-text facts and instructions. It is meant to
change how the assistant behaves, not to hold records. Notion holds tables,
but only for a user who connected it, and only in tables they built.

Many everyday requests are small structured lists:

- gift ideas for people;
- things lent out, and to whom;
- warranties and their end dates;
- a shopping list;
- meter readings;
- books to read.

Today these either end up in memory, where they crowd the prompt, or nowhere.

## Goal

- The user can say "keep a list of things I lend out, with who and when". The
  assistant proposes a collection with typed fields, and later adds, changes,
  checks off, and removes records by conversation.
- Questions such as "what have I lent to Sam?", "which warranties end this
  year?", or "how much did I spend on gifts?" are answered from the records,
  with filtering, sorting, and totals done in code.
- `/collections` shows each collection and its records, with Delete buttons,
  as `/memory` does.
- Collections need no integration and stay in the assistant's database.

## Decisions

- **Schema per collection, validated in code.** A new migration adds:
  - `collections`: principal, ID, name (unique per principal, folded case),
    a description, fields JSON, and created and updated times.
  - `collection_records`: principal, collection ID, record ID, values JSON,
    and created and updated times.

  Field types are `text` (≤500 runes), `number`, `money` (amount and
  currency), `date`, `datetime`, `bool`, `choice` (up to 20 options), and
  `person` (free text). A field can be required.
- **Limits.** 20 collections per principal, 20 fields per collection, and
  2000 records per collection. Queries return at most 50 records, and
  aggregates cover every match.
- **Tools.**
  - `collection_create`: proposes a name, fields, and a description. It
    always goes to the card, because a schema is hard to undo once records
    exist.
  - `collection_alter`: adds, renames, or removes fields. Removing a field
    goes to the card.
  - `collection_add`, `collection_update`, `collection_remove`: change
    records. In the user's own chat turn they use act-and-tell with Undo
    (plan 0079). Otherwise they go to the card.
  - `collection_query`: filters by field (`=`, `≠`, `<`, `>`, contains, is
    empty), sorts, limits, and returns count, sum, min, max, and mean for
    numeric fields. A `group_by` on one field is allowed.
  - `collection_list`: lists collections with their fields and record counts.
- **Prompt cost.** Only collection names and record counts go into the turn
  context, as procedure titles do (plan 0113). Fields and records are read by
  tools.
- **Notion first when named.** The guidance says to use Notion when the user
  names a Notion table, and a collection otherwise.
- **Dates feed reminders.** A record with a date can be the target of an
  ordinary one-time reminder. Nothing is scheduled automatically.

## Approach

1. Migration, store, the `internal/collection` package, value validation, and
   the query engine in Go.
2. Tools, registration, and the approval executors.
3. The `/collections` view in `internal/telegram`, with the
   `col:<action>:<id>:<page>` callbacks parsed against an allowlist, as
   memory callbacks are. Register the command.
4. Guidance, README, and `docs/architecture.md`.

## Files affected

- `internal/store/sqlite/migrations/NNN_collections.sql`, a new store file
- `internal/collection` (new)
- `internal/telegram` (a new view and command registration)
- `internal/wiring`, `internal/prompt`, README, docs

## Validation

- Tests with a temporary SQLite store and the fake model runtime:
  - creating a collection asks for Confirm;
  - adding a record in the user's own turn acts with Undo, and Undo removes
    it;
  - an invalid value is refused with the field named;
  - query filters, sorting, totals, and `group_by` match hand-computed
    results;
  - limits are enforced;
  - `/collections` pages and Delete work;
  - another principal's collections are never visible.
- A live check from the GoLand server: create a lending list, add two items,
  and ask who has what.
- Formatting, module, test, vet, build, and plan checks.

## Risks and open questions

- Sharing a collection with another household member is a separate feature
  and stays out of scope. The schema keeps a single owner.
- Plan 0121 can propose a record as a follow-up once this lands. Plan 0124
  could chart a numeric field.
- Plan 0129 should export collections.
