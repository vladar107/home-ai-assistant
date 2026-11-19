# 0165 — The assistant's own mailbox

Status: `in-progress`

## Context

The backlog asks how the assistant could have an email address, a phone number and card details
for tasks that need them, with consent before each use and nothing of them kept in memory. This
plan takes the first piece only: a mailbox that belongs to the assistant, not to any user. A
phone number, a payment card, and a store of the users' own personal details stay in the backlog
for later plans. A card in particular is useless before the assistant can complete a checkout,
which it cannot do today.

Today the assistant reads mail only through each user's own Gmail connection (0025, 0036), with
`gmail.readonly`, and has no way to send mail at all. When a user wants to write to a business,
the assistant can draft text but the user has to send it from their own address, and the reply
lands in their personal mailbox, mixed with everything else.

Experience shared in an AI engineering community points the same way as this design:
- people give their agent a separate account so it cannot damage the owner's own;
- they keep reads automatic and approve every write in chat;
- they treat incoming mail as the main prompt-injection channel.

Free providers were compared for this plan. Zoho, Proton and Tuta offer no IMAP or SMTP on their
free tiers, and GMX's free IMAP comes with weaker deliverability. Hosted agent-mail APIs keep all
the mail with a third party. Gmail reuses the existing client, simulator and OAuth knowledge.
Its risk is that Google suspends accounts that look automated, most often new accounts that
start heavy API traffic at once.

## Goal

The household has one assistant mailbox. Each user has their own address on it, made with a
plus tag, which they can give to businesses. Mail to that address reaches only that user, as a
short notice in their chat. The user can ask about a message, have the assistant read it, and
have it reply or write a new email. Nothing is sent until the user confirms the exact message.
The assistant signs as the user's assistant, and says that it is an AI whenever someone asks.
A Google
suspension costs only this mailbox, never a user's own Google connection.

## Decisions

### The account

- **One Gmail account, owned by the household, created by hand.** The operator creates it in a
  phone's browser with a recovery phone and two-step verification, and uses it lightly for a
  while before connecting anything. Its display name is the assistant's name alone. Its address, display name and every household detail live only
  in the ignored local and NAS env files, never in the repository.
- **A separate Google Cloud project and OAuth client** of type Desktop app, used for nothing
  else. A suspension of that project cannot revoke the users' Gmail and Calendar connections,
  which use the existing client.
  - The project is published to production, because refresh tokens in Testing status expire
    after seven days.
  - It is never submitted for verification. `gmail.readonly` is a restricted scope, so Google's
    unverified-app screen and its 100-user cap are expected and harmless here.
- **Scopes `gmail.readonly` and `gmail.send` only.** There is no `gmail.modify`, so the
  assistant cannot label, archive, mark read or delete. The processed state lives in SQLite.
- **Authorization runs once, on the operator's Mac.** A new `cmd/assistant-mail-authorize` runs
  the loopback authorization-code flow with PKCE against the separate client and prints the
  refresh token once. The operator stores it in Keychain with `scripts/keychain-set-secret`, and
  it reaches the NAS as a secret file, like the Home Assistant token. No chat flow exists for
  it, because no user owns this mailbox.
- **A refresh token Google rejects** stops the poller and the tools, and is logged and counted.
  The users' settings show the mailbox as unavailable until the operator authorizes again. This
  happens after a password change, or after six months without use.

### Per-user addresses and routing

- **Per-user addresses are plus tags assigned by the operator.** `HOME_AGENT_ASSISTANT_MAIL_TAGS`
  takes `principal=tag[,tag];principal=tag`, parsed like `HOME_AGENT_PARCEL_SOURCES`. Each tag is
  unique across the household and is lowercase letters and digits. Tags should not be household
  names. The first tag listed is the principal's current address, used in `Reply-To` and shown in
  `/settings`. A retired tag stays listed after it, so its mail never falls through.
  - This deviates from users configuring their own integrations. A tag is part of a shared
    account's address, so only the operator can hand one out. Each user still switches their
    own mailbox on and off.
- **Inbound routing, in order.**
  1. Every `+tag` found in `Delivered-To`, `To` or `Cc`, taken together. A message addressed to
     several tags goes to each of their principals. The routing record is keyed by Gmail id and
     principal.
  2. A thread the assistant sent for a principal. This covers replies to the plain address,
     since sites that refuse plus addresses are common.
  3. Otherwise the message is unrouted. It is recorded and counted, never given to any user, and
     stays readable in Gmail's web interface.

  Before C5a lands, the operator sends the account one test message to a tagged address.
  "Show original" in Gmail's web interface then confirms which headers keep the tag. C2 starts
  from Gmail's documented behavior, and its fixture is replaced by a scrubbed copy of those
  headers.
- **What the poller sets aside:**
  - Messages labelled TRASH or DRAFT are ignored.
  - Messages the assistant sent itself (SENT and not INBOX) are ignored here; they are recorded
    at send time.
  - Messages labelled SPAM are routed like any other but never shown. The principal's notice
    only says how many new messages went to spam, with no sender or subject. A verification
    mail filed there can then be found through Gmail's web interface. Spam is also counted in a
    metric.
- **A per-user switch**, `integration.Switch` "Assistant mailbox", on by default for a principal
  with a tag.
  - Off means no notices and no tools.
  - The time a user last switched it on is stored. A message whose Gmail internal date is
    earlier is recorded as already notified, so turning the switch back on announces nothing
    old. This holds even when every switch was off and the poller had paused.

### Polling and notices

- **Polling, not push.** One household poller reads `history.list` with `historyTypes=messageAdded`
  from a stored history id every five minutes, with up to a minute of jitter, and only while
  some principal has the switch on. The volume stays low and even, which is the point for an
  account that must not look automated.
- **Every Gmail id the poller has seen is recorded**, unrouted ones included, so polling and
  recovery are idempotent.
- **Recovering an expired history id.** The poller lists messages with an `after:` time a little
  before the newest recorded one, skips ids it has already recorded, and then resets the cursor
  from the profile's current history id.
- **Notices are deterministic.** One batched message per principal and poll lists, for each new
  message:
  - the sender's real address, not only its display name;
  - the subject;
  - the date;
  - a mark when `Authentication-Results` shows DMARC failing.

  Display names and subjects are stripped of control characters and markup and capped at 120
  characters. No model call is made. The delivered text is marked as external content, like the
  inbox intake results, so a turn that follows it is treated as exposed.

### Tools and sending

- **Tools, gated by the switch.** List and read are `RiskRead`, third-party and screened:
  - `assistant_mail_list`: the principal's recent routed messages in both directions, newest
    first, rendered like the notices.
  - `assistant_mail_read`: one message's text. It looks the id up among the caller's own routed
    messages first, so another principal's id finds nothing.
  - `assistant_mail_send` (`RiskWrite`): always `Propose`, never `Act`.

  When plan 0164's deferred groups exist, these three tools form one group.
- **What a proposal shows.** From, To, Cc, Subject and the full body, exactly as
  it will be sent. It also marks:
  - when the turn has read external content;
  - when a recipient is not already a participant in the thread being answered, or, for a new
    email, when this principal has never mailed the recipient before;
  - on a reply, whether the recipient came from the message's `From` or its `Reply-To`.
- **What a sent message is.** A UTF-8 `text/plain` message with no attachments and no Bcc:
  - at most five recipients;
  - `From` is the display name and the plain address;
  - `Reply-To` is the principal's tagged address;
  - a reply carries `In-Reply-To`, `References` and Gmail's thread id.
- **Inputs are checked at the tool and again at execution.** Addresses are parsed with
  `net/mail`. Any CR or LF in an address or subject is refused, and the subject is RFC 2047
  encoded. The model never supplies headers.
- **How the assistant presents itself to outsiders.** This is the send tool's guidance; the
  operator wants no AI line on every email.
  - The tone is polite, concise and businesslike, in the language the other side wrote in.
  - It signs with its name and "Assistant to" the principal's display name, which the operator
    sets to the full name.
  - It never claims or implies being human. It does not invent a place, a body, calls or
    meetings of its own.
  - Asked whether it is an AI, a bot or a real person, it says plainly that it is an AI assistant
    writing for the principal.
  - The user confirms every message before it is sent, so the user is the author of record.
- **Limits and failure.**
  - A per-principal daily send limit (`HOME_AGENT_ASSISTANT_MAIL_DAILY_SEND_LIMIT`, default 10)
    is counted when an execution is marked started, so unknown outcomes and concurrent
    confirmations count.
  - The body is limited to what a proposal can show, about 2,000 characters.
  - Gmail's send has no idempotency key, so an execution whose outcome is unknown, such as a
    timeout, is never retried. It is reported as possibly sent, with a hint to check the
    thread.
  - Each sent message is recorded with its thread, for reply routing.
- **Privacy.**
  - Mail content reaches the principal's model subscription only when that user's turn reads
    it. The `/privacy` reply gains a line saying so.
  - Logs and metrics carry no addresses, tags or subjects.
  - Tests, fixtures and docs use neutral names and `example.com`.
- Consult skipped: the forks (authorization path, poller vs scheduler job, deterministic
  notices) each had one approach that fits the existing patterns.
- Plan critique (advisor, 2026-11-18):
  - unrouted mail is no longer handed to any user, and retired tags stay mapped;
  - the authorize command waits for the simulator's send scope;
  - each chunk registers what it builds;
  - send inputs are validated against header injection;
  - notices are sanitized and show the real sender;
  - proposals mark external reads and new recipients;
  - the send limit is per principal and counted at start;
  - the inbound chunk is split.

  The second round added:
  - the header check moved before the simulator work (C2) and inbound routing (C5a);
  - the switch-on time;
  - the spam count;
  - union routing;
  - the current tag;
  - the first-contact mark.

  Operator review (2026-11-19): no AI footer and no "(AI assistant)" in the display name; the
  assistant signs as the user's assistant and says it is an AI when asked.

### Settled during implementation

- **The body limit is what a proposal can show.** A proposal is capped in size, so a body is
  limited to about 2,000 characters. The tool states the limit up front.
- **A reply keeps the thread's subject** ("Re: …"), so Gmail threads it; the proposal shows the
  result. Its intent carries no subject, because that text was written by the other side.
- **Notices also reach the next turn.** The sender and subject of announced mail are given to the
  user's next turn, so the model knows what the user saw. A message's text reaches the model only
  when the user asks to read it. `/privacy` says so.
- **Two more tables:**
  - `mail_switch_states` detects a switch turned back on, across restarts;
  - `mail_contacts` records whom a principal has mailed, for the first-contact mark.

  The migrations are 066 to 068.
- **Further limits:**
  - a send to the mailbox's own address is refused, and the subject is capped at 200 characters;
  - spam can be neither read nor answered;
  - a notice is given up after three failed deliveries, or at once for a chat that blocked the
    bot;
  - a message that fails three polls in a row is set aside as unreadable.
- **Sent messages the app never recorded** (an unknown outcome or a restart mid-send) are
  recovered by the poller from the message's tagged `Reply-To`.
- **Checks at startup and after re-authorizing.** The poller checks on its first run that the
  token belongs to the configured address. A rejected or mismatched token needs a new
  authorization and a restart.
- **The evaluation case is deferred.** The case for "is this an automated message?" needs the
  evaluation world to simulate this mailbox, so it moves to plan 0166's identity evaluations.

## Approach

Chunks, with Jev's tier where it was confident:

1. **C1 Configuration and operations** (Jev: complex 0.61, below the threshold; simple, because
   it follows the existing parcel configuration and secret-file patterns).
   - `internal/config/assistantmail.go` with:
     - enabled;
     - address and display name;
     - client id;
     - the client secret and refresh token, as values or files;
     - tags;
     - the daily limit and the poll interval.
   - `deploy/compose.app.assistant-mail.yaml` mounting both secrets.
   - `scripts/nas-sync-secrets`, `scripts/keychain-set-secret` and `scripts/deploy-app-nas`
     entries.
   - `.env.example` and `deploy/nas.env.example`.
   - Acceptance: config tests for:
     - valid and invalid tags;
     - a tag repeated across principals, and a principal repeated;
     - a tag assigned to an unknown principal;
     - missing secrets.
2. **C2 Gmail client and simulator** (Jev: complex 0.94).
   - `internal/connectors/gmail/client.go` gains:
     - `Send` (raw message, optional thread);
     - `History` since an id, `messageAdded` only;
     - `Profile` for the current history id.
   - `internal/testutil/googlesim`:
     - grants the send scope;
     - records sent messages and labels them SENT;
     - delivers plus-addressed mail with the `Delivered-To` header;
     - delivers spam;
     - expires old history ids.
   - Acceptance: client tests for each call and for an expired history id.
3. **C3 Store** (Jev: complex 0.26, below the threshold; simple, because it follows the existing
   repository pattern).
   - A migration and repository methods:
     - seen Gmail ids;
     - routing records keyed by Gmail id and principal, with thread, direction, time and notified
       time;
     - the history cursor;
     - a per-principal count of sends started today.
   - Acceptance: tests on a real temporary database, including that a principal never lists
     another's messages and that recording a seen id twice is harmless.
4. **C4 Authorize command** (Jev: complex 0.50, below the threshold; complex, because it handles
   an OAuth credential; after C1 and C2).
   - `cmd/assistant-mail-authorize`, with the loopback PKCE flow against the separate client.
   - Acceptance: it completes against the simulator and refuses a consent without the send
     scope.
5. **C5a Inbound polling and routing** (Jev: complex 1.0; after C1–C3).
   - New `internal/connectors/assistantmail`: the module and token refresh through the existing
     Google client with the separate client's credentials.
   - The poller with the skip rules, routing and recovery.
   - Metrics for polls, routed and unrouted messages, and refresh failures.
   - Registration in `internal/wiring`.
   - Acceptance: integration tests with the simulator for:
     - each routing rule, a multi-tag message, and a tag found only in `To`;
     - spam and self-sent messages skipped;
     - a rejected refresh token;
     - cursor recovery without duplicates.
6. **C5b Notices, switch, list and read** (Jev: complex 0.88; after C5a).
   - Batched sanitized notices marked external, and the switch.
   - The list and read tools and their registration.
   - Acceptance: integration tests that:
     - the notice shows the real address and the DMARC mark and strips markup;
     - spam is only counted;
     - turning the switch off and on announces nothing old, including after every switch was
       off;
     - another principal's id finds nothing;
     - reading marks the conversation external.
7. **C6 Outbound** (Jev: complex 1.0; after C5b).
   - The send tool, the proposal text with its marks, and the executor with message building,
     validation and limits.
   - The sent-message record and executor registration.
   - Acceptance: integration tests that:
     - a confirmed proposal sends exactly the shown message with its headers;
     - a rejected one sends nothing;
     - CR or LF in an address or subject is refused;
     - the size, recipient and daily limits refuse;
     - an unknown outcome counts toward the limit and is not retried;
     - a reply to the sent thread routes back to its principal;
     - the proposal marks external reads and new recipients.
8. **C7 Settings, privacy and documentation** (Jev: simple 0.60, below the threshold; simple,
   because it follows the existing switch sections; after C5b, in parallel with C6).
   - A Telegram settings section showing the user's own address and the switch, and the
     `/privacy` line.
   - `docs/architecture.md`.
   - `docs/operations.md`, with a runbook section for the account, the project, the client and
     its publishing status, authorizing again, and the header check.
   - Acceptance: a settings integration test with the fake Telegram server.

Waves:
1. C1 and C3.
2. C2.
3. C4, and C5a once the header check is done.
4. C5b.
5. C6 and C7.

The live check comes last.

## Files affected

- new `internal/config/assistantmail.go`, `internal/config/config.go`
- new `cmd/assistant-mail-authorize/`
- `internal/connectors/gmail/client.go`, `internal/testutil/googlesim/`
- `internal/store/sqlite/` (migration and repository)
- new `internal/connectors/assistantmail/`, `internal/wiring/`
- new `internal/telegram/assistantmail.go`, the `/privacy` text
- new `deploy/compose.app.assistant-mail.yaml`, `deploy/nas.env.example`, `.env.example`,
  `scripts/nas-sync-secrets`, `scripts/keychain-set-secret`, `scripts/deploy-app-nas`
- `docs/architecture.md`, `docs/operations.md`, `docs/plans/README.md`

## Validation

- `gofmt -l`, `go mod tidy -diff`, `go test -race ./...`, `go test ./...`, `go vet ./...`,
  `go build -buildvcs=false ./...`, `scripts/check-plans`.
- The integration tests named in each chunk, against the simulator and the fake Telegram server.
- Live, after the account has been in light use:
  - authorize, deploy, and see the address in `/settings`;
  - send mail from an outside address to the tagged address and receive the notice;
  - have the assistant reply, confirm, and see the reply arrive with its signature;
  - reply to it and see that routed back;
  - turn the switch off and see nothing announced.

## Risks and open questions

- Google may still suspend the account. The separate project limits the damage to this mailbox;
  the operator then moves to another provider, which needs a new client behind the same
  service.
- A notice shows a stranger's sender and subject in the chat, and a read puts a stranger's text
  before the model. Sends always need a confirmation that shows the full message and marks
  external reads and new recipients, so an injection can at most propose. The user can still
  confirm a bad proposal without reading it.
- Sites that strip or refuse plus tags send mail to the plain address. Only replies to a thread
  the assistant started find their principal. Other such mail stays unrouted and is visible
  only in Gmail's web interface, which the operator can open.
- A tag is visible to anyone the user gives the address to. It identifies nothing by itself.
- A phone number, a payment card, a store of the users' personal details, attachments, and
  push delivery through Pub/Sub are later plans.
