# 0159 — Fix the failures the agent evals found

Status: `done`

## Context

Plan 0155's calibrated suite (45 cases, 3 trials each) passes 71.9% of trials on the default
prompt (run 20261115-190237: 97/135, pass^k 53.3%, happy paths 24/36). The operator's review
agreed with the checks on 97.7% of an earlier run's trials, so most remaining failures are the
agent's. Reading every failing trial groups 30 of the 38 into seven causes; the other 8 are single
misses in as many cases (dt1, dt2, f4, m2, q2, r1, s2, t3).

1. **Film table (10 trials: h4 ×3, m1 ×2, w3 ×2, c1, r2, h1).** `movies_set_score`, `movies_add` and
   `movies_schedule` refuse a call without `data_source_id`, while `movies_next` finds the table
   itself; the model calls a write tool with an empty ID, fails, and gives up or retries past a
   call limit. `movies_next` names the previous evening only when nothing is upcoming, so "was the
   last one the film about X?" is answered from the next row. Shifting every evening by a week
   reaches for `movies_schedule`, whose film-and-date form re-dates a row, instead of updating the
   rows. A film the user names is not looked up in the table first.
2. **Heads-up lists what it did not check (5: n1 ×3, h8 ×2).** In these worlds no transit, home or
   health tools are offered, and step 8 demands that every trip "was checked and is reported", so
   the reply ends with "not checked: the trip, home, health".
3. **Weather "at home" (3: h12).** The model passes `home`; the saved place carries the user's own
   word for it; no household home is set in the eval world, so the guidance tells the model to ask,
   unlike production, which sets one. The case also demands a weather card where a one-place
   forecast is right.
4. **Calendar search language (3: s1).** `calendar_events` passes `query` to Google's word search, so
   a Russian keyword misses an English title, and the scheduled check reports no event.
5. **A bare line number read as a time (3: m3).** After a train answer, "а на 340?" is checked as a
   train at 03:40 or 08:34 instead of bus line 340.
6. **No watch offered for a site that is down (3: h3).** The agent reports the outage and stops;
   the watch guidance never suggests offering one.
7. **Can I make it in time (3: dt3).** One trial answered "Похоже, нет", which the case's pattern
   misses. Two found no train arriving by the event's start: the simulator returns only trips
   arriving by the requested time, which may not be what NS returns.

Separately, a scheduled run (s1 trial 1) translated the quiet token into the user's language; the
scheduler matches the exact token, so the translation would have reached the user.

## Goal

Raise the suite's pass rate toward 95% without lowering the happy-path rate, measured against run
20261115-190237. Fixing all seven causes reaches at most 127/135 (94.1%); 95% also needs some of
the single misses, which are not targeted here. Each fix changes the tool or the text the model
reads, so production gains the same behaviour.

## Decisions

- **Fix the cause where it lives.** A tool that rejects or hides what the model needs is changed;
  wording changes only where the model had what it needed and chose wrongly. No per-user data,
  such as saved place names, ever goes into guidance or tool descriptions.
- **The film write tools find the table as `movies_next` does**, and `movies_next` names the most
  recent past evening beside the next one and takes an optional `film` to report that film's own
  row. The existing "No upcoming movie night is scheduled. The latest was …" sentence stays.
- **The eval world sets a neutral household home** as production does, so "weather at home" is
  measured on production's guidance path.
- **The NS arrive-by behaviour was checked against the real API before changing anything.** An
  arrive-by request returned trips arriving around the requested time, later ones included, so
  the simulator is fixed to match and the train tool is left alone.
- **Prompt changes are plain changes to the defaults** with their golden files, verified by the
  suite before and after. Most fixes are in tools, which a variant cannot change.
- **The quiet token stays exact**, and every instruction that gives it says it is never translated.

## Approach

All workers are Sonnet, one tier below the session model. Jev's tier classification is shown as
"Jev: tier, confidence"; below 0.7 the tier is decided here. Chunks 1 to 5 touch separate
packages and run in parallel; chunk 6 follows.

1. **Film table tools** — Complex (Jev: simple, 0.31; three behaviour changes in proposal code).
   - Task: make `data_source_id` optional for `movies_set_score`, `movies_add` and
     `movies_schedule`, discovered as `movies_next` does (remove the refusal and drop it from the
     schemas' `required`); `movies_next` adds a line naming the most recent past evening with its
     film and picker when there is also an upcoming one, and with `film` reports that film's row
     (date, picker, scores) or says it is not in the table; `movies_schedule`'s description adds:
     "To move evenings that already have dates, such as shifting every evening by a week, update
     their rows with notion_update_page, not this tool."; `movieGuidance` adds that a film the user
     names is looked up with `movies_next` and its `film` before searching elsewhere.
   - Files: `internal/connectors/notion/movies.go`, `internal/connectors/notion/movies_test.go`.
   - Acceptance: against the Notion simulator, a score proposed with no ID; the last evening named
     beside the next; a named film's row returned and a missing one reported; the existing pinned
     `movies_next` texts in `movies_test.go` unchanged.
2. **Weather refusal points to saved places** — Complex (Jev: complex, 0.40; guidance wording).
   - Task: the refusal for a name that matches no saved place and no geocoding result adds
     "The user's saved places are listed by place_list; use one by its exact name."; the weather
     guidance's no-place line becomes "When the user asks about the weather at home or names no
     place, leave location empty to use the household's home, %s." where a home is set.
   - Files: `internal/connectors/web/weather.go`, `internal/connectors/web/tools.go`, their tests.
   - Acceptance: a test shows the refusal with the pointer; existing weather tests unchanged
     except the intended lines.
3. **Calendar search fallback** — Complex (Jev: complex, 0.56).
   - Task: when `query` finds nothing in a window of at most 48 hours after the inclusive end is
     normalised, the result opens with "Nothing matched <query>. Every event in the window
     follows; the search matches words in the event's own language." and lists the window's
     events; the guidance says the same about the search.
   - Files: `internal/connectors/calendar/tools.go`, `internal/connectors/calendar/render.go`,
     tests.
   - Acceptance: an English-titled event and a Russian query list the window's events under that
     opening; a matching query's output is byte-identical to today's; a longer window is unchanged.
4. **Arrive-by and line numbers** — Complex (Jev: complex, 0.73).
   - Task: one real NS arrive-by request (for 19:00) returned trips arriving from 18:55 to 19:29,
     so the simulator answers an arrive-by search with trips arriving from 90 minutes before to
     60 minutes after the requested time; the train tool marks each trip arriving after the
     requested time "(after the requested arrival time)", since its header says "arriving by".
     The bus guidance adds: "A bare number after a transit answer, such as 340, is first checked
     as a bus line with the bus tools when a line with that number serves the trip."
   - Files: `internal/testutil/nssim/simulator.go`, `internal/connectors/ns/render.go`,
     `internal/connectors/bus/tools.go`, tests.
   - Acceptance: an arrive-by search in the simulator returns a later trip; a later trip is marked
     and an earlier one is not; departure checks unchanged.
5. **Prompt text** — Complex (Jev: complex, 0.66). The sentences are fixed here; the worker applies
   them and regenerates golden files.
   - Heads-up step 4 starts "When transit tools are offered, check every trip:"; step 8's first
     check becomes "every trip on the timeline was checked and is reported with its times when
     transit tools are offered"; a sentence after step 8: "A source whose tools are not offered in
     this run is skipped silently, never listed as unchecked; a tool call that failed is still
     named."
   - Wherever the quiet token is given (`internal/prompt/unattended.go`, `internal/prompt/turn.go`,
     `internal/schedule/headsup.go`, `internal/reaction/reaction.go`), add "written exactly as
     shown, never translated".
   - Watch guidance, in the agent-watch branch: "When a site or service the user asks about is
     down, offer in one line to watch it and tell them when it is back; call watch_propose only
     after the user agrees."
   - Files: those four files, `internal/watch/tools.go`, golden files and tests pinning them.
   - Acceptance: golden files differ only by these lines; tests pass.
6. **Eval world, cases and verification** — Orchestrator.
   - Task: a neutral household home in the eval world (`internal/eval/fakes.go`); h12 accepts
     `weather_forecast` or `weather_card` for the home; h3's outage pattern accepts "не
     открыва"; dt3 accepts "Похоже, нет". Run the affected cases with five trials, then the full
     suite with three, and compare with run 20261115-190237; read every remaining failure.
   - Files: `internal/eval/fakes.go`, `evals/cases/h12-weather-saved-place.json`,
     `evals/cases/h3-shop-site-down.json`, `evals/cases/dt3-quiz-too-late.json`.
   - Acceptance: no case drops by 2 or more of 3 trials; the happy-path rate is at least 24/36;
     every remaining failure is listed with its cause.

## Files affected

- `internal/connectors/notion/movies.go`, `internal/connectors/web/`, `internal/connectors/calendar/`,
  `internal/connectors/ns/` or `internal/testutil/nssim/`, `internal/connectors/bus/tools.go`,
  `internal/schedule/headsup.go`, `internal/prompt/unattended.go`, `internal/prompt/turn.go`,
  `internal/reaction/reaction.go`, `internal/watch/tools.go`, their tests and golden files
- `internal/eval/fakes.go`, three cases under `evals/cases/`, `docs/plans/README.md`

## Validation

- Formatting, vet, build, module tidiness, the full test suite with and without the race
  detector, and `scripts/check-plans`.
- The affected cases with five trials and the full suite with three on the eval profile, compared
  with run 20261115-190237.

## Risks and open questions

- **Three trials per case show only large effects.** A case moving by one trial is within noise;
  the comparison is read per cause.
- **Production behaviour changes at once on deploy**: the Heads-up method reaches every household
  user's runs, and tool output changes reach every chat. The fallbacks add text only when the
  original call found nothing. Guidance and schema edits change the cached prompt prefix once per
  deploy; the Heads-up and scheduled-run text sits in the turn message and does not.
- **The calendar fallback can mislead a scheduled check** into taking an unrelated event for the
  match; the explicit opening sentence is the guard, and s1 shows whether it holds.
- **95% is not reachable from these causes alone** (94.1% at best); the single misses need their
  own reading after this plan.
