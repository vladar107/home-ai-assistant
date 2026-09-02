# Delivery Roadmap

## Delivery strategy

Build a walking skeleton first, then add one useful vertical capability at a
time. Every stage must leave the system deployable, observable, and no less
safe than before. Infrastructure is introduced when a vertical slice needs it,
not as an abstract framework exercise.

For the first NAS installation, "deployable and observable" means the
monitoring stack is started and verified before the home-agent container. CI
builds immutable images but never deploys; the operator selects and deploys an
exact digest manually from the Mac.

The existing `job-apply` and `mentoring` repositories remain operational until
each migrated capability reaches behavioural parity and an explicit cutover.

## Stage 0 — Architecture and runtime validation

Status: locally complete. NAS resource and credential-lifecycle validation
remain production gates.

### Delivered

- Architecture and decision documentation
- Verified Go → Pi RPC → `openai-codex` subscription experiment
- Two successful headless profile-reuse checks with tools disabled
- Go 1.27 root module and initial standard-library scaffold
- Versioned static module registry with dependency validation
- Built-in system lifecycle/readiness module
- JSON logging, graceful shutdown, and loopback-safe health endpoints
- Replaceable streaming `AgentRuntime` contract

### Remaining production evidence

- Pi memory and process behaviour on the NAS
- OAuth refresh after expiry and recovery after revocation
- Behaviour across pinned Pi upgrades
- Intended-use decision for a second household principal

## Stage 1 — Walking skeleton

Deliver a deterministic Telegram system shell without invoking a model or an
external integration. This establishes identity, persistence, deployment, and
operations before model-driven behaviour is reachable.

Status: Telegram, SQLite, OpenTelemetry instrumentation, container packaging,
CI workflows, and manual deployment tooling are locally implemented. Real
Telegram testing, image publication, Hearthwatch configuration, NAS deployment,
resource measurement, and backup/restore exercises remain in this stage.

### Deliverables

- SQLite driver decision, schema, and migrations
- Principal, Telegram identity, capability-grant, and polling-cursor records
- Environment/file configuration with secret references rather than secrets
- Telegram long polling and durable update-offset advancement
- Numeric two-user allowlist; private chats only
- Deterministic `/status` and `/privacy`
- OpenTelemetry metrics and traces exported over OTLP to Grafana Alloy
- Structured stdout logs correlated with trace/span IDs
- Separate bounded Prometheus, monolithic Tempo, Alloy, and Grafana services;
  Loki deferred
- Provisioned data sources, initial dashboard, and direct Hearthwatch Telegram
  alerts
- Non-root container build and local development commands
- Private GitHub CI publishing immutable `linux/amd64` images to private GHCR
- Manual digest-selected deployment from the Mac over the `home-nas` SSH alias
- Mac Keychain secret custody and consumer-specific protected runtime copies on
  the NAS
- Transaction-consistent backup, restore, health-gate, and image rollback
  procedure

### Acceptance

- Unknown Telegram users cannot invoke application or runtime behaviour.
- A known user receives `/status` without a model call.
- Restart resumes after the last committed Telegram update without replay.
- Invalid configuration fails before polling starts.
- Health, readiness, logs, and metrics expose no token or message content.
- SQLite state can be backed up and restored in a local exercise.
- Grafana can query application metrics in Prometheus and traces in Tempo over
  private networking, while only Grafana is reachable through an SSH tunnel.
- Grafana delivers a safe alert through the separate Hearthwatch bot without
  depending on the model or Hearthkeeper bot.
- CI has no runtime secrets or deployment authority; the NAS has no private
  registry credential.
- A failed deployment returns to the retained previous image digest.
- The application plus observability stack leaves measured safe headroom on the
  small NAS or deployment stops with an upgrade decision.

## Stage 2 — Model conversation

Connect the application to Pi only after identity and session ownership are
deterministic. Reuse the verified spike as the compatibility reference.

### Deliverables

- Locked-down Pi JSONL RPC adapter
- Per-principal sessions and conversation metadata
- Streaming response translation to Telegram
- Runtime timeout, cancellation, and deterministic `/cancel`
- Authentication-readiness health state
- Pinned Pi version and startup compatibility check

### Acceptance

- Each principal receives an independent session.
- Pi receives no tools, project context, extensions, or filesystem authority.
- Cancellation terminates the active turn and cleans up the subprocess.
- Runtime failure does not affect `/status` or corrupt control-plane state.
- After adapter parity, the standalone spike is removed and its verification
  procedure is retained in the runtime documentation.

## Stage 3 — Safety and automation core

Introduce only the reusable control-plane mechanics required by the first
automation pack. Keep workflows as explicit Go state machines rather than a DSL.

### Deliverables

- Narrow module contribution registrar
- Typed capability definitions and four automation safety levels
- Principal-bound connector execution context
- Typed event envelope and deduplication
- Versioned workflow instances, steps, and checkpoints in SQLite
- Policy evaluation, audit metadata, and result-size limits
- Approval create/expire/resolve boundary
- Durable Telegram notification outbox
- Strict-local privacy policy and typed `SearchIntent` boundary
- Principal-bound opaque result sets and deterministic local presentation

### Acceptance

- A module cannot access raw credentials, unrestricted storage, direct Pi tool
  registration, or direct Telegram delivery.
- In strict-local mode the model has no file/photo content-reading capability,
  and receives only result-set status/count from local search.
- Resolving or presenting an opaque result set rechecks its principal and scope.
- Duplicate events cannot repeat a completed side effect.
- Restart resumes a workflow from its last committed checkpoint.
- An approval resumes only the exact same-principal action before expiry.
- Sensitive source payloads do not appear in logs or audit metadata.

## Stage 4 — First automation vertical: job applications

Use the existing workflow as a behavioural oracle. Start with statistics, then
mailbox-reconciliation proposals; Gmail and Notion remain unmodified by the new
path.

### Deliverables

- Principal-bound Gmail read-only connector
- Typed Notion connector for the selected application database
- Job-application pack with campaign and monotonic-status rules
- Read-only funnel/statistics command
- Mailbox reconciliation producing proposed changes and evidence
- Comparison runs against the existing `job-apply` repository
- Bounded Telegram report through the outbox

### Acceptance

- Gmail uses `gmail.readonly` and cannot draft, send, label, or archive.
- Reconciliation performs no Notion mutation.
- Age alone never closes an application or sets Ghosted.
- Ambiguous/unmatched mail is reported without creating a card.
- Polling checkpoints advance only after a complete successful run.
- Existing job-apply automation continues operating during comparison.

## Stage 5 — Scheduling and personal notifications

Turn proven read-only capabilities into durable, user-owned scheduled work.

### Deliverables

- Scheduled jobs, occurrences, leases, and catch-up policies
- Notification retry, rate limiting, and occurrence deduplication
- Scheduled job-application status/statistics report
- Urgent-mail notification with deterministic rules first
- Calendar agenda and selected GitHub repository status
- Configurable morning briefing assembled from independent sections
- `/jobs`, pause, resume, and manual-run controls

### Acceptance

- A scheduled occurrence runs once across restart and retry.
- Telegram outage queues and later delivers one notification.
- Jobs and credentials remain isolated by principal.
- Quiet hours and per-workflow limits are enforced.
- A partial briefing names unavailable sections and sends successful ones.

## Stage 6 — Mentoring and recording edge worker

Migrate safe preparation and reminder flows before any recording or publication.
The NAS remains the control plane; macOS owns capture and local transcription.

### Deliverables

- Mentoring lookup, preparation, roadmap, and commitment-reminder workflows
- Exact/unique mentee resolution and selected Notion roots
- Proposed session notes without automatic publication
- Authenticated Mac edge-worker protocol
- Fresh consent and language requirement for every recording
- Protected audio archive, local transcription, and integrity metadata
- Compatibility wrapper around the existing proven Python commands

### Acceptance

- Ambiguous mentee matches stop without returning another mentee’s content.
- A schedule or model request alone cannot begin recording.
- Unclaimed audio blocks a new recording.
- Original audio is archived before transcription and never sent to Pi.
- Notion content is verified before the workflow reports success.
- The existing mentoring repository remains usable until explicit cutover.

## Stage 7 — Home operations

Add read-only infrastructure value after the personal automation path proves the
core security and workflow model.

### Deliverables

- NAS safe-root file and health connector
- UniFi read-only status connector
- Immich search and bounded preview delivery
- Local metadata/keyword search and optional local-semantic indexing
- Deterministic result rendering with configurable filename/metadata disclosure
- Host, storage, array, and supported container health metrics added to the
  existing Prometheus/Grafana installation
- Home-operations dashboards and backup-age checks

### Acceptance

- NAS paths cannot escape configured roots or symlinks.
- The agent has no Docker socket or privileged container access.
- UniFi credentials are read-only and distinct from Terraform where possible.
- Raw files/photos, OCR, embeddings, snippets, and preferably filenames remain
  local during strict-local search.
- Results are not passed back through the model for formatting.
- Sending a filename, thumbnail, or original through Telegram is evaluated and
  audited as a separate disclosure.
- Any future selected-item model disclosure or content-assisted search is
  disabled until its explicit scoped authorization capability is implemented.
- Grafana can report application failure without the model runtime.
- Resource usage is safe on 8 GB or produces a measured upgrade decision.

## Stage 8 — Controlled writes and production hardening

Promote one reversible action at a time, then validate deployment recovery and
security. A generic approval mechanism does not automatically authorize a new
kind of write.

### Deliverables

- One action-specific write, likely Notion Inbox or Calendar event creation
- Canonical action hash, expiry, same-principal confirmation, and idempotency
- Network-isolation decision for the NAS workload
- Restore exercise and credential-rotation procedures
- NAS Pi memory/failure measurements and runtime compatibility checks
- Evaluations based on observed failures
- Comparison with a direct API runtime on one narrow workflow

### Acceptance

- Modified arguments invalidate an approval.
- Duplicate confirmation or retry cannot duplicate the write.
- Ambiguous provider failure cannot retry a high-impact action automatically.
- Restore recovers principals, workflows, schedules, grants, and outbox safely.
- Network reach is limited to documented endpoints.
- Pi/runtime failure cannot corrupt control-plane state.

Job-application Ghosted closure remains a confirmed action. Sending email,
submitting or withdrawing applications, deleting files, changing network
configuration, and publishing mentoring notes remain unavailable until each is
individually designed and reviewed.

## Order within a vertical slice

Implementation should generally follow:

1. user, value, and deterministic domain rules;
2. state and policy boundary;
3. module descriptor and declared dependencies;
4. narrow connector/API client;
5. explicit workflow state machine when needed;
6. runtime tool exposure only when deterministic code is insufficient;
7. Telegram presentation through the outbox;
8. metrics, audit, failure, recovery, and comparison;
9. only then additional capabilities or writes.

## Definition of done for a capability

- Clear user and value
- Typed inputs and outputs
- Explicit credential owner and resource scope
- Risk class and automation safety level
- Timeouts, rate limits, pagination, and result bounds
- Safe error mapping
- Audit metadata
- Metrics and health behaviour
- Failure, retry, checkpoint, and idempotency semantics
- Privacy review
- Documentation updated
