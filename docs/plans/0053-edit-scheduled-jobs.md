# 0053 — Edit scheduled jobs

Status: `done`

## Context

A scheduled `model.turn` job can be proposed (`schedule_propose`) and deleted
(`schedule_remove`), but not changed. To adjust one, the model has to delete it
and propose a new one, which loses the job's id and pause state and takes two
confirmations.

Worse, the model cannot see what it is changing. `schedule_list` cuts each
job's instructions to 200 characters (`listedInstructionRunes`) to keep a long
list small, while a prompt may be up to 1000. Asked to tweak a job, the model
rewrites the prompt from its own guess instead of editing the stored text, and
silently drops whatever was past the cut.

## Goal

- The model can read one job in full, including its untruncated instructions.
- The user can ask in conversation to change a job's name, schedule, time zone,
  or instructions; the model proposes the edit, and the user confirms the
  resulting job with the same Confirm and Reject buttons as a new job.
- An edited job keeps its id and its paused or active state.

## Decisions

- Add a read tool, `schedule_get` with `job_id`, that returns one job with its
  full instructions. `schedule_list` keeps truncating, so the list stays small,
  and says to use `schedule_get` before editing.
- Add a write tool, `schedule_update` with `job_id` and optional `name`,
  `cron`, `at`, `prompt`, `timezone`. Omitted fields keep their stored values;
  `prompt`, when given, replaces the whole text, so the guidance tells the model
  to start from `schedule_get`'s text and change only what the user asked.
- An update is always confirmed, like `schedule_propose`, and is listed in
  `ActionTools`. The confirmation shows the job as it will be (the same
  rendering as a new job, headed "Change …") so the user reads the exact
  stored result, not the model's description of the change.
- Only `model.turn` jobs that are not finished can be updated. Reminders stay
  as they are: cancel and set again, which needs no confirmation anyway.
- A job keeps its kind: a recurring job takes `cron`, a one-time job takes
  `at`. Switching kind is a remove plus a new proposal, which keeps the
  per-kind limits in one place (`CreateScheduledJob`).
- The confirmed arguments carry the job's `updated_at` as seen at proposal
  time. Executing compares it in the `UPDATE`'s `WHERE`, so a job changed,
  paused, or advanced in between is reported as "changed since; nothing was
  updated" rather than overwritten. Because a tick also bumps `updated_at`, a
  recurring job that ran between proposal and press needs a fresh proposal;
  that is rare and safe.
- `next_run_at` is recomputed from the new schedule and now. A paused job
  stays paused; resuming already recomputes its next run.

## Approach

1. Store: add `UpdateScheduledJob(ctx, job Job, expectedUpdatedAt time.Time)
   (bool, error)` to `schedule.Repository` and the SQLite store, updating
   name, cron, run_at, zone, config, next_run_at, and updated_at where id,
   principal, `finished_at IS NULL`, and `updated_at = expected` match.
2. Service: add `Update(ctx, principalID, jobID, expected, prepared)` that
   logs `schedule.job.updated`. Reuse `Prepare` for validation (name, cron or
   at, minimum interval, prompt length), then copy over the stored job's
   `Enabled`.
3. Tools: add `schedule_get` (read) and `schedule_update` (write) definitions,
   handlers, and an `ExecuteApproved` case. Merge omitted fields from the
   loaded job before `Prepare`; reject a kind switch and reminder or finished
   jobs with a pointer to the right tool. Render the confirmation with the
   existing `proposalSummary`, given a verb.
4. Guidance: extend `listGuidance` and `proposeGuidance` with when to use
   `schedule_get` and `schedule_update`, and the rule to edit the stored text
   rather than rewrite it.
5. Docs: mention editing in the README's schedules feature.

## Files affected

- `internal/schedule/schedule.go`, `service.go`, `tools.go`
- `internal/store/sqlite/schedules.go`
- `internal/telegram/schedule_integration_test.go`,
  `internal/store/sqlite/schedules_test.go`
- `README.md`
- `docs/plans/README.md`

## Validation

- Integration test through Telegram: list, get returns the full 1000-character
  prompt, update proposes, Confirm changes the stored job and next run with
  the id and pause state kept.
- Store test: a stale `updated_at` updates nothing.
- Repository checks: format, modules, tests, vet, build, `scripts/check-plans`.
- Live: ask the bot to change the time and one sentence of an existing job,
  and confirm the stored instructions differ only in that sentence.

## Risks and open questions

- Telegram `/jobs` gains no edit button; editing is conversational only.
- A full replacement prompt still relies on the model copying the text
  faithfully; the confirmation shows the whole result so the user can catch a
  bad copy before it is stored.
