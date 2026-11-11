# 0139 — Updates for a trip while it is near or under way

Status: `done`

## Context

A tracked trip gets a start notice, check-in reminders, and an end notice. Nothing tells the user
when something about the trip changes: an organiser's announcement for the event they travel to,
a schedule or venue change, a strike at the destination, or a calendar event moved inside the
trip's dates. The Trips scan reads mail and calendars only to find bookings.

## Goal

From three days before a planned trip starts until it ends, the assistant checks about twice a
day for important news about it in the user's mail, on the web, and in their calendar, and posts
what is new to the trip's tab. It stays silent when nothing is new.

## Decisions

- No new scheduled job. The trips loop, which already sends start and end notices, runs the
  check, so the Trips switch, the job list, and the job cap stay as they are. A trip is checked
  only while its owner's Trips scan job exists and is enabled: the switch that finds trips is
  the switch that follows them.
- A trip is due when it is planned, now is between three days before its start and its end, the
  local time is between 08:00 and 21:00, and its last attempt was at least eight hours ago. The
  local time is the household's until the trip starts and the trip's own zone after that, so the
  two or so checks a day fall at sensible hours where the user is.
- A manual Trips scan also makes the trips then in the window due, once, so a check can be
  asked for.
- A check is one read-only turn through the scan's `Checker`, with a call budget of 30 and the
  chat turn's time limit, framed by its own prompt line since it is no scheduled job.
- At most one check runs at a time for the whole loop, in its own goroutine so notices are not
  held up; a cycle that finds one running starts none. The attempt time is recorded before the
  turn starts, so the next cycle does not pick the same trip. The check runs under the loop's
  context and is joined at shutdown; a check cancelled before it sent anything records nothing
  further, and one that already sent still records what it sent.
- The model returns strict JSON and code decides what is sent, as the scan does:
  - `updates`: source (`mail` or `web`), origin (the sender's or site's name), a reference that
    identifies the item (the mail's message ID or the page address), a title, and one or two
    sentences on what changed and why it matters;
  - `events`: the calendar events inside the trip's dates, with ID, title, start, end, and place,
    all-day events by their dates, and whether the list is complete.
- Important means it changes the user's plans, timing, access, safety, or costs. Marketing,
  newsletters, and general news about the destination are left out; the prompt says so. The
  prompt also lists what was already sent for the trip, as data, so the same news under a new
  address is not sent again.
- Code drops an update whose source and reference were already sent, and finds calendar changes
  itself by comparing `events` with the last complete snapshot:
  - a moved event (time or place) is reported;
  - a removed event is reported only after it was missing from two complete lists in a row;
  - an added event is reported once, whoever added it;
  - the first complete list is the baseline and reports nothing;
  - a list from a turn that ran out of budget, failed, or says it is incomplete changes nothing;
  - when the trip's own dates change, the next list is a new baseline.
- One plain-text message per check, at most five items, each naming its origin, under a first
  line that names the sources the items come from and, for mail and the web, says they are not
  verified. Links, web addresses, mentions, and commands are stripped, and each field and the
  whole message are length-limited. Items over the cap are not recorded as sent and the mail
  horizon stays behind them, so the next check offers them.
- The message is sent to the trip's tab with the existing `Sender` and handed to the user's
  next turn as outside content, per user as event messages are, so a reply to it does not start
  a turn that trusts mail or web text.
- Mail, web, and calendar text is data in the prompt, never instructions, as in the scan.
- Mail is searched back to the last successful check, at least two days and at most eight. When
  mail cannot be read the check still covers the other sources and the mail horizon does not
  move. A source the user has off is skipped and not mentioned.
- An item is recorded as sent only after its message was delivered, so a failed send is offered
  again. An unreadable reply or a failed turn sends nothing and is tried again after the same
  eight hours.
- Sent items (source, reference, origin, title, text, time) and the per-trip state (snapshot,
  last attempt, last success) are deleted with the trip. They are not exported, like the scan's
  own log: after a restore one item can repeat.

## Approach

1. Store: a migration for the sent items and the per-trip state, repository methods, and the
   two tables named in the export's list of what is deliberately left out.
2. Check: the prompt, JSON decoding and validation, limits, deduplication, calendar comparison,
   message text, and failure behaviour, in the trip package beside the scan.
3. Loop and wiring: the due rule, running and joining a check from the loop, the scan-enabled
   gate, the notice source built before the conversation takes its notices, the manual scan's
   hook, and log lines as the scan has.
4. Documentation: the trips part of `README.md` and the in-bot trips help.

Stage 1 comes first; stages 2 to 4 are one piece of work after it.

## Files affected

- `internal/store/sqlite/migrations/` and the trip repository in `internal/store/sqlite`
- `internal/trip/` (the check beside `scan.go`, `loop.go`, `trip.go`)
- `internal/prompt/unattended.go` and its golden text
- `internal/wiring/wiring.go`, `internal/wiring/trips.go`, `internal/wiring/export.go`
- `internal/telegram/trips.go` and its integration tests
- `README.md`

## Validation

- Repository tests on a temporary database: insert, duplicate reference, state round trip,
  cascade delete with the trip.
- Check and loop tests with a fake model runner and clock:
  - no trip in the window, a disabled scan job, or a trip at night runs no turn; the night is
    the household's before the trip starts and the trip's zone after;
  - a trip is checked again only after eight hours, or after a manual scan, whose request does
    not carry over to a trip that enters the window later;
  - an all-day event compares by its dates; a change of the trip's dates reports nothing;
  - an event removed, restored, and removed again is reported each time;
  - a second cycle during a running check starts no other check;
  - shutdown during a running check returns promptly and records no result;
  - a new update is sent once and not again, and appears in the next prompt as already sent;
  - a moved event is reported, the first list is silent, an added event is reported once, and a
    removed event is reported only after two complete lists;
  - an incomplete list or an exhausted budget leaves the snapshot alone;
  - unreadable JSON sends nothing; a failed send is offered again; items over the cap arrive
    with the next check;
  - links, mentions, and commands are stripped and each item names its origin;
  - with web search off the check asks for none;
  - the user's next turn receives the message as outside content;
  - a start notice due in the same cycle is not delayed by a running check.
- The configured format, module, test, vet, build, and plan checks.
- After deployment: a real trip with a known organiser mail in the window.

## Risks and open questions

- What counts as important is the model's judgment. Deduplication bounds repeats, not
  relevance; the wording is tuned on live trips.
- The text of an update is written by a model from mail and web content. It names its origin and
  carries no links, but a crafted mail can still be retold as an update.
- The check reads private mail and then searches the web in one turn, as the scan's check-in
  lookup does. The turn is read-only, its searches after outside content go through the existing
  search check, and the call budget limits them.
- Mail-derived text of sent items is kept in the database until the trip is pruned.
- A failed check is visible only in logs, since it is not a scheduled job with a report.
- Each trip in the window costs about two model turns a day on the user's subscription.
