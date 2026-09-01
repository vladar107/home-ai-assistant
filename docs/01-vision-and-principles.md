# Vision and Principles

## Vision

Create a private home agent that helps two people find information, understand
what needs attention, and safely operate selected personal services and home
infrastructure through a single Telegram conversation.

The project is also a practical environment for learning how production agents
should be built: identity before tools, deterministic authorization, durable
background work, observable execution, safe failure handling, and replaceable
model runtimes.

The goal is not to imitate a general-purpose voice assistant. The goal is a
small system whose capabilities are grounded in services the household already
uses.

## Users

The target system has exactly two known users.

Each user has:

- an immutable Telegram numeric user ID;
- a distinct principal and conversation history;
- independent Google, Notion, and GitHub credentials where applicable;
- explicit grants for shared services such as Immich, NAS, and UniFi;
- personal schedules, notification preferences, and quiet hours.

There is no public registration or guest access. Unknown Telegram identities do
not receive tool-enabled responses.

## Intended outcomes

The system should eventually answer or perform requests such as:

- “Show me photos from our Italy trip.”
- “Find the warranty and receipt for the vacuum.”
- “What is on my calendar today?”
- “Notify me when an urgent email arrives.”
- “What needs my attention this morning?”
- “Did any unknown device join the home network?”
- “Which of my pull requests are blocked or have failing CI?”
- “What tasks are due in my Notion workspace?”
- “Is Immich, the NAS, and the network healthy?”

Early answers should be read-only and evidence-backed. Actions are introduced
only after their authorization, confirmation, retry, and recovery semantics are
understood.

## Success criteria

The first useful release succeeds when:

1. Only the two allowlisted Telegram users can invoke the system.
2. Their sessions and personal credentials cannot cross.
3. A user can query at least two real integrations through typed tools.
4. A scheduled morning briefing survives restarts and is never sent twice.
5. Urgent email notifications are timely, deduplicated, and configurable.
6. Every tool call is attributable to a user and a request.
7. Integration failures are visible without exposing private content in logs.
8. The model runtime can be replaced without rewriting integrations.
9. No inbound internet port is required.
10. Resource usage remains safe on the current small NAS, or produces clear
    evidence that a memory upgrade is needed.

## Design principles

### Identity before intelligence

Resolve the principal, load capability grants, and select the credential scope
before sending a message to the model. The model is not an authentication or
authorization system.

### Deterministic control plane

The application owns sessions, schedules, policies, confirmations, credentials,
idempotency, audit, and integration execution. The model proposes typed intents;
it does not decide its own authority.

### Read first, write later

Begin with search, inspection, and explanation. Introduce writes one capability
at a time, with explicit risk classification and recovery behaviour.

### Native search before RAG

Use Immich search, Gmail queries, Notion content search, GitHub APIs, filesystem
metadata, and structured UniFi data first. Add a local index only after measured
failures show a concrete retrieval gap.

### Runtime portability

Pi is an initial runtime hypothesis, not the architecture. Application code
depends on an `AgentRuntime` contract. Model authentication and conversation
mechanics stay behind that boundary.

### Local-first operations, not local inference

Run the control plane, state, adapters, scheduler, and monitoring locally. Model
inference remains remote initially. Personal source data should not be copied
into a second store without a demonstrated need.

### Proactive but bounded

Scheduled workflows are named, configured, and auditable. The model does not
wake itself or invent future work. Deterministic monitoring and schedules decide
when work runs.

### Least privilege by construction

Each integration receives the smallest useful scope. Shared infrastructure
credentials and personal credentials are stored and selected separately. Tools
are exposed per principal and per request.

### Observable without becoming surveillance

Record operational metadata, policy decisions, timing, and outcomes. Avoid
persisting prompts, email bodies, filenames, photos, or document contents unless
explicitly required and protected.

### Simple deployment

Prefer one Go application, SQLite, and a small monitoring stack over
microservices, queues, distributed databases, and cluster orchestration.

## Explicit non-goals for the initial system

- Public access or user registration
- General autonomous background agents
- Multi-agent orchestration
- A custom vector database or ingestion pipeline
- Local LLM inference
- A large web frontend
- Broad smart-home automation
- Home Assistant while Apple TV is the only relevant device
- Unconfirmed destructive actions
- Replacing Grafana alerts with model judgement
- Serving multiple unrelated households or turning the project into a SaaS

## Product stance on automation

The system should automate repetition, not ownership. It may discover and
summarize; the user remains responsible for consequential actions. Over time,
specific low-risk actions can become pre-authorized, but only through explicit
policy changes rather than conversational implication.
