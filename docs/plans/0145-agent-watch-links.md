# 0145 — Links to what an agent watch found

Status: `done`

## Context

An agent watch's check ends with one JSON object: whether the condition holds, a short value, a
headline, and optional short details. The alert is the headline and the details. Nothing in that
shape asks for the pages of what was found, so an alert about new listings on a marketplace names
them without a way to open them, although the tool that found them returns each listing's
address.

An alert in another language than English is rewritten by a model before it is sent, and the
rewrite is accepted when it keeps every number of the original. An address is not protected by
that rule beyond its digits.

## Goal

An agent watch's alert carries a direct link to each thing it found, and the link reaches the user
exactly as the tool returned it.

## Decisions

- **The answer object gains `links`:** up to five entries, each a short title and an address. The
  check's instruction asks for the page of each item found, copied exactly from a tool result of
  this run, and for none when no tool returned one.
- **Code decides what is kept.** An address must be an absolute `https` address with a host, no
  user info, no more than 200 characters, and none of the characters the message markup reads:
  whitespace, a backtick, a square bracket, a round bracket, or two asterisks in a row. A title is
  made one line, stripped of control characters and of the same markup characters, clipped to 60
  characters, and falls back to the address's host. Entries that fail, and entries past the fifth
  that passed, are dropped without failing the check, and so is a `links` field or an entry of
  another shape than asked for. An entry whose title is not text is kept under its host.
- **The address shown is the address opened.** A chat client makes the link of a written-out
  address itself and ends it at some characters, so an address that ends in one of `.,;:!?` or
  holds a double quote, an angle bracket, or a vertical bar is dropped. So is one that holds an
  invisible or direction-changing character, and one whose host is not ASCII, so a name in another
  script shows only in its `xn--` form.
- **A title cannot stand in for an address.** Invisible and direction-changing characters are
  taken out of it, so it cannot reverse the address written after it. A title that reads as an
  address itself, by holding `://`, a word starting with `www.`, or a dot between a letter, digit,
  or hyphen and two letters, would become a second link the model chose, and is replaced by the
  host.
- **A link is one line of the alert, a marker, the title, then the address,** between the headline
  and the details, so the copies of an alert that are kept shorter than the message lose details
  before links. The address is written out, never hidden behind the title, so the user sees where
  it leads before opening it; the sanitising above is what keeps a title from forming a hidden
  link or a second line.
- **Only code writes a link line.** A marker at the start of a line of the model's headline or
  details is removed, and so are the invisible format characters in them, so no such line is read
  as a link line or looks like one.
- **Links do not change what counts as a change.** The watch's state stays the condition and the
  value.
- **Addresses never go through the rewrite.** Before an alert is rewritten into the watch's
  language, its link lines are taken out, and afterwards they are put back: at their places when
  the rewrite has as many lines as the text it was given, and at the end otherwise. The rewrite
  and its faithfulness rule see the text without them, so an address reaches the user exactly, and
  its digits cannot make a good rewrite fail. A rewrite that no longer fits one message with its
  links is not used, and the original is sent.
- **The note for the next chat turn never ends inside a link.** It is clipped to 1000 characters
  as before, and when that falls inside a link line, it ends before that line.
- **No setting.** Every agent watch gets the field; a check that found nothing to link sends none.

## Approach

1. The check instruction and the answer object in the agent evaluator, with the rules above, and
   the link lines carried by the observation into the alert's text.
2. Taking the link lines out of the text that is rewritten, and putting them back.
3. Tests: an agent watch whose check returns links alerts with them; bad and surplus entries are
   dropped; a title with markup, a bracketed link, or a line break in it shows as plain text on
   one line; a rewritten alert carries its addresses unchanged and in place; an alert with five
   long entries fits one message; a rewrite that fails or grows too long sends the original with
   its links; the note for the next turn ends before a link it cannot hold whole.

## Files affected

- `internal/watch/agent.go`, `internal/watch/service.go`, tests
- `docs/architecture.md` where it describes the agent watch's answer

## Validation

- Formatting, module, vet, build, race and non-race tests, and `scripts/check-plans`.
- Live, by the operator after deployment: the next marketplace alert opens each listing.

## Risks and open questions

- **The address is written by a model that read outside content.** A hostile page could have it
  point anywhere `https`. The address is shown in full, which is what a chat reply after outside
  content already does; checking it against the run's tool results in code would need the check
  runner to return them and is left as a follow-up.
- **The note of an alert that the next chat turn is told of is clipped to 1000 characters,** so
  with several long links it can lose the last ones, each whole. The alert kept for a reply holds
  them all.
- **The test for a title that reads as an address is wide.** A title such as a file name with an
  extension is replaced by the host too, which names the page less well but never wrongly.
- **Telegram previews the first address of a message.** For a listing that is a picture of the
  item, which is left on.
- **The model may still leave links out.** The instruction asks, code cannot require them, since
  many checks have nothing to link.
