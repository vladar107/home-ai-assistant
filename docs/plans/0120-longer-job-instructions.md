# 0120 — Longer scheduled job instructions

Status: `done`

## Context

A scheduled job's instructions are capped at 1000 characters (plan 0024).
Plan 0119 kept that cap for the Heads-up notes. In practice the cap is too
tight: in one week, four of six `schedule_update` calls were refused with
`the prompt must be between 1 and 1000 characters`. The model then trimmed the
text by trial and error, three times in a row in one turn, because the refusal
names the limit but not how far over the text is.

Three other limits sit behind the 1000-character cap and would surface first
if it were simply raised:

- A tool result is capped at 16 KB. `schedule_get` returns the instructions in
  full so that `prompt_edits` can quote them exactly; 10,000 characters of
  non-Latin text is about 20 KB.
- A confirmation, whether a proposal or a "Done" message with Undo, must show
  the exact action in one Telegram message. `schedule_propose` and a full
  `prompt` rewrite show the whole instructions beneath the schedule, so they
  are refused as too long to show beyond about 2,200 characters.
  `prompt_edits` shows only the changed passages and is not affected by the
  job's length, only by the edits' own.
- The schema's `maxLength` tells the model the limit before it calls.

## Goal

A scheduled job's instructions can be up to 10,000 characters, read back in
full with `schedule_get` in any script, and grown through `prompt_edits`. The
schema states the limits the model will actually meet, and a refusal for
length says exactly what to cut or which path to use, so the model fixes the
call in one retry.

## Decisions

- A job's stored instructions may be up to 10,000 characters, counted in runes
  as today. This replaces the 1000-character limit plan 0119 kept for the
  Heads-up notes.
- Confirmation still shows the exact action in one message. Long instructions
  are reached through `prompt_edits`; a new job or a full rewrite stays limited
  by what fits one confirmation. Attaching the text as a file or splitting it
  across messages was considered and rejected as more surface for little gain.
- Because a full `prompt` is always shown in full, its schema `maxLength` is
  2,000, below the confirmation ceiling, and its description says longer
  instructions are grown with `prompt_edits`. The 10,000 limit is enforced
  where the stored text is validated.
- A tool definition may declare its own result cap, `MaxResultBytes`; zero
  keeps the 16 KB default and the ceiling is 64 KB. Only `schedule_get` raises
  it, to 48 KB, which holds 10,000 four-byte runes and the job header. The
  global default stays, so no other tool's context cost changes.
- `prompt_edits` find and replace passages keep their 1000-character limit.

## Approach

1. **Per-tool result cap (orchestrator; Jev: simple, 0.77).** Add
   `MaxResultBytes` to `tools.Definition`, validated at registration like
   `Timeout` (not negative, at most 64 KB), carried into the turn's binding,
   and used by the oversized-result check in place of the constant. Done by
   the orchestrator: four small edits whose only purpose is to unblock
   chunk 2, not worth a separate worker wave.
2. **Scheduling limit and refusals (Opus worker; Jev: complex, 0.72; after 1).**
   - Raise the stored-instruction limit to 10,000. Split the over-limit case
     out of the shared refusal so it states the length, the limit, and how
     many characters to cut, as the memory refusal does since `fa4cf2a`.
   - Set `prompt`'s `maxLength` to 2,000 in `schedule_propose` and
     `schedule_update`, with descriptions saying a job's instructions can grow
     to 10,000 with `prompt_edits`. Update `proposeGuidance` the same way: a
     full rewrite must fit one confirmation, and longer text is grown with
     `prompt_edits`.
   - `schedule_get` declares the 48 KB result cap.
   - An update that leaves the instructions alone, such as a new name,
     schedule, or model, no longer repeats them in its confirmation; it says
     they stay as they are. Otherwise a job grown past one message could no
     longer be renamed or rescheduled.
   - When the approval service refuses as too long to show, answer by path:
     a new job or full rewrite is told to keep the instructions under about
     2,000 characters and extend them with `prompt_edits` once the job exists
     (after the user confirms, when it was only proposed); `prompt_edits` is
     told the edits together are too long to show, so make fewer or shorter
     ones in this call; `schedule_remove` keeps the generic text.
   - The existing integration case that expects a 1000-character `replace` on
     a short job to be refused now succeeds. Rewrite it to tip a job created
     near 10,000 characters over the limit and expect the new refusal with
     the count to cut.
3. **Plan and index (orchestrator).** This plan and its row in the index.

Waves: chunk 1 by the orchestrator, then chunk 2. Chunk 3 is written
alongside.

## Files affected

- `internal/tools/broker.go`
- `internal/schedule/modelturn.go`
- `internal/schedule/tools.go`
- `internal/telegram/schedule_editing_integration_test.go`
- `docs/plans/0120-longer-job-instructions.md`, `docs/plans/README.md`

## Validation

- Formatting, module, vet, build, the test suite with and without the race
  detector, and `scripts/check-plans`.
- The rewritten integration case covers the over-limit refusal and its count.
- After deployment: an edit that takes a job past 1000 characters is applied,
  and `tools.call.completed` shows no `oversized` outcome for `schedule_get`.

## Risks and open questions

- Each run of a long job sends its full instructions in the turn's message,
  which is not cached; 10,000 characters cost about 2,500–3,500 input tokens
  a run.
- Each `schedule_get` of a long job puts the full text into the chat
  conversation for the rest of the session, up to about 10,000–13,000 tokens.
- Longer Heads-up notes can grow back into the rulebook plan 0119 moved into
  code. The guidance that notes state lasting preferences, never steps, stays.
- The system prompt's job list and `schedule_list` already shorten
  instructions, `/jobs` never shows them, and scheduled runs have no message
  length check, so they are unaffected.
