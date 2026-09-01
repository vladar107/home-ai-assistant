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
- Photos and files subject to Telegram size limits
- Inline confirmation/cancellation buttons
- Progress messages for long operations
- Deterministic `/status`, `/cancel`, `/confirm`, and `/privacy` commands

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

- List directories within configured roots
- Search filename and filesystem metadata
- Download a selected file
- Report storage, disk, array, temperature, and backup health where available

### Access approach

Prefer read-only bind mounts for selected roots rather than administrative NAS
credentials. Normalize and resolve every path, reject traversal and symlink
escapes, and enforce root-level grants after resolution.

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

- Search using Gmail query syntax
- Read metadata, snippets, and selected message bodies
- Identify new unread candidates
- Produce user-specific urgent-mail notifications

### Authentication

Use OAuth per principal with `gmail.readonly` initially. Store refresh tokens in
the principal’s credential scope. Do not share one mailbox credential between
users.

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

### Initial capabilities

- Search explicitly shared pages and data sources
- Retrieve page properties and blocks on demand
- List tasks due or overdue from configured data sources
- Include selected results in a briefing

### Authentication

Use a private internal Notion integration for the owner’s workspace. A newly
created connection has no page access until pages are explicitly shared, which
provides the primary content boundary. The integration token belongs to one
principal.

Do not attempt to mirror the whole workspace. Configure one or more approved
roots, such as a Tasks data source and an Agent Inbox page.

Later writes may create an Inbox item or update a task after confirmation.

The connector must support separately configured roots and typed schemas for
general tasks, job applications, and mentoring. It does not infer domain status
transitions, mentee identity, or publication rules; those belong to their
automation packs.

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
