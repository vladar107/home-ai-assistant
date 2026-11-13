# 0146 — Trip updates that are true, short, and in the language of their tab

Status: `proposed`

## Context

Plan 0139 tells the user what changes about a trip that is near or under way. Used on a real trip
in another time zone, it fell short in three ways.

- **Calendar changes were false.** Every timed event of the trip was reported as moved by exactly
  one hour, and none had moved. The check is a model turn that lists the calendar with a tool whose
  output shows wall-clock times in the household's zone, without an offset, and the turn is asked
  to write each event's start and end as an instant with an offset. The turn is framed in the
  household's zone before the trip starts and in the trip's zone after, so the offset it writes
  changes when the trip starts. The baseline and the next check were on either side of the start,
  and the two lists differed by the hour between the zones. The place is transcribed by the turn
  as well, which is a second source of false moves.
- **The message was hard to read.** Each change repeats a prefix, the whole old and new range, and
  the full address of the place, also when only the time changed. Five changes filled the screen.
- **It was in English.** The header, the labels, and the sentences about calendar changes are fixed
  English text, and the items from mail and the web are written by a turn that is not told which
  language to use. Watches rewrite their alerts into a language; trips have none.

## Goal

A trip's updates are true, short, and in the language the user writes in.

Success: on a trip in another time zone, an unchanged calendar reports nothing across the trip's
start; a moved event reports one line with the new and the old time; and the message is in the
language of the user's latest message in the tab it is sent to.

## Decisions

- **Code reads the calendar for the comparison.** The trip package declares a small reader, which
  the wiring adapts to the calendar connector, as scheduled jobs anchored to an event already read
  it. The model turn keeps mail and the web; its prompt and its reply lose the events.
  - An event's key is its calendar and its event ID together.
  - The reader lists what the calendar tool lists: selected calendars the user can read in full,
    without cancelled events, with recurring events as their instances.
  - The range is the trip's start and end instants.
  - An all-day event is kept as its first and last day, both inclusive, whatever zone the
    connector read it in.
  - An account or calendar that fails, or a listing that stops at its limit, makes the list
    incomplete, and an incomplete list compares nothing, as today.
- **A snapshot taken the old way is not compared.** The stored snapshot gains a version. The first
  check after this change stores a new baseline, forgets which events were missing, and reports no
  calendar changes.
- **One line per change, saying only what changed.** Times are in the trip's zone. The place is
  shown only when it changed, as its text before the first comma. Examples, for a trip titled
  "Conference":
  - header with only calendar changes: `Conference: calendar changes`
  - moved within a day: `• Museum visit: Mon 9 Nov 13:00–14:00 (was 14:00–15:00)`
  - moved to another day: `• Museum visit: Tue 10 Nov 13:00–14:00 (was Mon 9 Nov 14:00–15:00)`
  - place changed: `• Museum visit: now at North Hall (was South Hall)`
  - added: `• Added: Team call, Thu 12 Nov 16:00–16:30`
  - removed: `• Removed: Team call, Thu 12 Nov 16:00–16:30`
  - all day: `• Added: Workshop, Thu 12 Nov (all day)`
- **Mail and web items are one line each:** `• Example Air (mail): gate changed to B12.` The origin
  stays, the title and the text become one sentence of at most 200 characters, and the reference
  that keeps an item from being sent twice stays stored and unshown. The header is then
  `Conference: news, not verified` and that note appears only when a mail or web item is present.
- **The language is the tab's.** A trip's message is written in the language of the user's latest
  message in the tab it is sent to; when the user never wrote there, of their latest message in
  any tab; and in English when they never wrote. No language is stored and none is detected: the
  rewrite is given that message as a sample and told to write in its language. The sample is the
  user's own text, read on their own subscription, and is clipped to a few hundred characters. One
  language chosen in the settings is a later plan.
- **What is rewritten:** updates, the notices that a trip started and ended, and the line that a
  scan updated a trip. They are rewritten where the trip's messages are sent, by the rewrite
  watches use, which gains the sample as another way to name the language. A rewrite is kept only when every number, every quoted event title, and every
  booking reference of the original is in it unchanged and it fits one message; otherwise the
  original is sent. The turn that reads mail and the web is given the same sample and writes its
  items in its language.
- **What stays English for now:** the pinned summary, which is edited in place on every write,
  the cards, and check-in reminders, whose text is fixed when they are scheduled.
- **The English text is what is stored** as sent, since the next check's prompt replays it.

## Approach

1. **Calendar reader.** The interface in the trip package, its adapter in the wiring, and the
   comparison on what it returns; the prompt and reply without events; the versioned snapshot.
2. **Text.** The lines and headers above.
3. **Language.** A store read of the user's latest message in a tab, and in any tab; the rewrite
   at the sender with the sample and its three rules; the sample in the mail and web prompt.
4. **Tests,** on the real assembly with the calendar and Telegram simulators: a trip in another
   zone with an unchanged calendar reports nothing in checks before and after its start; a moved,
   an added, a removed, and an all-day event each give the line above; a failed calendar compares
   nothing; an old snapshot gives a baseline; an update is sent in the language of the latest message in its tab, of the latest message
   anywhere when the tab has none, and in English when there is none; a rewrite
   that changes a number, a quoted title, or a booking reference sends the original; a sample
   that is itself an instruction changes nothing but the language.
5. **Documentation.** The trips part of the architecture document and of `README.md`.

## Files affected

- `internal/trip` (updates, loop, scan), tests
- `internal/store/sqlite` (the latest-message read)
- `internal/turn` (the rewrite's sample), `internal/wiring`, `internal/prompt`
- `internal/connectors/calendar` only if listing for the reader needs an exported call
- `docs/architecture.md`, `README.md`

## Validation

- Formatting, module, vet, build, race and non-race tests, and `scripts/check-plans`.
- The tests named in the approach.
- Live, by the operator after deployment: a test trip in another zone, "Scan now" twice with the
  calendar unchanged and once after moving an event, after writing in the trip's tab.

## Risks and open questions

- **A rewrite costs a model call per message** and can fail; the original English text is then
  sent, as for watches.
- **A calendar the user deselects, or a recurring series they edit, reads as events removed and
  added.** The removal is reported only after two complete lists, as today.
- **Reminders, cards, and the summary stay English** until a later plan gives them a language.
- **The sample is text the user wrote, and a model reads it.** It is framed as a sample of a
  language and nothing else, and the rules on numbers, titles, and references reject a rewrite it
  bent; a message that mixes languages gives whichever the model reads.
- **A tab's language follows its latest message,** so one message in another language changes the
  next update.
