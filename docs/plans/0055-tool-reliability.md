# 0055 — Tool reliability: fewer wasted calls, retries, and clean scheduled replies

Status: `done`

## Context

An audit of a month of the operator's own conversations (the other user's
were not read) found that many failed turns came from tool mechanics, not
from the model's reasoning:

- **Geocoding rejects "Town, Country".** The `location` description in
  `internal/connectors/web/tools.go` suggests "Zwolle, Netherlands", but
  `geocode` in `internal/connectors/web/weather.go` sends the whole string as
  Open-Meteo's `name`, with `count=1`. Open-Meteo matches the place name only,
  so the suggested form finds nothing. This was the most common tool error
  (about 45 failures in a month, still happening). Each failure costs a retry,
  and in scheduled runs it often used up the call budget, so whole reports
  said "weather unavailable". `weather_card` (`card.go`) shares the bug.
- **Failed calls spend the budget.** A turn may make 8 tool calls
  (`maximumCallsPerTurn` in `internal/tools/broker.go`). Not-found errors,
  retried lookups, and transient failures count the same as useful calls.
  The budget ran out 22 times. The effects: partial Grafana answers, morning
  reports without trains, and schedule edits whose proposals were refused
  halfway.
- **Transient upstream failures are not retried.** "NS travel information is
  unavailable right now" and "The weather service is unavailable" ended the
  check. The user had to type "try again", which then worked. No external
  client retries: each connector builds its own `http.Client`, and the only
  retry-like code is the back-off of cached feeds (NS stations, bus alerts).
  Some services fail on the first attempt and succeed right after.
- **Bundles fill after two schedule items.** `fits` in
  `internal/approval/approval.go` counts summary text against
  `bundleSummaryUnits` (2500). A schedule summary contains the whole prompt
  (up to 1000 characters), so a third schedule change is refused with
  `ErrBundleFull` well before `maximumBundleItems` (10). The model then
  spends turns asking the user to confirm part of a batch.
- **Scheduled replies include the model's working notes.** `appendText` in
  `internal/runtime/pi/runtime.go` joins the text of every assistant message
  in a turn. When the model writes a note before a tool call, that note is
  delivered as part of a scheduled report. In one case it was planning text
  in another language above the report itself.
- **Silent provider stalls.** Four chat turns produced no model output at all
  and ended at the 120 s turn timeout. The identical message, sent again,
  was answered in about 25 s.
- **Limits the model only learns by failing.** The 1000-character prompt
  limit is in the description, not in the schema, so over-long prompts are
  rejected after being written. The `weather_card` label and Notion row
  limits are already in their schemas, and Pi rejects violations before
  they reach the application.

## Goal

- "Town, Country", "Town, Region", and plain town names all find the place.
- A call that fails because of the caller's input, or a transient upstream
  failure, does not spend the turn's call budget. A runaway loop is still
  bounded.
- Every external service call retries transient failures with exponential
  backoff and jitter, spending at most 5 s on retries, before it reports a
  failure. A write is never repeated when it might already have taken effect.
- A bundle holds up to ten schedule changes. The user still sees each one in
  full.
- Scheduled runs and background tasks deliver only the model's final message.
- A stalled model call is detected early and retried once, when that is safe.
- Length and count limits are part of the tool schemas.

## Decisions

- **Geocoding** splits the query on the first comma. The town part is the
  `name`, and `count` rises to 10. Any remainder is matched case-insensitively
  against each result's `country`, `country_code`, and `admin1`. The first
  match wins, and the first result is used when nothing matches. A name with
  no results is retried once with the text before any parenthesis or
  hyphenated suffix. The description keeps "Town, Country" as a valid form.
- **Budget accounting.** Every budgeted call that ends in an error gives its
  slot back: a tool error, a broker timeout, or a failure. This covers input
  problems (unknown place, unknown station, invalid date) and upstream
  failures that survive the retries alike, without marking each error in
  every connector. A ceiling of twice the budget counts every budgeted call,
  failed ones included, so failures cannot loop forever. Pi's own schema
  validation errors never reach the broker and already cost nothing.
- **One retry policy for external services.** A small shared package
  provides an `http.RoundTripper` that wraps each connector's transport, so
  every client gets the same behavior without changing its call sites.
  - *Budget:* at most 4 attempts, and no new attempt starts more than 5 s
    after the first one began. Retries never extend the caller's deadline:
    the request context, client timeout, and tool deadline still bound the
    whole call, and no attempt starts when too little of it is left.
  - *Backoff:* exponential with full jitter. Before retry *n* the wait is a
    random duration between 0 and `min(2 s, 250 ms × 2ⁿ⁻¹)`. Concurrent
    calls therefore do not retry in step.
  - *Retried:* connection errors (refused, reset, DNS, TLS handshake), an
    attempt timing out while the overall deadline still has room, and
    responses 408, 425, 429, 500, 502, 503, and 504. For a 429 or 503 with
    `Retry-After`, that wait is used when it fits the remaining budget;
    otherwise the response is returned at once. Other statuses are returned
    unchanged.
  - *Idempotency:* GET, HEAD, and OPTIONS are retried. Other methods are
    retried only when the client marks the request as idempotent through
    its context. Notion data source queries, Google list and search calls,
    and TypeSafe judgments are POSTs that only read. An unmarked write is
    retried only after a failure that proves nothing was sent (a refused
    connection or DNS failure), so a Notion update or a Home Assistant
    action never runs twice.
  - *Bodies:* a request body is replayed through `GetBody`. A request
    without one is not retried. Each failed attempt's response body is
    drained and closed.
  - *Scope:* NS, OVapi, Transitous, Open-Meteo, the places lookup, Notion,
    Google (Gmail, Calendar, and the token refresh), Grafana, Home Assistant
    (REST and MCP reads), web search and page fetches (inside the SSRF
    guard's transport, so each retry is checked again), and TypeSafe.
    Telegram is out of scope: polling already repeats, and a resent message
    could arrive twice. The model provider is out of scope too, since Pi
    has its own retries and the stall watchdog below covers stalls.
  - *Visibility:* a counter of retried requests by service and final
    outcome (recovered, exhausted, not retryable). Each retry is logged at
    debug level with the service, attempt, and status. A failure that
    survives the retries is what tools report, and it costs no budget.
- **Bundles span messages.** A bundle whose summaries exceed one Telegram
  message is sent as several messages. Each record keeps the ID of the
  message that shows it, so no migration is needed. A single proposal too
  long for one message is refused instead of cut. The Confirm and Reject buttons are on
  the last one, and each item appears whole in exactly one message. The
  unit budget applies per message, and the item limit of 10 stays. Outcomes
  edit the message with the buttons; the earlier messages keep their text.
- **Final message only for runs without a chat.** `RunResult` gains
  `Final`, the text of the last assistant message. If that message has no
  text, `Final` is empty and the run fails as an empty response does, rather
  than sending an earlier note as the report. Scheduled runs and tasks
  deliver `Final`. Chat turns keep the full response, where short notes
  between tool calls are wanted.
- **Stall watchdog.** If the Pi event stream is silent for 45 s before any
  tool call has started or any reply text has streamed, the turn stops Pi,
  restores the session files to their pre-turn sizes (removing files the
  turn created), and runs again once, so the user's message is not added a
  second time. A silence after a tool call or text is only recorded: a
  second run would repeat work, and the model may still be thinking, so the
  turn waits for its own deadline as before. A second stall, or a failed
  restore, ends with the existing timeout reply. Any Pi event, including its
  own retry events, counts as activity. The threshold is a constant,
  overridable in the runtime's config for tests.
- **Limits in schemas.** The card labels and Notion limits already had
  them; the schedule tools gain `maxLength` on prompts (1000), reminder text
  (500), and names (60), matching their validation.

## Approach

1. Web connector: geocoding with country and region matching. Tests against
   a fake geocoding server cover "Town", "Town, Country", "Town, Region", an
   unknown remainder, and the parenthesis fallback.
2. Tools broker: the refund of failed calls and the hard ceiling.
3. The shared retry transport with its policy, idempotency marker, and
   metric. Install it in each in-scope connector client, inside the web
   connector's SSRF guard, and mark the read-only POSTs as idempotent.
4. Approval and Telegram: split bundle messages; edit the last one on a
   press; keep expiry and restart handling.
5. Pi runtime: track the last assistant message's text as `Final`; scheduled
   and task delivery use it.
6. Pi runtime: the silence watchdog and the single safe retry, with a metric
   for stalls and whether the retry succeeded.
7. Schema limits on the schedule tools.
8. `docs/architecture.md`: budget refunds, split bundles, and the watchdog.

## Files affected

- `internal/connectors/web/weather.go`, `card.go`, `tools.go`
- `internal/tools/broker.go`, `internal/runtime` (`Final`)
- a new shared retry package (for example `internal/httpretry`)
- the client constructors in `internal/connectors/{ns,bus,web,places,notion,
  google,gmail,calendar,grafana,homeassistant}` and `internal/judgment`
- `internal/approval/approval.go`, `internal/telegram/service.go`
- `internal/runtime/pi/runtime.go`, `internal/conversation/scheduled.go`,
  `internal/conversation/task.go`
- `internal/schedule/tools.go`
- `internal/observability` (stall metric), `docs/architecture.md`

## Validation

- Integration tests:
  - Geocoding forms, with a fake geocoding server.
  - Refunded and unrefunded calls, and the hard ceiling.
  - Retry transport against a fake server:
    - a server that fails twice and then answers;
    - the 5 s window and attempt cap, under a fake clock and seeded jitter;
    - `Retry-After` that fits the window, and one that does not;
    - an unmarked POST not retried after a 503, but retried after a refused
      connection;
    - a marked read-only POST retried;
    - context cancellation stopping the wait;
    - bodies replayed.
  - One connector end to end: an NS lookup whose first attempt gets a 503
    returns the answer.
  - A bundle of ten long schedule items sent as several messages, confirmed
    as one.
  - A scheduled run whose model writes a note before a tool call delivers
    only the final message.
  - A fake Pi that goes silent is retried once and then answers; a silent Pi
    after a tool call is not retried.
- `go test -race ./...`, `go vet ./...`, formatting, build, and
  `scripts/check-plans`.
- Live check: a weather question naming "Town, Country"; a scheduled test
  run of a multi-leg report.

## Risks and open questions

- The stall cause is unknown: it could be the provider or Pi's own retries.
  During implementation, check the application logs for the four stalled
  turns. If Pi was retrying internally, the watchdog should treat its retry
  events as activity, and the threshold may need tuning.
- Retries add up to 5 s to a failing call, and a turn that calls several
  failing services in a row can approach the turn timeout. Tool deadlines
  still cap each call. Watch the retry metric and the reply-latency
  histogram after deploy.
- Whether an MCP request to Home Assistant is a read depends on the MCP
  method and tool. Only the read methods and the live-context tool are
  marked idempotent; actions are not.
- Refunding errors makes the budget less predictable. The hard ceiling keeps
  the worst case at sixteen calls.
- Splitting a bundle across messages adds edit cases on expiry and restart.
