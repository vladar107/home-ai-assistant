# 0057 — Saved routes and easier schedule editing

Status: `proposed`

## Context

The audit behind plan 0055 found that editing scheduled reports caused the
most frustration by far: hours of back-and-forth and most of the explicit
annoyance.

- **Routes live inside prompts.** A commute report's prompt spells out every
  leg: walk or bike times, fixed train departures, transfers, arrival
  windows, and which legs apply on which weekday. The prompts reach the
  1000-character limit. A route that differs by weekday became five almost
  identical jobs, each with its own copy, and a change to the route meant
  five edits.
- **Edits are rewrites.** `schedule_update` takes the complete new prompt.
  The model rewrote prompts from scratch, dropped earlier rules, and turned
  loose preferences into hard rules nobody stated (a fixed buffer, a guessed
  walking time). Each rewrite is shown in full for confirmation, which also
  fills the bundle (plan 0055).
- **Testing means real runs.** The only way to see what a changed prompt
  produces is to confirm it and press "Run now". The user ran six real runs
  in eight minutes on one morning.
- **No way to withdraw.** When the model notices a mistake in a pending
  proposal, it can only ask the user to press Reject.
- **Each run plans each leg again.** Runs spent most of their budget
  planning legs one by one, and the same prompt gave different answers on
  different days.

## Goal

- The user can save named routes ("work", "Friday training") once, in
  conversation, and change one leg without restating the rest.
- One check of a saved route returns every leg's live status in a fixed
  format. A report prompt can then be as short as "report my work route".
- One weekday job can serve routes that differ by day.
- Schedule edits change only what the user asked for, and the user can
  preview a proposed job's output before confirming it.
- The model can withdraw its own pending proposals.

## Decisions

- **Routes are per-user records.** A route has a name, the weekdays it
  applies to, an optional arrival target, and a list of legs. Each leg is
  one of:
  - `walk` or `bike`, with minutes the user states;
  - `train`, with from and to stations and optional fixed planned
    departures;
  - `bus`, with a stop, a line, and optional fixed departures.
  There is one table, `routes`, with legs stored as checked JSON.
  Each route is owned by its principal, as everything else is.
- **Never invent a leg value.** Route tools reject a walking time,
  departure, or buffer the user did not give. The guidance tells the model
  to ask for a missing number instead of choosing one.
- **Route writes are confirmed**, as schedule writes are. `route_save`
  creates a route, `route_update` changes named fields or one leg, and
  `route_remove` deletes one. The summary shows the route as it would be
  saved.
- **`route_check` is code, not a prompt.** It checks a route for a date: each
  train and bus leg through the exact-departure checks from plan 0056, and
  walk and bike legs as stated. It returns one block per leg with a
  normalized status, then the overall result: whether the arrival target is
  still met and which legs failed to check. It counts as one budget call.
  Weather for the route's places is optional.
- **Routes are chosen by weekday.** `route_check` without a name checks the
  route that applies today. A single Monday-to-Friday job can then replace
  per-day copies. Existing jobs are not migrated automatically; the model
  can offer to consolidate them when the user next edits one.
- **Edits are patches.** `schedule_update` accepts `prompt_edits`: a list
  of `{find, replace}` pairs applied to the current prompt. The old
  `prompt` replacement stays for real rewrites. The confirmation summary of
  a patch shows only the changed passages, with the old and new text.
- **Preview before Confirm.** A proposal for a new or changed model-turn job
  gets a "Preview" button. It runs the proposed prompt once, read-only,
  through the same path as a scheduled run. The output is posted under the
  proposal, labelled as a preview. Nothing is saved, and no run is
  recorded. One preview runs per bundle at a time.
- **`proposal_withdraw`.** The model can withdraw pending proposals it
  made in the current bundle. The bundle message is edited to show what was
  withdrawn. Confirmed or running items cannot be withdrawn.

## Approach

1. Store: migration `018_routes.sql` and repository methods.
2. A routes package with validation, route tools, and `route_check` built
   on the NS and bus departure checks.
3. `/jobs` (or a routes page in `/settings`) lists routes read-only, with
   a delete button.
4. Schedule: `prompt_edits` in `schedule_update` and diff summaries.
5. Approval and Telegram: the Preview button and preview runs through the
   scheduled-turn path, without a run record.
6. Approval: `proposal_withdraw`.
7. Guidance: prefer routes in report prompts; ask instead of inventing leg
   values; patch, don't rewrite.
8. README, `docs/architecture.md`, and `docs/operations.md` for the
   migration.

## Files affected

- `internal/store/sqlite/migrations/018_routes.sql`, a new routes store
- a new `internal/routes` package (tools and the check)
- `internal/connectors/ns`, `internal/connectors/bus` (shared departure checks)
- `internal/schedule/tools.go`, `internal/approval/approval.go`
- `internal/telegram/jobs.go`, `internal/telegram/service.go`
- `internal/conversation/scheduled.go` (preview runs)
- `README.md`, `docs/architecture.md`

## Validation

- Integration tests:
  - Route save, update one leg, remove, and the confirmations.
  - `route_check` against recorded NS and OVapi data: on time, a
    cancelled leg, a failed check, and a missed arrival target.
  - Weekday selection.
  - Prompt patches and their summaries.
  - A preview run that leaves no job and no run record.
  - Withdrawing part of a bundle.
- `go test -race ./...`, `go vet ./...`, formatting, build, and
  `scripts/check-plans`.
- Live check: save a commute route in conversation, replace weekday copies
  with one job, preview it, then confirm.

## Risks and open questions

- Routes overlap with memory: someone could also store "I bike 12 minutes
  to the station" as a fact. The guidance should send route details to
  routes, and memory can point to a route by name.
- Preview runs cost a full model turn each. They use the scheduled path's
  timeout and one per bundle at a time.
- Station and stop names in legs are resolved when the route is saved, so
  a renamed station or stop needs the route to be edited.
