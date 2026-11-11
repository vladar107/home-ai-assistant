# 0142 — One text on a widget proposal

Status: `done`

## Context

Plan 0140 sends a proposal as a widget for a user who chose it. Used on the deployed bot, it
works: a tap arrives, is answered, and the widget redraws itself when its data is edited. Two
things came out of that use.

- The proposal is on screen twice, once in the widget and again in the caption under it.
- Every edit of a widget message first tries a text edit, which Telegram refuses, and only then
  takes the widget path. That is one expected error an edit in the API telemetry, and it depends
  on how Telegram words the refusal.

## Goal

A widget proposal shows its text once, and the bot edits a widget message without a refused
attempt first.

## Decisions

- **The widget holds the whole proposal and the caption is one line.** The approval service
  supplies that line with the text: what is proposed, as a single proposal's label or the number
  of changes, and what became of it once it is resolved. The Telegram layer does not guess it
  from the text, whose first and last lines are a warning, a heading or one item's result in
  several shapes of proposal.
- **A proposal is a widget only when its whole text fits the widget's data with its actions,
  unshortened.** Otherwise it is sent with buttons. Proposals that carry a long text of their
  own, such as a new scheduled job with its prompt, and large bundles therefore keep buttons.
- **The status is a field of its own in the data.** When an outcome makes the text too long for
  the data, the text is shortened and the status stays whole, and the full text is sent once as
  a reply. Once means when the data edit changed the data; the caption no longer tells.
- **The widget shows the first line as a title** and the status apart from the text. The text is
  plain: the formatting a message would show is not carried into the widget.
- **A proposal's record says that its message is a widget.** Sending reports whether a widget
  went out; the flag is written with the message, cleared when delivery fails, and never set for
  the message of a direct action. An edit of a flagged message is a widget edit from the start.
  A tap is on a widget and a button press is on a text message, which the Telegram layer knows
  without the record.
- **The repeated text edit stays as a net** for a widget recorded before the flag existed, which
  is a proposal pending while the bot is upgraded. Its older template draws the text alone, so
  its status stays in the text. Removing the net is a later change.

## Approach

1. Store: a column on approvals, read and written with the message.
2. Approval: the caption line and the status beside the text in what is sent and edited; the
   flag from sending to the record and from the record to edits.
3. Client: the fit rule, the data with its status, the reply decided by the data edit, and edits
   that go by the flag.
4. Template: title and status.
5. The settings page text and the architecture document.

## Files affected

- `internal/store/sqlite` (a migration, approvals)
- `internal/approval`
- `internal/telegram` (client, service, widget file and template, settings text)
- test doubles of the proposal deliverer in other packages
- `docs/architecture.md`

## Validation

- Integration tests against the fake Telegram server: the text is in the widget and not in the
  caption; the caption line of a single proposal, a bundle, a proposal with a warning, one with
  a heading, and a bundle whose items ended differently; a long proposal, and a shorter one in a
  script that takes several bytes a character, sent with buttons; an outcome too long for the
  data, shortened with its status whole and replied in full once, also on a repeated edit; a
  withdrawal that grows a pending widget; no text edit attempted for a flagged proposal on a
  tap, on expiry and on withdrawal; a widget Telegram refuses; the flag cleared by a failed
  delivery and set by the resend; the message of a direct action unflagged; an unflagged widget
  edited through the net; the template within Telegram's size limit.
- The migration applied to a database from before it.
- The configured formatting, vet, build, test and plan checks.
- On the deployed bot: a proposal shown once, confirmed in the widget, with no refused text edit
  in the API telemetry.

## Risks and open questions

- A reply with the full text that fails once is not sent again, and the widget then shows only
  the shortened text. Recording that the reply was sent would close it.
- An app without widgets shows only the one-line caption.
- Text in the widget is plain.
