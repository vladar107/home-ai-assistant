# 0028 — A visual day card with the reply as its caption

Status: `done`

## Context

Scheduled and interactive day plans arrive as a wall of text. The operator's
morning prompt, for example, lists each leg's weather in bullet form. The
operator wants the shape of a phone weather widget instead: a single photo
message. The top of the image shows the current temperature and feels-like, a
large icon, and a one-line summary ("Утром дождь, вечером морось."), followed
by one tile per leg (place, hours, icon, temperature, rain in mm, gusts). The
caption underneath carries the advice and the train status, one short
collapsible section per direction ("Туда: опоздания 12 мин", "Обратно: по
расписанию").

Today, the application:

- sends replies only as `sendMessage` in HTML parse mode, split at 4,000
  characters, through a durable claim/deliver path in
  `internal/conversation/service.go` and `internal/schedule/service.go`;
- sends outbound files only as documents (`delivery.SendFile`, plan
  [0023](0023-outbound-telegram-files.md)), during the turn and separate from
  the reply;
- has no image, font, or drawing dependency; the runtime image is
  `node:24-alpine` with no browser or fonts;
- passes no turn identity to broker tools (`Execute(ctx, principal, call)`);
- converts bold, code, and links from Markdown in `renderMarkup`, but not
  quotes.

## Goal

The model can call one tool to attach a rendered day card to its reply. The
user then receives a single Telegram message: the card as a photo and the
reply as its caption, with expandable quote sections where the model uses
them. Scheduled turns can do the same, so the morning prompt can become "card
+ advice + trains". A reply too long for a caption still arrives complete, as
the photo followed by the text. A turn that does not call the tool behaves
exactly as today.

## Decisions

- **Rendered in Go from a fixed template.** The operator chose this over
  headless Chromium (about 300 MB, and a browser on the NAS) and over text
  only. The renderer:
  - is a new `internal/daycard` package built on
    `golang.org/x/image/font/opentype` and the standard `image/png`;
  - embeds Inter (SIL OFL), which covers Latin and Cyrillic, in the Regular
    and Semibold weights;
  - uses a small pre-rasterised weather icon set under a permissive licence,
    committed as PNGs with its licence and source recorded under
    `third_party/`;
  - maps each WMO code, day or night, to one icon.

  Day or night comes from the daily sunrise and sunset times, which the
  weather code already requests. This avoids adding `is_day` to the hourly
  fields and leaves the `weather_forecast` request unchanged.

  The layout is fixed: a 1,080 px wide image, a header, and 1–4 leg tiles. No
  text is smaller than 36 px, because Telegram recompresses photos to JPEG. A
  gradient chosen from the header condition (clear, cloudy, rain, snow,
  storm) keeps text legible whatever the Telegram theme. Any text that does
  not fit is truncated with an ellipsis, never overflowing.
- **The tool fetches its own numbers.** The tool is
  `weather_card(title, summary, legs[{label?, place, date, from_hour,
  to_hour}])`, in the web connector because it uses the same Open-Meteo
  client and the same "Web access" switch. It:
  - geocodes and fetches the hourly forecast per leg with the 0026 code;
  - aggregates each leg into the dominant icon, the average temperature, the
    total precipitation, and the maximum gust;
  - takes the header from the first leg's first hour, with its feels-like
    temperature, labelled with that leg and time rather than as the current
    temperature.

  The model supplies only the labels and the one-line `summary`, so the
  numbers in the picture are never model-written. The tool is one broker
  call, whatever the number of legs, and it replaces the per-leg
  `weather_forecast` calls. This leaves the broker's 8-call budget for
  `ns_trips` and `ns_disruptions`. Places are cached within the call, so each
  place is geocoded once. The tool has `Definition.Timeout` = 45 s and
  `RiskRead`, so scheduled turns are offered it; staging a file for the
  user's own chat is accepted as a read-class side effect.
- **A per-turn outbox, not an immediate upload.** The tool only stages the
  PNG; it does not send it:
  - **Turn key.** The conversation sets the turn key from the request ID in
    `runTurn`. The schedule has no run ID on that path today, so `RunID`
    joins `Occurrence` and is passed through `modelTurn.Run` into
    `RunScheduled`. Both set it with `tools.WithTurn(ctx, key)`. The broker
    reads it in `toolsFor`, keeps it on the turn executor, and re-attaches it
    to every call's context. Delivery of the key therefore never depends on
    how the runtime builds its contexts, and `tools.TurnFrom` is what the
    tool reads.
  - **Staging.** The tool writes
    `<data>/outbox/<profile-dir>/<kind>/<id>.png`, where the directory name
    comes from `agentruntime.ProfileDirectoryName` as for attachments. The
    turn key is `chat/<request-id>` or `schedule/<run-id>`, so `/new`
    discards only the chat's cards and a scheduled run's card still goes out
    with its report. Directories are 0700 and files 0600. A later card in the
    same turn replaces the earlier one.
  - **Attaching to the reply.** Delivery attaches the card only when the
    turn succeeded (empty `error_kind`). An interrupted, timed-out, or failed
    turn deletes the card and delivers its failure text as today. When the
    card is attached, delivery sends `sendPhoto`, quoting the request in the
    conversation path and quoting nothing in the scheduled path. The first
    reply chunk becomes the caption when it fits. Otherwise the photo has no
    caption and the chunks follow as today.
  - **Caption fit** is measured on the entity-parsed text in UTF-16 code
    units, not on the HTML, and includes the scheduled prefix. A
    `can't parse entities` error re-sends the caption as plain text, like
    `sendMessage` does today.
  - **No duplicate photo on retry.** After `sendPhoto` succeeds, the file is
    renamed to `<id>.sent` with a byte saying whether the caption consumed
    chunk 1. A retry after a later chunk fails skips the photo and, where it
    applies, that chunk. A timeout after Telegram has already accepted the
    upload can still duplicate the photo, exactly as text can today.
  - **Cleanup.** The files are removed when the turn is marked delivered,
    cancelled, or skipped, and on `/new`. `/new` discards every pending chat
    card, including one whose reply is saved but not yet delivered, for
    example during a Telegram outage; that reply then goes out as text only,
    which is accepted. A sweep removes anything older than
    a day. It runs at startup from `conversation.Service.Start` beside
    `attachment.Purge`, and hourly on the schedule's prune tick. That also
    catches cards from turns cancelled after staging.

  Because the file sits beside the durable delivery row, a restart between
  the turn and delivery still sends the card. A call without a turn key,
  which should not happen, gets a refusal from the tool instead of a stray
  upload.
- **`sendPhoto` is a new client method** with multipart upload like
  `SendDocument`. On `can't parse entities` it keeps the existing HTML
  fallback, re-sending the caption as plain text.
- **Quotes become expandable blockquotes.** In `renderMarkup`, consecutive
  lines starting with `> ` become one `<blockquote expandable>`, with the
  same inline conversion inside. Lines inside fenced code stay code. Telegram
  collapses only long quotes, and then to about three lines, so the guidance
  puts the status first and in bold. Telegram also colours every quote in the
  bot's accent colour, so the green and blue pills from the mockup are
  approximated with a leading emoji (🚆 / 🏠, ✅ / ⚠️) rather than colour.
- **Guidance** tells the model:
  - to use `weather_card` for a day or trip plan with two or more places or
    times, and in scheduled summaries, at most once per turn, instead of
    per-leg `weather_forecast` calls;
  - to keep the reply to about 800 characters when it attaches a card, so it
    fits as a caption;
  - to put each train direction in a quote section whose first line is the
    status;
  - never to repeat, in the caption, the per-leg numbers the card already
    shows.
- **No new switch.** The card sends nothing new outside the house: the same
  place names go to the same Open-Meteo.

## Approach

1. Renderer:
   - `internal/daycard` with the embedded font and icons;
   - a typed `Card` model;
   - `Render(Card) ([]byte, error)`;
   - a test that writes sample cards to `$DAYCARD_SAMPLES` when it is set,
     for the operator's visual review, in Russian and English, with 1 and 4
     legs, and with long names.
2. Outbox and photo delivery:
   - the turn key in context;
   - `internal/outbox`, or a subpackage of `delivery`, with a stage, take,
     and remove API rooted in the data directory;
   - `SendPhoto` in the Telegram client;
   - the photo-plus-caption path in both the conversation and the schedule
     delivery;
   - cleanup and the startup sweep.
3. Blockquote support in `renderMarkup`.
4. `weather_card` in `internal/connectors/web`: leg aggregation over the
   existing hourly forecast, rendering, staging, and guidance.
5. Docs:
   - `docs/04`;
   - `docs/05`, since the outbox is new data at rest, per principal, deleted
     on delivery;
   - a runbook note on the outbox directory;
   - `third_party` licences;
   - this plan's results.

## Files affected

- new `internal/daycard/`, with its assets under `internal/daycard/assets/`
  and their licences under `third_party/`
- new outbox package; `internal/tools/` (turn context)
- `internal/telegram/client.go` (`SendPhoto`), `internal/telegram/markup.go`
- `internal/conversation/service.go`, `internal/conversation/scheduled.go`,
  `internal/schedule/service.go`, `internal/schedule/modelturn.go`
- `internal/connectors/web/` (new `card.go`, tools, guidance)
- `internal/app/app.go`, `go.mod`, `go.sum` (`golang.org/x/image`)
- `docs/04-integrations.md`, `docs/05-security-and-privacy.md`,
  `docs/13-operator-runbook.md`, `docs/plans/README.md`

## Validation

- `gofmt -l`, `go mod tidy -diff`, `go vet ./...`,
  `go build -buildvcs=false ./...`, `go test -race ./...`,
  `scripts/check-plans`.
- Renderer: every sample decodes as a 1,080 px wide PNG; long labels are
  truncated; every WMO code maps to an icon. The operator reviews the sample
  PNGs before the tool is built.
- Against the fake Telegram and the Open-Meteo simulator:
  - a turn calling `weather_card` delivers one `sendPhoto` with the reply as
    an HTML caption;
  - a long reply delivers the photo and then the text chunks;
  - a turn without the tool delivers exactly as today;
  - a scheduled run delivers the card with the "Scheduled:" caption;
  - a failed photo upload is retried with the card still staged;
  - a failed text chunk after a successful photo is retried without a second
    photo;
  - an interrupted, timed-out, or failed turn delivers its failure text
    without the card, and the card is removed;
  - a caption measured by entity-parsed length, including the scheduled
    prefix;
  - delivery removes the file, and the sweep removes an orphan;
  - a second principal's turn never sees the first principal's card;
  - Web access off removes the tool.
- Markup:
  - a single-line quote and a multi-line quote;
  - inline markup and code inside a quote;
  - `> ` inside fenced code;
  - a quote at the very start or end of the text;
  - CRLF line endings;
  - a `>` that is not at a line start.
- Live, from the operator's chat, after deploying: the morning scheduled
  prompt, rewritten to use the card and the NS tools from
  [0027](0027-ns-train-information.md), arrives as one photo message
  resembling the mockup, on both the phone and the desktop client.

## Revision after the first live card

2026-09-29, the first scheduled card on the operator's phone:

- **Text too small to read.** The phone showed the 1,280 px image at about
  45%, so 28 px text became unreadable.
- **Text cut off.** The summary and the leg labels
  ("Amersfoort → Utrecht…") were cut.
- **Train status not visible at a glance.** The status sat as prose in the
  caption, although the mockup's coloured status pills were the point.

Changes:

- **Readable at phone size.** The card becomes 1,080 px wide with a height
  that follows the content, and no text is smaller than 36 px.
  - The summary spans the full width under the header on up to two lines.
  - Each tile's label takes two lines: the place, then the hours.
  - The header label and the feels-like text grow to match.
- **Train status on the card, computed from NS.** Telegram colours every
  quote in the bot's accent colour, so colour can only live in the image.
  - `weather_card` takes an optional `trains` list of up to three
    directions. Each has a `label`, `from`, `to`, `date_time`, and
    `arrive_by`.
  - For each direction the NS connector plans the trip and picks the one the
    user would take: the first trip departing at or after the requested time,
    or, with `arrive_by`, the last one arriving by it.
  - It classifies that trip:
    - **red:** cancelled or part-cancelled, alternative transport, not
      reachable, no trip found, or 15 or more minutes late;
    - **yellow:** 3–14 minutes late, a journey 5 or more minutes longer than
      planned, or a trip status other than NORMAL;
    - **green:** otherwise.
  - The card draws one row per direction:
    - a coloured status pill with "on time", "+12 min", or "cancelled" in the
      card's language;
    - the label;
    - the departure → arrival, live where NS has live times and planned
      otherwise (a cancelled train shows its plan), since the pill already
      says how late it is.
  - The status text and times come from the connector, never from the model.
  - The web connector reaches this through a narrow `Trains` interface that
    `app.go` fills with the NS service. The service applies the principal's
    own NS switch; the list is refused, not ignored, when NS is off for them
    or not configured.
  - The tool's result lists each row's status and reason, so the caption can
    explain it. The guidance heads each caption quote with the matching
    🟢/🟡/🔴 emoji.

## Risks and open questions

- The Telegram caption limit of 1,024 characters is tight. When the model
  overruns it, the message splits into a photo and then text, which is
  acceptable but less pretty.
- The rendering quality of a hand-drawn Go layout depends on the icon set;
  choosing it is part of step 1 and needs the operator's eye.
- Font and icon assets add about 1 MB to the binary.
- The dominant-icon rule per leg (most severe condition, or most frequent) is
  a judgement call to settle on the samples.
- Train status rows depend on the NS connector; without it the card still
  draws the weather, and a `trains` list is refused rather than invented.