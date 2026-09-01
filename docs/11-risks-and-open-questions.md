# Risks and Open Questions

## Risk register

| Risk | Likelihood | Impact | Mitigation | Decision trigger |
| --- | --- | --- | --- | --- |
| Pi subscription auth is unsuitable for a persistent bot | Medium | High | Local Go/RPC auth path verified; monitor refresh and keep runtime replaceable | Refresh or intended-use failure |
| One subscription serves two users incorrectly | Medium | High | Per-user runtime/credential boundary; restrict experiment to owner; API fallback | Second-user access is required through same runtime |
| Pi coding tools remain reachable | Low after lockdown, high impact | High | `--no-tools`, no extensions/skills/context; isolated cwd; verify events | Any unexpected tool registration/call |
| Pi update breaks RPC protocol | Medium | Medium | Pin version, protocol adapter, startup compatibility check | Upgrade or incompatible event schema |
| NAS 8 GB is insufficient with Immich | Medium | Medium | Lean metrics-only stack, limits, measure memory, upgrade criteria | Swap/OOM or low headroom |
| Agent on Home VLAN has excessive reach | Medium | High | Read-only start; unique IP/rules or Services VLAN before writes | Write authority or valuable secrets added |
| Cross-user credential/session leak | Low if designed, high impact | High | Principal-bound stores/processes/grants; permission evals | Any ambiguous runtime profile sharing |
| Source content prompt injection | High | High | Treat content as data; narrow tools; deterministic policy; confirmations | Any tool proposal influenced by source instructions |
| Logs capture private content or tokens | Medium | High | Metadata-only logs, redaction, no raw provider errors | Debug capture or new telemetry backend |
| Scheduled jobs duplicate after restart | Medium | Medium | Unique occurrence keys, leases, transactional outbox | Crash/restart testing |
| Urgent-mail classifier becomes noisy | Medium | Medium | Deterministic rules first, confidence policy, feedback/preferences | Alert fatigue or false negatives |
| Gmail/Notion/GitHub OAuth expires | Medium | Medium | Refresh monitoring, per-integration degradation, re-auth docs | Repeated refresh failure |
| GitHub notification inbox needs broader token | High for inbox use case | Medium | Query selected repositories via GitHub App | User requires exact notification inbox parity |
| NAS/vendor API is unstable or undocumented | Medium | Medium | Prefer filesystem/exporter standards; narrow adapter | Vendor upgrade or missing health data |
| Terraform docs differ from live UniFi | High currently | Medium | Read live state before network mutation; reconcile separately | Any agent/network deployment change |
| Telegram account or bot token compromised | Low | High | Numeric allowlist, private chats, token rotation, disable principal | Unknown update or token disclosure |
| Cloud provider outage blocks model | Medium | Medium | Deterministic commands and alerts remain; graceful runtime failure | Availability requirements increase |
| Module boundary becomes cosmetic and packs bypass policy | Medium | High | Narrow registrar/services; no raw secrets, DB, delivery, or direct Pi access | First automation-pack implementation |
| Workflow definition changes strand in-flight instances | Medium | Medium | Version definitions; explicit state migration or finish on old version | First incompatible workflow change |
| Edge worker is impersonated or replays a completion | Low | High | Mutual authentication, event IDs, expiry, deduplication, integrity metadata | Recording-worker implementation |
| Migrated automation silently changes proven domain rules | Medium | High | Inventory invariants, observation mode, dual-run comparison, explicit cutover | Job-apply or mentoring parity mismatch |
| Shared connector leaks authority between packs | Low | High | Principal-bound execution context and resource grants on every call | A connector gains a second consumer |

## Pi and subscription questions

Verified locally on 2026-08-30:

- Pi OAuth login completed with the owner’s current ChatGPT plan.
- Two new headless Pi processes reused the persisted login through Go RPC.
- Both locked-down runs reported zero tool events.

Open questions:

1. Does Pi refresh an expired token when launched non-interactively from Go?
2. Can Pi run headlessly on the NAS after one explicit authorization ceremony?
3. Besides `auth.json`, what Pi state must be persisted and protected in the
   container deployment?
4. Can separate `PI_CODING_AGENT_DIR` profiles isolate two independently
   authorized users?
5. Is the intended personal use compatible with notifications generated for the
   second household user, or should that principal use API billing/another auth?
6. Can all built-in and discovered tools remain disabled across upgrades?
7. What memory and startup overhead does the Pi process add on the NAS?

The first experiment answers the local Go/RPC/auth and profile-reuse path. It
does not answer the remaining deployment, lifecycle, or intended-use questions.

## Product questions

### Notion

- Which existing page or data source should be the first approved root?
- Is the primary need search, due-task reminders, or Telegram capture?
- Which properties identify task owner, status, and due date?
- Should User B have any Notion capability?

### GitHub

- Which repositories should the GitHub App access?
- Which events deserve immediate Telegram notifications versus the briefing?
- Are private organization repositories in scope, and do their policies allow a
  private App installation?
- Is generic notification-inbox parity actually required?

### Gmail

- What deterministic senders, labels, and terms indicate urgency?
- What are quiet hours and escalation rules?
- How much subject/snippet content may appear in Telegram?
- Should the second user connect a mailbox independently?

### Briefing

- Preferred time and days
- Sections and ordering per user
- Behaviour on holidays/travel
- Maximum length and whether “nothing to report” should be sent

### Shared services

- Which Immich libraries/albums are shared?
- Which NAS roots may each user search?
- Which UniFi details are appropriate for each user?

### Job applications

- Which read-only workflow should migrate first: statistics, mailbox
  reconciliation proposals, or interview reminders?
- Which Notion database/schema version is the supported initial boundary?
- How many successful comparison runs are required before any bounded write?
- Which application transitions can ever qualify for Level 3, and which always
  require confirmation?
- Where should CV, job snapshot, preparation, and transcript references live?

### Mentoring

- Which preparation and reminder flow provides the first useful read-only slice?
- Which Notion roots and typed properties belong to the mentoring pack?
- Should session-note output remain proposed content until manual review?
- What authenticated transport should the NAS use to reach the Mac edge worker?
- Which parts of the current Python tooling should remain wrapped rather than be
  rewritten?

## Technical questions

- SQLite backup encryption and credential key-management mechanism on NAS OS
- Container networking support for macvlan/ipvlan and source-specific rules
- Stable Immich API and read-only credential scope
- Best standard source for NAS vendor disk/array/backup health
- UniFi local read-only API account support and version stability
- Telegram file-size strategy for large NAS/Immich originals
- Session retention and deletion controls
- Exact audit retention
- Stable module descriptor/contribution API and versioning policy
- Typed event schema compatibility and retention
- Workflow definition upgrade policy for in-flight instances
- Edge-worker mutual authentication, reachability, and offline queue behaviour
- Ownership and backup of artifact references shared with existing repositories

## Decision triggers

### Upgrade NAS memory

Upgrade when measured swap/OOM/low-headroom behaviour affects reliability, or
before adding heavier observability/VM workloads.

### Add a Services VLAN

Add when the agent gains write authority, broad personal credentials, or a
runtime/container isolation concern that host controls cannot sufficiently
limit.

### Add Loki or Tempo

Add only after a recurring diagnosis cannot be completed from metrics,
structured local logs, audit records, and correlation IDs.

### Add RAG

Add only after collecting failed representative searches and showing that
source-native search plus targeted fetches cannot meet them.

### Replace Pi

Replace or reject Pi if subscription use is unsupported, refresh is unreliable,
tool lockdown is incomplete, RPC upgrades are unstable, or operating the Node
sidecar costs more than implementing the needed runtime loop directly.

### Introduce dynamically deployed modules

Keep modules statically compiled until an independently released or non-Go
capability has a concrete lifecycle need. Prefer a versioned, process-isolated
sidecar contract over Go’s in-process plugin mechanism.

### Introduce an external workflow engine

Keep explicit Go state machines and SQLite checkpoints until measured workflow
volume, duration, or independently distributed workers exceed the single-NAS
model. Branching alone is not sufficient reason if the state machine remains
small and reviewable.

## Documentation uncertainty

The `home-network` repository is close to live state but not a complete
authoritative export. It contains uncommitted current-state changes and notes
that some firewall, Guest network, VNO, switch, VPN, DDNS, and DNS configuration
is UI-managed. Verify live UniFi state before implementing network restrictions
or moving workloads.
