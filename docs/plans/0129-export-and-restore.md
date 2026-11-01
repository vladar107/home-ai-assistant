# 0129 — Export and restore a user's own data

Status: `done`

## Context

Recovery is a first-class boundary of the product, but today the only
recovery path belongs to the operator: stop the service, copy the whole data
directory, and restore it whole (operations guide, sections 7 and 8). A user
cannot keep their own copy of what they set up, or recover from deleting
something by mistake. The same is true of the operator's own account.

Nearly every table is keyed by `principal_id`. The module registry (plan
0109) does not cover them all. It holds connectors, location, watch, and the
trip loop, while memory, schedule, routes, switches, and settings are wired
as broker providers and lifecycle steps.

## Goal

- `/export` sends the user one file holding what they configured and keep in
  the assistant:
  - memory and procedures;
  - scheduled jobs and reminders;
  - watches, routes, and trips with their bookings;
  - settings and integration switches;
  - collections, projects, places, intake records, and event source names,
    once those plans land.
- Sending that file back with `/restore` shows exactly what would be added.
  Confirm then adds what is missing, without overwriting what exists.
- Secrets, credentials, conversation history, and operational records are
  never in the file.
- A file that was edited, or made by anything but this installation, is
  refused.

## Decisions

- **An explicit export registry, filled in wiring.** It does not walk the
  module registry, which misses most of the core. Each owner of user data
  provides an exporter, which `internal/wiring` registers in restore order.
  Settled while implementing, because every owner's tables live in the one
  SQLite store:
  - An exporter describes its `Section`: a name, a label, a schema version,
    and its tables, each with the columns that never leave the database.
  - `Keep(table, row)` drops rows that do not belong in the file, and
    `Plan(ctx, planning, version, tables)` decides through the owner's own
    validation what is added, already there, or skipped, and queues the rows
    to add.
  - Records are the tables' rows by column, so the round-trip test compares
    them column by column. The export service writes a section's queued rows
    in one store transaction, so no owner needs a restore of its own.
  - A restored row holds what the owner's create path would store: the
    normalized watch spec, route days and legs, collection fields and record
    values, and a job's prepared name, zone, schedule, and configuration
    with its condition in canonical form. A record holding a column the
    table does not have is skipped and listed on its own.

  Each plan after this one that adds user data registers its section.
- **The sections, in restore order.** Places, routes, trips with their
  bookings, collections with their records (field keys and next key
  included), intake records, projects with their items and links, memory
  entries and procedures, memory settings, integration switches, event
  sources, scheduled jobs and reminders, and watches of every kind.
  - Left out of their rows: a trip's and a project's summary message, which
    belongs to one chat's tab; an intake record's pending actions; an event
    source's token hash, last use, and rotation time.
  - Not exported: deleted places, and one-time jobs that already ran.
  - The integration switches section lives in wiring, because the
    integration package sits below the approvals the export builds on.
- **What is excluded, by rule.**
  - Model connections, Google and Notion tokens, event source token hashes.
  - Conversation sessions and messages, approvals, schedule runs, watch
    states and alerts, and turn traces (plan 0128).
  - Telemetry counts, location fixes, and place presence.
  - Tab registry rows, because tabs belong to one Telegram chat.
  - Settled while implementing: the list is `wiring.ExcludedFromExport`,
    with a reason per table. It also holds credentials and access the
    operator grants, each tab's model choice, inbound events and the
    messages that show a token, the inbox and trips read positions, job
    reports and background tasks, reply feedback, ignored live locations,
    and the backlog, which `/backlog export` already gives out.

  A test asserts that every table with a `principal_id` column is either
  exported by a registered section or on this exclusion list. A new table
  therefore cannot be forgotten silently.
- **Signed files.** The installation keeps a 32-byte export key in the data
  directory: mode 0600, created on first export, and covered by the
  operator's data backup. Settled: it is written to a synced temporary file
  and linked into place, so racing creators agree on one key, and a key file
  that is there but corrupt is reported to the operator and never replaced. The manifest carries an HMAC-SHA256 over the
  manifest and every entry. `/restore` refuses a file whose signature does
  not verify, so editing the principal ID, or planting records, fails.
  Restoring into another installation needs that installation to hold the
  same key, which the operator copies deliberately. That is documented as a
  limit.
- **Format and limits.** The file is a zip:
  - `manifest.json`: format version, export time, app version (the build's
    revision), principal ID, and per section its file, version, row counts,
    and SHA-256, all as exact bytes, with the HMAC over a fixed tag and those
    bytes. A part the signed manifest does not list is refused.
  - One JSON file per section, named `<section>.json`; a part's name must be
    flat lower-case letters, digits, and underscores, so no name is a path.
  - At most 64 entries, 10 MB per entry after decompression, and 20 MB
    after decompression in total. The reader checks these while it
    inflates, not from the headers.

  It is sent as a Telegram document named
  `home-assistant-export-<date>.zip` through the existing delivery path.
- **Restore merges by ID, per principal, and never overwrites.**
  - "Already there" is checked within the restoring principal only.
  - Some IDs, such as job and watch IDs, are global primary keys. A record
    whose ID belongs to another principal gets a new ID. The preview does
    not reveal that a collision happened, so restore cannot probe other
    accounts. Settled: the new ID is derived with HMAC-SHA256 under the
    export key from the principal, the table, and the old ID, in the old
    ID's length, so restoring again finds it already there.
  - Settled: a record of the same name counts as already there, and what
    refers to the file's record follows the one here. That holds for a place,
    route, collection, procedure, or event source of the same name, a memory
    entry of the same text, an intake record of the same source, and a
    switch or memory settings already chosen.
  - Settled: bookings, project items, and project links come only with a
    new trip or project. A collection record is a record of its own, so one
    missing from a collection already here is restored into it, checked
    against its fields as they are now.
  - Settled: a booking's check-in reminder takes the ID its trip and
    booking have once restored, derived ones included, and a reminder whose
    booking is not restored is skipped. A recurring job's next run is
    computed from the restore, so it never catches up on runs it missed. A
    repeating anchor whose next run has passed waits for its resolver.
  - The owners' caps hold, so a record beyond one is skipped and listed.
    Settled: each cap is counted again inside the section's write
    transaction, and a place watch or a job on a place is written under the
    location service's place guard, so a record created or a place deleted
    meanwhile fails the section instead of passing a cap or leaving a
    dangling reference.
  - A missing record is added through the owner's own validation, so a
    restored watch or job is checked like a new one. Records that fail are
    listed and skipped. Settled: a watch of a kind whose connector is off
    here is skipped, and a job's condition is checked as a new one is,
    without looking anything up.
  - One-time jobs whose time has passed are skipped. Restored jobs and
    watches keep their enabled state.
  - Restored event sources (plan 0127) have no token until the user rotates
    one. The token hash column is required, so a restored source holds the
    hash of a token minted and dropped at once, only when writing, and no
    migration is needed.
  - A replace mode is out of scope.
- **The preview shows what changes behaviour, in full.** Counts alone could
  hide planted instructions. So the preview lists, in full:
  - every memory entry and procedure to be added;
  - every job's instruction text to be added;
  - every agent watch's instruction and condition to be added.

  These are paged over several messages when long. Other sections show
  counts and titles. Confirm goes through the approval flow.
  Settled: the file goes back with `/restore` as its caption, or `/restore`
  in reply to the message holding it. The preview's pages are sent as plain
  text, so no link, quote, or code in the user's data can hide anything, and
  are sized as sent. One confirmation offered outside any chat turn follows,
  which waits 15 minutes. The file is held in memory only, one per user,
  and dropped once its confirmation can no longer come, so after a restart
  the user sends it again. Confirming plans the file again: when it would
  add anything other than the preview showed, nothing is restored and the
  user is asked to send it again; otherwise each section is planned and
  written in turn. An agent condition on a job is shown from its own check,
  never from the description stored beside it.
- **Restore order and ID remapping.** Some sections refer to others:
  - a job's destination names a trip or a project;
  - anchors name a trip or a place;
  - weather and place watches name a place;
  - project links name collections and intake records.

  So sections restore in dependency order: places, trips, collections,
  intake records, and projects first, then jobs and watches. Each section
  returns a map from old ID to new ID, and later sections rewrite their
  references through it. A reference to a record that is neither restored
  nor already there makes the referring record invalid, so it is listed and
  skipped. It is never stored dangling.

  Settled against the landed code: a weather watch keeps the coordinates it
  was saved with, so only a place watch names a saved place, and a route
  watch names its route by name, so routes restore before watches too. A
  project link that cannot be followed is skipped on its own, and its
  project is restored without it. When a section fails, the records it would
  have added are missing for every later section.
- **Atomic per section.** Exporters belong to separate repositories, so one
  transaction cannot span them. Each section restores in its own
  transaction. The result lists each section as restored or failed, and a
  failed section leaves nothing behind. Running restore again is safe,
  because existing IDs are skipped.
- **Same principal only.** The signed manifest's principal ID must match the
  restoring user.
- **Version handling.** A section with a newer schema version than its
  exporter knows is refused, and the preview names it. An older one is
  upgraded by the exporter, or refused if it cannot be.
- **Privacy of the file.** The file contains private data, such as memory
  and places. Telegram stores sent files on its servers, the same as the
  conversation itself. `/export` says this before sending. Encrypting the
  file with a passphrase is a possible follow-up.

## Approach

1. The export registry and the `internal/export` service: the zip and its
   limits, the manifest and signing, the key file, and the restore planner
   and applier.
2. Sections for memory, schedule, watches, routes, trips, settings, and
   integration switches, and the table-coverage test.
3. Sections for the modules from plans 0121 to 0127 that have landed by
   then.
4. `/export` and `/restore` in Telegram, with the full-text preview, through
   the approval flow.
5. README and `docs/operations.md`: user export as opposed to operator
   backup, and the export key in the data directory.

## Files affected

- `internal/export` (new), `internal/wiring` (the registry)
- `internal/memory`, `internal/schedule`, `internal/watch`, `internal/routes`,
  `internal/trip`, `internal/integration`, and the later modules
- `internal/telegram` (commands), README, docs

## Validation

- Integration tests with temporary SQLite stores:
  - export from one database and restore into an empty one with the same
    key gives every section back, field by field;
  - restore into the same database adds nothing and reports everything as
    already there;
  - an edited file, an unsigned file, and a file signed with another key are
    refused;
  - another principal's signed file is refused;
  - a zip bomb, too many entries, and a nested path are refused while
    inflating;
  - a newer section version is refused;
  - an invalid record is skipped and listed;
  - a record whose ID belongs to another principal is added under a new ID,
    and the preview does not show the collision;
  - a trip restored under a new ID, and a job whose destination names it,
    are restored with the destination rewritten to the new ID;
  - a watch whose place is missing is skipped and listed;
  - a section that fails leaves nothing behind;
  - no secret column appears in any export;
  - every `principal_id` table is covered or excluded.
- A live check from the GoLand server: export, delete a memory entry and a
  watch, restore, and see both return.
- Formatting, module, test, vet, build, and plan checks.

## Risks and open questions

- Every future plan that adds user data must register a section. The
  coverage test enforces this.
- Losing the export key makes older exports unrestorable. The key lives in
  the data directory, so the operator's backup keeps it.
