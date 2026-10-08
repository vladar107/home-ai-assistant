# 0059 — Check each search instead of locking the conversation

Status: `done`

## Context

Since plan 0051, once a conversation has read text from outside the
household, web search is off until `/new`. The reason is in
`searchWithheld` (`internal/conversation/service.go`): the provider's search
(the model provider's own web search, run by the vendored Pi extension in
`third_party/pi-web-search`) can open an address the query names. An
injected instruction could therefore send data out inside a query, and a
search cannot be stopped once it starts.

In practice the lock fires on almost every useful turn. Calendar, Gmail,
Grafana, fetched pages, and earlier conversations all count as outside
content (`ThirdParty` without `Household`). A question such as "what should I
know about today?" reads the calendar. The next question, "what happened in
the news?", is then refused, and `/new` only helps until the next calendar
read. The user called this very annoying. Plan 0056 kept the lock and only
made the model explain it.

The risk is not "the conversation has read a calendar". It is "this query
carries data to an address, or was written by injected text". Both can be
checked per query, because every search passes through our extension
before it reaches the provider.

## Goal

- Web search stays available after outside content. Each query is checked
  before it runs.
- A query that could carry data out (an address, a host, an email address, a
  path, a long number or token, an encoded blob) never runs without the
  user's confirmation after outside content.
- When content screening has flagged an injection attempt in the
  conversation, searches need confirmation, as the lock does today.
- Nothing changes for conversations that have read no outside content.

## Decisions

- **The extension asks the application before every search.** The Pi
  extension posts the query to the existing tool bridge, with its per-turn
  token. The bridge answers `allow`, `confirm`, or `deny`, and the extension
  runs the provider search only on `allow`. A conversation without outside
  content always gets `allow`, with no further checks. If the bridge is
  unreachable, the search is denied (fail closed), because the bridge is
  local.
- **Deterministic check first.** After outside content, the query is run
  through the same detectors the TypeSafe redactor uses (`internal/judgment`):
  URLs, hosts and `site:` operators, email addresses, phone numbers, paths,
  IBAN and card numbers, IP addresses, tokens and keys, and long hex or
  base64 strings. Any hit means `confirm`. These detectors need no TypeSafe
  key, so the check works for every user.
- **Jev adds two checks for users who opted in to safety checks** (plan
  0029):
  - The existing screening of fetched pages and mail marks the principal
    suspicious. While a principal is suspicious, every search is `confirm`.
  - A Noul over the redacted query and the user's last messages asks
    whether this search serves what the user asked for. Below the
    threshold, the result is `confirm`. A TypeSafe failure fails open to
    the deterministic result, because that check alone already blocks the
    exfiltration channel.
- **`confirm` is an approval, not a refusal.** The search is proposed like
  any confirmed action: "Search the web for: …", with the reason. The
  model gets a result saying it is waiting for the user's confirmation.
  After Confirm, the plan 0054 continuation runs the search and goes on.
  The query is shown exactly, so the user sees what would leave.
- **Fetching stays as it is.** `fetch_page` keeps its rule that after
  outside content it opens only links the user sent or that appeared in
  what the conversation read.
- **Scheduled runs and tasks** use the same check. A scheduled run has no
  one to confirm, so `confirm` becomes `deny` there, with a clear result
  text.
- **Metrics:** search checks by decision (`allow`, `confirm`, `deny`) and
  by reason (`clean`, `pattern`, `suspicious`, `off_request`, `no_bridge`).

- **As built.**
  - The check has its own address, `HOME_AGENT_SEARCH_CHECK_URL`, set per
    turn beside the bridge token, because the tool address ends in `/call`.
  - The extension waits 5 s, and the Jev call is capped at 3 s inside that.
  - A turn without a check gets `deny`, because the runtime cannot see
    exposure.
  - A confirmed search is held in memory per principal: up to 16 entries for
    30 minutes, each used once, matched on the trimmed, whitespace-collapsed,
    lower-cased query. A restart between Confirm and the continuation
    therefore makes the model propose again.
  - The proposal shows the query as code, so a link in it cannot hide the
    address.
  - A search's results mark the conversation as outside content when they
    arrive, not when the call starts, so a call made alongside it is not
    judged by results the model has not seen yet.
  - The metric reasons are `clean`, `confirmed`, `suspicious`, `pattern`,
    `off_request`, `checked`, and `unrecorded`. An unreachable bridge is
    seen only by the extension, so no `no_bridge` reason is recorded.
  - Known false positives come from the redactor's own rules, for example
    "password manager" and "node.js".

## Approach

1. Bridge: a search-check endpoint on the Pi tool bridge, bound to the turn
   like tool calls. A policy in Go that combines exposure, the deterministic
   detectors, and the optional Jev checks.
2. Extension: call the check before the provider search, and honor `allow`,
   `confirm`, and `deny`. Keep the vendored change small and note it in the
   vendoring notes.
3. Approval: a `web_search` proposal type whose confirmed execution runs
   the search through the continuation turn.
4. Conversation: remove `searchWithheld` and the notice. The 0056
   `app_help` entry describes the new behavior.
5. Metrics, and README, `docs/architecture.md`, and `docs/operations.md`
   (security model: what is checked and why).

## Files affected

- `internal/runtime/pi` (bridge endpoint and extension flag)
- `third_party/pi-web-search/src` (pre-search check)
- `internal/judgment` (export the detectors, plus a search-intent question)
- `internal/approval`, `internal/conversation/service.go`,
  `internal/exposure`
- `internal/observability`, README, `docs/architecture.md`,
  `docs/operations.md`

## Validation

- Integration tests with the Pi fixture and a fake TypeSafe server:
  - an unexposed conversation searches freely;
  - after a calendar read, a plain news query runs;
  - after a calendar read, queries with a URL, a host, a `site:` operator,
    an email address, or a long token become proposals;
  - a suspicious principal's searches all become proposals;
  - the Jev off-request check, and a failure that falls back to the
    deterministic result;
  - a confirmed search runs and the continuation answers;
  - a scheduled run gets `deny` instead of `confirm`;
  - an unreachable bridge denies.
- `go test -race ./...`, `go vet ./...`, formatting, build, and
  `scripts/check-plans`.
- Live check: the "today" question followed by a news question, in one
  conversation.

## Risks and open questions

- Plain words can still describe private facts ("salary of …"). Without an
  address in the query, the provider has nowhere to send them except its
  own search backend. That is the same exposure as any search the user asks
  for, and it is accepted.
- The provider's search may browse result pages on its own. Those pages are
  chosen by the search engine, not by the query, and the check does not
  cover them. The provider adapters in the extension should be checked for
  options that disable fetching addresses named in the query.
- The vendored extension diverges further from upstream. Keep the hook to
  a single call site.
