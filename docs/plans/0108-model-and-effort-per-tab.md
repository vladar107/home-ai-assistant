# 0108 — Model and effort per tab, and subscription usage in /status

Status: `in-progress`

## Context

Every model turn runs on one configured model and effort (`HOME_AGENT_PI_MODEL`, today the
cheapest Luna model at medium). Background tasks, which can run for an hour and do the most
reasoning, run on the same cheap setting. A user cannot see which model answered, choose a
stronger one for a hard conversation, or see how much of their ChatGPT subscription is left.

Research on the pinned Pi 1.0.0 found:

- `--model <id>` and `--thinking <level>` pick the model and effort per run. A resumed session
  restores its last recorded model and level when the command line gives none, and keeps its last
  level when only a model is given, so both are passed on every run. Switching model inside one
  session file is supported; Pi silently clamps an unsupported level, so the app validates first.
- Each model's supported levels come from its `thinkingLevelMap` in Pi's catalog, which Pi caches
  per profile in `models-store.json` (written under Pi's file lock). A `null` entry means
  unsupported; which levels alias each other varies per model.
- Each assistant message reports the model that answered and its token usage. The effort used is
  not reported, so the effort shown is the one requested.
- Pi does not expose subscription usage. A read-only `GET /backend-api/wham/usage` with the
  user's own token was verified on 2026-10-18: it returns the plan type, a five-hour and a weekly
  window (used percent, reset time), whether the limit is reached, and per-model availability.
  It also returns account identifiers and the email, which are never logged, stored, or shown.
  `/backend-api/codex/usage` is blocked and is not used.

## Goal

- Chat keeps the configured default. Background tasks default to a new configurable task model,
  `gpt-6.1-sol:medium`.
- In any tab or the main view, `/model` shows the model and effort in use, lists every available
  model with a short description and its supported effort levels, and changes them with buttons
  or a typed shortcut. The choice holds until `/new` in that tab.
- `/status` shows the model and effort in use, the task default, the model that answered the last
  turn with its tokens, and the user's subscription usage.

## Decisions

- **The choice belongs to the lane until `/new`.** A lane is the main view or one tab (one
  principal and thread). The choice is stored per lane, so `/model` works right after `/new`, and
  every conversation in that lane uses it — including one resumed with `/sessions` or by replying
  to an older answer — until `/new` clears it. A tab the user closes clears it; a tab that only
  looked gone and comes back keeps it. A conversation resumed into a new tab starts on the
  defaults. As with every command, `/model` sent as the first message of a tab created moments
  before is answered in the main view and sets the main view's choice.
- **Storage.** Migration 041 adds `conversation_lane_models` (one row per principal and thread)
  and nullable `model`/`effort` columns on `tasks`. `/new` and a tab the user closes delete the
  lane's row in the same transaction as the session change; a tab taken for gone does not. Setting
  a choice stores an alias level as the effort the model runs, and is refused when a readable
  catalog does not offer the pair; with no readable catalog the pair is stored as given.
- **A stale choice falls back.** Each turn rechecks the lane's choice against the catalog when it
  is readable; a model that left the catalog or a level it no longer supports runs on the default,
  and `/status` says so.
- **Tasks are configured, not chosen per tab** (operator decision). `HOME_AGENT_PI_TASK_MODEL`
  (default `gpt-6.1-sol:medium`) is the task default; `/model` affects chat only. A task copies its
  model and effort when it is created — directly or when its proposal is confirmed — so an env
  change or a restart never moves a running task to another model. Its report run uses the same
  pair. A task created before this change and already started keeps the chat default it began on.
- **One parser for the defaults.** Both settings are parsed once into a model and an effort; a
  missing `:level` means `medium`, the level name is validated at load, and neither may be empty.
  Image generation receives the parsed model id instead of parsing the setting itself. Callers always pass the
  pair; the runtime falls back to the chat default only for runs no caller sets (tab naming,
  rewrites, scheduled runs and checks, which are fresh sessions). Image generation keeps using the
  chat default's model id, never a lane's choice.
- **A default the subscription cannot use falls back.** If a principal's catalog lacks the task
  model, or the last cached usage read marks it unavailable, the task runs on the chat default and
  `/status` says so. A missing or unreadable catalog, or no cached usage, is not a reason to fall
  back. A task queued before this change and not yet started takes the task default, with this
  check, at its first run.
- **The catalog is Pi's.** Choices are validated against the principal's cached catalog for the
  configured provider, read
  under Pi's lock or treated as missing when unreadable; a missing catalog offers only the models
  the defaults name. Levels come from each model's `thinkingLevelMap`, deduplicated by the effort
  they map to. Descriptions use the catalog's name, context size and relative cost.
- **Catalog reader.** `internal/modelcatalog` reads the store under the lock directory Pi takes
  (a two-second wait, since a person is waiting) and returns `ErrUnavailable` for a missing,
  locked, partial, or provider-less file. `Fallback` builds the defaults-only list, each model on
  its default's effort, for callers that need something to offer. The runtime passes `--model` and
  `--thinking` separately, never a `:level` suffix, and the answering model and summed tokens come
  back in the run result.
- **Short names.** A family name such as `sol` or `luna` resolves to the model of that family with
  the highest version number in the catalog; anything ambiguous is refused with the options.
  Availability marks need an exact id match with the usage response.
- **Buttons use their own callback prefix**, distinct from the `/settings` Model section's. A
  button tapped after `/new` applies to the lane's new conversation, which is the same lane;
  that is accepted rather than adding a freshness token that would not fit the 64-byte limit.
- **Last turn in memory.** The model and tokens of each lane's last turn are kept in memory;
  after a restart `/status` says no turn has run yet.
- **Usage reads are the user's own.** The usage read uses only the requesting principal's
  credential, through a dedicated GET path on the ChatGPT client that returns sentinel errors and
  never the response body. It decodes into a struct of the shown fields only, is cached about a
  minute per principal and account, and is bounded by a two-second timeout that includes any
  token refresh, since Telegram updates are handled one at a time for the whole household. Only
  `/status` reads it live; `/model` and task creation use the cached result and never wait on it. A principal who is not connected sees that; any other failure shows "usage
  unavailable".
- **No per-reply footer and no `/settings` page** (operator decision). The `/settings` Model
  section and the `app_help` model topic mention `/model`.
- The Home Agent dashboard in the monitoring repository is not changed here.

## Approach

1. Runtime and config: `RunRequest` gains the model and effort; the Pi runtime passes
   `--model`/`--thinking` on every run; its result reports the answering model and tokens; model
   telemetry gains the effort; a catalog reader lists models and levels per principal; config
   parses both defaults.
2. Usage: the dedicated usage read on the ChatGPT client.
3. Lanes: a migration for the lane choice; conversation methods to read, set, and clear it, cleared
   by `/new` and tab closing; chat turns pass the effective pair; last-turn model and tokens kept
   per lane.
4. Tasks: a migration for the task's copied pair; the copy on create, the fallback, and both task
   runs passing it.
5. Telegram `/model`: listing, buttons (model, then its levels, plus back to defaults) and typed
   forms with validation.
6. Telegram `/status`, `/help`, `/privacy`, `app_help`, the Model section line; docs.

## Files affected

- `internal/runtime/runtime.go`, `internal/runtime/pi/`, `internal/testutil/pifixture/`,
  `internal/observability/`
- `internal/config/conversation.go`, `.env.example`, `deploy/nas.env.example`
- `internal/chatgpt/`
- `internal/store/sqlite/` (migrations, conversation and task stores), `internal/conversation/`,
  `internal/task/`, the composition root (`internal/app` and, after plan 0107, `internal/wiring`)
- `internal/telegram/` (`/model`, `/status`, help, privacy)
- `docs/operations.md`, `docs/architecture.md`

## Validation

- Pi fixture tests assert the exact `--model`/`--thinking` per run kind, with and without a lane
  choice, including on a resumed session; the fixture echoes the model it was given.
- Integration tests: `/new` then `/model` then a message uses the choice; the choice holds across
  `/sessions` resume and is gone after `/new`; a task keeps its copied pair across a restart and a
  config change; the task default falls back when unavailable; `/model` lists models with levels,
  the buttons apply a choice, unsupported levels are refused with the supported ones; `/status`
  renders the pair, last-turn tokens and the usage block from an HTTP fake, "not connected" and
  "usage unavailable" on failure; no identifier from the usage response appears in any reply or
  log.
- Formatting, module, test, vet, build, and plan checks.
- Live, after deploy: `/model` in a test tab, one turn on a Sol model, a short task, `/status`.

## Risks and open questions

- The first turn after a model switch misses the prompt cache.
- A chat turn in a lane with a choice reads the catalog under Pi's lock every turn (up to the
  two-second wait); a lane without one never touches it.
- `/wham/usage` is not a documented API; if it changes, `/status` shows "usage unavailable" and
  nothing else breaks.
- Pi's cached catalog can lag the backend; a model missing from it cannot be chosen until Pi
  refreshes it.
- A stronger task default spends more of the subscription's limits.
- The planned turn-engine split moves conversation code this plan touches; this plan lands first.
