# Technology Decisions

## Decision status

| Status | Meaning |
| --- | --- |
| Accepted | Current architectural direction; change through an explicit decision |
| Proposed | Preferred but needs implementation validation |
| Experimental | Bounded spike required before adoption |
| Deferred | Deliberately excluded until a trigger occurs |
| Superseded | Historical direction replaced by a later explicit decision |
| Rejected for now | Considered and not justified in the current scope |

## Decision register

| ID | Decision | Status | Rationale |
| --- | --- | --- | --- |
| D-001 | Telegram is the initial interface | Accepted | Stable numeric IDs, files, buttons, notifications, no custom frontend |
| D-002 | Use outbound Telegram long polling | Accepted | Eliminates public ingress, DDNS, TLS termination, and webhook operations |
| D-003 | Run the primary service on the NAS | Accepted | Always on, close to Immich/files, x86 CPU, containers and backups |
| D-004 | Keep single-board computer as an optional edge node | Accepted | 512 MB is unsuitable for the full stack but useful for sensors/probes |
| D-005 | Build a modular monolith in Go | Proposed | Low resource use, concurrency, static deployment, strong HTTP/telemetry support |
| D-006 | Keep model runtime behind `AgentRuntime` | Accepted | Prevents harness/provider lock-in |
| D-007 | Use Pi through subprocess JSONL RPC for the prototype | Accepted | Two independent Go-driven runs completed through persisted `openai-codex` OAuth with tools disabled |
| D-008 | Application owns policy, tools, credentials, scheduling, and approvals | Accepted | These controls cannot be delegated to prompt instructions |
| D-009 | Use SQLite for durable control-plane state | Accepted | Fits one instance/two users and avoids an external database; migration, integrity, restart, and readiness behaviour are validated locally |
| D-010 | Use direct APIs by default and MCP selectively | Accepted | Avoids unnecessary protocol layers while preserving useful reuse |
| D-011 | Start integrations read-only | Accepted | Reduces risk while identity, isolation, audit, and failures mature |
| D-012 | Native/source search before RAG | Accepted | Existing services already provide search; no evidence for another index |
| D-013 | Use an application-owned durable scheduler and outbox | Accepted | System cron alone cannot provide ownership, catch-up, dedupe, or delivery state |
| D-014 | Start monitoring with Prometheus and Grafana | Accepted | Prometheus provides a bounded local metrics store and Grafana provides query, dashboards, and alerts within the 8 GB constraint |
| D-015 | Defer Loki, Alloy, and Tempo | Superseded by D-030 | The original metrics-only launch was replaced when application OpenTelemetry metrics and traces became an explicit requirement; Loki remains deferred separately |
| D-016 | Park Home Assistant | Accepted | Apple TV alone does not justify deployment/maintenance |
| D-017 | No public NAS endpoints | Accepted | Telegram polling and existing WireGuard cover initial access |
| D-018 | GitHub App for selected repositories | Proposed | Granular installation scope and short-lived tokens for a long-running service |
| D-019 | Notion internal integration with selected roots | Proposed | Private single-workspace use and explicit content sharing |
| D-020 | Poll Gmail/GitHub initially | Accepted | Simple outbound-only operation; push/webhooks can follow measured need |
| D-021 | Use statically compiled Go modules, enabled through configuration | Accepted | Keeps deployment portable and the trust boundary explicit; dynamic Go plugins add compatibility and security cost without a current need |
| D-022 | Separate connectors, automation packs, and edge workers | Accepted | External API access, domain policy, and device-specific execution have different ownership and safety boundaries |
| D-023 | Use typed events and explicit SQLite-backed Go state machines for durable workflows | Accepted | Supports checkpoints, retries, approvals, and audit without a workflow DSL or external orchestration platform |
| D-024 | Keep identity, policy, scheduler, workflows, approvals, audit, runtime brokering, and outbox in the core | Accepted | Modules must not bypass cross-cutting authorization and reliability guarantees |
| D-025 | Migrate job-apply and mentoring capability by capability while existing repositories remain operational | Accepted | Preserves proven behaviour and private data while allowing output comparison and reversible cutover |
| D-026 | Target Go 1.27 and introduce dependencies at their latest stable release | Accepted | Starts from the current toolchain and avoids knowingly adopting stale packages; versions remain pinned and upgrades are reviewed |
| D-027 | Make strict-local search the default privacy mode | Accepted | The cloud model emits only a structured `SearchIntent`; local search and deterministic presentation keep files, OCR, embeddings, snippets, and preferably filenames off the model boundary, while any Telegram or future model disclosure remains separately scoped and auditable |
| D-028 | Use `modernc.org/sqlite` for the control-plane store | Accepted | Its stable pure-Go `database/sql` driver avoids CGO and simplifies macOS development plus Linux/NAS builds; version `v1.57.0` was current when introduced and is pinned |
| D-029 | Call the Telegram Bot API directly with Go's standard-library `net/http` | Accepted | The initial slice needs only `getMe`, `getUpdates`, and `sendMessage`; direct calls keep the dependency surface tiny while allowing explicit token redaction and request timeouts |
| D-030 | Instrument application metrics and traces with OpenTelemetry, receive them in Alloy, and store them in Prometheus and monolithic Tempo | Accepted | Metrics and traces are required from the first NAS deployment; a collector boundary keeps backend details out of the application while separate bounded stores remain viable on 8 GB. Grafana is the interface, not a bundled data store. Structured stdout logs retain trace/span correlation and Loki remains deferred |
| D-031 | Keep production deployment operator-triggered, build the application on the NAS from checked-out source, and run monitoring as a separate Compose project | Accepted | The NAS is the production host, so its build context, local image, persistent state, and rollback record stay together. Independent monitoring can serve Home Agent and future applications without application secrets or coupling. CI may publish an optional image artifact but has no deployment authority |
| D-033 | Keep the monitoring stack in its own `home-monitoring` repository with one Compose file and an in-place deployment directory | Accepted | The stack serves more than Home Agent, is deployed first and independently, and changes on its own cadence; a separate repository removes the shared environment file and stage-switched secret scripts, and one `deploy/` directory replaced on every deploy mirrors the application's tag-pinned layout |
| D-032 | Keep canonical runtime secrets in macOS Keychain and only required protected runtime copies on the NAS | Accepted | The Mac remains the operator-controlled source of recovery. Always-running containers receive only their own read-only files from a `0700` NAS directory with `0600` files; secrets stay out of Git, CI, image layers, environment values, command arguments, and logs |

## Go

The root application and isolated Pi spike target Go `1.27.0`. When an external
dependency is first introduced, select its latest stable release after checking
its official release notes, compatibility, maintenance state, and security
history. Pin the selected version in `go.mod`/`go.sum`; “latest” does not mean
floating versions or automatic unreviewed upgrades.

### Why proposed

- Small steady-state memory footprint
- Straightforward subprocess and JSONL handling for Pi RPC
- Good concurrency model for polling, scheduling, and adapters
- Strong standard library for HTTP and OAuth plumbing
- Mature OpenTelemetry and Prometheus ecosystems
- Cross-compilation to x86 NAS and ARM Pi
- Simple container or static-binary deployment

### Costs

- Pi’s embedded SDK is TypeScript; Go uses RPC instead.
- Some integration SDKs may be more mature in JavaScript or Python.
- SQLite driver choice adds a dependency and build trade-off.
- Agent-runtime examples are often TypeScript-first.

The Pi RPC spike verified that the runtime boundary is operationally clean
enough for the prototype. D-005 remains proposed until the service skeleton and
its steady-state resource use are validated on the NAS.

## Module system

“Plugin” is a product-modularity term in the initial design. Modules are normal
Go packages linked into one binary and registered deterministically before
ingress starts. Configuration may enable or disable a compiled module, but the
model cannot load code or grant authority.

The implemented scaffold exposes versioned descriptors, validated dependency
ordering, lifecycle, and readiness. A separate narrow contribution registrar
for tools, events, workflows, scheduled-job types, commands, metrics, renderers,
and namespaced migrations will be added with the first real connector. It will
not expose the full application, raw secrets, unrestricted SQLite, or a direct
runtime tool-registration path.

Dynamic Go plugins are rejected for now. A versioned sidecar protocol is the
preferred future extension point if independently deployed or non-Go modules
become necessary. See [Module and automation model](12-module-and-automation-model.md).

## Pi versus direct OpenAI API

### Pi advantages

- Ready-made conversation/tool loop
- Streaming, retry, cancellation, compaction, and session mechanics
- Subscription provider support to validate for personal use
- Language-neutral RPC mode

### Pi costs and unknowns

- A separate Node.js process and package lifecycle
- Coding-agent defaults must be completely disabled
- OAuth profile reuse is verified; refresh after token expiry still needs
  operational validation
- Multi-user subscription isolation and intended use require care
- Pi protocol/version upgrades become an integration contract

### Direct Responses API alternative

A direct Go implementation offers one process and an official Go SDK, but the
application must own more runtime mechanics and API usage is billed separately
from a ChatGPT subscription. It remains the primary fallback if Pi is unsafe,
unsupported, or unreliable.

The local evidence and remaining limitations are recorded in
[Pi OpenAI subscription from Go](research/pi-openai-subscription-from-go.md).

## SQLite

SQLite should hold application state, not source-system mirrors. Expected data:

- principals and identities;
- grants and credential references;
- session metadata;
- scheduled jobs and runs;
- notification outbox;
- approvals and idempotency records;
- audit metadata;
- integration cursors and deduplication IDs.

The implementation uses the pinned pure-Go `modernc.org/sqlite` driver through
`database/sql`. It enables foreign keys, WAL, a five-second busy timeout,
`NORMAL` synchronous mode, SQLite defensive mode, and disabled double-quoted
string compatibility on every connection. Embedded sequential migrations are
recorded with SHA-256 checksums so a changed or non-sequential migration fails
startup rather than silently drifting the schema. Transactional job claiming
will build on this store when scheduling is introduced.

## Local-content privacy boundary

The cloud model is an intent interpreter, not a local-content reader, in the
default strict-local mode. It may see the user query and emit a typed
`SearchIntent`. A local NAS broker performs metadata, keyword, and
local-semantic search and returns only opaque result-set status/count to the
model. A deterministic local presenter renders authorized results without a
second model pass.

This is not described as zero knowledge because the provider sees the original
query. Telegram delivery is also an independent external disclosure boundary.
Future selected-item disclosure or content-assisted search requires a distinct,
explicitly authorized, scoped, and audited capability; the model cannot switch
privacy mode itself.

## Deployment and secret custody

The GitHub repository remains private. GitHub Actions validates source and may
publish an optional image artifact; it holds no Telegram, Grafana, NAS, or
application runtime secret and has no deployment path to the home network.

The operator Mac copies the checked-out source and versioned Compose
configuration over the `home-nas` SSH alias. The NAS builds the `linux/amd64`
application image locally. The monitoring and application Compose projects come
from separate repositories and have separate deployment directories, persistent
data, and rollback records. They share only
the private `home-observability` Docker network. The encrypted SSH private key
stays on the Mac with its passphrase cached in Keychain; the NAS contains only
the public key. No public endpoint is introduced.

Keychain is canonical for runtime secrets, but an always-on service necessarily
needs a decrypted runtime copy. Each NAS copy lives outside the versioned
deployment directory, has the narrowest consumer-specific ownership, and is
mounted read-only into only that container. Absolute paths and UID/GID are not
standardized in this decision because they must be verified against the live
NAS OS volume and container identities.

## MCP decision test

Use an MCP server only when most answers are “yes”:

1. Is it maintained and compatible with the required source version?
2. Does it expose the exact required capabilities without broad authority?
3. Does it preserve per-user authentication and structured errors?
4. Does it reduce code and operational burden versus a direct adapter?
5. Can its schemas, timeouts, result size, and audit behaviour be controlled?
6. Is adding another process/protocol justified?

Otherwise implement a narrow in-process adapter.

## Deferred technologies

| Technology | Reason deferred | Reconsideration trigger |
| --- | --- | --- |
| Home Assistant | One Apple TV is insufficient value | Multiple devices/automations |
| Vector database | No measured retrieval gap | Native search repeatedly fails |
| LangGraph/workflow graph | Typed Go state machines remain explicit and reviewable | Measured workflows cannot be expressed or maintained safely |
| Temporal/external scheduler | One process and SQLite-backed checkpoints are sufficient | High concurrency or independently distributed workers |
| Dynamic Go plugins | Toolchain coupling and in-process trust cost are unjustified | Independently distributed modules become a real requirement; prefer sidecars |
| Redis/message broker | SQLite outbox meets expected load | Multiple app instances or contention |
| Kubernetes | One NAS and a few containers | None expected for this project |
| Local LLM | Hardware and use cases do not justify it | Privacy/offline requirement plus new hardware |
| Public web frontend | Telegram covers initial interaction | Clear workflow Telegram cannot support |
| Loki | Bounded stdout logs plus trace/span IDs are sufficient initially; host/container log collection adds storage and privileged access questions | Repeated incidents cannot be diagnosed within retained local logs, with a measured resource and least-privilege collection design |

## Decision process

New technology should solve a measured problem. Record:

- the concrete failure or requirement;
- alternatives considered;
- security and operational impact;
- memory/storage cost on the NAS;
- migration and rollback plan;
- evidence used to accept it.
