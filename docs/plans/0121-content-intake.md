# 0121 — Content intake: incoming content becomes facts and proposed actions

Status: `proposed`

## Context

The user can send photos, PDFs, and forwarded messages, and the assistant can
read mail. Every write the user might want afterwards already exists as a
tool: a one-time reminder, a NAS save of the attachment (plan 0115), a Notion
row, a watch (plans 0058, 0068, 0074), and a trip (plan 0082). Plan 0042
already puts every write proposed in one turn on a single card.

What is missing is the step between them. Today the model answers whatever
the user asked about the content, and the user then has to ask for each
follow-up separately. Nothing looks at new mail except the trip scan, and that
scan looks only for bookings. Nothing remembers what a document said once the
conversation moves on.

The examples that motivated this are a letter with a payment deadline, an
invoice, a parcel notice, an appointment confirmation, and a warranty. Each is
handled the same way.

## Goal

- When the user sends a photo, PDF, or forwarded message without a question,
  or asks "what is this?", the reply says:
  - what the content is and who it is from;
  - the dates, amounts, and references in it;
  - what, if anything, the user must do and by when.
- The same reply proposes the follow-ups that fit, at most five, on the one
  card plan 0042 already builds. Examples: a reminder before a deadline,
  filing the document in a NAS folder, a row in a Notion table, a watch for a
  delivery, or a trip.
- Each item handled this way is kept as an intake record. Later questions such
  as "when was that invoice due?" can be answered without the original.
- A user who turns on **Inbox intake** in `/settings` also gets new mail
  checked on a schedule. Mail that needs action produces the same summary and
  card in an Inbox feed tab. Mail that needs nothing produces no message.

## Decisions

- **In chat, no new action machinery.** In a chat turn, the card is plan
  0042's bundle, and each item is a proposal from the write tool that owns
  it. Plan 0042 batches only chat turns, which is all this needs. Each action
  stays under its own connector's validation, switch, and executor. Intake
  adds:
  - guidance;
  - a tool that records the facts;
  - a scheduled mail scan;
  - an Inbox feed.
- **`intake_record` tool.** The model calls it once per item, after reading
  the content. Fields:
  - `source`: `photo`, `file`, `forward`, or `mail`, plus a source reference
    (attachment ID or Gmail account and message ID);
  - `kind`: a short free label, such as "invoice";
  - `title` and `sender`;
  - up to ten dated facts, each a label and an ISO date or date-time;
  - up to five amounts, each a label, value, and currency;
  - up to five references, each a label and value;
  - `action_required`: a short text, or empty.

  The tool returns the record ID, so the summary can name it. It is a write
  of the user's own derived data, not of outside systems, so it needs no
  card. It is recorded even when the conversation has outside content. The
  record holds facts the model extracted, never the raw content.
- **Outside content stays outside.** The content is third-party. Every
  proposed action therefore goes to the card, never through act-and-tell
  (plan 0079), and screening applies as today. The summary is the model's
  reading of untrusted text, and the guidance says the summary must quote
  amounts and dates as found.
- **Storage.** A new migration adds `intake_records`: principal, ID, source,
  source reference, kind, title, sender, facts JSON, `action_required`, and
  created and updated times, plus `pending_actions` JSON for scan proposals
  not yet offered. `UNIQUE(principal_id, source, source_ref)` lets the mail
  scan skip messages it already handled. Records are kept until the user
  deletes them.

  Two read tools, `intake_list` (recent, or by kind) and `intake_get`,
  answer later questions. Both are `ThirdParty`, because every record is
  derived from outside content. A later turn that reads one is exposed, so
  injected text in `action_required` cannot act without a card.
- **Inbox intake scan: model reads, code proposes.** An unattended turn has
  read-only tools (`Broker.ReadOnlyToolsFor`), so the scan cannot call
  `intake_record` or any write tool. It works as the trip scan does
  (plan 0086):
  - It is off by default, needs the Gmail switch, and runs at most hourly.
    Its default is every three hours between 08:00 and 22:00.
  - Each run is a `RunCheck`-style turn without a turn key. It reads the mail
    received since the previous run and skips travel bookings, which the trip
    scan owns. It returns strict JSON: for each message that needs action,
    the record fields and up to three proposed actions.
  - Code validates the JSON and skips messages that already have a record. It
    writes the records, then stages the actions with `approval.Offer` in the
    Inbox feed.
  - The scan's action types are a one-time reminder and an `agent` watch.
    Their proposals are built inline in the tools today, so this plan
    extracts exported builders, `schedule.ReminderProposal` and
    `watch.AgentProposal`. The tools and the scan share them, and the watch
    builder enforces the agent-watch cap. NAS filing needs the file in the
    attachment store, so it stays a chat-only follow-up.
  - Mail-derived summaries and proposals are screened, and carry the same
    outside-content warning the memory review gives its proposals.
  - One run offers at most ten actions, Offer's bundle limit. Actions beyond
    that stay in `pending_actions`, and the summary says so.
  - When an earlier Inbox card is still waiting, the run sends its summary
    but no new card, because Offer would replace the waiting bundle. Its
    actions stay pending and are offered by the first run after that card is
    resolved or expires.
  - Bundles are per tab. Checking for a waiting bundle and then offering
    would race with a chat turn in the same tab, so the check and the offer
    are one call: `OfferIfIdle`. It runs under the proposal lock and returns
    `ErrBundleWaiting`.
  - An offered action leaves `pending_actions` only when it reaches a final
    outcome: confirmed, rejected, or done. An action whose bundle was
    replaced by a chat proposal in the Inbox tab, or that expired, goes back
    to pending.
  - A run that records nothing and has nothing pending sends nothing.
  - Output goes to a new `inbox` feed (`thread.FeedInbox`).
  - The run reads at most ten messages. Beyond that it says how many were
    left for the next run.
- **Language.** Summaries and card text follow the user's language, as
  replies do.
- **The NAS stays add-only.** Filing means saving the attachment into an
  existing folder with `nas_save_attachment`. Moving files the user already
  has stays out of scope.

## Approach

1. Migration and store for `intake_records`. Add the `internal/intake`
   package with the tools `intake_record`, `intake_list`, `intake_get`, and
   `intake_delete`, and register it in the module registry.
2. Guidance in the prompt scaffolding (plan 0105): when content arrives
   without a question, summarise, record, and propose. Propose a trip
   instead of a reminder for a booking. Never propose more than five
   follow-ups.
3. Add the Inbox feed to `internal/thread` and the Telegram feed resolver.
4. Add the scan job type: the JSON contract, code-side validation, record
   writes, the reminder and watch proposal builders, `Offer` staging with
   the waiting-card rule, the `/settings` switch, and the default schedule.
5. README, `docs/architecture.md`, and `docs/operations.md`.

## Files affected

- `internal/store/sqlite/migrations/NNN_intake_records.sql`, a new store file
- `internal/intake` (new)
- `internal/prompt`, `internal/thread`, `internal/telegram` feeds and
  settings
- `internal/schedule` (job type registration, `ReminderProposal` extracted
  from `tools.go`), `internal/watch/tools.go` (`AgentProposal` extracted)
- `internal/approval` (`OfferIfIdle`), `internal/wiring`, `internal/app`
- README, `docs/architecture.md`, `docs/operations.md`

## Validation

- Integration tests with the fake model runtime and a temporary SQLite store:
  - a forwarded PDF with a deadline produces a record and one card with a
    reminder and a NAS save;
  - Confirm runs both, and Reject runs neither;
  - in a conversation with outside content, no item runs without the card;
  - a scan stages its actions on one Inbox card through `Offer`;
  - a scan while that card waits sends no new card and keeps the actions
    pending, and the next run after Confirm offers them;
  - eleven proposed actions give a card of ten, with one pending;
  - a chat proposal that replaces an Inbox card returns its actions to
    pending;
  - `OfferIfIdle` racing a chat proposal in the same tab never replaces it;
  - malformed scan JSON is a failed run and writes nothing;
  - a later turn that reads a record is exposed and cannot act directly;
  - a second scan does not record or propose for the same mail again;
  - a scan with nothing actionable sends no message;
  - the switch off means no scan job;
  - another principal's records are never visible.
- A live check from the GoLand server: send a photo of a neutral sample
  letter, and receive the summary and a card.
- Formatting, module, test, vet, build, and plan checks.

## Risks and open questions

- Each scan costs a model turn on the user's subscription. The scan is opt-in
  with a three-hour default.
- Extraction from photos can misread numbers. The summary shows what was
  read, and the card shows each action before it runs.
- Searching NAS document contents, not just names, could reuse the text this
  plan reads. It stays a follow-up.
- Once plan 0125 lands, adding a collection record becomes one more possible
  follow-up, through its own write tool.
- Plan 0129 should export intake records, but not `pending_actions`.
