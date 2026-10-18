# 0104 — One copy of the helpers that drifted apart

Status: `done`

## Context

A code-quality audit found small helpers copied into many packages whose
copies no longer behave alike:

- Tool argument decoding exists in about twenty copies. Most reject fields
  the struct does not know; the NAS, Notion, image generation, attachment,
  help, and conversation history tools accept them. Some treat empty
  arguments as `{}`, only two ignore surrounding whitespace, and the error
  text differs.
- Text clipping has as many copies. `clip` means a byte cap in the NS, bus,
  and web connectors, but a rune cap with an ellipsis in places and Grafana.
  `truncateRunes` appends "…" in some packages and not in others, and the
  ellipsis is sometimes counted inside the limit and sometimes added beyond
  it. Some callers pass a rune limit to a byte helper.
- Watches and routes each keep their own weekday list, validation, and
  description, and describe Monday to Friday differently ("weekdays" against
  "Mon–Fri").
- `conversation.TripSubject` duplicates `thread.TripDestination` and is
  unused.
- staticcheck and deadcode report unused code, a struct literal that could be
  a conversion, and tests using the deprecated `attribute.Value.Emit`.
- A few doc comments are misplaced or stale.

## Goal

Each helper exists once with one documented behaviour, every caller uses it,
and staticcheck (apart from the intentional capitalised error strings, ST1005)
and deadcode report nothing in production code.

## Decisions

- **`tools.DecodeArguments` rejects unknown fields.** Every tool schema
  already declares `"additionalProperties": false`, so a field the struct
  does not know is a model mistake that should be reported rather than
  silently dropped. No tool's schema lists a property its struct lacks, so no
  tool relied on the lenient copies. Empty or whitespace-only arguments
  decode as `{}`, and the helper fails with "the arguments do not match the
  tool's schema"; tools that answered in their own words keep them. Home
  Assistant's passthrough of MCP arguments into a map is not argument
  decoding into a typed struct and keeps its own helper.
- **Two clipping behaviours, in a new `internal/clip` package.**
  `clip.Bytes` caps a value's size in bytes on a rune boundary, with no
  marker, for size caps. `clip.Runes` caps text shown to a person or the
  model at a number of runes; when it cuts, the last rune is "…", so the
  result never exceeds the limit, and whitespace before the ellipsis is
  dropped. Callers that normalise whitespace or encoding first keep doing so
  around the shared call; Grafana's one-line clipping is renamed `excerpt`
  so `clip` no longer means two things. A caller that cut by bytes with a
  rune-named limit now cuts by runes; a caller that subtracted one from its
  limit to make room for the ellipsis no longer does.
- **Hard rune caps on stored or displayed names become `clip.Runes`.**
  Approval intent targets, connection names, attachment names, venue names,
  and judgment hints were cut by runes without a marker; they are shown text,
  so they now end with "…" when cut. Identifiers and MIME types, which are
  ASCII, are capped with `clip.Bytes`.
- **A new `internal/weekday` package** holds the Monday-first day names,
  the conversion from `time.Weekday`, normalisation (trim, lower-case,
  de-duplicate, validate, sort), and the description. Monday to Friday reads
  "Mon–Fri" and Saturday and Sunday "Sat–Sun" for both watches and routes,
  because "weekdays" is also used for "days of the week" in the tools'
  descriptions.
- **The trip prefix has one definition.** The report-subject SQL already
  binds `thread.TripDestination("%")`; the unused `conversation.TripSubject`
  is deleted.
- **Test-only helpers in production packages stay where several test
  packages share them** (`httpretry.WithoutWaits`,
  `observability.NewWithReader`). `conversation.AlwaysConnected`, used only
  by the package's own tests, moves into them. `nas.parseRelative`, whose
  only caller is its test, is deleted with the test.

## Approach

1. Add `tools.DecodeArguments` and replace every package copy.
2. Add `internal/clip` and replace every clipping copy.
3. Add `internal/weekday` and use it from watches and routes.
4. Remove the dead code and fix the staticcheck findings.
5. Fix the misplaced and stale doc comments in Notion and judgment.

## Files affected

- `internal/tools`, `internal/clip`, `internal/weekday` (new helpers)
- The tool files of the connectors and of backlog, memory, schedule, task,
  trip, watch, routes, image generation, reactions, attachments, approval
  withdrawal, conversation history, and Telegram help
- The render files that clip text, `internal/attachment`, `internal/approval`,
  `internal/judgment`, `internal/pdftext`, `internal/telegram/location.go`,
  `internal/conversation/history`
- `internal/watch/watch.go`, `internal/watch/reports.go`,
  `internal/task/service.go`, `internal/routes/route.go`
- `internal/conversation/types.go`, `internal/connectors/bus/ovapi.go`,
  `internal/connectors/nas/paths.go`, `internal/connectors/gmail/service.go`,
  `internal/testutil/calendarsim`, and the telemetry tests

## Validation

- Unit tests for the three shared helpers.
- The existing tool, render, watch, and route tests, updated where the
  wording or the clipped length changed.
- gofmt, `go vet`, build, the full race test suite, staticcheck, deadcode,
  and the plan check.

## Risks and open questions

- A model that sends a field outside the schema of a tool that used to
  accept extras now gets an error instead of a silently ignored field; the
  error names the schema, so the model can retry.
