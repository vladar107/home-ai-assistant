# 0074 — Watch anything: model-checked watches

Status: `in-progress`

## Context

Watches (plans 0058 and 0068) tell the user only when something changes, but
every kind is checked by code: a departure, a route, NS disruptions, and a Home
Assistant entity. The user wants the same for anything the assistant can look
at: a ticket price dropping below a number, a mail from a sender arriving, a
product coming back in stock, a page announcing a date.

Such checks cannot be code: the source and its shape differ every time. The
assistant can already answer each of them in a turn, with web search, web
fetch, Gmail, and the other read tools. Scheduled `model.turn` jobs run such a
turn on a schedule, but they report every time instead of only on a change.

## Goal

- The user can ask "tell me when a return flight Amsterdam–Lisbon on 20
  December drops below €150", "tell me when a mail from the tax office
  arrives", or "watch this page for the release date", and confirm it once.
- The service checks at the agreed interval, runs one read-only model turn per
  check on the owner's own subscription, and messages the user only when the
  answer changes into one that matches the condition, or, if they asked, when
  the matching value changes again or the condition stops holding.
- Failures, mute, pause, listing, editing, and removal work as for every other
  watch.

## Decisions

- **A new kind, `agent`.** `Target.Name` is a short label the model gives
  ("Lisbon flight price"). The spec gains a `Check`:
  - `instruction` (1–600 runes): what to look at and how, including any URL
    or mail search the model should use.
  - `condition` (1–300 runes): when to tell the user, such as "the cheapest
    price is below €150".
  - `interval_minutes`: 30–1440, default 60.
  - `until` (optional date): the last day the watch runs; it is deleted after
    that day, as a one-off watch is. It cannot be combined with a one-off
    `date`.

  Triggers are `enter` (the condition starts to hold, on by default),
  `change` (the reported value changes while the condition holds), and
  `clear` (the condition stops holding after an alert). The window defaults to
  every day `00:00`–`24:00`, as for home watches, and can be narrowed.
- **A migration admits the kind.** `022_home_watches.sql` rebuilt `watches`
  with a `CHECK` on `kind`; migration `025_agent_watches.sql` rebuilds the
  three watch tables again with `agent` added, keeping every row, state, and
  alert. `watch_alerts` gains a `private` flag, so an alert queued before a
  restart still reaches the next turn as private.
- **Each check is one isolated, read-only turn.** The evaluator asks the
  conversation service for an unwatched turn like a scheduled run: the
  owner's own subscription, a fresh session, read-only tools, web search when
  the owner has it on, and the owner's memory. The message carries the
  instruction, the condition, the last value reported (so the model can say
  what changed), and the current local time. It must end with a JSON object:
  `{"met": bool, "value": "short normalised value", "headline": "one line",
  "details": "optional short text"}`, or `{"error": "what failed"}` when it
  cannot do the check. A reply without a valid object, an `error`, or a failed
  turn counts as a failed check, so the existing failure notice applies. The
  check turn has no turn key: nothing goes out with it but the alert, so its
  tools stage no cards.
- **State from the verdict.** The observation's state is `met` or `unmet` with
  the normalised value, so the next check's message can name the value last
  reported. A changed value is stored silently unless the condition holds and
  `change` is on, so a changed price alerts again only when asked, and a move
  between met and unmet is never silent. Values are compared after trimming
  and folding case and whitespace. A `clear` message reads "No longer the
  case." rather than "Back to normal."
- **State across days.** What an agent watch watches changes over days, not
  within one window, so a new window starts from the previous window's state,
  alert flag, and last check time whatever the gap. A condition that keeps
  holding is therefore not reported again at midnight, and the interval runs
  on across days.
- **Checks never block the watch loop.** A model turn takes far longer than a
  code check. It takes no turn slot, as a background task does not, so the
  owner's chat turn never waits behind one. The evaluator therefore
  runs agent checks in the background, at most one at a time per principal and
  two in total: `Evaluate` starts a due check and returns `ErrSkip`, and the
  next `Evaluate` after it finishes returns its observation. Other kinds keep
  their pace. The kind is polled every minute; the evaluator decides when a
  check is due from `interval_minutes` and the last check, failed or not. A
  check of a watch edited while it ran is thrown away. When a check ends, the
  evaluator wakes the service so the result is collected at once, and the
  service stops running checks when it shuts down.
- **Limits.** Each principal may keep five `agent` watches within the existing
  twenty, checked when proposed and again when confirmed. Each window may send
  ten messages, and an alert expires 30 minutes after it is queued, as for
  home watches. A check turn has the usual turn timeout.
- **Alerts are outside content.** The headline and details are written by a
  model that read web pages or mail. Agent alert notices are handed to the
  next turn as outside notices, and as private when the check read private
  content, like scheduled reports. Alert text in Telegram shows the label,
  headline, and details; "Mute today" and "Pause" work unchanged.
- **Only the owner's integrations.** The check turn sees only the tools the
  owner has switched on. A watch whose check needs a tool the owner later
  switches off fails, and the failure notice tells them.
- **Guidance.** Propose an `agent` watch when the user wants to be told when
  something they cannot get from a transit or home watch changes or happens,
  such as a price, a mail, stock, or a page. Prefer a transit or home watch
  when one fits, because those cost no model turn. Pick the longest interval
  that still serves the request, and say the interval in the proposal. Write
  the instruction so that a fresh turn without the conversation can do it.
- **Telemetry.** Checks are counted by kind as today; agent checks also count
  their model turns' tokens through the existing turn metrics, and the kind's
  series are created at zero.

## Approach

1. Engine and store: migration `025_agent_watches.sql`; the `agent` kind,
   `Check` validation, `change` trigger, `until`, the per-kind cap, and labels
   and descriptions; tool schema and guidance.
2. Conversation: `RunCheck`, an unwatched check turn without a turn key that
   returns the final text and whether it read private content, sharing
   `RunScheduled`'s setup.
3. Evaluator in `internal/watch/agent.go`: the background runner, the message,
   parsing the verdict, and the state.
4. Notices: agent alerts as outside, private-aware notices.
5. Wiring in `internal/app`, metrics zeroing, README,
   `docs/architecture.md`, and `docs/operations.md`.

## Files affected

- `internal/store/sqlite/migrations/025_agent_watches.sql`, the watches store
- `internal/watch/watch.go`, `service.go`, `tools.go`, a new `agent.go`, and
  tests
- `internal/conversation/scheduled.go`
- `internal/app/app.go`
- `internal/telegram` integration tests and their harness
- README, `docs/architecture.md`, `docs/operations.md`

## Validation

- The migration test: existing transit and home watches, states, and alerts
  survive the rebuild, and an `agent` watch can be stored.
- Engine and evaluator tests with a fake clock, a temporary SQLite store, and
  the fake model runtime:
  - a first check with the condition unmet stays silent; a later met verdict
    alerts once; the same verdict again stays silent;
  - with `change`, a new value while met alerts again; without it, it does
    not;
  - `clear` sends a message only when asked for and only after an alert;
  - an unparseable reply counts as a failure, and three give one notice;
  - a slow check does not delay a home watch's check in the same cycle;
  - the interval is respected, and `until` deletes the watch after its day;
  - the sixth agent watch is refused;
  - the check turn is offered read-only tools only, and another principal's
    watches are never visible or changeable.
- A live check from the GoLand server: watch a public page for a value that
  changes, and receive one alert.
- Formatting, module, test, vet, build, and plan checks.

## Risks and open questions

- Each check costs a model turn on the owner's subscription; five watches at
  hourly checks are about 120 turns a day. The limits and the interval
  guidance keep this bounded; usage shows on the dashboard.
- Model verdicts can flip on ambiguous pages. Values are normalised, and the
  instruction should name a precise source; a flapping watch is visible and
  can be muted.
- Sites that block automated fetching or need a login cannot be watched this
  way.
