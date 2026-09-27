# 0026 — Web search, page reading, weather, and PDF text

Status: `done`

## Context

The model can reach only the user's own connected services: Notion, NAS files,
Gmail, and attachments. It cannot answer anything that needs the current web —
opening hours, prices, news, the weather, a recipe behind a link — and says so
or guesses. It also cannot read PDFs: `read_attachment` and the NAS read tool
refuse every non-text type, and plan
[0020](0020-telegram-attachments.md) deferred PDF text as needing its own
dependency decision.

Every tool today is a Go provider behind the core broker in `internal/tools`,
bridged into Pi by the application's own extension. Pi runs with
`--no-extensions`, `--no-builtin-tools` when the turn has tools, and
`--no-tools` when it has none; the adapter aborts a tool-less turn on any tool
event.

Two third-party Pi packages were reviewed on 2026-09-27:

- `pi-web-search` 1.6.0 (MIT, about 1,500 lines of TypeScript in `src/`)
  registers `web_search` and a Gemini-only `url_context`. For an
  `openai-codex-responses` model it calls the Codex Responses endpoint with the
  provider's native `web_search` tool, authenticating through
  `ctx.modelRegistry.getApiKeyAndHeaders()`, so a search runs under the current
  principal's own ChatGPT subscription from their own agent directory. It needs
  no API key and no configuration. It was built against Pi 0.80.3; the image
  pins 0.84.1.
- `pi-web-access` 0.32.0 (MIT, about 30,000 lines) adds search over roughly
  thirty providers, page fetching, GitHub cloning, PDF extraction, and video
  analysis. By default it sends queries to Exa's hosted MCP endpoint, it reads
  local video files by path, spawns `git`, `ffmpeg`, and `yt-dlp`, can read
  Chrome cookies, and can start a local curator web server. Inside the shared
  container every principal's data is readable by the same UID, so a path
  argument is a cross-principal read. Rejected.

Open-Meteo offers forecast and geocoding APIs without an account or key for
non-commercial use: `geocoding-api.open-meteo.com/v1/search` and
`api.open-meteo.com/v1/forecast`, both verified on 2026-09-27.

## Goal

Where the operator has enabled web access, every user whose web access is on —
the default — can ask about anything current and get an answer grounded in
search results, hand the assistant a link and have it read the page, and ask
about the weather anywhere, including hour by hour along a day's travel plan.
Any user can send a PDF, or point at one on the NAS or the web, and have its
text read. A user who turns web access off in `/settings`, or any user while
the operator has not enabled it, gets exactly today's behaviour apart from
PDFs. No request can reach the home network.

## Decisions

- **Search through `pi-web-search`, vendored.** The package source is copied
  into `third_party/pi-web-search/` at 1.6.0 with its licence, reviewed, and
  copied into the image, not installed from npm at build time, so the code that
  runs is the code in git. Only `web_search` is used; `url_context` removes
  itself on a non-Gemini model. Queries leave the house only to the provider
  the conversation already goes to, under the principal's own subscription.
- **Page reading and weather as core Go tools, not plugins.** A new
  `internal/connectors/web` provider contributes `fetch_page` and
  `weather_forecast` through the broker, so they get the broker's principal
  binding, grant recheck, deadline, size limit, untrusted-data prefix, and
  audit log. HTML reduction uses `golang.org/x/net/html`, already in the module
  graph, promoted to a direct dependency.
- **One per-user switch, "Web access", on by default where the operator
  enabled it globally.** It covers search, page reading, and weather —
  everything that sends something the user said to a service outside the house
  other than the model provider's conversation. It lives in `/settings` next to
  the other integrations. A global `HOME_AGENT_WEB_ENABLED` flag, default
  false, is the operator's opt-in: while it is off the section is hidden and
  nobody has web access; while it is on, every user has web access until they
  turn it off. The operator chose on-by-default after the first build, which
  had each user opt in. The state is an opt-out marker: an `integration` row
  for `web` (workspace "Web access off", `TokenKindInternal`, no credential)
  saved when the user turns web access off and deleted when they turn it back
  on, so no row means on and no migration is needed.
- **The search extension loads only for principals with the switch on.** Such
  a turn always starts Pi with `--no-builtin-tools` and
  `--extension <vendored index.ts>` in addition to the bridge, even when the
  broker contributes nothing; `--no-extensions` stays. The runtime contract
  gets a `WebSearch bool` on the run request so the adapter keeps Pi-specific
  paths to itself, and the adapter allows tool events whenever either the
  bridge or the search extension is present.
- **`web_search` is outside the broker, and that is accepted.** It runs inside
  Pi and never reaches the executor, so it has no untrusted prefix, broker
  audit line, or broker call budget. The guidance for the turn states that
  search results are data, the conversation's event sink logs each
  `web_search` call as `conversation.web_search.called` by name without
  content, and the provider
  bounds the call itself. Wrapping it in a broker tool would mean
  reimplementing its Codex request in Go; not worth it for a read that goes
  only to the conversation's own provider.
- **Outbound requests refuse the home network.** One guarded HTTP client
  serves `fetch_page` and the weather calls. Only `http` and `https` on ports
  80 and 443. The dialer resolves the host itself and refuses loopback,
  private, link-local, CGNAT, multicast, unspecified, and IPv6 ULA and
  site-local addresses on every connection, including each redirect hop, so
  DNS rebinding and a redirect to `nas.local` both fail. At most 5 redirects,
  no cookies, no credentials in URLs, a fixed user agent, 15 seconds.
- **`fetch_page` returns readable text in windows.** HTML is reduced to its
  title and visible text, dropping scripts, styles, navigation, and forms, with
  links kept as `text (url)`; `text/plain`, Markdown, and JSON pass through;
  PDFs go through the PDF extractor below. Results come in 12 KB windows with
  an `offset` to continue, like `gmail_read_message`; each window re-fetches,
  since nothing from the web is stored. Downloads are capped at 2 MiB for text
  and 10 MiB for PDFs, matching the attachment file cap. Images and other types
  report that they cannot be read.
- **`weather_forecast` takes a place name.** It geocodes the name with
  Open-Meteo, takes the best match, and returns the resolved place, current
  conditions, and a daily summary for up to 7 days — temperature range,
  precipitation probability and amount, wind, and a plain weather description
  from the WMO code — in metric units and the place's own timezone. With an
  optional `date` (today to 15 days ahead in the place's calendar) and
  optional inclusive `from_hour`/`to_hour` (0–23 local, default the whole
  day), it instead returns one line per hour — description, temperature and
  feels-like, precipitation probability and amount, wind and gusts — so a day's
  travel plan ("Amersfoort this morning, Utrecht 11–16, Rotterdam in the
  evening") gets the weather for each place at the hours the user is there.
  The guidance asks for one call per place, within the broker's 8 calls per
  turn, an answer leg by leg, and remembering the itinerary for the rest of
  the conversation. An
  optional `HOME_AGENT_WEATHER_HOME` names the household's place; the guidance
  tells the model to use it when the user names none, and to ask when it is
  unset. No coordinates or home address are stored anywhere else.
- **PDF text through `github.com/ledongthuc/pdf`, one shared extractor.** Pure
  Go, so a malformed file cannot corrupt memory, and nothing new in the image.
  Chosen by the operator over poppler's `pdftotext` (better extraction, C code
  in a subprocess) and `pdfcpu` (manipulation-oriented, weak text output),
  accepting weaker results on multi-column and unusually encoded PDFs. A new
  `internal/pdftext` package wraps it: it recovers from library panics, stops
  after 100 pages or 512 KB of text with a closing line saying where it
  stopped, runs under the caller's deadline, and refuses files that need a
  password or use encryption the library cannot handle. A file protected only
  by an owner password opens without one, so it is read like any other. It is pinned to the pseudo-version
  `v0.0.0-20260907135840-6c8c28e0e8a0`, since the module has no releases.
- **PDFs read in all three places.** `read_attachment` and the NAS read tool
  gain `application/pdf` (or a `.pdf` name) as readable, using the extractor
  with their existing windows and offsets; `fetch_page` uses it for PDF
  responses. An attachment's extracted text is cached beside its bytes as
  `<id>.txt` with the same 0600 mode, removed with the attachment on `/new` and
  by the startup sweep, so reading further windows does not re-parse. A PDF
  that yields no text says it is probably scanned and cannot be read. PDF
  reading is not behind the web switch, since attachments and NAS files never
  leave the house.
- **Scheduled turns may use all of them.** Every tool here is a read, so a
  scheduled prompt such as "morning news and weather" works through
  `ReadOnlyToolsFor`; the extension flag follows the same switch.
- **Privacy text changes.** `/privacy` gains a line, shown while the switch
  is on (by default, wherever the operator enabled web access), saying that web searches go to the configured model provider,
  pages are fetched from the home connection, and place names are sent to
  Open-Meteo for weather.

## Approach

1. Spike: load the vendored extension into Pi 0.84.1 in RPC mode with
   `--no-builtin-tools --extension` and a real per-principal agent directory
   logged in with `openai-codex`; confirm `web_search` registers, its imports
   of `@earendil-works/pi-ai/compat` resolve through the loader aliases, and a
   live search returns cited results. Record the result here before building
   the search part; a failure reopens the search decision without blocking the
   rest.
2. `internal/pdftext` with the pinned library, and PDF support in
   `read_attachment` (with the text cache and cleanup) and the NAS read tool.
3. Web connector: `internal/connectors/web` with the guarded HTTP client, the
   HTML reducer, `fetch_page`, `weather_forecast`, their guidance,
   `Status`/`SetEnabled` over the integration repository, and module
   lifecycle.
4. Vendor the search package under `third_party/pi-web-search/`, copy it into
   the image beside Pi in the `Dockerfile`, and add a config value for its path
   with the local `.env` pointing at the checkout.
5. Runtime: `RunRequest.WebSearch`, argument composition in the Pi adapter,
   tool events allowed when either source of tools is present, and a
   `fixture` scenario that emits a `web_search` tool event without a bridge
   call.
6. Wiring and Telegram: config flags, broker registration, coordinator and
   scheduler setting `WebSearch` from the switch, a `/settings` section with
   Turn on / Turn off, and the conditional privacy line.
7. Docs: `docs/02`, `docs/04`, `docs/05` (third-party code inside Pi, the
   outbound boundary, and PDF parsing of untrusted files), the runbook,
   `deploy/nas.env.example`, and this plan's results.

## Spike results

2026-09-27, Pi 0.84.1 installed from npm, vendored `pi-web-search` 1.6.0
`src/`, started in RPC mode with `--no-builtin-tools --extension <index.ts>
--no-extensions --no-skills --no-prompt-templates --no-themes
--no-context-files --no-approve` and model `openai-codex/gpt-5.6-luna:low`:

- The explicit `--extension` loaded despite `--no-extensions`, and the package's
  imports of Pi and `pi-ai/compat` resolved through the loader aliases.
- The active tool set was exactly `["web_search"]`: `url_context` removed
  itself on the Codex model and the built-in tools stayed inactive.
- With the operator's own Codex login, one prompt asking two current questions
  made four `web_search` calls in two parallel pairs, all successful, with
  results between 1 and 11 KB, and returned a cited answer in 24 seconds.
- The model searched twice per question on its own. Since these calls bypass
  the broker's per-turn budget, the guidance asks for as few searches as the
  question needs; a hard cap would need a hook in the extension and is not
  added now.

## Implementation results

2026-09-27, uncommitted work verified with `gofmt -l`, `go mod tidy -diff`,
`go vet ./...`, `go build -buildvcs=false ./...`, `go test -race ./...`,
`scripts/check-plans`, and `git diff --check`, all passing.

- `internal/pdftext` extracts page by page with `Page.GetPlainText`, which kept
  reading order where the library's row-based API scrambled real documents.
  Attachment text is cached as `<id>.txt`, removed by `Discard` (which `/new`
  uses) and by the startup sweep. NAS PDFs up to 10 MiB are read uncached.
- `internal/connectors/web` refuses a host when any resolved address is not
  public, dials only the vetted address, and repeats both checks on every
  redirect. Tests route fake public names to local servers through an injected
  resolver and dialer, so the production address check runs unchanged in every
  case. Both tools have a 35-second deadline to cover two sequential weather
  requests or a PDF download and extraction.
- The vendored extension matches the npm tarball's integrity
  (`sha512-UwUDER9J…NuWg==`, recorded in `third_party/pi-web-search/VENDORED.md`).
  The package ships no licence file; the MIT text declared in its
  `package.json` is reconstructed there.
- The Pi runtime receives the extension path only while
  `HOME_AGENT_WEB_ENABLED` is true, so a local run without the checkout path
  still starts. A turn asking for web search without a configured extension
  fails instead of silently dropping search.
- Web access became on by default after the first build, at the operator's
  request: `Status` reads a missing `web` row as on, turning off saves the
  opt-out row (workspace "Web access off") and turning on deletes it, with the
  same `web.access.enabled`/`web.access.disabled` log events; the `Status`,
  `SetEnabled`, and `WebSearchEnabled` signatures and all wiring are unchanged.
- `weather_forecast` gained the hourly day view. The date range is checked
  against today in the time zone the geocoder reports for the place, falling
  back to UTC, before the forecast request; a whole day is 24 lines, well
  under the broker's 16 KiB result limit.
- Deployed 2026-09-27 as `2026.09.27-ef7a777` (digest
  `sha256:8629fc82c6ba20c414087081178a0eda2dccf9265173dedb64492f89b5c80c54`)
  with `HOME_AGENT_WEB_ENABLED=true` and `HOME_AGENT_WEATHER_HOME=Amersfoort`;
  startup succeeded with the vendored extension present in the image.
- Live, from the operator's chat, confirmed by the operator and the
  application logs within the first hour: a `web_search` call logged as
  `conversation.web_search.called`; `weather_forecast` answered six times,
  three of them refused inputs the model then corrected, with no
  `web.weather.failed` lines, so Open-Meteo itself never failed; a
  `fetch_page` read of a public link succeeded; a scheduled job was proposed,
  created, and ran.
- Not exercised live: a PDF sent in the chat or read from the NAS, and a
  refused private address. Both are covered by the integration tests over the
  real guard and the real extractor; a live failure there belongs in a new
  plan.

## Files affected

- new `third_party/pi-web-search/`, `Dockerfile`
- new `internal/connectors/web/`, new `internal/pdftext/`
- `internal/attachment/`, `internal/connectors/nas/tools.go`
- `internal/config/` (web flag, weather home, extension path),
  `internal/app/app.go`
- `internal/runtime/runtime.go`, `internal/runtime/pi/runtime.go`,
  `internal/testutil/pifixture/fixture.go`
- `internal/conversation/`, `internal/schedule/` (setting `WebSearch`)
- `internal/telegram/settings.go`, `commands.go`, and a new
  `internal/telegram/web_integration_test.go`
- `go.mod`, `go.sum` (`github.com/ledongthuc/pdf` added,
  `golang.org/x/net` becomes direct)
- `docs/02-scope-and-capabilities.md`, `docs/04-integrations.md`,
  `docs/05-security-and-privacy.md`, `docs/13-operator-runbook.md`,
  `deploy/nas.env.example`, `docs/plans/README.md`

## Validation

- `gofmt -l`, `go mod tidy -diff`, `go test -race ./...`, `go vet ./...`,
  `go build -buildvcs=false ./...`, `scripts/check-plans`.
- The guarded client against local test servers reached through an injected
  resolver: refusal of `127.0.0.1`, `10.0.0.10`, `169.254.169.254`, `[::1]`,
  a public name that resolves to a private address, and a public URL that
  redirects to a private one.
- `fetch_page`: HTML reduced to text, windows and offsets, the download caps,
  a PDF response read as text, and an image refused.
- `weather_forecast` against an Open-Meteo simulator at the HTTP boundary: a
  place resolved and summarised, an unknown place reported, the configured
  home used when set, and the model told to ask when it is not; an hourly
  request carrying the hourly fields and the date as start and end, rendered
  for the whole day and for a `from_hour`/`to_hour` window, and a malformed
  date, a date out of range, reversed hours, and hours without a date each
  refused with a clear sentence.
- PDF text: small generated PDFs committed as test fixtures — a text PDF read
  in windows, an attachment's cached text reused and removed on `/new`, a NAS
  PDF read, an image-only PDF reported as scanned, an encrypted PDF refused, a
  truncated file failing cleanly without a panic.
- Telegram integration: the switch is hidden and no turn asks for search while
  the global flag is off; with the flag on it is on by default, so the first
  turn has the web tools, starts Pi with the extension, and `/privacy` shows
  the web line; turning it off mid-conversation fails the next web call closed,
  drops the extension on the next turn (`--no-tools`), and drops the privacy
  line; a second principal stays on and is unaffected; turning it back on
  restores the tools; a turn with the switch on and no other tools still
  starts with `--no-builtin-tools` and not `--no-tools`.
- Live, from the operator's chat after deployment: a current-events question
  answered with sources, a link summarised, a link to a `.home` name or the NAS
  address refused, today's weather at home and in another city, the weather
  along a day's itinerary of several places answered leg by leg, a real PDF
  sent in the chat and one read from the NAS, and a scheduled prompt that
  searches and reports the weather.

## Risks and open questions

- `pi-web-search` depends on Pi internals (`modelRegistry`, Codex request
  shape) that can change between Pi releases. Every Pi upgrade must re-run the
  live search check, as it already must for the device-code login.
- Search uses the principal's Codex quota. A heavy user could hit limits
  sooner; the existing quota reply covers it.
- The vendored copy needs a manual refresh for upstream fixes; the review is
  the point, so this is accepted.
- `ledongthuc/pdf` has no releases and extracts multi-column layouts and some
  font encodings poorly. The extractor sits behind one package, so swapping in
  `pdftotext` later touches only `internal/pdftext`.
- The library rejects `%PDF-2.0` files, which are then reported as damaged.
- A pathological PDF could keep the library busy past the deadline; Go cannot
  stop that goroutine, only stop waiting for it. The page and text caps limit
  the work, and the turn still ends on time.
- Open-Meteo's free tier is for non-commercial use and rate-limited per IP;
  household use is far below it, but an outage makes weather unavailable
  rather than degrading to search.
- Pages behind JavaScript rendering return little text. A headless browser is
  out of scope.
- YouTube transcripts would fit `fetch_page` later; a separate plan.
