# 0134 — Calendar listings that page instead of failing

Status: `done`

## Context

The weekly review filed a backlog item: `calendar_events` returned 20 tool errors and 16
oversized results in one week. The logs show where both come from:

- Every tool error is the 62-day span refusal ("the range can span at most 62 days"), returned
  before Google is called. They come from the daily trip scan, whose prompt asks the model to
  cover the next 180 days "in three calls"; the model's own split lands just over the cap, so the
  first calls of each scan are refused.
- Every oversized result comes from the same scan's wide windows. A listing holds up to 100
  events, but a rendered event takes roughly 150–350 bytes, so a busy two-month window passes the
  broker's 16 KiB result limit and the whole result is replaced by "ask for a narrower result".
  The model then retries with narrower windows until one fits, spending the scan's call budget.

The calendar package tests call the service directly, so no test ever passed a listing through the
broker's size check.

## Goal

`calendar_events` never fails only because a range is long or busy. A long range is read for its
first 62 days, a busy one is listed up to a moment before which it is complete, and the result
names that moment to continue from. The trip scan asks for windows that fit. The next weekly
review shows no span refusals and no oversized `calendar_events` results.

## Decisions

- A range over 62 days is read for its first 62 days, not refused; the result says where it
  stopped and how to continue. The cap keeps its purpose (bounded Google reads) without costing a
  failed call.
- `calendar_events` declares its result cap explicitly, kept at 16 KiB, as `collection_query`
  declares its own. Events are rendered within the cap minus 2 KiB, less the problems lines;
  the header is written after the cut is known, from a bounded set of parts, with the quoted
  query's length taken from the budget. Problems lines clip
  calendar names like event lines do, and together are held to 4 KiB, ending with a count of the
  calendars or accounts left out, so many unreadable calendars cannot crowd out the events.
- One continuation rule covers every way a listing can be cut short (the byte budget, the
  100-event cap, a calendar whose first page had more): the continuation moment is the earliest
  of the first unshown event's start and, for each calendar that had more, the start of the last
  event on its raw page, counting events the listing skips (cancelled, unreadable times).
  Events starting at or after that moment are dropped before rendering, so every result is
  complete for the span it names. Google lists events whose end is after the range start, so a
  continuation can repeat an event already listed but cannot skip one; the result says repeats
  are possible.
- A calendar that answers with no events but a next-page token is read on, page by page, up to
  three pages, until it yields an event or ends; if it still has none, it is reported like a
  calendar that cannot be read and does not hold back the other calendars' events.
- If the continuation moment is not after the requested start (more events at one instant than
  fit), the result keeps today's advice to narrow with
  a query instead of offering a continuation that would loop.
- The trip scan prompt names three explicit, contiguous date windows from the scan's scheduled
  date, with inclusive date-only ends: D to D+59, D+60 to D+119, D+120 to D+179, 60 days each.
  Date-only bounds are read in the calendar tool's own zone, so the windows stay contiguous and
  under the cap whichever zone produced D. The prompt says to continue where a result says it
  stopped. The mail part of the prompt is unchanged.

## Approach

1. `internal/testutil/calendarsim`: a way to make a calendar answer with short or empty pages
   that still carry a next-page token, and to list cancelled events, so the cut-short paths are
   reachable in tests.
2. `internal/connectors/calendar`: clamp the end to start + 62 days when it is later; track each
   cut-short calendar's last raw start; follow empty pages as decided; compute the continuation
   moment and drop events at or after it; render within the budget and then write the header
   with the covered span and, when cut short, the `start` to pass next with the same end, query,
   and account, and that repeats are possible. Update the `end` schema description to say a longer range is read for its first 62
   days.
3. `internal/trip/scan.go`: `ScanPrompt` takes the scheduled date and names the three windows;
   its only caller passes the occurrence's scheduled time in its location.
4. Tests (below).

## Files affected

- `internal/testutil/calendarsim`
- `internal/connectors/calendar/tools.go`, `render.go`, `calendar_test.go`
- `internal/trip/scan.go`, `internal/trip/scan_integration_test.go`
- `docs/plans/0134-calendar-listing-pages.md`, `docs/plans/README.md`

## Validation

- Calendar tests against the simulated Google Calendar, each paging loop bounded so a regression
  fails rather than hangs, and every result checked against the definition's declared cap:
  - 130 short events, and a set of long-titled events, are listed across successive calls
    following the stated continuation; every event appears at least once.
  - Two calendars, one cut short early and the other busy later: no event is skipped, and no
    result shows an event at or after its continuation moment.
  - A calendar whose first page is empty but carries a token is read on; one that stays empty is
    reported without hiding the other calendar's events.
  - A 90-day range reads 62 days and names the continuation; the over-span case leaves the
    refused-ranges test, and the "more exist than the 100 shown" assertion is replaced.
  - Many problems lines still leave a valid result within the cap, also when every calendar
    fails.
  - Events at one instant beyond what fits get the narrow-with-a-query advice.
- `scan_integration_test.go`: for a fixed date the prompt names the three windows, each 60 days
  counted with inclusive ends; the existing mail wording assertions still pass.
- Format, vet, module, test (including `-race`), build, and `scripts/check-plans`.
- After deploy, the next morning's trip scan logs `calendar_events` with no `tool_error` or
  `oversized` outcomes; the backlog item is closed after that.

## Risks and open questions

- Paging spends calls from the turn budget; a very busy 60-day window can need several. The scan
  already stops and reports what it has when the budget is exhausted.
- A zero-length event that starts exactly at the continuation moment is not returned by Google
  (its end is not after the range start). Accepted as negligible.
- The `end` description change alters the tool definitions once, a one-time prompt cache miss.
