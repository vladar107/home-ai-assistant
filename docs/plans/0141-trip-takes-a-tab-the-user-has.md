# 0141 — A trip takes a tab the user already has

Status: `done`

## Context

A planned trip gets a tab the assistant makes for it (plan 0089), and its summary, reminders,
notices, and updates go there. A user who already keeps a tab of their own for the trip ends up
with two: their own, where they talk about it, and the assistant's, where everything about the
trip arrives. Nothing lets the trip use the tab that is already there.

## Goal

The user can say, in a tab of their own, that this is the tab for a trip, and the trip moves
there with everything it sends. A trip added from a tab the user named takes it from the start.

## Decisions

- The tab is the one the turn runs in, which tool calls already know (`thread.From`), and for a
  proposed change the one recorded on its approval. The tools never take a tab as an argument,
  so a turn can only give away the tab it is in.
- `trip_update` and `trip_add` take `use_this_tab`.
  - On `trip_update` it is a change of its own, alone or with edits, not with cancel or end now,
    and only for a planned trip that has not ended.
  - It is part of the recorded arguments, and a proposal's card says the trip moves to this tab.
  - Whether the tab can be taken is checked again when the change is carried out.
- A tab can be taken when its registry row has no subject and was named by the user or by the
  assistant for a conversation. A tab whose trip has ended or was cancelled can be taken too,
  and that trip then has no tab; when the assistant had made that tab it stays the assistant's
  to name, so it takes the new trip's title, and the ended trip's summary there is edited to
  say so. The main view, a feed tab, a tab of an open trip or of a
  project, and a tab the registry does not know are refused with the reason, as is any tab while
  tabs are off.
- `trip_add` without the flag takes the tab by itself only when the user named it. A tab the
  client opened for a message from the main view was named by the assistant, so a trip added
  there gets a new tab, as do a trip accepted from a card and one found by the scan. When the
  tab turns out not to be takeable at that moment, the trip gets a new tab.
- Taking a tab is one transaction: the row of the tab the trip had loses the trip's subject and
  becomes a tab named by the user, the taken row gets the subject and becomes named by the user,
  the trip's recorded summary message is cleared, and every active conversation of the trip
  loses the subject while the taken tab's conversation gets it if it has none or was the ended
  trip's. An earlier conversation of the trip that the user opens again in the tab it left
  comes back without the subject.
- After it: the old summary is edited to say the trip moved, and the trip's summary is sent and
  pinned in the taken tab. If the process stops in between, the missing summary is sent with the
  trip's next message and the old one stays as it was.
- The take runs under the lock that already orders a trip's tab work, with the ended trip's as
  well when a tab is taken from one, and in place of making a tab: for `trip_add`, and for a
  move that comes with edits, which moves first. No tab is made and then left behind.
- A taken tab keeps the name and icon the user gave it. A trip's fixed name and icon are applied
  only to a tab the assistant made for it; no migration and no new kind of tab name are needed.
  The user can rename a taken tab, and so can they the tab the trip left.
- The tab the trip had stays, as an ordinary tab with its messages; the result says the user can
  delete it. A message already on its way when the trip moves can still arrive there.
- Moving a trip to the tab it already has changes nothing, and a trip can move again, also back.
- A taken tab the user later deletes is handled as any trip tab that is gone.
- Only trips move. Projects keep their own tabs; the shared tab code changes only where a trip
  needs it.

## Approach

1. Store: the one transaction above, with its tests.
2. Trip tabs: fixed name and icon only for a tab the assistant made, taking a tab under the
   trip's lock, the note on the old summary, and the reasons for refusing a tab.
3. Trips: the tab passed into the add and the move, the tools' flag, the automatic case, result
   and card texts, and guidance.
4. Documentation: the trips part of `README.md` and the in-bot trips text.

## Files affected

- `internal/store/sqlite/tabs.go`, `conversation.go`
- `internal/telegram/objecttabs.go`, `triptabs.go`, `feeds.go`, `trips.go`
- `internal/trip/tools.go`, `service.go`, `trip.go`
- `README.md`

## Validation

- Store tests: the transaction moves the subject, clears the summary, and leaves one active
  conversation with the trip's subject, also when the trip's conversation was in a feed tab.
- Integration tests on the real assembly with a fake Telegram:
  - saying it in a user tab moves the trip: the summary is pinned there, the old summary says
    the trip moved, and a later reminder, notice, and update arrive in the taken tab;
  - the taken tab keeps its name and icon when the trip changes and after a restart, and a
    rename by the user is kept;
  - the next turn in the taken tab is the trip's conversation, and a reply to a reminder there
    continues it;
  - the main view, a feed tab, an open trip's tab, a project's tab, and a suggested or ended
    trip are refused with a reason; with tabs off the tool refuses;
  - a tab of an ended trip can be taken;
  - `trip_add` in a tab the user named takes it and makes no new tab; in a tab the client opened
    for a message, and in the main view, the trip gets a new one;
  - a proposed `trip_add` confirmed in a user tab takes that tab, and its card says so;
  - moving twice, moving back, and moving to the tab the trip has, leave one tab for the trip;
  - a taken tab that was deleted is replaced as a gone trip tab is today.
- The configured format, module, test, vet, build, and plan checks.
- After deployment: move the current trip into the user's own tab and watch the next reminder.

## Risks and open questions

- The model has to recognise the request; a user who phrases it unusually may need to repeat it.
- A trip added in a user-named tab that is about something else takes that tab. It can be moved.
- Some tabs count as named by the user although the user never typed a name: tabs from before
  the assistant named tabs, a tab first seen without its opening message, and a tab a trip left.
  A trip added there takes it by itself.
- The old tab keeps a pinned message that only says the trip moved, until the user deletes the
  tab.
- A deleted taken tab the user wrote in recently is replaced only once Telegram's refusal is
  taken for final, as for any tab.
