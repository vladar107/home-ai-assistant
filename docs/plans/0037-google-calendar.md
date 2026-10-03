# 0037 — Read-only Google Calendar

Status: `done`

## Context

[0036](0036-google-accounts.md) makes a Google account one grant with `calendar.readonly` next to
`gmail.readonly`, and a per-account Calendar switch. Nothing reads calendars yet, so "what do I
have tomorrow" or a scheduled morning summary of the day's events cannot be answered.

## Goal

A principal's turn, chat or scheduled, can list the events in a time range across every calendar
of every Google account with Calendar switched on, and read one event's details. Nothing can
create, change, or answer an event, because the credential cannot.

## Decisions

- **Scope `calendar.readonly` only**, granted through 0036's sign-in. No writes and no approvals.
- **Tools, both `tools.RiskRead`, so scheduled runs get them too:**
  - `calendar_events`: events between `start` and `end` (RFC 3339 or a local `YYYY-MM-DD`, at most
    62 days apart), with an optional free-text `query` and an optional `account` address. It reads
    every calendar the account has selected in Google Calendar, expands recurring events, orders
    them by start, and returns up to 100 with account, calendar name, event id, title, start and
    end in `HOME_AGENT_TIMEZONE` (all-day events as dates), location, and whether the user has
    accepted, declined, or not answered.
  - `calendar_read_event`: one event by `account`, `calendar_id`, and `event_id` from
    `calendar_events`: the fields above plus organizer, attendees with their responses, the
    conference link, recurrence, and the description reduced to text and bounded to 8 KB.
- Event titles, descriptions, and locations can be written by anyone who sends an invitation, so
  the guidance treats them as data, never instructions, like email. `calendar_read_event` is
  screened; the broker's untrusted-data prefix marks both results.
- **The connector holds no credential.** It asks 0036's Google module for the principal's Calendar
  accounts and an access token per account, and refreshes once on a 401.
- A calendar the user hides in Google Calendar (`selected` false) is skipped; an account whose
  calendars cannot be read is reported in the result instead of failing the whole call.

## Approach

1. `internal/connectors/calendar/client.go`: `calendarList.list` and `events.list`
   (`singleEvents=true`, `orderBy=startTime`, `timeMin`, `timeMax`, `timeZone`, `q`) and
   `events.get`, with the Gmail client's error classification. Complexity: moderate.
2. `internal/connectors/calendar/service.go`, `tools.go`, `render.go`: the provider, the two tools,
   their guidance, and rendering in the configured time zone. Complexity: moderate.
3. `internal/testutil/calendarsim`: a loopback Calendar API simulator that maps each bearer token
   to an account through a function the test passes. Complexity: moderate.
4. `internal/app/app.go`: build the provider when Google is enabled and register it with the
   broker. Complexity: simple.
5. The `/privacy` sentence and `docs/architecture.md` and `docs/operations.md`. Complexity:
   simple.

## Files affected

- new `internal/connectors/calendar/` (`client.go`, `service.go`, `tools.go`, `render.go`, tests)
- new `internal/testutil/calendarsim/`
- `internal/app/app.go`, `internal/telegram/google.go` (privacy line)
- `docs/architecture.md`, `docs/operations.md`, `docs/plans/README.md`

## Validation

- `gofmt -l`, `go mod tidy -diff`, `go test -race ./...`, `go vet ./...`,
  `go build -buildvcs=false ./...`, `scripts/check-plans`.
- Against the simulator: events from two accounts and several calendars merge in start order with
  their account and calendar; a hidden calendar is skipped; recurring events arrive expanded;
  all-day events render as dates and timed ones in the configured zone; `account` narrows the
  search; a range over 62 days or with the end before the start is refused; a read returns
  attendees and a bounded description; an expired token is refreshed once; an account whose
  Calendar switch is off is not read.
- Live, from the operator's phone: ask about tomorrow's events and schedule a weekday summary.

## Risks and open questions

- Invitations put a stranger's text into the model's context, the same exposure as email.
- Large shared calendars can return many events; the limit and the range cap bound a call.
- Free/busy across other people's calendars, creating events, and answering invitations are left
  for later plans.
