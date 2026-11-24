# 0176 — Quiet parcel messages, postcode tracking links, and cards that name the parcel

Status: `in-progress`

## Context

Plan 0160 added parcel tracking. After a week of real use, four problems showed up:

- **Noise.** Status notices such as out for delivery, ready for pickup, and delivered sound
  during the day. So do new cards and the card that offers numbers found in mail. Today only
  quiet hours and the baseline are silent.
- **Links ask for a postcode.** The user's postcode is in the Parcels settings, and adding a
  number to a carrier integration already uses it. The tracking links never do, so two
  carriers' pages ask for it before they show anything. Both carriers have a deep link that
  takes the postcode: one carrier's own integration hands out exactly that form, and the other's
  integration builds it.
- **Wrong carrier.** A carrier's mail came from a regional sender domain that is missing from
  its domain list. The mail's 14-digit number then matched only another carrier's pattern, and
  `resolveCarrier` lets a pattern that matches a single carrier override the carrier the model
  named. The card showed the wrong carrier and linked to its page. Because of that, the parcel
  was never offered to the right integration. The carrier's tracking service confirmed that this
  number and the 8-character track ID from the shop's mail are the same shipment, and that its
  parcel numbers are 14 digits.
- **No name.** `PlainText` removes every bare domain from outside text, so that a title cannot
  become a link in Telegram. Many shop names are domains ("SHOP.NL"). The model's title "Package
  from SHOP.NL", written in the user's interface language, was stored as "Package from". The
  carrier integration's own title, which is just the shop's name, was stored empty, so the stub
  stayed. The card also shows "From: <carrier domain>", which mail sent by the carrier itself
  records as the sender.

## Goal

- Every parcel message arrives without sound: cards, notices, the tracking summary, and the
  mail-numbers offer, including a resend by the approval sweep. Other approval offers keep their
  behaviour.
- With the user's postcode set, a card's link opens the carrier's page for the parcel without
  asking for it.
- A carrier's mail from any of its known domains is filed under that carrier. A number shape
  shared by two carriers is settled by the carrier the mail names.
- A card shows the shop's name when a source has it, and never shows the carrier's own domain as
  the sender.

## Decisions

- Consult (advisor): should the postcode go into the link when the parcel is stored, or when a
  card is drawn? Decided: when drawn. Mail parcels are written once, so a stored link would never
  pick up the postcode or a later change to it. The stored link, which the parcel tool shows the
  model, also stays free of the postcode.
- `Carrier` gains an optional postcode link template and a `LinkFor(number, postcode)` resolver.
  The resolver falls back to `Link(number)` when the postcode is empty, the carrier has no such
  template, or the postcode does not have the template's country shape.
- The notifier replaces a card's link only when the stored link equals `Link(number)` for the
  parcel's carrier. Order links and links a source supplied pass through unchanged.
- If reading the settings fails, the card keeps the stored link. The postcode is never logged.
- A postcode change does not count as a card change; cards pick it up at their next edit.
- `PlainText` keeps a bare domain that has no path, with its real dots. Links, and domains with
  a path (`shop.example/` counts), are still removed, as before. Senders, order keys,
  tombstones, and the parcel tool all keep the real text.
- When a card or notice is drawn, a domain in the title, the sender, or the pickup point gets a
  one-dot leader (U+2024) in place of each dot. It reads the same, and Telegram does not link
  it. This also covers the mail sender line, which is linkable today. Plan review (advisor,
  second round) moved the swap from stored text to drawing, because stored leaders would split
  sender identity.
- The parcel tracker always sends silently, so the `silent` parameter leaves the `Cards`
  interface. The quiet-hours clock stays, for the mail interval that still uses it.
- The approval service gains a silent variant of `OfferIfIdle`. The flag is stored on each
  record, so the sweep's resend stays silent too. The deliverer gets it through `Shown`, and
  `SendText` takes it as well. The text card, the proposal widget, and the changes widget all
  send with notifications off.
- A sender that is one of the parcel carrier's own domains is not drawn, on the card or in
  notices.
- The carrier gets its regional sender domains, and the 14-digit pattern its numbers have. It
  shares that pattern with the carrier that had it. Shared patterns already go through
  `Settle`/`Twins`, where the carrier the mail names decides.

## Approach

1. Silent offers (wave 1, Opus; Jev said complex at 0.61, below its threshold; the persistence
   and the sweep path decide it):
   - Add a migration with a `silent` column on `approvals` (default 0), and a `Record.Silent`
     field the repository reads and writes.
   - Add a silent variant of `OfferIfIdle`, and a `Shown.Silent` field that `SendText`,
     `SendProposal`, and both widget senders honour.
   - The mail job uses the variant. This wave owns every `Deliverer` fake.
2. Parcels (wave 2, Opus; Jev: complex at 0.97). It shares the eval and mail test fakes with
   wave 1, so it runs after it:
   - Make the tracker silent everywhere.
   - Add the carrier link templates and `LinkFor`, and give the notifier a settings reader
     where it is built.
   - Keep domain-shaped names in `PlainText`, and hide the carrier's own domain as the sender.
   - Add the domains and the pattern.
3. Production data, after the operator deploys: the operator stops the duplicate card from the
   card itself, which tombstones that number. The other number stays tracked, through the pending
   offer to add it to the carrier integration. No database write is needed.

## Files affected

- `internal/approval/` (offer, record, `Shown`), `internal/store/sqlite/approvals.go`, a new
  migration, `internal/telegram/client.go`, `proposal_widget.go`, `changes_widget.go`, and
  `widget.go`, and `internal/parcel/mail.go` (the call site).
- `internal/parcel/carriers.go`, `tracker.go`, and `parcel.go` (`PlainText` and the `Cards`
  interface); `internal/telegram/parcels.go`; `internal/app/app.go`, where the notifier is
  built; `docs/architecture.md`; and the fakes in `internal/eval/`.
- Tests beside each.

## Validation

- Integration tests against a real temporary SQLite and the fake Telegram server:
  - a parcel card, a notice, and the mail offer carry `disable_notification`, and so does its
    sweep resend. The offer is tested with one item and with several, with widgets on and off;
  - a chat turn's proposal does not;
  - a card's link carries the postcode when one is set, and the plain link when the postcode is
    unset or invalid, or when the settings fail to load. Neither the log output nor the stored
    row contains the postcode;
  - a 14-digit number in mail is filed:
    - under the old carrier when the mail names no carrier;
    - under the newly named carrier when a shop's mail names it;
    - under the carrier when the mail comes from its regional domain alone;
  - a domain-shaped shop name is stored with its real dots, from a mail title and from an
    integration's title. The text sent to Telegram has no dot inside it. A link, or a domain
    with a path (including a trailing slash), is still removed;
  - a stopped order-only parcel whose sender is domain-shaped stays tombstoned;
  - the carrier's own domain is not drawn as the sender, whether it came from mail or from an
    integration.
- Format, module, vet, test, build, and `scripts/check-plans`; CI green.
- In production, the next parcel card arrives silently and shows the shop's name, and that name
  cannot be tapped. Its link opens without a postcode prompt (checked by hand for both carriers).

## Risks and open questions

- The postcode templates are country-specific. The country-shape check keeps any other postcode
  on the plain link.
- A later mail cannot move a parcel to another carrier. If a shop's mail without a carrier
  filed a 14-digit number under the first carrier, the carrier's own mail for the same number
  leaves it there; only Home Assistant corrects such a guess. This is unchanged from before.
- Every 14-digit number now matches two carriers. It can stand under either of them when
  parcels are matched, and mail that names neither carrier still goes to the first in the list.
- The one-dot leader makes a shown domain unlinkable, but it is not a real dot, so copying it
  from a card does not give a working address. Stored titles already cut short stay as they are
  until a source updates them.
- Senders from integrations that used to be stripped to nothing are now kept, so they can match
  a mail sender's order key where they never did before.
- The postcode reaches only the Telegram message. Cards are drawn by the notifier, not by a model
  turn, so it never enters a session transcript. Link previews are already off.
- An older binary refuses a database with more migrations, so rolling back this release needs
  the database backup taken before it.
