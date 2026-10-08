# 0058 — Watches: tell me when my train or route goes wrong

Status: `done`

## Context

The assistant acts only when the user writes or a schedule fires. The audit
behind plan 0055 showed the user polling by hand. "How is NS?" and "did NS
break again?" came up at least ten times in a month. The morning and
evening reports were built mostly to answer one question at a fixed time:
are my usual trains and buses running? A report at 07:00 cannot catch a
cancellation at 07:25. It also costs a model turn, and its quality varies
from run to run.

Plan 0057 adds saved routes and `route_check`, which turns a route into
normalized leg statuses in code. Plan 0056 adds exact-departure checks. A
watch can build on both without a model in the loop.

## Goal

- The user can ask, in conversation, "tell me if my 07:42 is cancelled or
  more than 5 minutes late on weekdays". They can also ask "watch my work
  route" or "warn me about disruptions on my route before I leave", and
  confirm it once.
- During the watch's window the service checks on its own, and a message
  arrives only when something changes: a cancellation, a delay over the
  threshold, a platform change, a missed arrival target, a new disruption
  on the route, and later the return to normal.
- Silence means "fine". If checking keeps failing, the user is told once.
- Watches survive restarts, can be listed, paused, and removed, and are
  strictly per user.

## Decisions

- **Code checks; the model sets up.** Watch checks and alert texts are code.
  No model turn runs per check, so no status can be invented and checking
  costs no tokens. The model only proposes, changes, and explains watches.
  Replying to an alert starts an ordinary conversation turn about it.
- **Kinds in this plan:**
  - `departure`: one train or bus departure (fixed planned time, or a leg
    of a saved route). Triggers: cancelled, partly cancelled, delay of at
    least N minutes, platform change.
  - `route`: a saved route checked with `route_check`. Triggers: any leg
    failing a trigger, or the arrival target no longer reachable.
  - `disruptions`: NS disruptions and engineering works that touch the
    stations of a route or of a station pair. Triggers: a new disruption,
    or a change in an existing one.
- **Windows instead of quiet hours.** A watch has weekdays (or one date for
  a one-off watch, which then expires) and a window relative to the
  departure, by default 90 minutes before until departure. A disruption
  watch has a fixed time window. Checks run every 5 minutes in the window,
  and every 2 minutes in its last 30 minutes. Global quiet hours come with
  the household and mail kinds that need them.
- **Alerts happen on changes only.** A state table keeps the last
  normalized state for each watch occurrence. An alert is sent when the
  state changes to one that matches a trigger. A "back to normal" message
  follows only if an alert was sent earlier. Each occurrence sends at most
  five messages. Identical upstream data never alerts twice.
- **Failures are visible.** After three failed checks in a row within a
  window, one "I can't check your 07:42 right now" notice is sent. A
  recovery message follows only if a real alert would be due.
- **Proposals are confirmed.** `watch_propose`, `watch_update`, and
  `watch_remove` are confirmed writes. Their summaries list the kind,
  target, triggers, days, and window. `watch_list` is a read. Each user may
  have 20 watches.
- **Delivery is durable.** Alerts are rows that go from `ready` to
  `delivered`, as scheduled runs do. They are retried on transient Telegram
  failures and recovered after a restart. A pending alert older than its
  window is dropped, never sent late. Alert messages have "Mute today" and
  "Pause" buttons.
- **Checks are shared.** Within one check cycle, identical upstream
  queries (the same station pair and time, or the same disruptions scope)
  are answered from one call. NS requests stay well within the API's
  limits for a household.
- **A separate automation module.** `internal/watch` is registered as an
  automation module with its own loop. It does not run as a scheduled job
  type, because job runs are model turns with a run per occurrence. It is
  switched on with `HOME_AGENT_WATCHES_ENABLED`.
- **Later kinds:** Home Assistant state (a door left open, an appliance
  finished), mail rules, and weather on a route (rain on a bike leg). The
  state, deduplication, and delivery design is meant to carry them, but
  they are separate plans.

- **As built: the engine.** A watch's definition is one JSON column. Its
  window is local times within one day, so a window that would cross
  midnight is refused. The lead time is 10–240 minutes, the delay threshold
  1–120 minutes, and a disruption watch takes 1–10 stations. A label is
  generated, so there is no name field. A change from one triggering state
  to another alerts again, within the cap of five. An edit resets the
  watch's window state, and a one-off watch is deleted once its window ends.
  Kinds are offered only once an evaluator is registered. Evaluators get
  the window's last state (`Previous`), can store a change silently
  (`Silent`), and resolve station and stop names when a watch is proposed
  (`Resolver`). Alerts are passed to the next turn as ordinary notices, not
  outside content, because code writes them from transit data.
- **As built: checks.** A departure watch makes one exact-departure check
  per cycle. A delay below the threshold counts as on time. Above it, the
  delay is put in 5-minute buckets, and "not in the timetable" always
  alerts. A disruption watch asks NS per station both now and at the end
  of the window, so engineering works that start before the user leaves
  are reported in advance. Its state is each disruption's ID with a
  fingerprint of its content, so a changed expected end alerts as
  "changed" while wording churn does not. A route watch uses
  `routes.Service.Check` and judges each leg by the departure the route
  actually takes. A cancelled departure that a later one replaces does not
  trigger the leg trigger by itself; the arrival trigger reports it when it
  makes the target late. When only some legs fail to check, an active alert
  is held, so no false "back to normal" is sent.

## Approach

1. Store: migration `018_watches.sql` with `watches`, `watch_states`, and
   `watch_alerts`, and the repository.
2. `internal/watch`: the model and validation, windows and check timing,
   evaluators for the three kinds on top of the plan 0056 departure checks
   and `route_check`, state changes, failure notices, and the check cache.
3. Delivery: durable alerts through the conversation deliverer, the
   "Mute today" and "Pause" callbacks, and restart recovery.
4. Tools: `watch_propose`, `watch_update`, `watch_remove`, `watch_list`,
   and guidance on when to suggest a watch instead of a scheduled report.
5. `/jobs` lists watches next to jobs, with pause and delete.
6. Metrics: checks by kind and outcome, alerts sent, failure notices, and
   upstream errors, plus a Grafana panel in the home-agent dashboard.
7. Config: `HOME_AGENT_WATCHES_ENABLED` in `.env.example`, the local
   `.env`, and the NAS env file; README, `docs/architecture.md`, and
   `docs/operations.md`.

## Files affected

- `internal/store/sqlite/migrations/018_watches.sql`, a watches store
- a new `internal/watch` package
- `internal/routes`, `internal/connectors/ns`, `internal/connectors/bus`
  (shared checks)
- `internal/telegram/jobs.go`, `internal/telegram/service.go` (callbacks)
- `internal/app` (wiring), `internal/config`, `internal/observability`
- `.env.example`, README, `docs/architecture.md`, `docs/operations.md`

## Validation

- Integration tests with a fake clock and recorded NS and OVapi responses:
  - on time stays silent;
  - a delay crossing the threshold alerts once, and a later recovery sends
    "back to normal";
  - a cancellation alerts;
  - a new disruption on the route alerts, and the same one does not alert
    again;
  - three failed checks send one notice;
  - a restart in the middle of a window keeps the state, so no duplicate
    alert;
  - a late alert past its window is dropped;
  - mute and pause;
  - another user's watches are never visible or changeable.
- `go test -race ./...`, `go vet ./...`, formatting, build, and
  `scripts/check-plans`.
- Live check for a week: one departure watch and one disruption watch on
  the operator's commute, compared with the NS app.

## Risks and open questions

- Delay data near departure can change quickly. The 2-minute interval and
  "changes only" rule should avoid floods, but the threshold and the
  five-message cap may need tuning after the live week.
- A watch on a saved route depends on route data staying correct. A route
  edit applies to the next check, and the watch summary names the route
  instead of copying it.
- Whether a watch alert should also update the next scheduled report (for
  example "your 07:42 was cancelled at 07:21") is left for later.
