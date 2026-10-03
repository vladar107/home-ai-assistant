# 0041 — Grouped /settings with an overview and pages

Status: `in-progress`

## Context

`/settings` renders every section into one message: model, Notion, NAS,
Google, web, trains, home, buses, and safety checks, each with one to three
lines of prose and its own button row, and Google adds two rows per account.
With nine sections the message runs to about fifteen lines above a dozen
button rows, and states that need action (a Google account to reconnect, an
invalid Notion token, paused home actions) sit in the middle of the list.

Each section already owns a `<prefix>:<action>` callback vocabulary, and a
press re-renders the whole message in place with `editMessageText`.

## Goal

`/settings` opens a short overview with one line per group and a button per
group. A group page either shows its sections' controls directly or links to
a page per section. Every page has a Back button, navigation edits the same
message, and anything that needs attention is flagged on the overview and on
the button that leads to it.

## Decisions

- Five groups, in this order:
  - Model: the model section.
  - Accounts: Google and Notion, one page each.
  - Home & files: home and NAS, controls on the group page.
  - Everyday tools: web, trains, and buses, controls on the group page.
  - Safety & privacy: safety checks, controls on the group page.
- A group with no configured sections is left out. A group with a single
  section links straight to that section's page.
- Navigation is stateless: `nav:<page>` callbacks name the page, and a section
  action re-renders the page that holds that section. A stale or forged press
  still only refreshes a view, and an old pre-change settings message keeps
  working because section callbacks are unchanged.
- Each section gains a one-line summary for the overview and an optional
  attention line; its existing render becomes its page content.
- A toggle's confirmation becomes a line on the refreshed page instead of a
  separate message. Messages the user must act on stay separate: model and
  Google sign-in instructions, the Notion token intake, and failure replies.
- `/privacy` is unchanged; the privacy pointer line stays on the overview.

## Approach

1. Split the section result into a page note and a separate message, and move
   the existing confirmations into notes.
2. Add `summary` to every section and a fixed group table in `settings.go`.
3. Render the overview, group pages, and section pages, and route `nav:`
   callbacks and section actions to the right page in `processCallback`.
4. Update the existing settings assertions in the integration tests to the
   new pages.

## Files affected

- `internal/telegram/settings.go`
- `internal/telegram/{google,web,ns,homeassistant,bus,typesafe}.go`
- `internal/telegram/service.go` (settings branch of `processCallback` only)
- `internal/telegram/*_integration_test.go` assertions on settings output

## Validation

- `gofmt`, `go mod tidy` check, `go vet`, `go test ./...`, `go build`, and
  `scripts/check-plans`.
- Live check through the operator's GoLand server: open every page, toggle a
  setting on each group page, and press a button on a settings message sent
  before the change.

## Risks and open questions

- Group pages that hold several sections keep each section's full prose and
  button row; shortening that text is left for later.
- Another agent is changing `internal/telegram/service.go` for plan 0040;
  edits here stay inside the settings branch, and the commit stages only
  these hunks.
