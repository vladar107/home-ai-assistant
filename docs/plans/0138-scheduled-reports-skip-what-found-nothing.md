# 0138 — Scheduled reports leave out what found nothing

Status: `done`

## Context

Scheduled reports carry filler. A Heads-up run wrote that no trip to the office or home was
needed, described an all-day event that led to no trip or action, and kept a section whose only
content was that nothing was known. The scheduled-job prompt has one quiet rule, for a task that
reports only when something holds; nothing tells a run to drop a single part that found nothing.
The Heads-up method asks for what needs attention, but two of its steps invite the opposite:
step 6 asks to say whether each disruption affects a trip, and step 8 asks that every trip be
covered. The user's notes on how to read the calendar then surface as statements of what does
not apply.

## Goal

A scheduled report contains only what there is to report. A part of the task that found nothing
is left out, and an absence is not stated unless the task asks for it.

## Decisions

- The rule lives in the prompts every run gets, `prompt.ScheduledJob` and `headsUpMethod`, so it
  reaches existing jobs without editing their stored instructions.
- `prompt.ScheduledJob` gains the part-level rule only: leave out a part that was checked and
  found nothing, and do not state that something is absent, unchanged, or not needed unless the
  task asks for that. When every part found nothing the report says so in one line.
  Whether a whole report is sent stays as it is: a job that summarises something still reports when
  there is little to say, and the existing quiet rule covers tasks that report only on a
  condition.
- Heads-up keeps its own rule to send nothing when nothing needs attention.
- A check that failed is still reported; failing is not the same as finding nothing.
- Heads-up notes say how to read the calendar; they are not items to report. A trip that exists
  is still reported with its times, also when everything runs on time.
- In Heads-up, an all-day or marker event that leads to no trip and nothing to do is left out. A
  timed event the user attends is still theirs to hear about when it needs attention.
- The user's stored notes are not changed.

## Approach

1. `prompt.ScheduledJob`: add the part-level rule beside the existing quiet rule.
2. `headsUpMethod`:
   - step 2: notes say how to read an event and are not reported themselves; an all-day event,
     or one that only marks the day, is left out when it leads to no trip and nothing to do;
   - step 6: report a disruption only when it affects one of the user's trips;
   - step 8: every trip on the timeline was checked and is reported with its times; nothing in
     the draft states that a trip, event, or item is absent or not needed;
   - closing line: write a section only when it holds something to act on or know.
3. Regenerate the scheduled-job golden text.

## Files affected

- `internal/prompt/unattended.go`, `internal/prompt/testdata/scheduled-job.txt`
- `internal/schedule/headsup.go`

## Validation

- The scheduled-job golden pins the new rule. The Heads-up method has no test that pins its
  wording and gets none: a test that mirrors the text proves nothing about the reports.
- The configured format, module, test, vet, build, and plan checks.
- After deployment: read the next Heads-up reports on a day without a commute and on a day when
  one source has nothing new, and one report of another scheduled job.

## Risks and open questions

- This is prompt wording, so the effect is judged on live reports, not proven by tests.
- A job whose parts all found nothing still sends one line saying so unless its instructions
  say to stay silent.
