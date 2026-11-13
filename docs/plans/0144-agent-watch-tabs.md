# 0144 — A tab of its own for each agent watch

Status: `done`

## Context

Every watch that is not a home watch sends its alerts to one shared Watches tab. An agent watch
is different from the others: it follows one thing the user cares about over days or weeks, such
as a search on a marketplace, and its alerts are worth reading together and talking about. In the
shared tab they are mixed with train and weather alerts, and when that tab is missing they fall
back to the main view.

Trips and projects already have a tab of their own each, through one shared mechanism: a kind of
object registers how it is read and summarized, the tab registry records which tab serves it, the
tab carries a pinned summary, and a conversation started in the tab is about the object. A
conversation can already be about a watch, with the subject `watch:<id>`, when the user replies to
an alert.

## Goal

Each agent watch has its own tab. The tab appears when the watch is created, carries a pinned
summary of what is watched, receives every message of that watch, and a message the user writes
there is about that watch.

Success: with two agent watches and a train watch, each agent watch's alerts land in its own tab,
the train alert lands in the Watches tab, and writing in an agent watch's tab reaches a turn that
knows which watch it is about.

## Decisions

- **Agent watches only.** The other kinds are many, short-lived in what they report, and read
  well together, so they keep the Watches and Home tabs. A principal has at most five agent
  watches, which bounds the tabs.
- **Watches become the third kind of object with a tab of its own,** next to trips and projects,
  through the same registration. Its destination is `watch:<id>`, which is the subject a
  conversation about a watch already has, so no subject is renamed. The kind reads only agent
  watches; any other watch is unknown to it and its alerts resolve to the feed as today.
- **The tab is made when the watch is created** and is kept in step after every change to the
  watch: the summary is edited, and the tab renamed when the watch's name changed. A watch that
  exists already gets its tab with its next alert or change.
- **The pinned summary** is the watch's name, what it checks and when it tells the user, as the
  watch describes itself, and the stored facts that a change to the watch writes: that it is
  paused, and that it pauses while the user travels. A mute for today is left out, because it ends
  without a write and the summary would go stale.
- **All of the watch's messages go to its tab:** alerts, the notice that the condition no longer
  holds, and the notice that it cannot be checked. When the tab cannot be had, the message goes to
  the Watches tab, and to the main view while tabs are off, as today. A message sent to a tab that
  Telegram no longer has lands in the main view, as one sent to a gone feed or trip tab does, and
  the next message makes the tab again.
- **A paused watch keeps its tab; a removed watch's tab stays** with its messages until the user
  closes it. Nothing more is sent there, and a conversation started there is about nothing in
  particular, as in the tab of a removed project. A message that follows an alert still in the
  tab continues the conversation about that alert's watch, as a reply to the alert of a removed
  watch does in the Watches tab.
- **An Undo follows the same rules.** Undoing the creation of a watch removes the watch and leaves
  its tab with the summary, as for a removed watch. Undoing a removal brings the watch back into
  the same tab with a fresh pinned summary beside the old one.
- **A watch's tab cannot be taken by a trip.** The refusal the trip tools give names whose tab it
  is, a project's or a watch's.
- **The summary message is a column on the watch,** like a trip's and a project's, so the registry
  can forget it with the tab. The watches export section omits the column, as the trips and
  projects sections do; a restored watch gets a fresh tab and summary. The record an Undo keeps of
  a watch leaves the column out as well.
- **One active conversation is about a watch, as for a trip.** A conversation already open about
  the watch elsewhere, such as one started by replying to an earlier alert in the Watches tab,
  keeps the subject, and a conversation started in the watch's new tab is then about nothing in
  particular until that one ends. Making a tab moves no conversation, as it moves none for a trip.
- **The name a conversation about a watch goes by** stays the label of its last alert. Before the
  first alert there is none, and the turn is told of the watch by its ID, which already works.

## Approach

1. **Store.** Migration 055 adds the summary message column to watches. The watch repository reads
   it and records it. Watches join the tables of objects with a tab, so forgetting a tab forgets
   its summary and a conversation starting in the tab takes the watch as its subject; the note on
   those tables no longer promises a title column, which watches do not have. The export section
   omits the new column, and a test pins it.
2. **Thread.** The object kind `watch` and its destination helper.
3. **Watch service.** A reader for one watch with its summary text and summary message; a small
   interface the service calls after a watch is created, updated, paused, resumed, or its travel
   pause changes, including when an undo does it, and for each agent watch after a restore added
   watches. A mute calls nothing, since the summary leaves it out, and neither does a removal,
   since the tab of a watch that is gone is left as it is. Delivery resolves the watch's
   destination with the Watches feed as the fallback, in place of the feed alone.
4. **Telegram.** The watch kind beside the trip and project kinds, with its icon, a magnifying
   glass, and fallback name; the notifier that keeps a watch's tab in step; the trip-tab refusal
   wording.
5. **Wiring and tests.** Register the kind and the notifier; end-to-end tests through the Telegram
   simulator for the success case above, a paused and a removed watch, a tab that cannot be made
   and one found gone, tabs off, a restored watch, a message written in the tab, a message written
   in the tab while a conversation about the watch is open in the Watches tab, and a reply to
   another kind of watch's alert.
6. **Documentation.** The tabs section of the architecture document. No help text lists where
   messages go, so the guidance of the watch tools tells the model that an agent watch gets its
   own tab.

## Files affected

- `internal/store/sqlite` (a migration, watches, tabs), tests
- `internal/thread`
- `internal/watch` (service, a tabs file, tools, undo, export where they change a watch)
- `internal/telegram` (a watch tabs file, tab set, trip tabs wording), tests
- `internal/wiring`, `internal/app`, the integration test harness
- `docs/architecture.md`

## Validation

- Formatting, module, vet, build, race and non-race tests, and `scripts/check-plans`.
- The end-to-end tests named in the approach.
- Live, by the operator after deployment: the existing agent watch gets its tab with its next
  alert, and a new one gets it at once.

## Risks and open questions

- **Every query over object destinations now matches `watch:` too.** Conversations already use
  that subject for all kinds of watch, replied to from the shared tab. The rule that one active
  conversation is about an object, and that it lives in the object's tab once there is one, then
  applies to agent watches; replies to another kind of watch's alert must behave as before, which
  a test pins.
- **Telegram's icon set may not offer the icon;** the tab then has none, as for other kinds.
- **This plan lands after plans 0143 and 0145,** which change the same tab store file and the same
  watch service and architecture files.
- **A removed watch's tab is not marked as such.** Editing the summary to say so is a possible
  follow-up.
