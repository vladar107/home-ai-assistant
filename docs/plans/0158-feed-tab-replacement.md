# 0158 — Resolve feed tabs at first send and replace a gone one at once

Status: `in-progress`

## Context

With tabs on, what nobody asked for goes to a tab of its own: a feed tab of its kind
(`thread.Feed*`) or, for a job, trip or agent watch with a destination, the object's tab. The
sources are the scheduler's reminders, reports and inbox checks (`feed()` in
`internal/schedule/service.go`), watch alerts, inbound events, and trip notices. Each has its own
`inFeed`, which asks `telegram.Tabs` for the tab (`Thread`, or `Destination` with a destination)
and puts the answer on the context with `thread.WithDelivery`. The Telegram client reads it
through `sendThread`, and for every send that names a tab `inTab` reports the outcome to the
`TabObserver` (`telegram.Service`).

Observed in production, with nothing deleted by the app:

1. A morning inbox check had a report. Its send to the Inbox tab was refused with "message thread
   not found"; the topic had disappeared on Telegram's side. `TabRefused` found the tab neither
   recently written in nor fresh, took it for gone, closed its conversation and dropped its
   registry row, and `inTab` sent the report again to the main view. The log had
   `telegram.send.thread_gone` with the thread ID, but not Telegram's own error text.
2. The next inbox check, hours later, resolved its tab at the start of the run:
   `schedule.Service.execute` calls `inFeed` before `Handler.Run`, so that files and cards a run
   sends land in its report's tab. No Inbox tab was registered, so `Tabs.Thread` made one. The
   check then found nothing and the run was quiet, which left a new, empty Inbox tab.

Every send of a scheduled run and how it finds its tab today:

| Send | Path | Tab |
| --- | --- | --- |
| Report text | `deliver` → `SendTrackedReply`, chunk by chunk, progress in `DeliveredChunks` | `inFeed` in `deliver` |
| One-time reminder with snooze buttons | `deliver` → `ReminderSender.SendReminder` | same |
| Report card (a staged picture) | `deliver` → `conversation.SendCard` → `SendPhoto` | same |
| Files a run's turn sends (NAS file, generated image) | `delivery.SendFile` / `SendImage` → `SendDocument` / `SendPhoto` | `inFeed` in `execute` |
| Inbox check's approval card | `intake.scan.stage` reads `thread.Delivery(ctx)` → `approval.OfferIfIdle` | same |
| Weekly memory review's cards | the review reads `thread.Delivery(ctx)` → `approval.Offer` | same |
| Trips scan, Heads-up, anchored jobs' notices | report text as above | `inFeed` in `deliver` |

Watch alerts (`watch.Service.inFeed`), inbound events (`events.Service.inFeed`) and trip notices
(`telegram.TripNotifier.inFeed`) resolve the same way, just before their single send.
`TripNotifier.TripTab`, which `trip.Language.Sample` asks only to read the user's latest message
there, also goes through `inFeed`, so asking it can make a trip or Reminders tab.

What else matters here:

- `approval.offer` puts the given tab on the context with `thread.With`. It uses that tab for the
  idle check (`PendingApprovals`), stores it as each record's `ThreadID`, and sends there; the
  sweep (`resendUndelivered`) later sends undelivered records to their stored tab. `OfferIfIdle`
  returns `ErrBundleWaiting` without sending while a card waits, and the inbox check's summary
  treats follow-ups waiting behind a card as no news, so such a check is quiet.
- `inTab` reads the tab once for its report to the observer, and each send closure reads it again
  through `setThread` or `sendThread`. `SendTyping` and `SendMessageDraft` read it through
  `setThread` too.
- `isThreadGone` matches only Telegram's 400 "message thread not found". Any other error, transient
  or permanent, returns unchanged: a scheduled delivery stays ready and the next tick retries it
  from `DeliveredChunks`, or a permanent one ends the run as undeliverable.
- `TabRefused` keeps a tab as suspect, and the send falls back to the main view, when the user
  wrote in it within a day or the bot made it and it never took a send. Otherwise the tab is gone:
  its lane closes, the registry forgets it, and the next resolution makes a new one.
- Each later send of the same delivery still names the gone tab, so a report of several chunks, or
  a card and its text, are each refused, logged and sent to the main view.
- Sends that may not fall back (a document whose body cannot be read again, `ForwardMessages`,
  `SendInTab`, `SendPlainInTab`) return the refusal with its verdict.
- `apiError.Error()` leaves Telegram's description out on purpose, so provider text never reaches a
  log. No log carries it today.
- `thread.Feeds` is implemented by `telegram.Tabs` and, with a main-view answer, by
  `eval.Chat`; `telegram.TripFeeds` embeds it. No test fakes it: the Telegram tests use the real
  `Tabs`.

## Goal

- A tab is made only when something is sent there. A quiet run and a language sample make none.
- When a send to a feed tab or an object's tab is refused and the tab is taken for gone, its
  replacement is found at once (the feed's new tab; for an object, its new tab or its feed), and
  that send and every later send of the same delivery go there. The main view is the fallback only
  when no replacement can be had.
- Offered approval cards stored against the gone tab follow it to the replacement, so the sweep and
  the next idle check see the right tab.
- Every refusal of a tab send logs Telegram's description.
- The suspect rules and conversation tabs behave as before.

## Decisions

- **A route on the context, resolved at first send.** `internal/thread` gains a `Route`: where a
  delivery nobody asked for goes, found the first time a send needs its tab and then kept for the
  route's life. `thread.WithRoute` puts it on the context under the same key as `WithDelivery`, so
  whichever was set last wins, exactly as two `WithDelivery` calls do today.
  - `thread.Delivery(ctx)` resolves a route, and the memory review keeps using it, since it always
    sends.
  - `thread.Peek(ctx)` returns the tab a route would use without making one: its resolved tab, the
    registry's tab for its object or feed, or zero with tabs off; and reports false when resolving
    would make a tab.
  - `thread.Resolved(ctx)` returns a route's tab only once it is resolved, the main view
    otherwise, and never reads the registry.
- **The inbox check keeps resolving before its idle check.** A review of this plan proposed that
  `intake.scan.stage` peek first, so that a check whose follow-ups wait behind a card would not
  make a tab. Implementing it showed the peek changes nothing:
  - When the registry has the Inbox tab, resolving returns it and makes nothing.
  - When it has none, resolving makes a tab that no card can already wait in, so `OfferIfIdle`
    finds that new tab idle and sends the card there; the tab is never left empty.
  - `stage` therefore keeps reading `thread.Delivery`. `approval.offer` is unchanged: the tab it is
    given is the idle check's lane and the stored `ThreadID`, and its sends go through the route on
    the context, which resolves to that same tab.
- **`TripTab` peeks.** A language sample reads the tab the trip's messages would go to and never
  makes one; with no tab yet it reads the main view, then anywhere, as `Sample` already does.
- **`telegram.Tabs` builds the routes.** `thread.Feeds` gains
  `Route(ctx, principalID, chatID, destination, feed) thread.Route`; an empty destination means the
  feed alone. The four `inFeed` helpers (schedule, watch, events, trips) become
  `thread.WithRoute(ctx, feeds.Route(...))`. `Thread` and `Destination` stay; the route calls them.
  `eval.Chat` returns a main-view route.
- **Which context resolves.** The route keeps the principal, chat, destination and feed, and the
  language of the context it was built with, so scheduled runs keep naming tabs in English. It
  resolves under the sending context: that send's cancellation, deadline, trace and logger, with
  the route's language put on it and the route itself taken off it, so nothing the resolver sends
  (an object tab's summary) can re-enter the route. A resolution that fails because the sending
  context ended is not kept, and the next send tries again. Any other failure leaves the route in
  the main view for its life, as a failed `inFeed` does now, and the next delivery tries again. A
  route is safe for concurrent sends and resolves once.
- **`inTab` reads the tab once.** It takes the tab from the context once, then makes the first
  attempt under `thread.WithDelivery(ctx, tab)`, a resend in the replacement under
  `WithDelivery(ctx, replacement)`, and the fallback under the main view. The observer always hears
  about the tab that attempt actually named. `SendTyping` and `SendMessageDraft` use
  `thread.Resolved`, so an indicator never makes or looks up a tab.
- **Replace a gone tab at once.** After the observer's verdict is `TabGone` and the context
  carries a route, `inTab` asks `Route.Replace(ctx, gone)`:
  - A route whose tab is no longer `gone` (another send of it already replaced it) returns its
    current tab, so one delivery makes at most one new tab.
  - A feed route asks `Tabs.Thread` again, under the per-feed lock. `TabRefused` has already
    dropped the gone tab's row, so `Thread` makes the replacement, or returns the one a concurrent
    delivery just made.
  - An object route re-runs its own lookup, `Destination`, whose object lock serializes it the same
    way: an open object gets a new tab with a fresh summary, and an object that is no longer open,
    or whose new tab cannot be had, resolves to its feed.
  - An answer equal to `gone` (the row could not be dropped, or the chat has no principal), or the
    main view, is no replacement.
  - A route attempts one replacement. A send that may be made again is resent in the replacement;
    when that is accepted the observer confirms the new tab as usual. When the replacement is
    refused too, or none was had, the observer hears of it, the send falls back to the main view,
    and the route sends the rest of its delivery there.
  - A send that may not be made again still repoints its route, so later sends of the delivery go
    to the replacement, and returns the refusal as it does now.
- **Suspect stays suspect.** A `TabSuspect` verdict never replaces: the send falls back to the main
  view and the tab is kept. A replacement that takes its send is confirmed, so the suspect window
  still costs at most one new tab per feed per day.
- **Every feed and every object.** Routes and replacement serve all six feeds and the trip, project
  and agent watch tabs. Watch alerts, events and trip notices resolve right before their send, so
  only replacement changes for them. Conversation tabs, which no route names, behave as before.
- **Offered cards follow the tab.** On a replacement, `Tabs` moves the principal's pending offered
  approval records (those with a heading, which continue no conversation) from the gone tab to the
  replacement in one statement. A moved record whose card had been sent is marked undelivered,
  keeping its chat, because its card went with the topic; the approval sweep then shows it again in
  the replacement. A record whose card is being sent at that moment has no message yet and gets one
  when that send, now in the replacement, completes. Two concurrent replacements move the same
  records to the same tab, so the move is idempotent.
- **A gone tab's conversation keeps what it had.** Chat-turn proposals and background tasks
  stored against the gone tab belong to its closed conversation and stay where they are; their
  sends keep falling back to the main view. Moving them would attach a closed conversation's
  proposals or task to the replacement tab's conversation, which confirming or a task report would
  then continue. The operator accepted this. Message records (report and alert messages, reply
  messages) describe messages that went with the topic and are not moved.
- **Telegram's description on every refused tab send.** `TabRefused` takes the refused error, and
  the observer gains `TabSendRefused(ctx, chatID, threadID, err)` for any other 400 to a send that
  named a tab. They log `telegram.send.thread_gone`, `telegram.send.thread_gone_suspect` and
  `telegram.send.refused` (method, error code, thread), and a replacement logs
  `telegram.feed.replaced` (the feed or object kind, gone and new thread IDs). Each carries a
  `description` attribute: Telegram's own description, with control characters replaced and cut to
  200 runes. It is never the message text, which only the request carries. No other event gains
  it, and `apiError.Error()` keeps it out.
- **Counted.** `RecordTab` counts a replacement as a new fixed outcome, `tab_replaced`.

## Approach

Five chunks in four waves; each lands with the full suite passing.

| Wave | Chunks |
| --- | --- |
| 1 | 1, 2 |
| 2 | 3 |
| 3 | 4 |
| 4 | 5 |

The simulator in `tabs_integration_test.go` already answers a send to a tab marked gone with
Telegram's 400 "message thread not found", records each refusal, counts made tabs, fails
creation with `createStatus`, and holds creation open with `createHold`. Chunk 1 adds a switch
that answers a tab's sends with any given 400 description.

1. **Telegram's description on refused tab sends** (Simple; no dependencies). `TabRefused` takes
   the error; `TabSendRefused` is added; the cleaned, capped description is logged.
   - Files: `internal/telegram/client.go`, `tabs.go`, `tabs_integration_test.go`.
   - Acceptance:
     - A send to a gone tab logs `description="Bad Request: message thread not found"` on
       `telegram.send.thread_gone`. Fails today: the simulator's description is never logged.
     - A tab send the new switch answers with another 400 logs `telegram.send.refused` with that
       description, and no message text reaches the logs. Fails today: nothing is logged.
2. **Routes resolved at first send** (Complex; no dependencies). `thread.Route`, `WithRoute`,
   `Delivery`, `Peek`, `Resolved`; `Tabs.Route`; `inTab` reads once and sends under
   `WithDelivery`; typing and drafts use `Resolved`; the four `inFeed` helpers; `TripTab`'s peek;
   `eval.Chat`.
   - Files: `internal/thread/thread.go`, `internal/telegram/feeds.go`, `objecttabs.go`,
     `client.go`, `trips.go`, `internal/schedule/service.go`, `internal/watch/service.go`,
     `internal/events/events.go`, `internal/eval/chat.go`, tests.
   - Acceptance:
     - A quiet scheduled inbox check with no Inbox tab registered makes no tab; a later check with
       a report makes it and lands there. Fails today: the simulator records a tab made at the
       start of the quiet run.
     - A weekly review run for a Russian-speaking user still names the tab it makes "Reports"
       (`TestWeeklyReviewBatchReadsInRussian`, kept). It would fail if the route resolved in the
       sending context's language.
     - With `createHold` holding the tab's creation, cancelling the sending context leaves the
       route unresolved, and the run's next send makes the tab and lands there. Fails if the
       route kept the main view after a cancelled resolution.
     - A trip language sample with no trip or Reminders tab makes none. Fails today: `TripTab`
       makes the tab.
     - Kept: a run's file, card, report and approval cards land in its feed tab, the run's
       context has `thread.From` zero, and deliveries of one feed at once make one tab. Tests that
       expected the tab at the start of a run wait for it at its first send instead
       (`feeds_integration_test.go`, `review_integration_test.go`).
3. **Replace a gone tab at once** (Complex; needs 2). `Route.Replace` for feeds and objects, its
   use in `inTab` for both kinds of send, the once-per-route rule, the main view after a failed
   replacement, the event and the outcome.
   - Files: `internal/thread/thread.go`, `internal/telegram/feeds.go`, `objecttabs.go`,
     `client.go`, `telemetry.go`, `internal/observability/telemetry.go`, tests.
   - Acceptance, each failing today because the simulator refuses the gone tab and the send
     falls back to the main view with no tab made:
     - A scheduled inbox report whose old, confirmed Inbox tab is gone lands in a new Inbox tab;
       nothing reaches the main view; the registry holds the new tab, confirmed; the report's
       message row records the new tab.
     - A report of several chunks with a card, whose tab is gone, makes one new tab, and the card
       and every chunk land there. Today each is refused and sent to the main view.
     - A document whose body cannot be read again, sent under a route to a gone tab, returns the
       refusal, and the route's next send lands in the new tab. Today the next send is refused
       again.
     - Two routes resolve the old Inbox tab while it is healthy. Then it is marked gone,
       `createHold` is set, and both send at once. The test waits for both refusals and for one
       creation, held. Only then does it release the hold. Exactly one new tab is made, and both
       messages land in it. Today both go to the main view.
     - A trip notice to a gone Reminders tab lands in the new Reminders tab
       (`TestAGoneFeedTabIsMadeAgainOnItsNextMessage` changes to say so).
     - A message to a gone tab of an open trip lands in the trip's new tab, after its fresh
       summary; a message to the gone tab of a trip that has ended lands in the Reminders tab. The
       same holds for a tab a trip took and for an agent watch's tab
       (`TestADeletedTripTabIsMadeAgainWithAFreshSummary`, `TestTripNoticesLandInTheTripsTab`,
       `TestADeletedTakenTabIsReplaced`, `TestAWatchWhoseTabCannotBeHadFallsBackAndGetsItAgain`
       change to say so).
     - When the replacement cannot be made (`createStatus` 429), the message and the rest of the
       delivery go to the main view, and the next delivery makes the tab. This passes today too
       and guards the fallback.
     - Unchanged: a fresh feed tab that refuses sends is not replaced for a day and its messages
       go to the main view; a gone conversation tab behaves as before.
4. **Offered cards follow the tab** (Medium; needs 3). A store method that moves pending offered
   approvals from one tab to another and marks the delivered ones undelivered, called on every
   replacement.
   - Files: `internal/telegram/tabs.go` (`TabStore`), `feeds.go`, `objecttabs.go`,
     `internal/store/sqlite/approvals.go`, tests.
   - Acceptance:
     - An inbox card whose send is refused as gone lands in the new Inbox tab, its records name
       the new tab, and the next check, with new follow-ups, sends no second card while it waits.
       Fails today: the records keep the gone tab, so the next check finds the new tab idle and
       the simulator receives a second card.
     - A card that was shown in the Inbox tab before the tab disappeared is shown again in the
       replacement by the approval sweep. Fails today: the record keeps its old message and the
       sweep sends nothing.
     - A chat turn's pending proposal in the gone tab is not moved.
5. **Documentation** (Simple; after 2–4). Update the Feed tabs and object tab sections of
   `docs/architecture.md`: routes, resolution at first send, peeking, gone versus suspect tabs,
   replacement, and moved cards.

## Files affected

- `internal/thread/thread.go`
- `internal/telegram/client.go`, `tabs.go`, `feeds.go`, `objecttabs.go`, `trips.go`,
  `telemetry.go`, and their integration tests
- `internal/schedule/service.go`, `internal/watch/service.go`, `internal/events/events.go`
- `internal/store/sqlite/approvals.go`
- `internal/eval/chat.go`
- `internal/observability/telemetry.go`
- `docs/architecture.md`, `docs/plans/README.md`

## Validation

- `gofmt -l cmd internal`, `go mod tidy -diff`, `go vet ./...`, `go build ./...`,
  `go test ./...`, `go test -race -timeout 30m ./...`, `scripts/check-plans`.
- Integration tests through the Telegram simulator, as listed per chunk.
- After deployment, by the operator: the next refused tab send carries a description, a
  replacement shows `telegram.feed.replaced`, and a quiet inbox check leaves no new tab.

## Risks and open questions

- **Gone without a replacement.** When no replacement is had, and the next delivery makes the tab,
  that tab does not know the gone one, so offered cards stay against the gone tab, as today.
- **A card that had fallen back to the main view** and is then moved is shown again in the
  replacement, leaving the main-view copy with stale buttons. Its records follow the new message.
- **Multi-message proposals.** A bundle too long for one message, refused partway, has its earlier
  messages marked undelivered by the move, and the sweep resends from the first of them, which
  repeats the later ones. Such bundles are rare.
- **Messages already in the gone tab.** Chunks a delivery sent before the refusal, and earlier
  messages, went with the topic and are not sent again. Edits and deletes of them address messages
  by ID, name no tab, and neither cause nor wait for a replacement; they fail as they do today.
- **Other refusal wording.** Only "message thread not found" counts as a gone tab. If Telegram
  answers a deleted topic in other words, the send fails as permanent and the run is
  undeliverable, as now; `telegram.send.refused` will show the words.
- **A wrong verdict costs a tab.** A tab Telegram refuses while it still exists, and that is not
  suspect, is replaced at once instead of at the next delivery. The registry already forgets it at
  that moment, so only the timing changes.
- **Transient errors never replace.** Rate limits, server errors and timeouts return as before; a
  scheduled delivery retries on the next tick through a new route that finds the replacement in the
  registry, so a retry makes no second tab. A resend that times out after Telegram posted it can
  repeat a chunk, as any transient failure can today.
- **Tab creation moves into sends.** A run's first send now waits for its tab to be made, inside
  that send's deadline.
- **Dashboard.** `tab_replaced` is a new value of an existing counter; a panel for it belongs to
  the separate monitoring repository and is not part of this plan.
