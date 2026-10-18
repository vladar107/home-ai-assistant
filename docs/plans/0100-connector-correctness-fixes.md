# 0100 — Connector and tool correctness fixes

Status: `done`

## Context

An audit of the connectors and tools found a set of small correctness faults.
Each one was checked in the code before it was fixed:

- `fetch_page` moves a continuation offset forward to the next rune start with
  no upper bound. An offset inside the last multi-byte rune of a page indexes
  past the end and panics, so the tool call breaks.
- Gmail, Calendar, and the Google OAuth client each copy the same error type
  and HTTP status classifier, and the copies disagree. Calendar maps 403 to
  forbidden. Gmail has no 403 case, so a refused or withdrawn Gmail permission
  is reported as "unavailable right now". The OAuth client has no 403 or 404
  case.
- In `calendar_events`, one calendar that fails, such as a shared calendar
  answering 403, aborts its whole account, although failures are already
  collected per account. A Gmail search does the same for one message that
  fails.
- `ha_history` prints `change: last - first` for every series. For a series of
  per-bucket changes, such as an energy meter's daily kWh, that difference
  means nothing: daily 10, 12, and 9 kWh is reported as "change: -1 kWh".
- The Grafana client has no 429 case and checks the body size before the
  status, so an oversized error page reads as "too large". Grafana failures are
  never logged, unlike the NS, bus, and places connectors.
- When the movie table has more rows than are read, only `movies_next` says
  so. `movies_schedule` and `movies_set_score` act on the partial table.
- `nas_save_attachment` accepts a name that contains `/`. A name such as
  `../x` becomes a proposal that can never succeed, since traversal is still
  refused when the save runs.
- `attachment.Stage` leaves the files it already wrote on disk when a later
  file fails, until the next startup purge.
- `connection.Ready` does not check that the credential file belongs to the
  bound account, while `connection.Credential` does.

## Goal

Each fault above is gone, and a regression test covers it. No tool flags,
settings, or other connectors' behaviour change.

## Decisions

- **One Google error type.** The `google` package exports `Error`,
  `ErrorClass`, and `Classify`. `Classify` maps 400, 401, 403, 404, and 429.
  The error keeps a product name, so its text still says Gmail, Google
  Calendar, or Google. Gmail and Calendar use this type. Their own copies are
  removed.
- **A connector reads only its own product's failures.** `ProductError` finds
  an error of the named product. A failed token refresh is a Google error with
  no product, so Gmail and Calendar still report it as unavailable, as they did
  when it was a different type. A 400 from the token endpoint is never read as
  bad search syntax.
- **Notion keeps its own classifier.** Its classes have different meanings
  (`restricted` for 403, `not_shared` for 404), and it treats 529 as a rate
  limit. Merging it with Google would not make the code simpler.
- **A Gmail 403 has its own text.** It says that Google refused access to the
  mailbox, and that the user can check the connection in /settings. A 403 can
  also mean a usage limit. Google's reason is in the body, which is never read,
  so the text names both causes.
- **The OAuth client keeps its token handling.** A 403 from a refresh is still
  not treated as a revoked grant. Only `invalid_grant` is, and a sign-in is
  still rejected only on `invalid_grant` or 400.
- **Calendar failures are collected per calendar.** A calendar that fails adds
  a line to the problems. 401 still fails the account call, so the token is
  refreshed and the account is read again. An account counts as failed only if
  its calendar list failed, or if every calendar it tried to read failed. The
  call is an error only when every account failed.
- **Gmail failures are collected per message.** A message that fails is
  skipped and counted in the result, with the reason. 401 still fails the
  mailbox, so the token is refreshed and the search is retried. A rate limit
  stops reading more messages from that mailbox, because they would fail too.
  The messages that were already read are kept. A mailbox where no message
  could be read counts as failed, like a calendar account, so the result never
  says that no messages match.
- **A change series reports its total.** When the values are per-bucket
  changes, `ha_history` prints `total:` (the sum of the buckets shown) instead
  of first, last, and change. Mean and state series are unchanged.
- **Grafana follows the other connectors.** 429 maps to a rate-limit class.
  The status is checked before the size, so an oversized error body still gets
  its status class, and an oversized query error is reported as a query error.
  Every Grafana request failure is logged as `grafana.request.failed`, with the
  principal, the tool, and the error class.
- **Movie writes refuse a partial table.** New films are added at the bottom,
  and rows are read in creation order. So the rows that are not read are the
  newest ones, and the latest date, the undated films, and the most recent row
  of a film may all be among them. `movies_schedule` and `movies_set_score`
  refuse the call and say why. `movies_next` keeps its note.
- **The movie table is read in pages of 100.** It used to be read 25 rows at a
  time, the cap for pages the model asks for, so the 10 pages it reads ended
  at 250 rows. A film diary grows past that in a few years of weekly evenings,
  and the refusal would then block both writes on every call. This internal
  read now uses Notion's maximum of 100 rows per page, which raises the limit
  to 1000 rows. The client accepts up to 100 for its own reads. The 25 cap now
  sits in the tools' `limit`, which every model-facing search and query passes
  through, so what the model can ask for does not change.
- **A save name is a file name.** A name that contains `/` or `\` is refused
  when the save is proposed. The message says to put folders in `folder`.
- **Stage cleans up after itself.** If a file fails, the files that were
  already written are removed, and so is a partly written file.
- **Ready checks the account like Credential does.** The credential file must
  have the bound account's ID. `Status` uses the same check, so /settings never
  shows "connected" for a subscription that a turn would refuse. This cannot
  lock out a user with the right account. The application writes the bound
  account's ID when it connects. Pi's refresh writes the ID again, taken from
  the same `chatgpt_account_id` claim of the new token, and the database never
  holds an empty ID. So only a credential for a different account fails the
  check, and `Credential` already refuses that one.

## Approach

1. Bound the rune-start loop in `renderWindow`, and check the end again.
2. Add the shared Google error to `internal/connectors/google`, and move Gmail
   and Calendar to it. Add the Gmail 403 text.
3. Collect per-calendar and per-message failures.
4. Print the total for change series in `ha_history`.
5. Align the Grafana status handling and add failure logging.
6. Refuse movie writes on a truncated table.
7. Refuse save names that contain a separator.
8. Clean up staged files on failure.
9. Check the account ID in `Ready` and `Status`.

## Files affected

- `internal/connectors/web/fetch.go`
- `internal/connectors/google/client.go`, `internal/connectors/gmail/`,
  `internal/connectors/calendar/`, `internal/testutil/calendarsim/`,
  `internal/testutil/googlesim/`
- `internal/connectors/homeassistant/history.go`
- `internal/connectors/grafana/`
- `internal/connectors/notion/movies.go`, `client.go`, `tools.go`
- `internal/connectors/nas/writes.go`
- `internal/attachment/attachment.go`
- `internal/connection/service.go`
- Regression tests beside each package.

## Validation

- A regression test for each fault, plus the full formatting, module, vet,
  build, test, and plan checks.

## Risks and open questions

- A Gmail 403 caused by a usage limit is reported with the access-refused
  text, which names both causes. Reading Google's error reason from the body
  could tell them apart. That is left for later.
