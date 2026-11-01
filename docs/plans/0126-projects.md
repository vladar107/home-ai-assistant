# 0126 — Projects: a goal with its own tab, checklist, and nudges

Status: `done`

## Context

Trips (plans 0082, 0086, and 0089) show what a long-running goal looks like
in the assistant: one object, its own tab, a pinned summary kept current, and
reminders routed into the tab. Trips are specific: they have bookings,
check-ins, and a scan of mail. They stay as they are.

Other goals have the same shape but no home: "renovate the bathroom", "find a
new dentist", "prepare the tax return", "plan a birthday". Conversations
about them get lost among other tabs, and nothing tracks what is left to do.

## Goal

- The user can start a project by conversation: "let's make a project for
  finding a dentist". With tabs on, it gets its own tab:
  - it opens with a pinned summary (goal, open items, next due date, and
    status) that is kept current;
  - talking in the tab is talking about that project.
- A project has a checklist of items, each with optional due dates and
  notes. Items are added, checked off, and changed by conversation, and links
  to related things are kept: Notion pages, NAS paths, collections, and
  intake records.
- If the user asks, a project nudges on a cadence ("every Sunday, tell me
  what's next on the bathroom"). Items with due dates can get reminders that
  arrive in the project's tab.
- `/projects` lists projects and their status. A done project keeps its tab
  until the user closes it.

## Decisions

- **Its own object, modelled on trips, without changing trips.** Migration
  050 adds:
  - `projects`: principal, ID, title (≤100 runes), goal (≤500 runes), status
    (`active`, `paused`, `done`, `abandoned`), `summary_message_id`, created
    and updated times.

  Child tables carry `principal_id` with composite foreign keys, as
  `collection_records` does (`trip_bookings` has plain foreign keys).
  - `project_items`: principal, project, ID, position, text (≤300 runes),
    `done_at`, `due_on`, and a note (≤500 runes).
  - `project_links`: principal, project, kind (`notion`, `nas`, `collection`, `intake`,
    `url`), reference, and label. A collection link stores the collection's
    ID, checked against the principal's own collections, with its name as
    the label.

  There are at most 20 open (active or paused) projects per principal, 100
  items and 30 links each. The open cap is checked in the store transaction
  that creates or reopens a project.
- **Tabs generalise plan 0089's destination mechanism.** Today that
  mechanism is hard-wired to trips:
  - `Tabs.Destination` handles only `TripOf`;
  - the store matches `trip:%` subjects and joins `trips`, in the tab and
    conversation queries.

  This plan puts a small subject registry in front of it. Each kind (`trip`
  and `project`) supplies how to parse its subject, check that the object
  exists and is still open, get its title, make its tab, and clear its
  summary message when the tab is deleted. Every trip-only spot moves behind
  it:
  - `Tabs.Destination`;
  - the summary clearing on tab delete in the store's `tabs.go`;
  - the `laneTabSubject` match and the `reportSubject` cases in the
    conversation store;
  - `SubjectTrip` in `internal/conversation/types.go` and its callers.

  The registry has three parts, one per layer, because no layer may import
  the others:
  - `thread.ObjectKinds` lists the kinds, and `thread.ObjectOf` parses a
    destination into kind and ID; `TripOf` and `ProjectOf` sit on it.
  - The SQLite store maps each kind to its table, which has `id`,
    `principal_id`, `title`, and `summary_message_id`, and builds the
    prefix matches from the kinds.
  - `telegram.Tabs` holds one `objectKind` per kind it was given a reader
    for (load, open, title, summary, record the summary, icon, log events),
    and the trip tab code becomes the generic object tab code with the
    trip's kind. Its log events keep their `telegram.trip_tab.*` names.

  The SQL matches by registered prefix. Trip behaviour stays exactly as it is, and
  the trip tab tests guard that. The subject is `project:<id>`, and
  `thread.ProjectDestination` resolves it. The tab is
  made when the project is created with tabs on, or lazily on its first
  routed message. It is named after the title with a fixed 📝 icon, and its
  registry row is `named_by = fixed`. The summary is edited in place and
  re-pinned as trip summaries are. A done or abandoned project keeps its tab
  but gets no new one; its messages then go to the feed of their kind, the
  Reminders feed for reminders. With tabs off there are no feeds, so project
  messages stay in the main view like everything else.
- **Context in the tab.** A turn in a project's tab gets a project line with
  the title, goal, status, open items (at most ten, with their IDs), and next
  due date. Unlike the trip line, which is built from the title the store
  loads with the request, the line is read when the turn starts through a
  `conversation.ProjectLines` reader, since it shows more than the name.
  Each item is cut to 80 runes and the goal to 200 in the line, so it stays
  small. The scheduled run of a job routed to a project starts from the same
  line, read through the scheduler's project reader, so a nudge knows what is
  next.
- **Telegram limits.** The pinned summary and `/projects` are clipped by
  UTF-16 units to Telegram's 4096, and each `/projects` line to 120. The trip
  summary keeps its own clipping.
- **Tools.**
  - `project_create` uses act-and-tell in the user's own turn.
  - `project_update` changes the title, goal, or status.
  - `project_item_add`, `project_item_update`, `project_item_check`,
    `project_item_remove`.
  - `project_link`, `project_list`, `project_get`.

  Every change tool acts and tells like collection records. A change applies
  to the project as it is when it executes, under a per-project lock, and
  saves the whole project only while it is unchanged since it was read, so
  several items checked off in one turn all apply. Items are addressed by
  short IDs derived from the approval's ID. Undo puts back the project as the
  change found it and is refused once the project changed since; undoing
  `project_create` abandons the project rather than deleting it, so its tab
  and summary say so. The read tools are private, as the collection tools
  are. Which tools a turn is offered, and the open-project cap of a new
  project, come from a count query, and listing projects reads all their
  items and links in two queries.

  Reminders and nudges are ordinary schedule jobs. Today a destination is
  set only by code (`SetDestination`), so the job tools that create or edit
  jobs, `schedule_propose`, `reminder_set`, and `schedule_update`, gain an
  optional `project_id` (the plan named a `schedule_create` that does not
  exist). It is checked against the principal's own open projects when
  proposed and again when executed, and stored as
  `destination = project:<id>`. The model cannot name any other destination,
  and trip destinations stay set by code. `project_id` on a job whose
  destination is a trip is refused. Editing, Undo, anchors, conditions, and
  travel pause behave as for every job. On `schedule_update`, `project_id`
  `none` takes a job off its project, and leaving it out keeps the job where
  it goes. An update's Undo payload records whether the update moved the job
  to or off a project, as it records a travel flag change: only then does the
  Undo put the destination back, and it is refused once the job has been
  moved since; any other Undo leaves the destination as it is, so undoing an
  older change never reverts a later move. When a project
  becomes done or abandoned, the tool result names the jobs still routed to
  it, and the model offers to delete them with `schedule_remove` or
  `reminder_cancel`.
- **Trips stay separate.** A trip is never turned into a project. The
  guidance says to use a trip for travel and a project for anything else.

## Approach

1. Migration, store, and the `internal/project` package with its tools.
2. The subject registry, with trips moved onto it unchanged, then project
   destinations and tab creation in `internal/thread`, `internal/telegram`,
   and the store queries.
3. `project_id` on the schedule tools, with ownership validation.
4. The project line in the turn context, and summary rendering and editing.
5. The `/projects` view and command.
6. Guidance, README, `docs/architecture.md`, and `docs/operations.md`.

## Files affected

- `internal/store/sqlite/migrations/050_projects.sql`, a new store file
- `internal/project` (new)
- `internal/thread`, `internal/telegram` (a new `projecttabs.go` and view, and
  `objecttabs.go`, the trip tab code made generic, with `triptabs.go` keeping
  the trip's kind)
- `internal/schedule` (`project_id` on the tools), `internal/conversation`
  (project line, `types.go` subjects)
- `internal/store/sqlite/tabs.go` and `conversation.go` (subject matching by
  registered prefix)
- `internal/wiring`, `internal/app`, `internal/prompt`, README, docs

## Validation

- Telegram integration tests with the existing harness:
  - creating a project with tabs on makes a tab with a pinned summary;
  - checking an item edits the summary;
  - a reminder with the project's destination lands in its tab;
  - a turn in the tab gets the project line;
  - tabs off keeps project messages in the main view, and a done project's
    messages without a tab go to the Reminders feed;
  - `/projects` lists projects;
  - every existing trip tab test passes unchanged after the subject registry;
  - `project_id` naming another principal's project, or set on a
    trip-destination job, is refused;
  - deleting a project tab clears its summary message ID;
  - another principal's projects are never visible or routable.
- A live check from the GoLand server: create a test project, add items, set
  a nudge, and receive it in the tab.
- Formatting, module, test, vet, build, and plan checks.

## Risks and open questions

- Tabs depend on Telegram's private-chat topics, as trip tabs do. The
  registry rules of plan 0085 apply.
- Plan 0129 should export projects, items, and links.
