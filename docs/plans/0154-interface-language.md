# 0154 — An interface language each user chooses: English or Russian

Status: `in-progress`

## Context

A backlog item asks for a language setting with English and Russian, and for the assistant's
system messages, its notifications and service texts, to be written in the chosen language.

Today every fixed text the code writes is English. The model already answers in the language the
user writes in, so a conversation reads naturally in Russian, but everything around it does not:
command replies, `/help`, the slash menu, `/settings` and its buttons, lists such as `/jobs` and
`/memory`, proposal messages and their outcomes, Undo results, widget labels, errors, connection
steps, background-task cards, export and restore previews, and the texts drawn on day cards and
charts.

Two features already put code-written text into another language, each with its own idea of
which language that is, and both through a rewrite on the user's own model subscription
(`turn.RewriteAlert` and `RewriteAlertLike`):

- A watch has its own `Language`, which the model sets when the user writes in another language;
  its alerts are rewritten into it.
- Plan 0146 rewrites a trip's updates into "the language of their tab": nothing names that
  language, so the user's latest message in the trip's tab, or anywhere, is the sample the
  rewrite follows. A user who never wrote gets English.

A count of the prose-like string literals in non-test code (an AST pass that skips logging,
errors, SQL and imports) finds about 4,300. Most are written for the model: tool descriptions
and schemas, tool results, prompts and rewrite instructions, and metric help. The fixed text a
user reads is roughly 1,100 strings:

| Area | User-facing strings | Where they cluster |
| --- | --- | --- |
| Telegram commands, settings, pages and lists | ~600, plus ~100 button labels | `commands.go`, `service.go`, `settings.go`, `switch_pages.go`, `trips.go`, `model.go`, `memory.go`, `sessions.go`, `services.go`, `status.go`, `events.go`, `help.go` |
| Approvals and widgets | ~55 | `approval/approval.go`, `direct.go`, `preview.go`; ~10 in `telegram/widgets/*.js` |
| Proposal summaries and Undo outcomes | ~150 | 80 `approval.Proposal` sites in 21 packages, and six `undo.go` files |
| Scheduled, watch and trip notices and descriptions | ~150 | `schedule/anchor.go`, `reports.go`, `service.go`; `watch/watch.go`, `service.go`, `place.go`, `disruptions.go`; `trip/updates.go`, `summary.go`; `weekday.Describe` |
| Turn errors, activity lines, connection, tasks, export | ~65 | `turn/types.go`, `conversation/activity.go`, `connection/types.go`, `task/task.go`, `export/render.go`, `archive.go` |
| Images | ~30 | `daycard`, `connectors/web/card.go`, `chart/render.go` |

Dates add to this: 56 time layouts in 35 files write English weekday and month names, and some of
them reach the user. The embedded Inter fonts the images draw with already hold every Cyrillic
letter, so images need no new font.

Some text is read by both the user and the model. An approval record's `Outcome`, an executor's
result or an Undo's result, is shown in the proposal's status line and also returned to model
turns (`approval/direct.go`, the act result and the Undo notices). A stored summary is parsed by
its English intent-warning prefix: `warned`, `summaryLine` and `TitleOf` in `approval.go` strip
or match `IntentWarning` and `DirectIntentWarning`.

Per-user choices are kept today as integration switches: one row per principal and integration
in `integration_switches`, no row meaning the integration's default, read through
`integration.Switches` and shown as a `SwitchPage` in `/settings` (plan 0149's Widgets switch is
the latest). A switch is a boolean, and a language is not.

The slash menu is registered once at start for the default and all-private-chats scopes, with
English descriptions that `/help` also lists.

## Goal

Each user chooses English or Russian in `/settings`. From that moment every fixed text the
assistant writes for that user, in replies, buttons, menus, notifications, proposals, widgets
and images, is in the chosen language, and nothing changes for any other user. A user who never
chose keeps English, as today.

The model's chat turns and the text it reads are unchanged. Two things it writes change on
purpose, because nothing else tells it the user's language: an unattended turn (a scheduled job,
a watch check, a trip scan, a background task) is told the chosen language, and the existing
watch and trip rewrites use it when they have no language of their own.

## Decisions

- **A plain Go catalog in a small package, `internal/locale`, and no new dependency.**
  `golang.org/x/text/message` is already an indirect dependency, but its catalog is keyed by
  the English format string, held in a global, separate from the code that uses it, and falls
  back to the key without telling anyone; its plural syntax (`plural.Selectf`) is awkward and its
  CLDR breadth is not needed for two languages. The package holds:
  - `Language` (`en`, `ru`), with English the zero value.
  - `T(en, ru string) Text`: one message in both languages, written side by side where it is
    used. `Text.In(Language)` returns a message that has no verbs in a language;
    `Text.Format(ctx, args...)` formats it in the context's language with `fmt.Sprintf`, and
    `Text.FormatIn(Language, args...)` does the same where there is no context.
  - `N(enOne, enOther, ruOne, ruFew, ruMany string) Count`: a message that depends on a number.
    `Count.Format(ctx, n, args...)` and `Count.FormatIn(Language, n, args...)` pick the form and
    pass `n` as the first argument.
    Russian uses the CLDR integer rule: one when `n%10 == 1 && n%100 != 11`, few when `n%10` is
    2 to 4 and `n%100` is not 12 to 14, many otherwise. Counts are whole numbers only.
  - `With(ctx, Language)` and `From(ctx)`, as the `thread` package carries a tab. A context
    with no language is English.
  - Named date styles, such as a day with weekday and time, a day, and a day with year, written
    with Russian weekday abbreviations and month names in the genitive (`пн 2 янв 15:04`),
    replacing the English layouts at the sites a user reads.
  - The few words many pages share: on, off, Back, Confirm, Reject, Preview, Undo, Cancel.
- **No string keys.** A message is a Go value, its name is its key, and the compiler finds every
  use. The exported reply constants the tests compare against become `locale.Text` values that
  the tests read in English.
- **One test proves the catalog complete.** It walks every non-test Go file of the module and,
  for each call of `locale.T` and `locale.N`, requires string-literal arguments (constant
  concatenation allowed), none empty, and the same format verbs with the same argument indexes in
  every form of both languages. Russian word order uses explicit indexes such as `%[2]s`. `go vet`
  does not check `Text.Format`, so the same test resolves each `.Format` call on a package-level
  message of the same package and compares its argument count with the verbs, and under
  `testing.Testing()` `Format` panics when its output holds a `%!` error. The plural rule and the
  date styles get table tests in the package; that logic is isolated and worth a unit test.
- **A message is a whole sentence with its arguments, never English fragments glued together.**
  Text such as `"Restored the watch " + name + " as it was."` becomes one formatted message, and
  summaries built from fragments, such as a job change's list of what changed, are rebuilt as
  sentences per language. Text from the user or from outside, such as a job's name, a calendar
  title, or a mail's subject, is an argument and is never translated.
- **What the model reads stays English; what the user reads is translated.** An approval
  `Outcome` keeps its English `Text`, which model turns read, and gains a `UserText` in the
  user's language, which the status line shows when it is set; a record stores both, so the
  model's notices never change language. `approval.Intent` stays English: it goes to the
  external intent check, and its verbs and kinds are code values.
- **A record says whether it carries an intent warning.** A new `warned` column replaces matching
  the summary's English prefix; the migration sets it for existing records whose summary starts
  with either warning. `TitleOf` and `summaryLine` strip the warning in both languages, so a
  proposal made in one language reads correctly after the user switches to the other.
- **The choice is stored in its own table, `principal_languages`**: principal, language (`en` or
  `ru`), and when it changed. No row means English. Plan 0153 takes migrations `058` to `060`, so
  this plan's are `061` (languages) and `062` (the approval columns); the plan and migration
  numbers are confirmed when it lands. The store reads and writes the choice through a
  `locale.Preferences` interface, the way switches use `SwitchRepository`. The user's export
  carries it as a `language` section; restore adds it only when the user has not chosen here.
- **The language reaches a message through the context.** The Telegram service reads the
  principal's language once per update, right after it authorizes the sender, and puts it in the
  context, which already flows into commands, presses, turns and the tools a turn calls, as the
  tab does. Work that starts with no update reads it from the store at its own entry: the
  conversation lane's start (chat and scheduled turns alike), the task runner, the approval
  service's background edits and previews, the watch, trip and schedule loops, and the
  connection ceremony's completion. A function that composes user text and takes no context
  takes the `Language`.
- **Text is written in the language of the moment it is composed.** A proposal's summary is
  stored as it was composed, so a change of language does not rewrite proposals or messages
  already sent; status lines, buttons and pages rendered later follow the new choice.
- **The setting is a Language page at the top of `/settings`.** The overview's first line is
  `Language: English` or `Язык: русский`; the page has two buttons, each written in its own
  language, `English` and `Русский`, with callback data `language:en` and `language:ru`. The
  settings message is redrawn in the new language at once. The change is logged as
  `language.changed`. There is no new command.
- **The slash menu follows the user.** At start the default and all-private-chats scopes keep
  the English list, and each principal who chose Russian gets the Russian list in the chat scope
  of their private chat (`ResolveActiveChat`). A change of language sets that chat's list again.
  Telegram's per-`language_code` lists are not used: they follow the language of the user's
  Telegram app, not this setting. A failed registration is logged and leaves the old menu, as
  today.
- **Telegram's `language_code` is not read.** A user who never chose stays in English, so a
  deploy changes no one's messages.
- **The system prompt does not change**, so it stays cacheable. Tool descriptions, schemas, tool
  results, prompts, rewrite instructions, logs, metric names, event names, callback data and
  stored values stay English. `app_help` is a tool result, not the prompt, and renders its pages
  in the user's language so the model names the buttons the user sees.
- **The setting feeds the two rewrites that exist.** A watch without its own language has its
  alerts rewritten into the user's chosen language when that is Russian. A trip's updates keep
  the sample of plan 0146 and use the chosen language when there is no sample. Translating alert
  and update bodies through the catalog instead of the model is a later plan.
- **An unattended turn is told the chosen language** in its turn message, beside the line
  `prompt.Time` writes, in `turn/scheduled.go` and `turn/task.go`, which serve scheduled jobs,
  scans, watch checks, trip updates and background tasks. A chat turn (`turn/engine.go`) is not
  told: the model keeps answering in the language the user writes in. A user who never chose gets
  no line, so their turns are byte-identical to today.
- **Widget labels travel in the widget's template**, in English and Russian together, and the
  data carries only `g`, the language to draw in, because Telegram limits a widget's data to a
  kilobyte. Each kind still has one template. A view draws English when the data names no
  language, so messages sent before the change keep working.
- **Tests stay out of each other's way.** Chunk 2 converts every exported reply constant that a
  test reads and every such test use at once, through a test helper that reads a message in
  English, so later chunks change only their own non-test files and add their Russian tests in
  new test files of their own. A Russian test asserts a few distinctive phrases and runs every
  text the fake received through a check that flags words written only in Latin letters, with an
  allowlist for commands, product names such as Telegram and Notion, and values from the test's
  own data.

## Approach

Fifteen chunks in five waves, at most five at a time, with disjoint files inside a wave. Every
chunk leaves the full test suite passing and adds a Russian integration test for the surface it
moves, through the Telegram harness with a real temporary SQLite database, or through its
package's existing store-backed harness. An independent review follows each chunk.

Wave 1:

1. **The catalog** (Complex).
   - Task: `internal/locale` as decided: `Language`, `Text`, `Count`, the context helpers, the
     date styles, the shared words, the completeness and argument test, and the Latin-word check
     the Russian tests use.
   - Files: `internal/locale/` (new).
   - Depends on: nothing.
   - Accepted when: the plural table covers 0, 1, 2, 5, 11, 12, 14, 21, 22, 25, 101, 111; a
     call with an empty Russian text, a missing plural form, `%s` against `%d`, or a `.Format`
     call with too few arguments fails the test; a context without a language gives English.

Wave 2:

2. **The setting and its plumbing** (Complex).
   - Task: migration `061` and its store; the `language` export section; reading the language
     after authorization in the Telegram service and at the conversation lane's start; the
     Language settings page and its overview line; the chat-scoped slash menu and the scope type
     that carries a chat; the menu descriptions and `/help` lines; every exported reply constant
     a test reads, as a `locale.Text` with its Russian, and every test use of it, through the
     English test helper; the change event; `docs/architecture.md`.
   - Files: `internal/store/sqlite/migrations/061_principal_languages.sql`,
     `internal/store/sqlite/languages.go`, `internal/wiring/wiring.go`,
     `internal/wiring/export.go`, `internal/telegram/language.go` (new), the group line in
     `settings.go`, `botCommands`, `helpReply` and the language read in `service.go`,
     `SetMyCommands` in `client.go`, the exported constants' declarations in the Telegram
     package, `internal/conversation/service.go`, the existing test files that read those
     constants, `docs/architecture.md`.
   - Depends on: 1.
   - Accepted when: a user who presses Русский sees the settings redrawn with the Russian
     overview line and buttons, and `/help` in Russian; a second user in the same harness still
     gets English; a restart keeps the choice; the fake records the Russian menu in that user's
     chat scope and the English one in the two shared scopes; export and restore round-trip the
     choice and do not overwrite one already made; no later chunk needs to edit an existing test
     file for a constant.

Wave 3, after 2:

3. **Commands and status** (Simple).
   - Task: every remaining reply and button in these files; dates through the date styles.
   - Files: `internal/telegram/commands.go`, the remaining text of `service.go`, `status.go`,
     `privacy.go`, `model.go`, `help.go`, `voice.go`, `media.go`.
   - Accepted when: `/status`, `/privacy`, `/new`, `/cancel`, `/model`, `/usage`, an
     unsupported sticker and an over-long message read in Russian.

4. **Settings pages** (Simple).
   - Task: as chunk 3, for every settings section and the `/memory` pages that share its file.
   - Files: `internal/telegram/settings.go`, `switch.go`, `switch_pages.go`, `google.go`,
     `services.go`, `location.go`, `headsup.go`, `inbox.go`, `events.go`, `feeds.go`,
     `memory.go`.
   - Accepted when: every settings page and `/memory` read in Russian; the switch pages' events
     and callback data are unchanged.

5. **Lists and tabs** (Simple).
   - Task: as chunk 3, for the list commands, their pages and buttons, and tab texts.
   - Files: `internal/telegram/sessions.go`, `tabs.go`, `tabset.go`, `tabnames.go`,
     `objecttabs.go`, `jobs.go`, `reminders.go`, `collections.go`, `projects.go`,
     `projecttabs.go`, `tasks.go`, `backlog.go`, `watches.go`, `watchtabs.go`, `routes.go`,
     `trips.go`, `triptabs.go`, `export.go`, `why.go`.
   - Accepted when: `/jobs`, `/sessions`, `/tasks`, `/backlog`, `/collections` and the trip
     card's own labels and buttons read in Russian, with counts in the right plural form
     ("1 задача", "3 задачи", "5 задач") and Russian dates; tab names the model chose are
     unchanged. The trip summary's text is chunk 10's.

6. **Approvals and widgets** (Complex).
   - Task: `Outcome.UserText` and the record's stored user outcome; the `warned` column with the
     two-language warning stripping; the approval service's status lines, batch headers,
     previews, Undo results and warnings; the proposal and changes widgets' labels in their
     data, with English fallbacks in the views and new golden files; the language read in the
     approval service's background edits and previews, by the record's principal.
   - Files: `internal/store/sqlite/migrations/062_approval_language.sql`, the approval store
     file, `internal/approval/` (`approval.go`, `direct.go`, `preview.go`, `offer.go`),
     `internal/telegram/widget.go`, `proposal_widget.go`, `changes_widget.go`,
     `changes_tap.go`, `widgets/*`, the widget golden files. Not `service.go`, which is
     chunk 3's.
   - Accepted when: a Russian user's proposal shows Russian buttons and, once rejected, expired
     or confirmed, a Russian status line, including an expiry the background sweep edits; the
     model's act result and Undo notice for the same record are the English ones of today; a
     warned proposal made in English keeps its title and warning after the user switches to
     Russian, and the reverse; existing warned records are flagged by the migration; a widget
     message whose data has no labels still draws in English; the templates stay within
     Telegram's limits.

7. **Turn, conversation, connection, tasks, export and weekdays** (Simple).
   - Task: the turn's error replies, the activity lines, the connection outcomes and steps, the
     background-task cards and the task runner's language read, the export and restore texts,
     the export's proposal summary, and a context-taking `weekday.Describe` beside the English
     one.
   - Files: `internal/turn/types.go`, `internal/conversation/activity.go`,
     `internal/connection/types.go`, `internal/task/task.go`, `internal/task/service.go`,
     `internal/export/render.go`, `archive.go`, `service.go`, `internal/weekday/weekday.go`.
   - Accepted when: a timed-out turn, a usage-limit turn, a task card and a restore preview read
     in Russian for a Russian user; weekday ranges read in Russian.

Wave 4, after 6 and 7:

8. **Schedule proposals and Undo** (Complex).
   - Task: each proposal's summary, rebuilt as sentences where it is built from fragments
     (`schedule/tools.go`, the change summary), each Undo's `UserText`, and the job descriptions
     `/jobs` shows.
   - Files: `internal/schedule/tools.go`, `undo.go`, `anchor.go`.
   - Accepted when: proposing, confirming and undoing a new and a changed job read in Russian,
     the stored summary is the Russian one, and the model's results stay English.

9. **Watch proposals and Undo** (Complex).
   - Task: as chunk 8, and the watch description lines.
   - Files: `internal/watch/tools.go`, `undo.go`, `watch.go`.
   - Accepted when: as chunk 8, for a watch.

10. **Trip proposals and summary** (Complex).
    - Task: as chunk 8, and the trip summary the trip card shows.
    - Files: `internal/trip/tools.go`, `summary.go`.
    - Accepted when: a trip proposal and the trip card's summary read in Russian.

11. **Memory, project and task proposals** (Complex).
    - Task: as chunk 8; the weekly review alone builds twenty proposals.
    - Files: `internal/memory/tools.go`, `procedures.go`, `review.go`,
      `internal/project/tools.go`, `internal/task/tools.go`.
    - Accepted when: one proposal of each kind, and a weekly review's batch, read in Russian.

12. **Collection, location, route and backlog proposals and Undo** (Simple).
    - Task: as chunk 8.
    - Files: `internal/collection/tools.go`, `undo.go`; `internal/location/tools.go`,
      `undo.go`; `internal/routes/tools.go`, `undo.go`, `route.go`;
      `internal/backlog/tools.go`.
    - Accepted when: one proposal and one Undo from each package read in Russian.

Wave 5, after wave 4:

13. **Intake, search check and connector proposals** (Simple).
    - Task: as chunk 8.
    - Files: `internal/intake/scan.go`, `service.go`; `internal/searchcheck/searchcheck.go`;
      `internal/connectors/homeassistant/action.go`, `confirmed.go`;
      `internal/connectors/nas/writes.go`; `internal/connectors/notion/movies.go`,
      `writes.go`, `undo.go`.
    - Accepted when: one proposal from each file, and a Notion Undo, read in Russian.

14. **Notices, rewrites and unattended turns** (Complex).
    - Task: fixed notices of scheduled runs and watches ("Nothing to report right now.", "Back to
      normal.", place arrivals, disruption and route lines); the watch fallback to the chosen
      language and the trip fallback when there is no sample; the chosen-language line beside
      `prompt.Time` in unattended turns; the language read in the watch, trip and schedule
      loops; the prompt golden files that gain the line.
    - Files: `internal/schedule/service.go`, `reports.go`; `internal/watch/service.go`,
      `place.go`, `disruptions.go`, `route.go`; `internal/trip/language.go`, `updates.go`,
      `loop.go`; `internal/turn/scheduled.go`, `task.go`; `internal/prompt/turn.go`;
      `internal/prompt/testdata/scheduled-job.txt`, `scheduled-scan.txt`, `background-task.txt`,
      `background-task-limit.txt`, `background-task-resume.txt`, `watch-check.txt`,
      `trip-updates.txt`, `weekly-review.txt`, and any other golden file the line reaches.
    - Accepted when: a Russian user's watch with no language of its own sends its alert through
      a rewrite into Russian (the rewrite faked at the runtime boundary); a trip update for a
      user who never wrote is rewritten into Russian; a scheduled turn's and a background task's
      message name Russian, while a chat turn's does not and an English user's unattended turn
      is unchanged; the system prompt is byte-identical for both languages.

15. **Images** (Simple).
    - Task: the texts and dates drawn on day cards, weather cards and charts.
    - Files: `internal/daycard/`, `internal/connectors/web/card.go`, `weather.go`,
      `internal/chart/render.go`.
    - Accepted when: a day card and a chart render Russian labels and dates for a Russian user,
      drawn in the embedded fonts.

Before deploy the operator reads the Russian of chunks 2 to 6 in the diff: those are the texts
every user meets first.

## Files affected

- `internal/locale/` (new)
- `internal/store/sqlite/migrations/061_principal_languages.sql`, `062_approval_language.sql`,
  `internal/store/sqlite/languages.go`, the approval store file
- `internal/wiring/wiring.go`, `internal/wiring/export.go`
- `internal/telegram/`: most non-test files, a new `language.go`, `widgets/*` and the widget
  golden files, and the test files that read exported reply constants
- `internal/approval/`, `internal/turn/types.go`, `scheduled.go`, `task.go`,
  `internal/conversation/activity.go`, `service.go`, `internal/connection/types.go`,
  `internal/task/`, `internal/export/`, `internal/weekday/weekday.go`
- Proposal and Undo files in `internal/schedule`, `watch`, `trip`, `memory`, `project`, `task`,
  `collection`, `location`, `routes`, `backlog`, `intake`, `searchcheck`, and
  `internal/connectors/homeassistant`, `nas`, `notion`
- Notice files in `internal/schedule`, `watch`, `trip`; `internal/prompt/turn.go` and its golden
  files
- `internal/daycard/`, `internal/connectors/web/card.go`, `weather.go`, `internal/chart/render.go`
- Tests beside each, `docs/architecture.md`, `docs/plans/README.md`

## Validation

- The completeness and argument test, the plural and date tables, and each chunk's Russian
  integration test with the Latin-word check.
- The existing tests pass in English, changed only in chunk 2 where a constant became a
  `locale.Text`.
- A test that a user's choice changes nothing for another user in the same harness.
- The model-facing texts of an approval (act result, Undo notice) are English for a Russian user.
- Formatting, vet, build, module tidiness (no new module), the full test suite, the race run on
  the Telegram, approval, store and wiring packages, and `scripts/check-plans`.
- After deploy, with a database backup taken first: the operator chooses Русский and reads
  `/help`, the slash menu, `/settings`, `/jobs`, a proposal with its outcome, a widget, and the
  next scheduled report; then chooses English and finds everything as before.

## Risks and open questions

- A string missed in a migrated file stays English. The completeness test finds a broken
  translation, not a missing one; the Latin-word check, each chunk's Russian test and the
  operator's read are the check.
- Russian is about 10 to 20 percent longer. A proposal near the 4096-unit message limit or a
  button near its width may overflow sooner; a too-long summary is refused as today.
- The previous image does not start on a database with migrations `061` and `062`. Going back
  means restoring the backup taken before the deploy, or fixing forward.
- A language change does not rewrite messages already sent, stored proposal summaries, or the
  names of built-in jobs already stored.
- Open: should a user who never chose follow Telegram's `language_code`? The plan says no,
  so a deploy changes nothing; a later change could offer the choice once to a user whose
  Telegram is in Russian.
- Open: should watch alerts and trip updates move from the model rewrite to the catalog? The
  plan keeps the rewrite and only feeds it the setting.
- Open: the Russian texts are drafted by the implementer; the operator's review of the most
  visible chunks is the quality gate.
