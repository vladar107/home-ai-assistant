# Home Agent

A private, production-like home assistant for up to two explicitly authorized
household members. You talk to it in ordinary language through Telegram. It runs
as a Go service on the home NAS and reaches personal and home services through
narrow, per-user integrations.

Identity, authorization, secrets, observability, and recovery are treated as
product features, not afterthoughts. Every request resolves to a known user
before any model or tool is involved. Each user brings their own model
subscription and connects their own integrations, and nothing is shared or
borrowed between users.

## What it does

- **Conversation.** Natural-language chat with per-user context, backed by the
  [Pi](https://github.com/badlogic/pi-mono) agent runtime on the user's own model
  subscription. `/backlog`, `/cancel`, `/collections`, `/export`, `/jobs`, `/memory`, `/model`, `/new`,
  `/projects`, `/restore`, `/sessions`, `/settings`, `/tasks`, and `/why` are deterministic shortcuts in the slash menu, with
  `/close` there too while tabs are on, and
  `/status` and `/privacy` answer when typed. Asked how to
  set something up, the assistant reads its own help, built from the same
  `/settings` pages and command list the user sees.
- **Earlier conversations.** `/sessions` lists recent conversations to resume,
  and replying to an earlier answer continues its conversation. The assistant
  can search and read the user's own earlier conversations when asked about
  something discussed before.
- **"Why did you say that?"** Replying `/why`, or a short "how do you know?" or
  "where is that from?", to any of the assistant's messages, a chat reply, a
  scheduled report, a background task's report, a watch alert, or an assessed
  event, shows what it was based on: the tools that ran in order with their
  outcomes, a short label of what each looked at, such as a page's address or a
  search's query, whether outside or private content was read, and the model
  that answered, all from a record code kept, then a few sentences of
  explanation from one turn without tools on the user's own subscription and
  the runtime's default model. A bare "why?" goes to the conversation as
  before. A watch alert checked in code is explained from the watch's rules
  with no model turn. The record holds no arguments or results and is kept 30
  days.
- **Tabs.** With topics turned on for the bot in BotFather, each tab of the
  private chat is its own conversation, and tabs answer in parallel. `/new`,
  `/cancel`, and `/sessions` act on the tab they are sent in. Resuming an
  earlier conversation opens it in a new tab with its earlier messages
  forwarded in. After the first answer of a conversation, a tab the bot
  opened, or one opened without a name, gets a short name and an icon that fit
  the topic, chosen once per conversation on the user's own subscription. A
  tab the user named, or renames, keeps that name, and so does every tab that
  was open before this was added. A conversation another tab holds is listed
  with that tab's name, and Open posts there to bring it to the top, since a
  bot cannot switch the user's tab. `/close`, or Close in `/sessions`, ends a
  tab's conversation, which stays in `/sessions`, and deletes the tab. A tab
  Telegram no longer finds is noticed on the next send there: its messages go
  to the main view, and its conversation can be resumed in a new tab. The
  client opens a tab for anything sent from the main view, so a command that
  is the first message of a tab just opened is answered in the main view, and
  that tab is deleted. With topics off, the chat is one conversation as
  before.
- **Feed tabs.** With tabs on, nothing arrives in the main view unasked, so it
  can be muted without losing a notification. Messages nobody asked for go to
  tabs the bot makes on first use, each with a fixed icon: Reminders
  (reminders, Trips scans, and suggested trips), Reports (scheduled job reports
  and Heads-up), Home (Home Assistant watches), Watches (the other
  watches), Events (inbound events), and Inbox (the inbox check's summaries and
  cards). Every message keeps its sound; muting a feed tab quiets it. A
  deleted feed tab comes back with its next message. In a feed tab each job
  and watch has its own conversation: replying to a report or an alert
  discusses it, and typing right after one does too, without replying. An
  answer that moves to another job's or watch's conversation starts with a
  line naming it, such as "About “Bins”:", and `/new` starts a fresh
  conversation there. A reply to a conversation another tab holds names that
  tab instead of running.
- **Trip tabs.** With tabs on, each planned trip gets its own tab, named after
  the trip with a ✈️ icon. It opens with a pinned summary: the destination, the
  dates, each booking with its reference, and when its check-in opens. The
  summary is kept current as the trip changes, and says when the trip ended
  or was cancelled. The trip's check-in reminders, its start and end notices,
  and changes the Trips scan finds arrive in the tab. Talking there,
  or replying to its reminders, is a conversation about that trip, and `/new`
  starts another one about it. A deleted tab of a trip still ahead comes back
  with its next message; `/close` removes the tab of a trip that is over.
- **Act and tell.** A clear request to set up or change a scheduled job, a
  watch, a saved route, a memory instruction, or a Notion property is done at
  once. A "Done" message follows the reply with an Undo button per action,
  which works once for 24 hours. Once the conversation has read anything from
  outside the household, such as a web page, mail, or a file the user sent,
  the same changes wait for Confirm.
- **Notion.** Search, read pages, and query data sources. A property update the
  user asked for runs at once with Undo; new pages and appended content are
  proposed and run only after Confirm.
- **Gmail.** Read-only search and reading of the user's own mailbox.
- **NAS files.** Browse and read the user's own folders, receive files in the
  chat, and file photos into an inbox after confirmation.
- **Web.** Search, page reading, PDF text, and weather. After the conversation has read
  outside content, each search query is checked, and one that could carry data out waits
  for Confirm.
- **Trains.** NS trips, departures, and disruptions, checks of the exact
  planned departures a user names, one normalized status each, and the
  OV-fiets rental bikes available now at a station.
- **Home.** Reads and controls the devices exposed to Home Assistant's Assist,
  and answers from their history, as totals or as daily, weekly, or monthly
  trends. Everyday actions run immediately; sensitive devices are confirmed.
- **Monitoring.** Read-only questions about the household's Grafana for the
  operator: dashboards, metrics, and logs, plus active alerts and alert history.
- **Buses.** Live departures, direct buses, and operator disruption notices from
  OVapi, trips at any time or with changes planned by Transitous, and checks of
  one planned departure of a line.
- **MCP servers.** Read tools of Model Context Protocol servers the operator
  lists in a catalog, such as a listings search or a price tracker. Each user
  turns a server on in `/settings`, and connects their own key where the server
  needs one; only the tools the catalog allowlists are offered, all of them
  reads, so an agent watch can use them too ("tell me when this drops below
  €100"). Their results count as outside content. Servers are reached over
  HTTP; one that only speaks stdio runs as a sidecar container set up outside
  this repository, never inside the application.
- **Shared location.** Share a pin or a live location in the chat and ask about
  "here": buses and weather at that point, and nearby pharmacies, shops, or cafés
  from OpenStreetMap. Only the latest position is kept, briefly. "Save this as
  Gym" keeps a shared position as a named place with a radius, until a day
  after the user deletes it, for Undo; places are only ever saved when the user
  asks. While a live
  location is shared, the assistant knows which saved place the user is at, and
  arriving or leaving can alert a watch ("tell me when I leave the office") or
  run a reminder or job ("when I get home, remind me to water the plants").
  Saved places also work as weather places.
- **Voice and pictures.** Voice notes and audio files are transcribed and
  answered like typed messages, and the assistant draws pictures, or edits a
  photo the user sent, on the user's own ChatGPT subscription.
- **Day cards.** A rendered image with the day's weather and trains, sent with
  the reply as its caption.
- **Charts.** "Show the living room temperature this week" or "compare solar
  production this month with last month" draws up to four series from Home
  Assistant history, the household's metrics, or numbers a tool just found, and
  the reply's figures (change, extremes, mean, totals, outliers) are computed in
  code from every point.
- **Schedules.** Recurring or one-time model turns ("every weekday at 07:30, plan
  my trip to work") that are set up when the user asks, can change later in conversation, and
  that survive restarts, and reminders ("remind me at five to call mom") that are
  set at once and can be snoozed. The conversation knows each job's instructions
  and the reports they sent: replying to a report, or asking about "the report
  this morning", works after `/new` and restarts.
  A job meant to "only tell me when…" stays silent until it has something to
  say. A reminder or job can also follow an event instead of a clock time
  ("remind me 30 minutes before the dentist", "an hour before every meeting
  titled Planning, send me the agenda", "when my trip starts, tell me the
  weather there", "when I get home, remind me to water the plants"): it moves
  when the event moves, and ends with one line when the event is cancelled. Any job can carry a condition checked when it is due
  ("at 07:40 on weekdays, tell me only if rain is coming in the next hour"), and
  stays silent when it does not hold.
- **Heads-up.** Turned on in `/settings`, the assistant looks over the user's
  day at 07:30 and 18:30 (every trip in the window checked against saved routes
  and live trains, with an alternative when a train fails; weather, home
  devices, health and energy trends) and writes first, only when something
  needs attention. It follows a built-in method plus the user's own notes, and
  it is an ordinary job the user can move, add notes to, or pause in
  conversation.
- **Trips.** A trip the user mentions ("I fly to Lisbon on 12 March and stay a
  week") is recorded with its dates and bookings, and every turn knows about the
  active or next one. With Google mail or calendar on, an optional Trips scan
  in `/settings` reads them as soon as it is turned on, then daily at 07:30,
  and *Scan now* runs it again at any time. The first scan looks at booking
  mail from the last 180 days and the scans after it at the last 8, and each
  booking is read in full. Each new booking arrives as a card with Track and Ignore
  buttons; nothing is planned until Track is pressed, and an ignored trip is
  never offered again. Asked about trips in chat, the assistant searches mail
  and calendar before it says there are none, and mentions when the scan is
  off. Each booking with a check-in or pickup
  time (flight, hotel, train, car) reminds the user in their own chat when
  check-in opens. The time comes from the booking itself, else from the
  provider's website, else it is 24 hours before the booking starts, and the
  reminder says which. Recurring jobs and watches marked "pause while
  traveling" stay silent during a trip, resume by themselves, and are named in
  one notice when the trip starts and one when it ends. `/jobs` and `/watches`
  show and toggle the mark.
- **Memory.** Each user has a small memory of instructions and facts that the
  assistant keeps across `/new`, scheduled runs, and restarts. Facts are saved
  at once, and so are instructions the user states, with Undo, unless the
  conversation has read device or outside content. A fact after outside
  content, an instruction after either, and lessons from a 👎 are confirmed
  first. After a 👎, the assistant may
  propose one instruction so the mistake does not recur, and when a correction
  states a lasting preference it offers to keep it. After a request that took
  several steps, it may offer to keep how it was done as a procedure, confirmed
  on a card that shows the steps; later turns list only procedure titles, and
  the assistant reads a procedure's steps, in chat and background tasks, when a
  request matches it. With the weekly review turned on in `/settings`, every
  Sunday evening the assistant looks back at the replies marked 👎, the tools
  that failed, and the kept instructions and procedures, and offers at most
  three memory changes, such as merging instructions that overlap, and two
  backlog items as one card in the Reports tab; a quiet week sends nothing.
  `/memory` lists and deletes entries and procedures, marking learned ones, and
  `/settings` turns memory, learning from 👎, learning procedures, or the
  weekly review on and off.
- **Collections.** Small typed lists kept in the assistant's own database, with
  no integration: "keep a list of things I lend out, with who and when" proposes
  a collection with typed fields (text, number, money, date, date and time, yes
  or no, choice, or person) on a Confirm card, and records are then added,
  changed, checked off, and removed in conversation, at once with Undo. "What
  have I lent to Sam?" or "how much did I spend on gifts?" is answered from the
  records, with filtering, sorting, totals, and grouping done in code. Each turn
  carries only the collection names and record counts. `/collections` shows
  each collection's records and deletes records or whole collections. A user
  keeps at most 20 collections of 20 fields and 2000 records each.
- **Projects.** A goal worked toward over days or weeks, such as "let's make a
  project for finding a dentist": a title, a goal, a checklist of items with
  optional due dates and notes, and links to related Notion pages, NAS paths,
  collections, intake records, or web addresses, all changed in conversation at
  once with Undo. With tabs on, each project gets its own tab with a 📝 icon and
  a pinned summary of its open items and next due date that stays current;
  talking there is about that project, and each turn names its open items.
  A nudge ("every Sunday, tell me what's next on the bathroom") or a reminder
  for an item is an ordinary job routed to the project's tab. A done project
  keeps its tab until `/close`, and the assistant offers to delete the jobs
  still routed to it. `/projects` lists them. Trips stay trips. A user keeps
  at most 20 open projects of 100 items each.
- **Content intake.** A photo, PDF, or forwarded message sent without a
  question, or with "what is this?", is answered with what it is and who it is
  from, its dates, amounts, and references as found, and what the user must do
  and by when. The same reply proposes up to five follow-ups on one Confirm
  card, such as a reminder before a deadline, filing the file in a NAS folder,
  a Notion row, a collection record, a watch for a delivery, or a trip; nothing
  runs without Confirm, because the content is someone else's. The facts are
  kept as an intake record, never the content itself, so "when was that
  invoice due?" is answered later. With Google mail on, an optional Inbox
  intake switch in `/settings` checks new mail every three hours between 08:00
  and 22:00, and *Check now* runs it at once: mail that needs action is
  summed up in the Inbox tab with its follow-ups on one card, at most ten at a
  time, and mail that needs nothing sends nothing. While an inbox card waits,
  later checks send their summary and keep their follow-ups for the next card.
- **Backlog.** Ideas for developing the assistant itself ("add to the backlog:
  answer weather questions with a chart") are kept as a per-user backlog that
  never changes how the assistant behaves. `/backlog` marks items done or
  deletes them, and `/backlog export` sends them as Markdown to paste into a
  development session. On the home network, a development session reads the
  whole backlog and marks items done through `/api/backlog`.
- **Export and restore.** `/export` sends the user one file with what they set
  up and keep here: memory and procedures, jobs and reminders, watches, routes,
  trips with their bookings, saved places, collections, projects, kept facts
  from incoming content, event source names, and the memory and integration
  switches. Secrets, connections, tokens, conversations, and records of what
  ran are never in it. Sending the file back with `/restore` as its caption
  first lists everything it would add, every memory entry, procedure, job
  instruction, and agent watch check in full, then adds only what is missing,
  never changing what is there, once confirmed. The file is signed by the
  installation, so an edited file, one from elsewhere, or another user's is
  refused.
- **Background tasks.** Long requests, such as a month of mail or research across
  many pages, run as a sub-agent beside the chat and report back as a reply;
  `/tasks` shows and cancels them.
- **Watches.** "Tell me if my 07:42 is cancelled or more than 5 minutes late":
  a departure, saved route, or set of stations is checked in code during a daily
  window, and the user hears only when something changes, with Mute today and
  Pause buttons; `/jobs` lists them. Train and bus departures, saved routes, and NS
  disruptions at stations are checked, and so are Home Assistant devices: "tell me
  if the balcony door stays open for 10 minutes", "warn me if the freezer goes above
  -15 °C", or "tell me when the washing machine is done". "Tell me when rain is
  coming" checks the 15-minute precipitation forecast at a place, also in code,
  and "tell me when I leave the office" alerts on leaving a saved place while a
  live location is shared.
  Anything else the
  assistant can look at is watched by a model turn per check on the user's own
  subscription: "tell me when the return flight drops below €150", "tell me when a
  mail from the tax office arrives", or "watch this page for the release date". A
  watch set up in another language sends its messages in that language.
- **Inbound events.** The user's own systems, such as a backup script or a home
  automation, message them through an event source set up in `/settings`: each
  source has its own token, shown once, for `POST /api/events` on the home
  network. An event reaches only that source's owner, in the Events tab, either
  as it is or after one model turn on the owner's subscription decides from
  their memory whether it is worth telling them; a critical event always
  arrives. Sources can be paused, given a new token, or deleted, and
  conversation can ask what one reported.
- **Routes.** Named journeys such as "work", saved in conversation with their
  weekdays, arrival target, and legs (walking or cycling minutes, fixed trains
  and buses), changed one leg at a time, and checked in one call: each leg's
  live status and whether the arrival target still holds, so a scheduled report
  can be as short as "report today's route". `/jobs` lists and deletes them.
- **Safety checks.** Optional, per-user TypeSafe screening of outside content,
  approvals, and tool choice.

## Quick start

Requires Go 1.27. Conversation also needs Node and the pinned Pi version.

```sh
cp .env.example .env && chmod 600 .env    # bot token, your Telegram user ID, feature flags
go run ./cmd/home-agent
curl http://127.0.0.1:8080/readyz
```

Everything beyond health endpoints is opt-in through `HOME_AGENT_*_ENABLED`
variables, and MCP servers through a catalog named by
`HOME_AGENT_MCP_SERVERS_FILE`; `.env.example` lists them. Tests need no
credentials:

```sh
go test -race ./...
```

## Documentation

| Document | Contents |
| --- | --- |
| [Architecture](docs/architecture.md) | C4 context, container, and component diagrams; security model; key decisions |
| [Operations](docs/operations.md) | Bots and users, local development, NAS deployment, integrations, rollback, backup |
| [Plans](docs/plans/README.md) | The implementation record: one plan per change, immutable once done |

Monitoring (Alloy, Prometheus, Tempo, and Grafana) lives in the separate
`home-monitoring` repository.

## Repository layout

```text
cmd/home-agent/        entry point
internal/app/          configuration, Telegram assembly, and lifecycle
internal/wiring/       builds and connects every service but Telegram
internal/telegram/     Telegram client, poller, commands, /settings
internal/conversation/ turn coordination
internal/runtime/pi/   Pi subprocess and tool bridge
internal/prompt/       system prompt and [system] lines the model reads
internal/turn/         one model turn, chat or unattended
internal/turnstate/    recent messages and trust per tab
internal/provenance/   "why?" about a message, from its turn's trace
internal/tools/        tool broker
internal/approval/     Confirm / Reject proposals and direct actions with Undo
internal/exposure/     which conversations read outside content
internal/judgment/     TypeSafe screening and intent checks
internal/searchcheck/  per-query web search checks
internal/schedule/     durable jobs, reminders, and scheduled turns
internal/task/         background sub-agents
internal/watch/        windowed watches, in code or by model turn
internal/events/       event sources and inbound events from the users' own systems
internal/routes/       saved journeys checked leg by leg
internal/trip/         trips, bookings, and travel reminders
internal/transit/      shared vocabulary of the train and bus services
internal/connectors/   notion, google, gmail, calendar, nas, ns, bus, web,
                       homeassistant, grafana, places, mcp
internal/chatgpt/      transcription, image generation, and usage
internal/imagegen/     drawn pictures on the user's own subscription
internal/reaction/     emoji reaction tool
internal/modelcatalog/ models a subscription offers
internal/location/     shared positions, their expiry, and saved places
internal/memory/       per-user memory, its tools, and prompt rendering
internal/backlog/      per-user backlog of ideas for developing the assistant
internal/collection/   per-user typed collections, their tools, and the query engine
internal/project/      per-user projects with checklists, their tools, and tab summaries
internal/intake/       intake records of incoming content and the inbox check
internal/export/       a user's own data as a signed file, and restoring it
internal/clip/ localtime/ weekday/ httpretry/ testutil/  small shared helpers
internal/store/sqlite/ persistence and migrations
deploy/                Compose files and configuration examples
scripts/               operator scripts (deploy, secrets, checks)
third_party/           vendored code and assets with their licenses
```

## License

Copyright (C) 2026 Vladislav Ramazaev

Home Agent is free software: you can redistribute it and/or modify it under the
terms of the GNU Affero General Public License as published by the Free
Software Foundation, either version 3 of the License, or (at your option) any
later version (`AGPL-3.0-or-later`). See [LICENSE](LICENSE). Vendored code and
assets keep their own licenses, listed under `third_party/`.
