# Integrations

## Integration policy

Use direct, typed APIs by default. Use MCP when an existing server is mature,
maintained, permission-aware, and materially reduces adapter work. Do not expose
every internal adapter as MCP merely for architectural symmetry.

Each adapter must define:

- authentication and credential ownership;
- supported capabilities and risk classes;
- timeouts and rate-limit behaviour;
- pagination and maximum result sizes;
- error normalization;
- fields safe to log;
- test or health-check strategy;
- polling or webhook requirements.

Adapters are called **connectors** in the module model. A connector owns
source-specific authentication, schemas, API behaviour, and normalized errors;
it does not own a domain workflow. For example, Gmail exposes bounded message
reads while the job-apply pack decides whether a message changes an application
proposal. Notion exposes typed queries and writes while job-apply and mentoring
retain their separate domain invariants.

Connectors are statically compiled modules and may be shared by several
automation packs. Sharing implementation never shares credentials or grants:
every call carries a principal-bound execution context through core policy.

## Telegram

### Role

Telegram is the user interface and notification channel.

### Connectivity

Use Bot API `getUpdates` long polling. The NAS initiates all traffic to Telegram;
no webhook or public home endpoint is required. Telegram does not permit polling
and webhooks simultaneously.

### Identity

- Accept private chats only initially.
- Map the numeric Telegram user ID to a principal.
- Do not use usernames as authorization identifiers.
- Reject or silently ignore unknown identities before runtime invocation.

### Interaction primitives

- Text and Markdown-safe responses
- Received photos and files subject to Telegram size limits: a photo is passed
  to the model with the message that carried it, and any other file is read
  only when the model asks for it by id, as text or, for a PDF, as its
  extracted text
- Sent files: a NAS file the user asks for is uploaded to their own chat as a
  document, streamed from the mounted root without passing through the model,
  bounded at 20 MB and addressed by principal rather than by chat
- Voice, audio, and video are answered with an explicit refusal, because no
  model reachable through the runtime accepts them
- Inline confirmation/cancellation buttons
- Scheduled reports: a job's reply is sent unquoted to its owner's own chat
  under the job's name; `/jobs` lists the owner's jobs with pause, resume,
  run-now, and delete buttons
- Progress messages for long operations
- Deterministic `/status`, `/privacy`, `/cancel`, `/new`, and `/settings`
  commands; settings and confirmations use inline buttons rather than
  further commands

Reference: [Telegram Bot API](https://core.telegram.org/bots/api).

## Pi runtime

### Role

Pi is the selected prototype implementation of `AgentRuntime`. It should provide
model interaction, streamed events, conversation state, cancellation, retry,
and compaction while the Go application retains policy and integration control.

### Prototype deployment

Run Pi as a child process using strict JSONL RPC over stdin/stdout. The Go
adapter owns process lifecycle, timeout, cancellation, event translation, and
health checks.

### Lockdown

The generic runtime process must not inherit coding-agent authority:

- disable built-in tools;
- disable discovered extensions, skills, templates, themes, and context files;
- use an explicit working directory;
- avoid unrestricted shell and file tools;
- mount only required runtime state;
- use one isolated profile/process where credential separation requires it.

RPC mode has no host-side tool protocol; tools exist only when an extension
inside the Pi process registers them. The application therefore bridges
broker-granted tools with its own embedded extension, loaded explicitly with
`--extension` while `--no-extensions` and `--no-builtin-tools` stay in force.
The extension registers exactly the definitions written for the turn and
forwards each call to a loopback listener with a per-turn bearer token; the
core broker validates the grant and bounds the call before any connector runs.
Pi persists tool arguments and results in the principal's session file
alongside the conversation history. See
[plan 0009](plans/0009-model-tool-calls-through-pi.md).

Web search is the one tool that runs inside Pi rather than through the broker.
For a principal with web access on, the turn also loads a vendored copy of the
third-party `pi-web-search` extension (MIT, 1.6.0, in
`third_party/pi-web-search/`) with a second `--extension`, still under
`--no-extensions` and `--no-builtin-tools`. It calls the provider's native
search through the principal's own model login, so queries go only where the
conversation already goes and need no key of their own. It is vendored and
copied into the image, never installed at build time, so the code that runs is
the reviewed code in git. See [plan 0026](plans/0026-web-weather-and-pdf-tools.md).

### Authentication and lifecycle caveat

Local login and reuse by two new headless Go-driven processes are verified for
Pi’s `openai-codex` provider. Token refresh after expiry, NAS deployment,
multi-user isolation, and intended-use limits still require validation. See
[Pi subscription from Go](research/pi-openai-subscription-from-go.md).

References:

- [Pi providers](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/providers.md)
- [Pi RPC mode](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/rpc.md)
- [OpenAI authentication](https://learn.chatgpt.com/docs/auth)

## Immich

### Initial capabilities

- Text/semantic search
- Date and metadata filters
- Person and album lookup where permitted
- Preview delivery
- Original-file delivery with explicit user request and size checks

### Authentication and authorization

Use a dedicated read-only API credential if Immich supports sufficient scope.
If credentials are broader than required, enforce capability restrictions in the
tool broker and isolate the container. Shared credentials do not imply that both
principals receive every Immich capability.

### Data handling

In strict-local mode, Immich and NAS-file search return only an opaque result-set
status and count to the runtime. Metadata, filenames, OCR, embeddings, snippets,
thumbnails, and originals remain outside the model boundary; a deterministic
local presenter handles any separately authorized Telegram delivery. Model
access to an explicitly selected thumbnail, photo, file, or derived content is a
future scoped, auditable disclosure capability rather than a search default.
Telegram delivery should stream from source to destination rather than persist
duplicate files where possible.

## NAS

### Initial capabilities

- List the folders granted to a user, and the folders inside them
- Search file and folder names
- Read the text content of a selected file, or a PDF's extracted text, only
  when the user asked for it
- Add a file the user sent in the conversation, after an explicit confirmation,
  to the user's own inbox or to a folder they name
- Report storage, disk, array, temperature, and backup health where available

### Access approach

File access is through directories mounted into the container, decided in
[plan 0021](plans/0021-nas-file-access.md). The application already runs on the
NAS as the household's own user, so there is no NAS account and no NAS password
anywhere in this system.

Per-user SMB with dedicated accounts was built first and abandoned. SMB is
share-scoped and NAS OS defines the personal-folder share as `path = %H`, which
resolves to the home of whoever authenticates. A dedicated agent account
therefore reaches only its own home, and no share exposes a person's home to
anyone else. Reaching the household's own files over SMB would have required
storing each person's real NAS password, which is a worse trade than losing
NAS-enforced scoping.

Each principal is granted a named set of mounted directories in the roots file.
A path the model writes always starts with one of those names, is resolved
inside that directory, and is checked again after the filesystem has followed
every symlink, so a link planted inside a root cannot reach outside it.
Symlinks are omitted from listings rather than followed.

Because one process serves the whole household, the per-user boundary is this
grant rather than the NAS's own permissions. That is the cost of the approach
and the reason roots are mounted narrowly and named explicitly per principal.

The connector cannot modify or remove anything already on the NAS. It offers no
rename, move, delete, or overwrite tool; a new file is created exclusively so a
name in use is refused rather than replaced; and every mount is read-only except
the inbox directories a confirmed save writes to, so the kernel refuses a write
anywhere else even if this application is wrong.

Reading a file's content is separate from browsing it. Listings and name
searches are always available to a principal with roots, while reading content
is reserved for a file the user has asked about.

System-health data may come from NAS-supported metrics, standard Linux
exporters, or a narrow vendor API. Do not mount the Docker socket into the agent.

## UniFi

### Initial capabilities

- Controller, WAN, switch, and access-point status
- Connected clients and their assigned network
- Newly observed or unrecognized clients
- Recent disconnects and basic Wi-Fi diagnostics

### Authentication

Use a dedicated read-only local API credential when available. The existing
Terraform API key should not automatically become the runtime credential.

### Network placement

The controller is at `<router-address>`. The NAS is on Home VLAN, which currently
has management and cross-VLAN reach. The application should nevertheless use
only the controller endpoint and capabilities it needs.

Write operations such as blocking a client or changing a network remain
unavailable until exact approval and recovery flows exist.

## Gmail

### Initial capabilities

- Search using Gmail query syntax (`gmail_search`), returning headers, system
  labels, and Gmail's snippet
- Read one message's text (`gmail_read_message`): the plain-text part, or the
  HTML part reduced to text, with attachments listed but never opened
- Scheduled prompts use both tools, so a recurring mail summary is a job rather
  than a feature

Urgent-mail detection and notifications are later work on top of these tools.

### Authentication

OAuth per principal with `gmail.readonly` only, through the household's own
Google OAuth client ([plan 0025](plans/0025-gmail-read-connection.md)).
Google's device-code flow does not permit Gmail scopes, so the user completes
an authorization-code flow with PKCE from the phone: `/settings` sends the
consent link, the browser fails to open the client's loopback redirect, and the
user pastes that address into the chat, where it is intercepted before the
model and deleted. The refresh token is stored in the principal's own
credentials file; access tokens stay in memory. One mailbox belongs to one
principal, and disconnecting revokes the grant at Google.

### Change detection

Start with five-minute polling and deduplicate Gmail message IDs in SQLite.
Queries can use Gmail’s `q` parameter. Later, Gmail `watch` plus a Cloud Pub/Sub
pull subscription can reduce polling without creating inbound home access.

The urgent-mail, morning-briefing, and job-application packs may consume Gmail
events. Initially the job-application consumer produces reconciliation
proposals only. Gmail remains read-only and cannot draft or send mail.

References:

- [List Gmail messages](https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.messages/list)
- [Gmail push notifications](https://developers.google.com/workspace/gmail/api/guides/push)

## Google Calendar

### Initial capabilities

- Today’s agenda
- Events in a requested range
- Availability summaries
- Event details from explicitly selected calendars

Use OAuth per principal and read-only scopes first. Calendar writes require
confirmation and idempotency because retries can create duplicate events.

## Notion

### Current capabilities

Implemented by [plan 0010](plans/0010-notion-workspace-connection.md) as read
tools offered to the model through the core broker:

- `notion_search`: pages and data sources shared with the user's connection,
  by title, with IDs, URLs, and key properties
- `notion_get_page`: one page's properties and flattened text content
- `notion_query_data_source`: entries of one data source with Notion filter
  and sorts objects passed through for Notion to validate. A table nested in
  a page appears in that page's content as a `[database]` line carrying its
  database ID, and the tool accepts that ID: a rejected query is resolved
  with `GET /v1/databases/{id}` and repeated against the database's single
  data source, or answered with the data source names and IDs to choose from
  when the database holds several.

[Plan 0011](plans/0011-confirmed-notion-writes.md) adds confirmed writes,
offered only while the user's writes switch in `/settings` is on:

- `notion_create_page`: a page under a shared page or in a data source, with
  a title, property values, and body paragraphs
- `notion_update_page`: property values on a shared page
- `notion_append_blocks`: paragraphs appended to a shared page

A write tool only records a proposal through the core approval service; the
bot shows the exact proposal after the reply, and the same user confirms or
rejects it. A confirmed proposal is executed once with that user's token;
creates and appends are never retried after an ambiguous failure, updates are
retried once on a transient error.

Every request carries a ten-second deadline, one retry on `429` or `529`
honouring `Retry-After` up to five seconds, a page size capped at 25, and
normalized error classes (unauthorized, restricted, not shared, rate limited,
invalid, unavailable). Page content is capped so a rendered page stays below
the broker's result limit. Responses are never logged.

### Authentication

Each principal connects their own token; the token belongs to one principal
and is never shared or used as a fallback. The recommended credential is an
internal connection with read, update, and insert content capabilities, shared
only with the pages and data sources the assistant should see, which provides
the primary content boundary. A personal access token is accepted for a user
who cannot create a connection; it acts as its creator with everything that
person can access and expires within a year. The token is stored only in
`data/integrations/<principal>/notion.json`; SQLite keeps the workspace,
bot name, token kind, and writes flag. `GET /v1/users/me` verifies a token
before it is stored or adopted.

Notion offers no device-code flow and its OAuth flow needs a public redirect
endpoint, so the token is copied from Notion's developer portal and installed
either by the operator with `scripts/notion-set-token` or, when chat connect
is enabled, pasted by the user after pressing Connect Notion in `/settings`.
The pasted message is deleted before verification and never reaches the model.

### Data handling

Notion content read through the tools reaches the user's own model
subscription; this is inherent to the feature and stated in the connect
instructions and `/privacy`. Pages are untrusted content: the broker labels
results as data, and every write needs the user's confirmation of the exact
proposal.

Do not attempt to mirror the whole workspace. The connector does not infer
domain status transitions, mentee identity, or publication rules; those belong
to their automation packs.

References:

- [Notion internal connections](https://developers.notion.com/guides/get-started/internal-connections)
- [Notion API overview](https://developers.notion.com/guides/get-started/overview)

## GitHub

### Initial capabilities

- Pull requests awaiting the user’s review in selected repositories
- User-owned open pull requests and merge/check status
- Failed or cancelled Actions runs
- Assigned issues
- Releases and security state where explicitly permitted

### Authentication

Prefer a private GitHub App installed only on selected repositories. Request
read-only permissions for the actual endpoints used. GitHub Apps provide
short-lived tokens and repository-level installation boundaries.

The generic authenticated-user notifications endpoint currently does not work
with GitHub App tokens or fine-grained personal access tokens. Initial design
therefore queries selected repositories rather than reproducing the complete
GitHub notification inbox.

No webhook is required initially. Poll using conditional requests and respect
rate-limit headers. A webhook can be reconsidered if polling becomes inadequate.

References:

- [When to build a GitHub App](https://docs.github.com/en/apps/creating-github-apps/about-creating-github-apps/deciding-when-to-build-a-github-app)
- [GitHub notification endpoints](https://docs.github.com/en/rest/activity/notifications)

## Web

### Current capabilities

- Search the web (`web_search`) through the vendored Pi extension described
  under [Pi runtime](#pi-runtime), on the principal's own model subscription
- Read a public page, text file, or PDF by its address (`fetch_page`): HTML is
  reduced to its title, visible text, and links, and the text comes back in
  12 KB windows with an offset to continue
- Forecast the weather for a place name (`weather_forecast`) through
  Open-Meteo's keyless geocoding and forecast APIs: current conditions and up
  to seven days, or with a `date` (today to 15 days ahead) and optional
  `from_hour`/`to_hour`, one line per hour for that day, so a day's travel plan
  gets the weather for each place at the hours the user is there; metric, in
  the place's own time zone; the optional
  `HOME_AGENT_WEATHER_HOME` names the household's place for questions that name
  none
- Scheduled prompts may use all three, since each is a read

### Access

One per-user switch, "Web access" in `/settings`, covers all three. While the
global `HOME_AGENT_WEB_ENABLED` flag is on, it is on for every user until they
turn it off; while the flag is off, the section is hidden and nobody has web
access. Turning it off saves an opt-out integration row without a credential;
turning it back on deletes the row. Page reading and weather are core
Go tools behind the broker, so they get its grant recheck, deadline, size
limit, untrusted-data prefix, and audit log. Every request they make goes
through one guarded client that refuses the home network; see
[Security and privacy](05-security-and-privacy.md#network-security).

### PDF text

PDFs sent in the chat, read from the NAS, or fetched from the web go through
one pure-Go extractor (`internal/pdftext`, over `github.com/ledongthuc/pdf`).
It is not behind the web switch, since attachments and NAS files never leave
the house. Multi-column layouts and unusual font encodings extract poorly; a
scanned PDF with no text layer is reported as unreadable.

## Monitoring

The agent exposes application metrics to Prometheus and queries health through
bounded internal adapters. Grafana sends infrastructure alerts directly to
Telegram so alert delivery does not depend on a healthy agent runtime.

Monitoring credentials and data sources are administrative shared resources.
Only explicitly authorized principals may query detailed operational state.

## Recording and transcription edge worker

Job interviews and mentoring sessions share one future macOS edge worker. The
NAS initiates or coordinates a named workflow, while the Mac performs capture
and local transcription under its OS audio permissions.

Every recording requires fresh, explicit consent and language selection. Raw
audio stays in the protected local archive and is not passed to Pi. The worker
returns bounded metadata and a transcript reference through a typed,
authenticated protocol. The edge worker does not own user identity, scheduling,
approval policy, or Notion publication.

The existing job-apply and mentoring tooling remains operational during
incremental migration; wrapping proven Python commands is preferable to a
rewrite made only for language consistency.

## Apple TV and Home Assistant

Parked. Apple TV is the only present control use case, which does not justify a
Home Assistant deployment and its operational cost. Reconsider when multiple
devices or recurring home automations exist.
