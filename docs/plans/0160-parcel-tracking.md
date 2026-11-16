# 0160 — Parcel tracking: a live card for every parcel a user is waiting for

Status: `in-progress`

## Context

Users ask the assistant where a parcel is, and the only way to follow one today is an agent watch
(plan 0074): a model turn every interval that reads a tracking page or searches mail. That costs a
turn per check, cannot see carriers whose pages need a login, and tells the user only when a
condition holds, not how the parcel is moving.

Research for this plan, done with the operator, settled where parcel data can come from without
paying for a service:

- Carriers give recipients no official API. The ones that matter here hand out keys only to
  business customers, and aggregators with an API either charge or give a one-time allowance.
- Community Home Assistant integrations cover several carriers, either by reading the user's own
  carrier account (every parcel addressed to it, no numbers needed) or by tracking a number the
  user adds. The operator chose a small set, reviewed their code, and installs them pinned to the
  reviewed commits; the repositories and pins stay in the operator's ignored local notes.
- One large online shop's own deliveries have no tracking API at all, and logging into the shop
  account would mean storing a credential that can act as the whole account. Its shipping mail
  carries the status instead.
- Every carrier sends shipping mail, and the assistant already reads the user's Gmail (plans 0121,
  0133).

The household's Home Assistant is one instance with one token from a non-admin user (the
`homeassistant` connector). Inbox intake (plan 0121) scans new mail and offers follow-ups on one
approval card. Feed tabs (plans 0087, 0088, 0158), widgets (plans 0140, 0149), the trip loop
(plans 0139, 0153), and the approval executor pattern are what this builds on.

Carrier names are not household-private: the code names the carriers and Home Assistant domains it
supports, as the flights connector names its provider. What stays out of the repository is which
integrations, accounts, and versions the household runs.

## Goal

- A user who turns on **Parcels** in `/settings` sees one card per parcel they are waiting for, in
  a Parcels tab: carrier, what it is, status, the expected day and window, the pickup point when
  there is one, and a link to the carrier's page.
- Parcels arrive without typing: from the user's carrier accounts in Home Assistant, and from
  shipping mail. A pasted or forwarded number works too.
- The card updates silently. The user gets a new message only when it matters: out for delivery,
  ready at a pickup point, delivered, or a problem.
- Mail is the fallback for every carrier: a parcel no integration covers is followed through its
  mail, and any delivery notice the inbox check finds can become a parcel.
- "Where is my parcel?" is answered from the same data, without a web search.

## Decisions

### Sources

- **Two sources, no paid services.** Home Assistant carrier integrations, read through the existing
  household connection, and the user's Gmail. No carrier API keys, no aggregators, no shop logins.
- **A closed list of Home Assistant domains, read-only.** Parcel sensors are read only from
  integrations whose domain is on a list in code. Two attribute shapes are supported:
  - an account shape: one sensor per account whose attributes hold `enroute` and `delivered` lists
    of packages with a key, name, URL, status message, and planned and expected windows;
  - a normalised shape shared by a family of carrier integrations: incoming, en-route-to-pickup,
    awaiting-pickup, and delivered sensors, each with a `parcels` list of carrier, number, sender,
    status, planned window, delivered time, pickup flag and point, and URL.
  The `raw` and `history` attributes are never read, parsed, or logged, even on a decode error:
  they carry addresses and other people's names.
- **How entities are found is settled by a spike (chunk 0).** The plan assumes the non-admin token
  can read the entity registry with platform, unique ID, and config entry, and the config entries'
  titles. The spike confirms which registry commands that token may call and what they return,
  and the field schema of the integrations' add and remove services, including how a domain with
  several entries picks one. The fixtures are built from made-up parcels in the confirmed shapes.
  If the full registry is refused, the sensor kind comes from the translation key and grouping
  from the device registry.
- **One household-wide read.** Every 10 minutes, the discovered parcel entities are read with
  `GET /api/states/<entity_id>`, once for the household, and split by owner. The connector's
  snapshot is not used: it holds no attributes. Without a Home Assistant token, Parcels works on
  mail alone. A user's **The home** switch does not affect Parcels. A user with no assigned entry
  never waits on Home Assistant: their reading is empty. The read runs on its own goroutine with
  its own timeout, so a caller that gives up, as at shutdown, returns at once.
- **Mail, the assistant's own code.** A per-user job searches Gmail with a deterministic query:
  known carrier and shop sender domains, or delivery words in the subject in English and Dutch.
  New messages, after a cursor of epoch seconds plus the IDs already read (as intake's overlap
  does), go in one batch to one toolless model turn with the mail framed as untrusted, returning
  strict JSON per message: carrier, number, order reference, title, status, expected day or
  window, pickup point. Code validates it, upper-cases the number, strips spaces and hyphens,
  checks it against `^[A-Z0-9]{8,35}$`, maps the status, and merges by carrier and number, or by
  sender and order reference when there is no number yet. Caps: 10 messages a run, 40 a day. The
  first run looks back 3 days. Mail that is unreadable, or Mail being off, shows on the settings
  page.
- **Links never come from mail.** Shipping mail links are click-tracking redirects. The link on a
  card is built from a per-carrier URL template and the normalised number (the shop: its order
  page and the order reference). A carrier without a template gets no link.
- **Hand-off.** A shop mail that names another carrier and number merges with that carrier's
  parcel, or creates it for that carrier.
- **One parcel, whichever source sees it.** An account integration's shipment key is the
  barcode, the country, and the postcode, so a package whose key starts with a number of the
  carrier's shape, or of an international postal item (UPU S10), carries that number and its
  link, and merges with its mail by number. Mail is left alone for a carrier the user has a live
  account source for with that same carrier (read in the last day) when its parcel cannot be
  matched: it has no number, or the account lists packages without one. Liveness is kept in
  memory, so after a restart during a Home Assistant outage mail is not held back; accepted.
  Where the number patterns of several carriers overlap, a number they share is one parcel under
  any of their names, since mail can only guess between them: it is stored under one of them,
  never under "other", is found and tombstoned under each, and Home Assistant's carrier replaces
  mail's guess but never the reverse.
- **Outside text from Home Assistant** (titles, senders, pickup points) goes through the same
  sanitiser as mail's: one line, no links or bare domains, no control or format characters. A
  sender named only by its domain is therefore left empty.

### Ownership

- **Sources are assigned in configuration.** Home Assistant is household-wide, and an account
  integration's entry is titled with the account's login, so the users do not see or claim each
  other's entries. `HOME_AGENT_PARCEL_SOURCES` maps a principal to the Home Assistant config entry
  IDs that are theirs, like the Telegram allowlist. An entry belongs to at most one principal;
  unassigned entries are never read.
- **A number belongs to whoever added it.** Every number added through the assistant is recorded
  with its owner, domain, and entry. That record decides who sees the parcel, in both directions,
  whatever entry tracks it.
- **Adding a number targets the adder's own entry.** For a domain that tracks numbers, the call
  goes to that domain's entry assigned to the adder. A user with no assigned entry of that domain
  gets the parcel followed by mail instead, so an unassigned or someone else's entry is never
  written to or read for them. The postcode, where the integration takes one, is the adder's own
  setting, kept in a per-user parcel settings table.

### Writes

- **One narrow write tool.** `parcel_track` takes a number and an optional carrier. For a carrier
  whose integration tracks numbers, its executor calls that domain's add service; for a carrier
  covered by an account integration, or any other carrier, it creates a parcel followed by mail.
  Only the add and remove services of listed domains can be called, with a validated number.
  The postcode is added by the executor at execution, so it is never stored in the proposal.
  **Undo** removes the number again through the remove service, removes the parcel Home Assistant
  was following for it unless another source lists it, and leaves the number free to be proposed
  again.
- **`approval.Act` as implemented.** It acts at once only when the run answers the user's own
  message and the conversation has read no outside content. Otherwise it proposes. So:
  - a number typed by the user runs at once;
  - a forwarded message is outside content, so a number in it is proposed;
  - after `parcel_list` (third-party, below) in a conversation, `parcel_track` is proposed until
    the conversation is reset;
  - every number found by the mail job or the inbox check is proposed for adding to Home Assistant.
- **One check before every proposal.** `parcel_track`'s proposal path, shared by chat, intake, and
  the mail job, skips a number that is already tracked, tombstoned, pending, or declined. Every
  proposed number is recorded in the parcel store as proposed, added, or declined. A small hook in
  `internal/approval` tells the parcel side when its proposal is rejected; "declined" means an
  explicit reject only, and a proposal that expired or was replaced may be offered again, on at
  most three cards in all. A number the user asks for in their own message passes a decline and
  unanswered offers (a decline is set back to proposed); one they stopped following is still
  refused.
- **Proposals from mail.** The mail job's proposals go on one card, headed like intake's, in the
  user's language, with its outside-content warning and a 24-hour lifetime, to the Parcels tab, through `OfferIfIdle`, so a
  proposal already waiting in that view, including the user's own chat proposals when tabs are
  off, is never replaced. Numbers not offered because the view was busy are offered on a later
  run.
- **Mail parcels need no approval.** A parcel created from mail is local, reversible with **Stop
  tracking**, and touches nothing outside the assistant.

### Inbox intake

- When Parcels is on for the principal, intake's scan prompt offers a delivery notice as a
  `parcel` follow-up (a `parcel_track` proposal) instead of a watch, and intake's chat guidance
  points a forwarded shipping message at `parcel_track` instead of `watch_propose`. Both take the
  principal now. A notice from a sender the parcel job does not know is still caught this way.
- With Parcels and Inbox intake both on, a shipping mail can be read twice, once by each job.
  Accepted: the parcel job reads only matching mail, and the cost is one extraction each.
- Existing agent watches for parcels keep running until the user removes them; the enable line on
  the settings page says so.

### Cards and notices

- **One card per parcel, edited in place, never deleted.** Telegram lets a bot delete only recent
  messages, so a delivered or stopped parcel's card is edited to a final state and left.
- **Widget when the user has Widgets on, text otherwise**, both rendered from the stored parcel.
  - The widget has no inline keyboard, because some Telegram apps then show it as unsupported.
    **Stop tracking** is a control in the widget, which arrives as a `WidgetQuery` update and must
    be answered within 15 seconds. The carrier link goes in a short caption line (carrier name as
    the link text), kept short so it never falls back to plain text without the link.
  - The widget's data must fit 1,024 bytes: title 60, sender 40, pickup point 60 runes, plus
    status, window, carrier, link, language, and parcel ID. The view renders outside text with
    `textContent` only. Its data format stays readable by older copies of the view, because a card
    lives for weeks.
  - The text card has **Open at <carrier>** as a URL button and **Stop tracking** as a callback
    button whose data fits 64 bytes.
- **Labels in the user's language** (plan 0154); title, sender, and pickup point are outside text,
  shown as plain text, never as links or markup. Notices are templates, with no model call.
- **Notices on transitions.** A short message replying to the card, with
  `allow_sending_without_reply`, for `out_for_delivery`, `at_pickup_point`, `delivered`, and
  `problem`, once per parcel and status, recorded so a restart never repeats one. Between 22:00
  and 07:00 in `HOME_AGENT_TIMEZONE`, notices are sent with notifications off. A window change
  only edits the card.
- **A silent baseline.** The first read after a source is assigned, Parcels is turned on, or a
  restore sends no notices for the statuses it finds. It edits a parcel's stored card back to
  current when the card still exists, sends a new card, with notifications off, only for a parcel
  not yet delivered that has none, and posts one summary line ("Tracking 3 parcels"). Later reads
  notice changes. Whether a baseline is owed is stored per user (when it last ran and a fingerprint
  of the assigned sources), so a restart neither repeats it nor skips one that never finished.
  When Home Assistant cannot be read, or an entry assigned to the user has never read well (its
  sensors unavailable, say), the baseline finishes on what it has and stores that Home Assistant
  is still owed. Its reads then stay silent, after a restart too, until one finds every assigned
  entry read well: no notice for what they find, new cards without sound. An entry that never
  reads keeps the user's Home Assistant notices silent until it does or is unassigned; it shows
  as not reading in the settings. Cards are repaired on every pass whether or not Home Assistant
  answers.
- **A gone card or tab.** If editing a card fails because the message or its tab is gone, the card
  is sent again, through the feed's replacement (plan 0158).
- **Replies are ordinary chat.** A reply to a card or notice is not tied to its parcel; the turn
  has `parcel_list` to find it. Tying replies to parcels, as job reports and watch alerts are, is
  left for later.
- **A Parcels feed.** A new feed, `parcels`, with its own name and the package icon, or the feed
  fallback if Telegram does not offer it. With tabs off, cards go to the main view.

### Lifecycle

- **Stop tracking** edits the card to "Stopped", removes a number the user added from Home
  Assistant, and leaves a tombstone.
- **Tombstones.** A parcel deleted by retention or stopped leaves a tombstone keyed by owner and
  carrier plus number (or source key) that outlives it, so a source still listing the parcel never
  brings it back or repeats a notice. A tombstone is dropped once no source has listed it for 30
  days.
- **Retention.** A parcel is deleted 14 days after delivery or stop.
- **Idle parcels.** A parcel not yet delivered that no source has updated for 30 days is dropped
  by the loop: its card is edited to a final "No longer followed" state and its record is deleted,
  so it no longer counts as on its way for the mail cadence. It leaves no tombstone, since the user
  did not stop it: new mail or a source listing it again follows it afresh with a new card, and so
  does the user's own request. Only the user's stop, through the tool or the card, keeps a number
  away. A source that keeps listing a parcel keeps it followed.
- **Parcels off.** Assignments stay. No reads, no model turns, no notices. Open cards are edited to
  "Tracking off". Turning it on runs the silent baseline again.
- **Cadence.** Home Assistant every 10 minutes. The mail job every 30 minutes from 07:00 to 22:00
  in `HOME_AGENT_TIMEZONE` while the user has a parcel not yet delivered, and every 3 hours
  otherwise, including at night.

### Model access and privacy

- `parcel_list` returns the user's parcels, those on their way first and the newest first, at most
  30, each field on one line, and is marked third-party, so its result is framed as
  untrusted and counts as outside content for later writes. `parcel_stop` stops one. The tools'
  guidance, in their contribution, points parcel questions at `parcel_list`; the system prompt is
  not changed.
- Logs and traces carry parcel IDs, carriers, and statuses only: never titles, senders, pickup
  points, numbers, or postcodes.
- The operations notes say never to expose parcel entities to Assist, since the generic Home
  Assistant tools would then show every user's parcels to everyone. The connector logs a warning
  when the live context lists a discovered parcel entity, matched by its friendly name.
- **A per-user switch, off by default.** `Parcels`, an `integration.Switch`, with a settings page
  holding the switch, the user's assigned sources and their state, the postcode, and the mail
  status. A source not read yet since a restart shows as such and raises no attention note.
- **Export.** Parcels without card message IDs, added-number records (`parcel_added_numbers`),
  proposed-number records (`parcel_proposed_numbers`, without the approval ID), tombstones, and the
  postcode are the user's export (plan 0129). Stopped parcels are left out, since their tombstones
  stand for them. The notices-sent table, the mail cursor, and the baseline state are excluded.
  Restored parcels get new cards under the silent baseline. Source assignments are configuration
  and are not exported.

## Approach

0. Spike the registry, config entries, and services against the household's Home Assistant with
   the non-admin token; record here only command names, field names, and which calls the token
   may make, never entry or entity IDs, titles, numbers, or senders; build made-up fixtures.
1. Store, model, carriers, and the interfaces the parallel chunks meet at.
2. The Home Assistant source and its executor.
3. The mail source and the inbox intake changes.
4. The Telegram card, notices, feed, and settings.
5. The tracker loop, then the tools, wiring, and the end-to-end test.
6. Documentation and operations.

## Implementation chunks

Waves: {0, 1} in parallel → {2, 3, 4} in parallel (2 also after 0) → 5a → 5b → 6.

0. **Spike.** Orchestrator: it needs the household's Home Assistant and its token. Files: this plan,
   `internal/connectors/homeassistant/testdata/parcels/`. Accepted when this plan records, by
   command and field name only, which registry and config entry commands the non-admin token may
   call, which fields they return, and the add service's fields with several entries; and the
   fixtures cover both shapes with made-up parcels. No household identifiers enter the plan.

   Confirmed by the spike, with the household's non-admin token over the WebSocket API:
   - `config/entity_registry/list` is allowed and returns, per entity, `entity_id`, `platform`,
     `unique_id`, `config_entry_id`, `device_id`, `translation_key`, `disabled_by`, and
     `hidden_by`, among others. Discovery uses it: platform on the domain list, unique ID suffix
     for the sensor kind, config entry for ownership.
   - `config_entries/get` is allowed, with or without a `domain` filter, and returns `entry_id`,
     `domain`, `title`, and `state`, but not an entry's options.
   - `config/device_registry/list` is allowed; not needed.
   - The add and remove services are ordinary services, not admin-only, so the token may call them.
     Their fields: `tracking_code` always; with several entries, one integration selects the entry
     by `config_entry_id`, and the other, whose entries are per postcode, by `postal_code`, which
     is why the user's postcode setting exists. The executor passes the adder's assigned entry or
     postcode, and never relies on a domain's only entry.
   - None of the parcel integrations is installed yet, so the attribute shapes come from the
     integrations' source. Chunk 2 builds the fixtures from that source, with made-up parcels, and
     the live check under Validation confirms them once the operator installs the integrations.
1. **Store, model, carriers, interfaces.** Complex. No dependency. Files: `internal/parcel/`
   (`parcel.go`, `status.go`, `carriers.go` with sender domains, number patterns, and link
   templates, `export.go`, and the `parcel_track` tool name and its `{number, carrier}` argument
   type), the Parcels `integration.Switch`, the interfaces `Sources` (list, assignment), `Cards`
   (send, edit, notice), and `Tracker`, the reject hook in `internal/approval`,
   `internal/store/sqlite/parcels.go`, and a migration with the next free number at implementation
   (063 today) holding parcels, added numbers, proposed numbers and their outcome, tombstones,
   notices sent, the mail cursor, and per-user parcel settings (postcode). Accepted when repository
   tests on a temporary database cover insert, update, merge, per-user scoping, added and proposed
   numbers, tombstones, retention, the notices-sent table, and export and restore; and an approval
   test shows the reject hook firing on an explicit reject only.
2. **Home Assistant source.** Complex. Depends on 1. Files: `internal/connectors/homeassistant/parcels.go`,
   `internal/testutil/homesim` (registry, config entries, parcel states, services), tests.
   Accepted when homesim shows both shapes normalised, an unlisted domain and an unassigned entry
   ignored, `raw` and `history` never read, a failed read keeping the last parcels as stale, the
   executor calling the add service once on the right entry and refusing another service, an
   unlisted domain, or a malformed number, Undo calling the remove service, and the live-context
   warning.
3. **Mail source and intake.** Complex. Depends on 1. Files: `internal/parcel/mail.go`,
   `internal/connectors/gmail` (a search and read API for code, across the user's readable
   mailboxes), `internal/turn` (a toolless extraction run), `internal/intake/scan.go` and
   `service.go` (per-principal prompt and guidance), `internal/testutil/googlesim`, tests. Accepted
   when googlesim and a fake model show: creation, a later update, merging by number and by sender
   plus order reference, the hand-off, malformed JSON and malformed numbers rejected, the cursor
   never rereading a message, the caps; intake offering a `parcel` follow-up (its action type,
   scan JSON, and validation in `scan.go`) only when Parcels is on; neither intake nor the mail job
   proposing a number already tracked, tombstoned, pending, or declined; and a chat proposal
   waiting in the same view surviving a mail-job run.
4. **Telegram.** Complex (Jev: simple at 0.35, below the threshold; a widget view, the settings
   pattern, and two languages). Depends on 1. Files: `internal/telegram/parcels.go`,
   `internal/telegram/widgets/parcel.{html,css,js}` and the kind in `widget.go`,
   `internal/thread/thread.go`, `internal/telegram/feeds.go`, `switch_pages.go`, `settings.go`,
   `help.go`, `privacy.go`, and the `WidgetQuery` handling in `internal/telegram/service.go`.
   Accepted when simulator tests show: text and widget cards in English and Russian; the worst-case
   widget data within 1,024 bytes; the worst-case caption keeping its link; the widget template
   within Telegram's size limits (the existing widget test); Stop tracking from both; outside text
   plain and capped; a gone card resent; the settings page showing sources, postcode, and mail
   status.
5a. **Tracker loop.** Complex. Depends on 2, 3, and 4. Files: `internal/parcel/tracker.go` and its
   test. Accepted, with a fake clock stepped slot by slot, when: Home Assistant is read every 10
   minutes; mail every 30 minutes from 07:00 to 22:00 with a parcel open and every 3 hours
   otherwise; the baseline is silent and posts one summary; each transition sends one notice and a
   restart does not repeat it; notices at night go without sound; a tombstoned parcel still listed
   never returns; retention deletes after 14 days; Parcels off stops all reads and marks the cards,
   and turning it on again edits those cards back instead of sending new ones.
5b. **Tools, wiring, end to end.** Complex. Depends on 5a. Files: `internal/parcel/tools.go`,
   `internal/wiring` (registration, `export.go` exclusions, config for `HOME_AGENT_PARCEL_SOURCES`),
   `internal/config`, and the end-to-end test `internal/telegram/parcels_wiring_integration_test.go`,
   beside the harness that builds the real wiring. Accepted when an end-to-end
   test with homesim, googlesim, a fake model, and the Telegram simulator covers: `parcel_track`
   typed by the user running at once, forwarded or from mail proposing, a declined number never
   proposed again unasked, Undo; `parcel_list` framed as untrusted and making the next write a proposal;
   one user never seeing another's parcels; a user without an assigned entry getting a
   mail-followed parcel; `HOME_AGENT_PARCEL_SOURCES` naming one entry for two principals, or a
   principal not on the allowlist, stopping startup with a message; and export and restore under
   the baseline.
6. **Documentation and operations.** Simple (Jev: simple at 0.61, below the threshold; prose in
   existing sections). Depends on 5b. Files: `README.md`, `docs/architecture.md`,
   `docs/operations.md`, `.env.example`, `deploy/nas.env.example`, and the operator's local `.env`
   and `deploy/.env.nas`. Accepted when the README has a Parcels section (what it does, settings,
   privacy), the architecture doc a parcel subsystem entry, and the operations doc sections on
   assigning sources, installing and pinning parcel integrations, reviewing every update before
   taking it, and never exposing parcel entities to Assist.

## Files affected

- New: `internal/parcel/`, `internal/store/sqlite/parcels.go` and a migration,
  `internal/connectors/homeassistant/parcels.go` and `testdata/parcels/`,
  `internal/telegram/parcels.go`, `internal/telegram/widgets/parcel.*`,
  `internal/telegram/parcels_wiring_integration_test.go`, `internal/wiring/parcels.go`,
  `internal/config/parcels.go`, an eval case and its parcel seed.
- Changed: `internal/approval` (reject hook), `internal/connectors/gmail`, `internal/turn`,
  `internal/intake`, `internal/testutil/homesim`, `internal/testutil/googlesim`, `internal/thread`,
  `internal/telegram` (feeds, widget, settings, switch pages, help, privacy, service),
  `internal/wiring`, `internal/config`,
  `README.md`, `docs/architecture.md`, `docs/operations.md`, the env examples.

## Validation

- The acceptance tests of every chunk, the full test suite, `go vet`, the build, and
  `scripts/check-plans`.
- An eval case under plan 0155: "where is my parcel?" answered through `parcel_list`.
- Live, with the operator's integrations installed and sources assigned: current parcels appear
  under a silent baseline, a number from a forwarded mail is added through the approval card, and
  a parcel is followed to delivery with one notice per transition.

## Risks and open questions

- The community integrations are unofficial and can break when a carrier changes its app or site.
  A broken one shows as a stale source, and mail keeps the parcel moving.
- Their attributes are the contract. Every update is reviewed before it is taken, and the tests pin
  both shapes.
- Mail extraction costs a model turn per run with new shipping mail, on the user's own
  subscription, bounded by the caps.
- Titles, senders, and pickup points come from shops and carriers: plain text on cards, untrusted
  framing for the model, never in logs.
- The widget's data format is fixed once cards are out; a change must stay readable by older views.
- Home Assistant has no per-entity permissions, so anyone with a Home Assistant login can see the
  parcel sensors of every assigned account there. Accepted as household trust: the assistant shows
  each user only their own, mail-derived parcels never enter Home Assistant, and the operations
  notes advise no parcel dashboard and no exposure to Assist.
- Home Assistant sources are assigned by the operator rather than chosen by each user, a deliberate
  exception to user-scoped integrations: the household's Home Assistant is shared, and its entry
  titles are account logins. Each user still turns Parcels on and off and sets their own postcode.
