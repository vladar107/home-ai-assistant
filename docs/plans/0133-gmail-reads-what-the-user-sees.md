# 0133 — Gmail reads the message as the user sees it

Status: `done`

## Context

`gmail_read_message` renders a message's body from its `text/plain` alternative and reads the
`text/html` alternative only when there is no plain text. Mail clients show the HTML. The two
alternatives are written separately by the sender and can disagree: an airline booking
confirmation said one date of birth in its plain text and another in its HTML. The evening report
read the plain text, warned the user that the booking held the wrong date of birth, and the user
found the right date in the message they saw.

Every reader of mail goes through `gmail_read_message`: chat, the trip scan, and the Inbox intake
check. The HTML fallback is a set of regular expressions that glue neighbouring table cells
together and keep text that is hidden from the reader, while the web connector already reduces
HTML to visible text with a parser. Message bodies are also decoded as UTF-8 whatever charset the
part declares.

## Goal

- A message with an HTML alternative is read as its HTML shows it, so the model and the user see
  the same facts.
- Table cells, headings, and list items stay readable, and text hidden from the reader is left
  out.

## Decisions

- **HTML first.** The body is the message's first body part, skipping attachments; of a
  `multipart/alternative` body, the `text/html` alternative reduced to text is read, and the
  `text/plain` alternative only when the HTML reduces to no text. HTML that holds only images
  reads as their alt text and does not fall back. The search stays inside that body, so a plain
  message is not read as the HTML of a message attached to it.
- **One reducer.** The web connector's parser-based reducer moves unchanged to a shared
  `internal/htmltext` package, and the Gmail regular expressions are deleted. The reducer takes
  options so that mail can differ from web pages without changing how pages are fetched:
  - **Inline styles hide.** For mail, an element whose inline `style` declares `display: none` or
    `visibility: hidden` is skipped. Declarations are parsed, with each property and value
    trimmed, lowercased, and stripped of `!important`, and the last declaration of a property
    wins. A substring match would wrongly skip
    `overflow: hidden`, which mail layouts use everywhere. Web pages keep today's rule, because
    pages reveal inline-hidden content with scripts.
  - **Links read as their text.** For mail, a link reads as its text alone, as the HTML fallback
    does today. Tracking addresses of hundreds of bytes would otherwise push a booking's details
    out of the first 12 KB window, which is often the only window the scans read. A link with no
    text and no image adds nothing.
  - **Invisible padding is dropped.** For mail, zero-width and soft-hyphen characters (U+200B,
    U+200C, U+200D, U+034F, U+00AD, U+FEFF) are removed, because preheaders pad with long runs of
    them.
- **Declared charsets.** A body part is decoded from the charset its `Content-Type` declares,
  using `golang.org/x/net/html/charset` as the web fetch already does. A missing or unknown
  charset keeps today's UTF-8 reading, and so do UTF-8 and US-ASCII, which the browser label
  table maps to windows-1252 although senders label UTF-8 bodies US-ASCII.
- **No other change.** Window size, offsets, attachments, headers, and the search snippet stay as
  they are.

## Approach

1. `refactor:` move `reduceHTML` and its helpers from `internal/connectors/web/html.go` to
   `internal/htmltext`, exported as one function returning the title and the text, and point the
   web fetch at it. Behavior does not change.
2. `fix:` add the mail options to the reducer; in `internal/connectors/gmail/render.go` decode
   parts by declared charset, make `messageText` prefer the reduced HTML with the plain-text
   fallback, and drop the regular expressions; update the tests and the Google integration
   fixtures that pinned the plain-text preference.
3. Plan index and checks. The operator deploys and runs the live check.

## Files affected

- `internal/htmltext/` (new), `internal/connectors/web/html.go`, `fetch.go`
- `internal/connectors/gmail/render.go`, `tools_test.go`, `render_test.go`
- `internal/testutil/googlesim/simulator.go`, which declares a charset on a message's parts
- `internal/telegram/google_integration_test.go`
- `docs/plans/0133-gmail-reads-what-the-user-sees.md`, `docs/plans/README.md`

## Validation

- Through the Gmail connector's test harness, with a read helper beside its search helper and
  messages built by the Google simulator:
  - a `multipart/alternative` message whose plain text and HTML give different dates is read
    with the HTML's date and without the plain text's;
  - a table row of a label cell and a value cell reads as `Date of birth 12 May 1980` on one
    line, with the next row on its own line;
  - elements styled `display:none!important` and `visibility: hidden !important` are not read,
    while elements styled `overflow:hidden; max-height:0` or `display:none; display:block` are;
  - a link reads as its text with no address;
  - a preheader of zero-width padding adds no characters;
  - an HTML part that reduces to no text falls back to the plain text, and an HTML part of one
    image with alt text reads as `[image: …]`;
  - a part declared `charset=windows-1252` reads its accented letters correctly, and so does a
    UTF-8 part declared `charset=us-ascii`;
  - a plain message with an attached message holding HTML is read as its own text;
  - a message with only plain text reads as before, and the windowing test still passes.
- `internal/telegram/google_integration_test.go` asserts the HTML's text for the message that has
  both alternatives, and the paragraph spacing the reducer produces.
- The web connector's existing fetch tests pass unchanged.
- Formatting, module, vet, test (with and without `-race`), build, and plan checks.
- After deployment, the operator asks the bot to read the booking confirmation that prompted this
  plan and confirms it reports the date shown in the mail client, with the passenger details in
  the first window.

## Risks and open questions

- HTML mail reduced to text can still be longer than its plain alternative because of layout
  text, so long messages may take more windows.
- Text hidden by a stylesheet class rather than an inline style is still read.
- An element hidden with `visibility: hidden` is skipped with its children, even a child that
  sets `visibility: visible`.
- A charset declared only in an HTML `<meta>` tag is not read; such a part is read as UTF-8, as
  before.
- Only the first `text/html` part is read, as only the first `text/plain` part is today, so a
  message whose HTML is split around inline attachments loses the later parts.
