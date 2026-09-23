# Scope and Capabilities

## Capability model

A capability is a typed operation with an owner, risk level, input schema,
credential scope, timeout, and audit policy. Integrations are implementation
details; capabilities are the product contract.

Capabilities are supplied through three product-level module types:

- **Connectors** provide narrow access to external systems such as Gmail,
  Notion, GitHub, NAS, UniFi, and Immich.
- **Automation packs** own domain logic such as job applications, mentoring,
  morning briefings, and home operations.
- **Edge workers** perform device-specific work such as recording and local
  transcription while the NAS application remains the control plane.

The application core owns identity, policy, scheduling, durable workflow state,
approvals, audit, and notification delivery. See
[Module and automation model](12-module-and-automation-model.md).

Risk classes:

| Class | Meaning | Default policy |
| --- | --- | --- |
| Read | Retrieves or explains data without changing the source | Allowed when granted |
| Reversible write | Creates or updates state that can be restored | Confirm initially |
| High-impact write | Deletes, publishes, sends, blocks, or changes infrastructure | Strong confirmation or unavailable |

Automation execution has a separate four-level scale:

| Level | Mode | Initial availability |
| --- | --- | --- |
| 1 | Observe without external mutation | Enabled capability by capability |
| 2 | Notify through the core outbox | Enabled with rate and privacy limits |
| 3 | Bounded, reversible, idempotent write | Later, promoted one action at a time |
| 4 | Exact action with named, expiring user confirmation | Later, after approval infrastructure exists |

## Capability inventory

| Domain | Initial capabilities | Later capabilities | Ownership | Initial mode |
| --- | --- | --- | --- | --- |
| Telegram | Receive text, photos, and files; send text/buttons and NAS files as documents | Send photos, voice messages, richer UI | Per user | Operational |
| Immich | Search by text/person/date/album, inspect metadata, return previews/originals | Create albums, upload, change metadata | Shared service with grants | Read |
| NAS files | List granted roots, search names, read text files on request, send a file to the user in Telegram | Move, rename, delete | Per user through mounted roots | Read, plus confirmed saves |
| NAS health | Capacity, temperatures, disk/array/backup status | Acknowledge maintenance tasks | Shared | Read |
| UniFi | WAN/AP/client health, known/unknown clients, recent disconnects | Block client, create guest access, change settings | Shared/admin | Read |
| Gmail | Search/read selected messages, identify potentially urgent mail | Draft, label, archive, send | Per user | Read |
| Calendar | List events, availability, daily agenda | Create/update/cancel event | Per user | Read |
| Notion | Search shared pages, read blocks/data sources, list due tasks | Create inbox item, update task/page | Per user | Read |
| GitHub | Selected repository PRs, issues, Actions and security status | Comment, create issue, rerun workflow, merge | Per user/repository | Read |
| Scheduler | Recurring questions to the user's own model, proposed in conversation, confirmed, and run with read-only tools; `/jobs` to pause, resume, run, or delete | Morning briefings and other deterministic job types, reminders, interval checks | Per user | Controlled write to local state |
| Monitoring | Query health and receive alerts | Acknowledge or silence alerts | Shared/admin | Read |
| Apple TV | None while integration is parked | Playback/device control | Shared | Deferred |

## Automation-pack inventory

| Pack | Initial capabilities | Shared dependencies | Initial mode |
| --- | --- | --- | --- |
| Morning briefing | Assemble independent calendar, mail, task, development, and home sections | Gmail, Calendar, Notion, GitHub, NAS | Observe and notify |
| Job applications | Statistics, mailbox reconciliation proposals, interview reminders and preparation | Gmail, Notion, bounded web retrieval, artifact references | Observe and notify |
| Mentoring | Session preparation, commitment reminders, mentee/roadmap lookup | Notion; recording edge worker later | Observe and notify |
| Home operations | NAS capacity/health, network and service status | NAS, UniFi, Immich, monitoring | Observe and notify |
| Development awareness | Reviews, CI failures, issues, releases | GitHub | Observe and notify |

Job-application and mentoring writes remain outside the initial action boundary.
Their existing repositories stay operational while individual capabilities are
compared and migrated incrementally.

## Initial conversational capabilities

### System status

- “Is the home agent healthy?”
- “How much space is left on the NAS?”
- “Did any containers restart?”
- “Was the internet unstable last night?”

System status should use structured metrics and integration health records, not
model recollection.

### Personal briefing

- Today’s calendar events
- User-selected important unread email
- Notion tasks due or overdue
- GitHub reviews, failing CI, or blocked pull requests
- Household alerts such as low NAS storage or a new network client

The briefing is assembled from independent sections. One failed integration
does not suppress successful sections.

### Urgent email

The system checks new mail, applies deterministic priority rules, optionally
classifies ambiguous messages, and sends one Telegram notification per matching
message. It does not modify the mailbox initially.

### Personal knowledge lookup

- Search Gmail for receipts or travel details
- Search Notion for notes and tasks
- Search NAS files by safe metadata
- Search Immich for related photos
- Correlate results in a response without copying all source data into an index

### Development awareness

- Pull requests awaiting review
- Own pull requests with failing or incomplete checks
- Assigned issues
- Releases in selected repositories
- Dependabot or security state where explicitly permitted

The generic GitHub notification inbox is not an initial capability because its
authentication support differs from selected-repository APIs.

## Background workflows

| Workflow | Trigger | Model usage | Delivery |
| --- | --- | --- | --- |
| Morning briefing | Local-time cron | Summarize gathered facts | Telegram |
| Urgent Gmail | Five-minute interval initially | Classify only uncertain candidates | Telegram |
| GitHub attention | Five- or ten-minute interval | Optional prioritization | Telegram or briefing |
| Notion due tasks | Daily or included in briefing | Usually none | Telegram or briefing |
| Job-application reconciliation | Manual, then scheduled after validation | Classify only ambiguous evidence | Proposed changes in Telegram |
| Mentoring preparation/reminder | Manual or calendar-relative schedule | Optional summarization | Telegram |
| Integration health | Interval | None | Metrics/Grafana |
| NAS capacity/availability | Prometheus alert rule | None | Direct Grafana → Telegram |

## Access and ownership rules

- Gmail, Calendar, Notion, and GitHub credentials belong to one principal.
- A principal cannot ask the model to switch to another user’s credentials.
- Immich, NAS, UniFi, and monitoring may use shared service credentials, but
  access still requires an explicit capability grant.
- Scheduled jobs inherit one owner and cannot run with another owner’s context.
- Telegram group chats are out of scope initially; accept private chats only.
- Unknown users receive no integration-derived information.

## Initial action boundary

The first implementation remains read-only outside local application state.
Local writes required for operation are allowed:

- saving sessions and summaries;
- storing schedules and last-run state;
- recording audit metadata;
- maintaining deduplication keys;
- queueing and marking notifications delivered;
- refreshing authorized OAuth tokens.

Creating calendar events, writing Notion pages, sending email, changing an
application status, publishing mentoring notes, modifying photos, moving files,
changing UniFi configuration, and GitHub writes are later phases.

## Deferred capability triggers

Introduce a deferred capability only when a real request justifies its cost:

| Deferred item | Trigger to reconsider |
| --- | --- |
| Home Assistant | Multiple controllable devices or recurring automation needs |
| RAG/vector index | Repeated evidence that native search cannot retrieve needed content |
| Raspberry Pi edge service | A sensor, BLE, IR, GPIO, or independent probe use case |
| Multi-agent orchestration | Independent specialist work with measurable benefit over one runtime |
| External workflow engine | Explicit SQLite-backed state machines cannot meet measured concurrency or recovery requirements |
| Public webhook ingress | Polling latency or API limits become unacceptable |
