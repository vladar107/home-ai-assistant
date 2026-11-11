# 0140 — Proposals as a Telegram widget

Status: `done`

## Context

Bot API 10.4 lets a bot send a message whose body is a small HTML, CSS and JavaScript program
that Telegram draws inside the chat bubble. A tap in it reaches the bot as a `widget_query`
update over the same long polling the bot already uses, and the bot changes what the widget
shows by editing up to 1024 bytes of data attached to the message.

A throwaway script outside the repository sent test widgets through the bot. It established
that:

- widgets are accepted in the private chat and in its tabs;
- one Telegram app renders them, with the caption below the widget, and another shows only the
  caption;
- that second app shows a widget message that also carries an inline keyboard as "unsupported",
  with neither caption nor buttons, so buttons cannot be the fallback on the same message;
- the bot can edit a widget message's data and its caption, and cannot edit its text.

What the script could not test is the bot answering a tap, because that needs the poller. The
proposal a user confirms or rejects is the smallest real flow that exercises it.

## Goal

A user who chose to can confirm, reject and preview a proposal in a widget, with the same effect
as the buttons, and sees the outcome in that message. Everyone else keeps the buttons they have
today.

## Decisions

- **Each user chooses.** A switch in `/settings`, off by default, sends that user's proposals as
  widgets. It gates sending only: taps on widgets and edits of widget messages are always
  handled, so switching it off never strands a proposal already in the chat.
- **No inline keyboard on a widget message**, because of the "unsupported" rendering above. The
  caption carries the proposal text, so an app without widgets still shows what was proposed;
  the settings page says that such an app cannot confirm it.
- **Only the message that has the buttons today becomes a widget.** When its text does not fit a
  caption, its data does not fit the limit, or Telegram refuses the widget, the proposal is sent
  as today.
- **One template, written by the operator and embedded in the binary.** The model never supplies
  HTML or JavaScript. It draws from the message's data: the proposal text, and either the three
  actions or an outcome. A test fails when it no longer fits Telegram's compressed-size limit.
- **A tap sends what a button press sends**, so one parser and one authorization serve both: the
  private chat, the sender's identity, then the proposal's owner.
- **A tap is answered before its actions run**, so the answer deadline never depends on how long
  they take. An answer Telegram no longer accepts does not stop the press. The widget disables
  its controls after a tap and says that it is working. A preview's answer carries what the
  button's answer carries.
- **A tap carries no tab**, so the proposal's own tab is restored before it is handled, as a
  preview press does. A tap is taken only from the message its proposal is shown in.
- **Outcomes go through the caption and the data**, in the proposal's own edit calls only. A
  text edit Telegram refuses because the message has no text is repeated as a caption edit; the
  data and the removal of the actions are sent whether or not the caption edit succeeds. An
  outcome too long for a caption is clipped there and sent in full as a reply.
- **The widget draws from its current data when it loads**, so the outcome shows even where the
  live update is not delivered.
- **Confirm still covers the whole bundle.** Choosing items one by one changes what a proposal
  means and is a later plan.

## Approach

1. Client: the `widget_query` update and its place in the polled update types; sending a widget
   through the tab-aware send path; answering a query; editing a widget's data; the caption
   path in the proposal's edit calls.
2. Service: handling a widget query beside callback queries, sharing the press code through a
   small value that names the chat, the message and how to answer.
3. Approval: ask a per-user preference whether to send the proposal as a widget, and expose the
   proposal's tab to the Telegram layer.
4. Template and its size test.
5. The `/settings` switch and its help topic.
6. The proposal flow in the architecture document.

## Files affected

- `internal/telegram` (client, service, sessions, settings, a new widget file and template)
- `internal/approval`
- `internal/wiring`
- `docs/architecture.md`

## Validation

- Integration tests against the fake Telegram server: the switch on and off; confirm, reject and
  preview from the widget; a tap by another user; a tap in a tab continues in that tab; a button
  press and a later tap on the same proposal; a long outcome; an answer Telegram refuses; the
  switch turned off with a widget pending; expiry and withdrawal of a widget proposal; a
  proposal too long for a caption; text in scripts that take several bytes a character; a tap
  that names a message other than the proposal's.
- The configured formatting, vet, build, test and plan checks.
- On the deployed bot: a real proposal confirmed in the widget, and one read on an app without
  widgets.

## Risks and open questions

- Each edit of a widget message first fails as a text edit, which shows as one expected error in
  the API telemetry. Recording the kind of message would avoid it and needs a schema change.
- Whether the widget redraws on a data edit without being reopened is documented, not yet seen.
- An image rolled back past this plan no longer receives taps; a pending widget proposal then
  expires unanswered.
- Long proposal text is shortened inside the widget; the caption keeps all of it.
