# 0068 — Home Assistant watches: tell me when something at home changes

Status: `done`

## Context

Plan 0058 built watches: code-checked, change-only alerts with durable
delivery, "Mute today" and "Pause", and a failure notice. It named Home
Assistant state as a later kind and said the state, deduplication, and
delivery design should carry it. Today the only kinds are `departure`,
`route`, and `disruptions`.

Plan 0039 gives the assistant one household Home Assistant token. The model
reaches the entities exposed to Assist, and entities labelled
`assistant-confirm` through proposals. Nothing tells a user about a change
at home unless they ask.

## Goal

- A user can ask "tell me when the washing machine is done", "warn me if the
  balcony door is open for more than 10 minutes", or "tell me if the freezer
  goes above −15 °C", and confirm it once.
- The service checks in code, without a model turn, and messages the owner
  only when the entity enters the watched condition. If the user asked for
  it, a message also follows when the condition clears.
- Watches stay per user. Either household member can watch any entity the
  assistant can see, under their own Home switch.

## Decisions

- **One new kind, `home`, with a fixed contract.** `Target.Entity` holds the
  entity ID. `Target.Name` holds its friendly name as resolved. The spec
  gains a `Condition`:
  - `state`: a list of states, such as `open` or `idle`
  - `above` and `below`: numeric thresholds, given as pointers
  - `for_minutes`: 0 or 1–240

  Exactly one of `state`, `above`, or `below` is required. The triggers are
  `enter` and `clear`. The spec is stored in the existing JSON `config`
  column.
- **A migration admits the kind.** `watches.kind` has a
  `CHECK (kind IN (...))` constraint (`018_watches.sql`). Migration
  `022_home_watches.sql` therefore rebuilds the table with `home` added.
  It also adds a nullable `since` column to `watch_states`, and rebuilds
  `watch_states` and `watch_alerts` to raise their message and sequence
  limits from 5 to 10.
- **Triggers.**
  - `enter` alerts when the condition starts to hold.
  - `clear` sends a message when the condition ends after an alert.
  - Both are on by default for numeric conditions. Only `enter` is on by
    default for `state` conditions, so "washer finished" does not ping
    again when the next wash starts.
- **Duration is tracked by the watch, not by `last_changed`.** A numeric
  sensor's `last_changed` moves with every new value, and a Home Assistant
  restart resets it, so it is not used to measure duration.
  - The engine stores when the condition started holding. Evaluators read
    it as `Occurrence.Since` and set it through `Observation.Since`.
  - A `state` condition seen holding for the first time is seeded from
    `last_changed`, so "open for 10 minutes" counts from the real opening.
    While it keeps holding, the stored start is kept, so a Home Assistant
    restart neither resets the duration nor alerts again.
  - While the condition holds but `for_minutes` has not passed, the
    observation is silent.
- **Skipping a check is not a failure.** Today any evaluator error counts
  toward the failure notice. A new sentinel, `watch.ErrSkip`, makes the
  engine skip the check without counting it or changing state.
  - The `home` evaluator returns it while the owner's Home switch is off.
  - Transit kinds keep their current behavior.
- **Binary sensors report `on` and `off`.** The resolver maps common words,
  such as open, closed, detected, clear, wet, and dry, to `on` and `off` for
  `binary_sensor` entities and refuses other values. Headlines use the
  sensor's device class, so a door reads "open" or "closed".
- **Alert numbering survives edits.** An edit forgets the window's state but
  not its sent alerts, so a window's numbering continues after the last
  alert already sent and its cap still counts them.
- **What may be watched.** The resolver accepts only entities the assistant
  can already see: those exposed to Assist, or labelled `assistant-confirm`.
  - The friendly name must resolve to exactly one entity ID through the
    snapshot. An ambiguous or unknown name is refused, and the model is
    shown the candidates.
  - A lock can be watched ("tell me if the front door stays unlocked"), but
    cannot be controlled through this kind. Presence entities stay out of
    reach.
  - At check time, the entity must still be visible in the snapshot, which
    lives for 5 minutes and is reused rather than refreshed every minute.
    If it is no longer visible, the check fails, and the existing failure
    notice tells the owner once a day while it stays unchecked.
- **Whole-day windows.**
  - A `home` watch runs on weekdays or one date, like the other kinds. Its
    window defaults to `00:00`–`24:00`. `24:00` is accepted only for this
    kind and means the end of the day, so the window has no blind minute.
  - The user may narrow the window ("only between 07:00 and 23:00"). A
    narrow window is how quiet hours are expressed, so no global quiet
    hours are added.
  - Transit kinds keep their 4-hour and 12-hour limits.
- **No second alert across midnight.** A window's first check has no state
  row for its date. When the window runs `00:00`–`24:00` and the previous
  date's last check ran in the minutes before midnight, it carries over the previous day's `Key`,
  `Alerted`, and `Since`, but not `Messages` or `Failures`. A door that stays
  open past midnight therefore alerts once, and its `clear` message still
  follows when it closes. After any other gap, such as a narrowed window
  overnight, a skipped weekday, or a muted day, the window starts fresh. The
  engine cannot tell what happened during the gap, so a condition that still
  holds is reported again.
- **Polling, shared per cycle.** Home watches check every minute. All of a
  cycle's home watches share one `GET /api/states`, under a household-wide
  key in the existing check cache. Each kind has its own check interval,
  and `Spec.Describe` states the interval of the watch's kind. Pushing
  events over Home Assistant's WebSocket API is a possible later plan.
- **Message limits and expiry.**
  - The cap on messages per window is 10 for `home`; transit kinds keep 5.
  - A home alert expires 30 minutes after it is queued rather than at the
    window's end, so one queued at 23:59 is still delivered.
- **Alert text.** Alerts are written by code from the household's own Home
  Assistant names and states. Like transit alerts, they are passed to the
  next turn as ordinary notices, not outside content.

## Approach

1. Engine, store, and Telegram (chunk A):
   - Migration `022_home_watches.sql` and the store changes for `since` and
     loading the latest earlier state.
   - The `home` kind and condition validation.
   - `ErrSkip`, `Since`, and the per-kind interval, cap, and alert expiry.
   - `24:00` window ends and the carry-over of state across midnight.
   - Labels, descriptions, and tool schema and guidance.
   - Watch rendering in Telegram, which goes through the watch's label and
     description, so `internal/telegram` needs no change.

   The kind is offered only once its evaluator is registered.
2. Evaluator and resolver, plus wiring (chunk B, after A), in
   `internal/connectors/homeassistant/watch.go`:
   - Resolve the name to one entity and check the owner's switch.
   - Read the states once per cycle through the cache and check visibility
     against the snapshot.
   - Normalize the state and write headlines such as "Balcony door has been
     open for 10 minutes."
   - Register the evaluator in `internal/app`, zero the metrics for the kind,
     and update the README, `docs/architecture.md`, and
     `docs/operations.md`.

## Files affected

- `internal/store/sqlite/migrations/022_home_watches.sql`, the watches store
- `internal/watch/watch.go`, `service.go`, `tools.go`, and tests
- a new `internal/connectors/homeassistant/watch.go` and tests
- `internal/app/app.go`
- README, `docs/architecture.md`, `docs/operations.md`

## Validation

- Existing transit watch tests pass unchanged.
- The migration test: an existing transit watch survives the rebuild, and a
  `home` watch can be stored.
- Engine tests with a fake clock and a temporary SQLite store cover:
  - `ErrSkip` neither counts a failure nor changes state
  - a `24:00` window end
  - carry-over across midnight, with no second alert and a `clear`
    message afterwards, and a fresh start after a narrowed window's gap
  - an edit mid-day followed by an alert that is still delivered
  - the per-kind cap and alert expiry
- Evaluator tests against a fake Home Assistant HTTP server cover:
  - `state` and numeric conditions
  - `for_minutes` seeded from `last_changed` for state conditions and from
    the first check for numeric ones
  - an ambiguous name being refused
  - an entity that is no longer visible giving one failure notice
  - the Home switch off giving no checks
  - one `/api/states` read per cycle for several watches
- `gofmt`, `go mod tidy` diff, `go vet`, `go test ./...`, `go build`, and
  `scripts/check-plans`.
- A live check against the household Home Assistant from the operator's
  Mac, with the GoLand server: watch an exposed binary sensor, toggle it,
  and receive the alert.

## Risks and open questions

- Minute polling means an alert can arrive up to about a minute late.
- An appliance's "finished" state depends on how its integration models it.
  Some need a template sensor in Home Assistant first. That is Home
  Assistant configuration, not code here.
- Both users read through one token, so either could watch a shared device
  the other uses. This is accepted for household devices. Presence entities
  stay unexposed.
