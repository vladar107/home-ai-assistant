# 0168 — Security audit fixes: outside text, link previews, revocation

Status: `done`

## Context

A source-and-sandbox security audit of the repository at `c23a307` confirmed seven low or medium
findings and left four leads open. Live follow-up checks then settled two of those leads.

- Telegram's preview crawler fetches a hidden-label link in a bot message about a second after
  the send, with no tap, unless the message disables link previews.
- The app container shares the client network with the MCP smoke run.

Nothing found lets an outsider take over an account or read across principals. The operator chose
which findings to fix, and this plan covers them. Two are left as they are:

- `generate_image` stays read-risk, a deliberate choice in plan 0066.
- Home Assistant alias collisions are not addressed.

## Goal

- The bot never asks Telegram to fetch a link in a message it sends or edits.
- A crafted web page cannot grow its reduced text past the input cap or make the reducer do
  unbounded work.
- **Map text marks the conversation.** OpenStreetMap results mark the conversation as having read
  outside content.
- **Forwarded positions stay out of the location line.** A forwarded location or venue is never
  stored as the user's own, so its text cannot enter the line.
- **Location-line text is quoted.** Text that stays in the location line (venue fields the user
  picked, the reverse-geocoded address) is quoted as data. This mitigates, rather than closes,
  the case of a user picking a venue with a hostile name; see Risks.
- **Removing a user stops everything.** Removing someone from the Telegram users file ends every
  delivery to them and every background run on their behalf, and adding them back restores both.
- **No links in amounts.** Mail-derived amounts carry no links.
- **The DMARC gate reads only real clauses.** Text inside a comment or a quoted value cannot
  satisfy the gate for routine replies.
- **The smoke check runs no third-party code.** The MCP smoke check runs no third-party code on
  the client network.

## Decisions

- **Link previews are off for every `sendMessage` and `editMessageText` call.**
  - The Telegram client's `execute` sets `link_preview_options` to disabled for those two methods
    when the caller has not set it. Every path goes through `execute`, including the HTML event
    send in `events.go`, which sets its text directly.
  - The per-call helper that the trip and parcel cards use goes away.
  - Photo captions and message drafts are out of scope because neither method takes the option.
  - Keeping previews only for code-written messages would need every call site classified, and a
    missed one would reopen the channel.
  - Rich previews of links the assistant shares are lost. Nothing relies on them.
- **A forwarded location or venue is refused, not stored.**
  - It is someone else's position, so storing it as the user's own is wrong even without the
    injection.
  - The check uses the existing `Message.Forwarded()`.
  - The bot replies that it keeps only the user's own location.
  - Keeping just the coordinates would still record a position the user never shared.
- **The location line drops point-of-interest names.**
  - The reverse geocoder's `PLACE` matches are crowd-edited map names. The line keeps addresses
    and transit stops.
  - The describer's cache is in memory, so the deploy restart clears names that are already
    cached.
- **Text left in the location line is quoted.**
  - The venue title, the venue address, and the `near` text are each quoted as data with
    `strconv.Quote`.
  - Pins stored before the change expire within their lifetime (one hour by default, at most 24
    hours), so they are not cleared separately.
- **`nearby_places` is outside content:** `ThirdParty: true, Screen: true`, like the web and mail
  readers.
- **The HTML reducer stops at 2 MiB, the input cap.**
  - Once the output reaches 2 MiB, the reducer stops walking the document, so it stops resolving
    links as well as writing.
  - It leaves out link targets over 2048 bytes.
  - Mail reduction gets the same cap. Its output is unchanged below the cap.
  - `html.Parse` is already bounded by the 2 MiB input cap.
  - The web guard refuses redirect targets over 2048 bytes, the bound the model's own address
    already has.
- **The users file is the source of truth for the allowlist.**
  - On startup, the bootstrap revokes allowlist rows whose Telegram user is not configured,
    stamping `revoked_at` with the later of `allowed_at` and now so a fast clock cannot break the
    CHECK.
  - It clears the revocation when a user is configured again. Clearing `revoked_at` alone
    satisfies the CHECK, so `allowed_at` is left alone.
  - Every background delivery already resolves its chat through `ResolveActiveChat`, which honours
    the revocation. The callers are:
    - schedule runs and previews;
    - watch alerts;
    - event delivery and event-token authentication;
    - background tasks;
    - assistant-mail notices and triage;
    - the inbox check;
    - memory review;
    - trip, parcel, watch and project cards;
    - file delivery;
    - language changes.
  - Schedule runs, tasks and triage check the chat before starting a model turn. Watch checks
    now do too: a principal without an active chat has no watch checked. So the removed user's
    subscription and upstream services are not used either.
- **DMARC clauses are read with a single-pass scanner.**
  - The scanner skips RFC 8601 comments (nested, with backslash escapes) and quoted strings, and
    splits on `;` only outside them.
  - Each skipped span becomes a space, so tokens cannot fuse.
  - An unterminated comment or quote refuses.
  - More than one `dmarc=` clause refuses.
  - The existing check for `dmarc=fail` anywhere still runs on the raw values.
- **The MCP smoke client runs on the plain Python base image.**
  - The image is the one already pinned in the server Dockerfile. The script reads it from the
    `PYTHON_IMAGE` argument, so the two cannot drift.
  - The client runs as `python -I -S`. Docker pulls the image by its pinned digest if the build
    kept it only in the build cache.
- **No consult was needed.** Each fix has one clear approach. The product choices above belong to
  the operator, who approves them with this plan.

## Approach

Every chunk is one commit and adds a test that fails without its fix. Workers run in their own
worktrees from `main`, and the orchestrator rebases the commits onto `main` in order. Shared and
small items are the orchestrator's.

1. **A — link previews off** (Sonnet).
   - `execute` in `internal/telegram/client.go` disables previews for `sendMessage` and
     `editMessageText`.
   - Remove `setNoLinkPreview` and its four call sites in `trips.go` and `parcels.go`.
   - New test file `internal/telegram/link_preview_test.go`. Its fake Bot API fails any
     `sendMessage` or `editMessageText` request that lacks the disabled option. It drives:
     - a chat reply;
     - an edit;
     - an HTML event send and its plain fallback;
     - a trip card.
2. **B — bounded page text** (Opus).
   - `internal/htmltext` stops walking once its output reaches 2 MiB, for both pages and mail.
   - It skips link targets over 2048 bytes.
   - `checkURL` in `internal/connectors/web/guard.go` refuses target URLs longer than
     `maximumURLBytes`.
   - Tests:
     - a 2 MiB page of `<a href=?>` with a long base URL reduces to at most 2 MiB, and the walk
       stops early, shown by the output size and a call count or a bounded allocation;
     - a redirect to an over-long URL is refused;
     - existing mail rendering tests pass.
3. **C1 — forwarded positions and quoted location text** (Opus).
   - `processLocation` in `internal/telegram/location.go` refuses a forwarded message with a new
     EN/RU reply beside the other location replies in that file.
   - `TurnLine` in `internal/location/service.go` quotes the venue title and the `near` text (the
     venue address or the reverse-geocoded name). The settings line and the share acknowledgement
     stay unquoted for the user.
   - New integration test file `internal/telegram/location_forward_test.go`, through the
     conversation harness. It checks that:
     - a forwarded venue is answered with the refusal and not stored;
     - a later turn's location line carries quoted venue text.
4. **C2 — map text is outside content** (Opus, same worker as C1, separate commit).
   - `nearby_places` in `internal/connectors/places/tools.go` gets `ThirdParty` and `Screen`.
   - `PLACE` leaves `matchRank` in `internal/connectors/places/describe.go`.
   - Tests in `internal/telegram/location_forward_test.go`:
     - a write after `nearby_places` is proposed, not done;
     - a reverse geocode whose only match is a `PLACE` gives no name in the location line.
5. **D — DMARC clause scanner** (Opus).
   - Rewrite `dmarcPassed` in `internal/connectors/assistantmail/routine.go` as decided above.
   - Tests in `routine_test.go`:
     - refused: a forged quoted mail-from, `;` inside a comment, nested and escaped parentheses,
       an unterminated quote or comment, and two `dmarc=` clauses;
     - accepted: the real Gmail form `dmarc=pass (p=NONE sp=QUARANTINE dis=NONE)
       header.from=…`;
     - the existing routine tests pass.
6. **E — allowlist follows the users file** (Opus).
   - `BootstrapConfiguredPrincipals` in `internal/store/sqlite/telegram.go` revokes unconfigured
     rows and restores re-configured ones in the same transaction, as decided above.
   - Tests:
     - bootstrap two users, then bootstrap again with one. The removed principal no longer
       resolves an active chat, its event token no longer authenticates, and a due schedule run
       for it delivers nothing;
     - revoke, restore, revoke again;
     - revoke when `allowed_at` is ahead of the clock.
7. **F — intake amounts without links** (orchestrator).
   - `internal/intake/intake.go` validates amount values with links stripped, so a value left
     empty is dropped.
   - A test drops a linked amount and keeps a plain one.
8. **G — smoke client off the server image** (orchestrator).
   - `mcp-servers/scripts/smoke` changes as decided above.
   - Run `shellcheck` if present.
9. **Bookkeeping** (orchestrator).
   - Update `docs/architecture.md` where it describes link previews, forwarded locations, the
     location line, or the allowlist bootstrap.
   - Update this plan and its index row.
   - Run the full check set.
   - Commit the plan with the last chunk that lands.

All workers (A, B, C1+C2, D, E) run in one wave; F, G and bookkeeping follow after the rebases.

## Files affected

- **A:** `internal/telegram/client.go`, `trips.go`, `parcels.go`, `link_preview_test.go`.
- **B:** `internal/htmltext/htmltext.go` and its tests; `internal/connectors/web/guard.go` and its
  tests; one line in `internal/connectors/web/fetch.go` so the refusal logs as `url_too_long`.
- **C1:** `internal/telegram/location.go`, `internal/location/service.go`,
  `internal/telegram/location_forward_test.go`; existing location tests updated for the quoted
  turn line.
- **C2:** `internal/connectors/places/tools.go`, `describe.go`, and their tests; the conversation
  test harness gains a places simulator option, and its stand-in geocoder answers with an address.
- **D:** `internal/connectors/assistantmail/routine.go`, `routine_test.go`.
- **E:** `internal/store/sqlite/telegram.go` and its tests; `internal/watch/service.go`, so a watch
  check needs an active chat, with a test; new tests in `internal/events` and
  `internal/schedule`; a watch test that bootstrapped a third user on its own now inserts that user
  directly, since a bootstrap now revokes whoever it does not list.
- **F:** `internal/intake/intake.go` and its tests.
- **G:** `mcp-servers/scripts/smoke`.
- **Orchestrator:** `docs/architecture.md`, this plan, `docs/plans/README.md`.

## Validation

- Each chunk's new test fails on `main` and passes with the chunk.
- The full check set passes:
  - `scripts/check-plans`;
  - gofmt;
  - `go mod tidy -diff`;
  - `go vet`;
  - `go test ./...` with and without `-race`;
  - `go build`.
- A final review of the whole diff.
- Before deploying, the operator confirms the NAS users file lists every household member. The
  first start revokes anyone it does not list.
- After deploying, the operator checks that:
  - a chat reply containing a link shows no preview card;
  - one `scripts/smoke` run passes.

## Risks and open questions

- **Previews disappear everywhere.** Users lose link preview cards in every bot message,
  including links the assistant shares on purpose.
- **Forwarding a pin no longer works.** A user can no longer forward a friend's pin to ask about
  it. The bot refuses it and asks for their own location.
- **Picked venues keep their names.** A venue the user picks from Telegram's crowd-sourced venue
  search still puts its name into the location line, quoted but without marking the turn. Picking
  a hostile venue is an unlikely act by the user themselves, so this residual risk is accepted.
- **Large pages are cut short.** A very large page is cut at 2 MiB of text. The model already
  reads such a page through a 12 KiB window.
- **Revocation waits for a restart.** Revocation takes effect at the next start, and editing the
  users file already requires one. A users file that is missing a member on the NAS revokes that
  member at the first start, hence the pre-deploy check.
- **Fewer places get names.** The location line may name fewer places when the only nearby match
  is a point of interest.
