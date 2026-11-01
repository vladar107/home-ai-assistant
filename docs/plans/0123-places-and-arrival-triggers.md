# 0123 — Saved places, and arriving or leaving as a trigger

Status: `proposed`

## Context

Plan 0043 keeps the user's latest shared location: a pin, a venue, or a live
location that Telegram updates by editing the message. Only the latest fix is
stored, and the only place it is used is the model's turn line. The user
cannot save a place, and nothing happens when they arrive somewhere or leave
it.

Routes (plan 0057) are journeys between stations, not coordinates. Trips
store a destination name.

## Goal

- The user can save a named place from a pin, a venue, or their current
  location: "save this as Gym", or "this is Office". They can also list,
  rename, resize, and delete places.
- While the user shares their live location, arriving at a saved place and
  leaving it become events:
  - a `place` watch can alert on them ("tell me when I leave Office");
  - plan 0122's `place` anchor can run a job on them ("when I get home,
    remind me to water the plants").
- The turn line names the saved place the user is at, so the model knows
  "the user is at Office".
- Saved places can be targets of plan 0122's weather kind and of the weather
  tool.

## Decisions

- **Places are per-user configuration.** A new migration adds `places`:
  principal, ID, name (1–40 runes, unique per principal, folded case),
  latitude, longitude, `radius_m` (50–2000, default 150), and created and
  updated times. There are at most 20 per principal.
  - Coordinates are private data. They are stored only in the database, never
    logged, and never put into metric labels or span attributes.
  - Tool results show them rounded to four decimals.
- **Tools.**
  - `place_save` takes a name and either "current location" or the
    coordinates the user just shared. Its radius is optional.
  - `place_update` renames or resizes a place.
  - `place_list` lists places.
  - `place_delete` deletes one. A place that a watch or an anchored job uses
    is refused, and the refusal lists those dependents so the user can
    remove or change them first.
  - Saves and updates use act-and-tell with Undo in the user's own chat turn,
    and the card otherwise, as routes do.
- **Arrive and leave are decided in code, with hysteresis.** Each live
  location update is checked against every saved place:
  - The user is *inside* when the distance is at most the radius and the
    fix's accuracy is at most the radius.
  - The user is *outside* when the distance is more than 1.5 × the radius
    plus the accuracy.
  - Anything between those keeps the previous state.
  - A change of state must last for two consecutive fixes, or two minutes, to
    count, so one noisy fix does not trigger.
  - The state per (principal, place) is stored in `place_presence`.
  - A fix without `horizontal_accuracy` has unknown accuracy and never
    changes the state.
  - A one-off pin or venue never touches presence, because a pin may be
    about somewhere the user is not.
- **Live location is a precondition, and the user is told.** Telegram sends
  updates only while the user shares live location, for a period they choose
  or indefinitely. When a `place` watch or anchored job is created and the
  user is not sharing, the reply says that it works only while live location
  is shared.

  When live sharing ends, the presence state becomes `unknown`, and the next
  fix sets it without triggering. Telegram sends an edit only when the user
  stops a share early. A share that runs out (`live_until`) or goes silent
  sends nothing, so the location prune loop also resets presence when it
  expires the live fix.

  Presence is also reset to `unknown` whenever the stored live fix stops
  being the one presence was computed from. A pin or venue that replaces it
  is one case: later edits of the old live message are dropped. Forgetting
  the location is the other. Otherwise presence would freeze as inside and
  later give a false `leave`.
- **Events go two ways.**
  - The new `place` watch kind (triggers `arrive` and `leave`, with the usual
    window and mute) is evaluated on location events, not on the poll loop.
  - The schedule's `place` anchor resolver (plan 0122) is called with the
    same events and makes a run due immediately, after its offset if one is
    set.

  Plan 0122's watch-table rebuild already admits `place`, so this plan only
  lifts the tool-level refusal and adds no watch migration.
- **Privacy wording.** The location policy text and the README say that only
  the latest fix is kept. They gain one line: saved places, which the user
  creates, are kept until deleted, but their coordinates never come from a
  fix silently.
- **Turn line.** "At Office (saved place)" replaces or follows the coordinate
  line when the user is inside a place.

## Approach

1. Migration and store for `places` and `place_presence`. Add the
   `internal/location` place model, the tools, and the presence engine with
   hysteresis, driven by `Share` and `processEdit`.
2. The `place` watch kind and its evaluator, and the presence reset in the
   prune loop.
3. The `place` anchor resolver for plan 0122, replacing its stub.
4. Allow a place as a target in the weather tool and the weather watch.
5. Guidance, README, `docs/architecture.md`, and `docs/operations.md`.

## Files affected

- `internal/store/sqlite/migrations/NNN_places.sql`, a new store file
- `internal/location`, `internal/telegram/location.go`
- `internal/watch`, `internal/schedule` (resolver),
  `internal/connectors/web/weather.go`
- `internal/wiring`, `internal/prompt`, README, docs

## Validation

- Tests with a temporary SQLite store and synthetic fix sequences:
  - entering the radius with good accuracy gives one `arrive` after two fixes;
  - jitter at the boundary gives nothing;
  - a fix with poor accuracy changes nothing;
  - leaving gives one `leave`;
  - ending a live share, or letting it expire, sets `unknown` without an
    event;
  - a fix without accuracy changes nothing;
  - a pin never touches presence, and a pin replacing a live share resets it
    to `unknown`;
  - forgetting the location resets presence;
  - deleting a place that a watch uses is refused, with the watch named;
  - an arrival runs a `place`-anchored reminder once;
  - another principal's places and presence are never visible.
- A live check from the GoLand server: share live location, walk out of a
  test place with a small radius, and receive the `leave` alert.
- Formatting, module, test, vet, build, and plan checks.

## Risks and open questions

- Location updates depend on the user's phone and Telegram. Updates can
  pause in the background, and arrival may be noticed late. The guidance
  says this is approximate.
- Depends on plan 0122: its watch-table migration admits `place`, and its
  scheduler runs `place` anchors. Land 0122 first.
- Plan 0129 should export places but not presence.
