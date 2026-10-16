# 0087 — Feed tabs: messages nobody asked for go to a tab of their kind

Status: `proposed`

## Context

Backlog item `0bc46ab8` asks that nothing be written to the main view when
tabs are on. Each message should go to the tab it belongs to. The user also
gets every main-view message on their watch, and wants to mute the main view
without losing any notifications.

With tabs on (plan 0083), only messages that answer a tab stay in it. Every
message nobody asked for is sent from a context without a thread, so it lands
in the main view:

- `schedule.Service.deliver`: model-job reports, Heads-up, reminders, check-in
  reminders, and the texts of Trips scan runs.
- `watch.Service` alerts, through `WatchAlertSender`.
- `TripNotifier`: suggested-trip cards, start and end notices, and update
  lines.
- Files and images that a scheduled run sends through `delivery`.

Grafana alerts come from a separate send-only bot in `home-monitoring`, so
they are not affected.

The user agreed with these decisions:

- There is one tab per kind.
- Every message keeps its sound.
- Talking about one report must not mix with talk about another.

## Goal

With tabs on:

- Messages nobody asked for go to four feed tabs that the bot makes on first
  use:

  | Feed | Gets |
  |---|---|
  | Reminders | `reminder.text` jobs (including check-in reminders), Trips scan runs, suggested-trip cards, trip notices |
  | Reports | `model.turn` jobs, including Heads-up and manual runs |
  | Home | Home Assistant watches |
  | Watches | departure, disruption, route, and agent watches |

  Plan 0089 later moves trip messages into each trip's own tab.
- The main view gets nothing unprompted, so muting it loses nothing.
- Each job and watch has its own conversation in its feed tab. Two jobs
  never share one.

With tabs off, nothing changes.

## Decisions

- **Feed tabs live in the tab registry** (plan 0085).
  - Migration 034 adds a nullable `subject` column to `telegram_tabs`, with a
    unique partial index on principal and subject. A feed tab's subject is
    `feed:reminders`, `feed:reports`, `feed:home`, or `feed:watches`. Plan
    0089 adds `trip:<id>` to the same column, so the registry is the only
    record of which tab serves what. When plan 0085's observer drops a gone
    row, the feed or trip loses its tab with it.
  - Feed rows are `named_by = fixed`.
  - A `Feeds` resolver in `internal/telegram` returns a feed's thread. It
    creates the tab when there is none, named "Reminders", "Reports", "Home",
    or "Watches". Plan 0088 adds icons.
  - Creation is serialised per principal and feed, so two deliveries at the
    same time make one tab.
- **Delivery sets the thread, not the sender.**
  - `schedule.Service` and `watch.Service` gain `SetFeeds`, with an interface
    `Thread(ctx, principalID, feed) (int64, error)`.
  - Delivery puts the feed's thread on its context with a new, delivery-only
    key, `thread.WithDelivery`. Only the client's thread setter reads that
    key, and it takes precedence over `thread.From`. The client wrappers from
    plan 0085 send the thread and handle a gone tab, `sendPlain` included.
  - The scheduled run's own context gets the same delivery-only thread, so
    files and images that a run sends land in the same tab.
  - `thread.From` stays a lane's thread, which scopes attachments,
    approvals, deferred continuations, and the judgment exchange. A run in the
    Reports tab therefore never sees that tab's conversation.
  - `TripNotifier` resolves the Reminders feed itself.
  - A nil resolver, or tabs off, means thread 0, which is today's behaviour.
- **Failures fall back to the main view without churning tabs.**
  - If a feed tab cannot be created, the message goes to the main view and
    the failure is logged. The next delivery tries again.
  - A send to a gone feed tab reaches the main view through the client's
    fallback.
    - Plan 0085's observer drops a gone row, so the next delivery makes a
      fresh tab. A user who deletes a feed gets it back on its next message;
      muting the tab is how to quiet it.
    - A fresh feed tab that never accepted a send is suspect for 24 hours
      under plan 0085's rule. Its row is kept and messages go to the main
      view meanwhile. Telegram's bug (#905) therefore costs at most one new
      tab per feed per day, not one per message.
- **Each job and watch has its own conversation.**
  - Sessions get a `subject`, such as `job:<id>` or `watch:<id>`. The column
    comes from plan 0085's migration. Migration 034 adds a unique partial
    index on principal and subject where the session is active.
  - Migration 034 adds `thread_id` to `job_report_messages`. It is the thread
    the message actually landed in, read from the sent message's
    `message_thread_id`, so a message that fell back to the main view
    records 0. The tracked send methods return it beside the message ID.
  - Migration 034 also adds `watch_alert_messages`. Each row holds:
    - principal and Telegram message ID
    - the thread it landed in
    - watch ID
    - the sent text, capped at 2000 runes
    - `private` and `external` flags, as on `job_reports`
    - the sent time

    Rows are pruned with job reports, after 14 days or past 200 per
    principal. `SendAlert` returns the message ID.
  - **Only in feed tabs.** Subject routing applies only to a message sent in
    a thread that the registry marks as a feed. In the main view, and with
    tabs off, replies behave exactly as today.
  - **What counts as a reply.** In a tab, Telegram sets every message's
    `reply_to_message` to the tab's root service message. A reply to the
    root, or to a message no table knows, is treated as no reply.
  - A reply in a feed tab is resolved in this order:
    1. It replies to a message found in `conversation_reply_messages`: it
       returns to that session, as today.
    2. It replies to a message found in `job_report_messages` or
       `watch_alert_messages`: it resolves that subject's most recent session.
       That session is switched into the lane with `switchActiveSession`, or
       a new session with that subject is created there. The lane's previous
       session is closed as `switched`.
  - A message that is not a reply goes to the subject of the most recent
    feed message in that tab, if that message arrived after the lane's last
    exchange. Otherwise it continues the tab's current conversation.
    - The lane's last exchange is its newest request, or the start of its
      active session if that is later. So after `/new`, the next message
      stays in the new conversation.
    - If that subject's session is active in another tab, the message
      continues the current conversation instead. Only an explicit reply is
      refused as open elsewhere, so a tab can never get stuck.
    - So typing right after a report discusses that report, and follow-ups
      need no reply.
    - This replaces what the user was told earlier: "a message without a reply
      starts a new conversation". The user is told about the change before
      this plan is implemented.
  - When a turn lands in a different subject's conversation than the tab's
    previous one, the answer starts with a line naming the job or watch, for
    example "About “Bins”:". The user always sees which conversation they
    are in, and `/new` starts a fresh one.
    - The enqueue stores the subject's label on the request, in a new
      nullable `conversation_requests.subject_label`.
    - `conversation.Service` delivery prepends it where it prepends
      `ResumedLine` today, as one combined line when both apply.
    - Creating a new subject session returns `SwitchedFrom`, as a switch
      does, so the previous session is left properly.
  - The bot's replies in a subject session are recorded as usual, so
    replying to them stays in that conversation.
  - A subject session that is active in another tab stays there. On an
    explicit reply, the turn does not run, and the answer names that tab, as
    Open does in plan 0085: "That conversation is open in “Bins”. Write
    there to continue it."
  - Plan 0085 reopens a `tab_gone` session when its tab is written in again.
    It skips that reopen when the session's subject is already active
    elsewhere, so the unique subject index never fails an enqueue.
- **The conversation knows its subject.**
  - Each turn of a session with a subject gets one line in the message (not
    the system prompt, which stays cacheable). For example: "[system] This
    conversation is about the user's scheduled job “Bins” (id …);
    schedule_get shows it in full." Watches name `watch_list`.
  - A reply to a watch alert quotes the alert into the turn, as replies to
    job reports already do (plan 0072). A quote provider in `internal/watch`
    mirrors `schedule/reports.go`, and the turn is marked private or outside
    content from the row's flags.
- **Notices stay.** Watch, job-report, and task notices are still taken by
  the next turn of any tab.
- **Privacy.** Logs carry IDs and feed names, which are fixed words. Metric
  labels are fixed.

## Approach

### Chunks

1. **Store.** Jev: complex (1.00).
   - Migration 034: the tab `subject` with its index, the session subject
     index, `job_report_messages.thread_id`, `watch_alert_messages`, and
     `conversation_requests.subject_label`.
   - Store methods for feed tabs, subjects, and alert messages, with pruning.
   - The feed-tab routing in `EnqueueConversation`: the root counted as no
     reply, then replies, then the latest feed message after the last
     exchange, then the current session, plus the elsewhere outcome and
     `SwitchedFrom`.
   - Acceptance, from integration tests on real temporary SQLite:
     - In a feed tab, replies to reports of two jobs land in two sessions, and
       a reply to a later report of the first job returns to its session.
     - A non-reply after a new report goes to that report's subject. A
       non-reply with no newer feed message, or sent after `/new`, continues
       the current session.
     - A reply to the tab's root counts as a non-reply.
     - A reply in the main view behaves as today.
     - An explicit reply to a subject open elsewhere is reported, not
       switched. A non-reply in that case continues the current session.
     - Another principal's message IDs resolve nothing.
2. **Feed routing and alerts.** Jev: complex (0.98). Depends on chunk 1.
   - The `Feeds` resolver, and `SetFeeds` on the schedule and watch services.
   - `thread.WithDelivery` on delivery and run contexts, and `TripNotifier`
     on Reminders.
   - Tracked sends return the landed thread. `SendAlert` returns its ID and
     thread, and the watch service stores the alert message.
   - The watch alert quote provider.
   - The elsewhere answer in the Telegram enqueue path.
   - Wiring in `internal/app`.
   - Acceptance, from integration tests against a fake Telegram boundary:
     - Each kind lands in its feed tab, and concurrent deliveries make one
       tab.
     - A gone feed tab is made again on the next delivery.
     - A never-confirmed failing tab is not replaced within 24 hours.
     - A failed creation falls back to the main view.
     - With topics off, no thread is sent.
3. **Subject context.** Jev: simple (0.86); overridden to complex because it
   decides whether a turn is marked private or outside content. Depends on
   chunk 2, for the quote provider.
   - The subject line in `conversation.Service`, and the alert quote with its
     exposure marks.
   - The "About …" line, prepended in delivery from the request's
     `subject_label`.
   - Acceptance, from integration tests:
     - The line is on every turn of a subject session and on no other.
     - The "About …" line starts the answer exactly when the subject changed,
       combined with the resumed line when both apply.
     - The alert is quoted once.
     - A private alert marks the turn private, and an agent watch's alert
       marks it outside content.
4. **Docs.** Done by the orchestrator. The README covers feed tabs and how to
   discuss a report, and the architecture notes cover delivery threads.

### Waves

- Wave 1: chunk 1.
- Wave 2: chunk 2.
- Wave 3: chunks 3 and 4.

## Files affected

- `internal/store/sqlite/`: `migrations/034_feed_tabs.sql`, `tabs.go`,
  `conversation.go`, `conversation_history.go`, `watches.go`, and the job
  report pruning
- `internal/telegram/`: the new `feeds.go`, `watches.go`, `trips.go`,
  `reminders.go`, `service.go`
- `internal/schedule/service.go`, `internal/watch/service.go`, and the new
  `internal/watch/reports.go`
- `internal/thread/thread.go`
- `internal/conversation/`: `service.go`, `types.go`
- `internal/app/app.go`
- `README.md`, `docs/architecture.md`, `docs/plans/README.md`

## Validation

- `gofmt`, a `go mod tidy` diff, `go vet`, `go test -race ./...`,
  `go build`, and `scripts/check-plans`.
- A live check on the operator's bot with topics on:
  - Mute the main view, and see a reminder, a model-job report, and a watch
    alert each arrive in their feed with sound.
  - Reply to two different reports, and see two conversations in
    `/sessions`.
  - Type right after a report without replying, and see the "About …" line.
  - Delete the Reports tab, and see it come back on the next report.

## Risks and open questions

- The Bot API bug that rejects sends to private-chat tabs makes such a
  message fall back to the main view, so a muted main view can still swallow
  one. Each fallback is logged by plan 0085.
- The latest-feed-message rule can send a follow-up about an earlier job to
  the newer report's conversation. The "About …" line shows it, and a reply
  to the earlier report or answer moves it back.
