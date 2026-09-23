# 0023 — Send a NAS file to the user in Telegram

Status: `done`

## Context

[0021](0021-nas-file-access.md) gave each principal a set of mounted NAS roots
they can list, search, and read as text. [0020](0020-telegram-attachments.md)
gave the conversation an inbound file path: a photo or document the user sends
is downloaded, stored, and reachable by the model.

Nothing goes the other way. Every channel the model has to the user carries
text: `conversation.Deliverer` is `SendReply`, `SendTyping`, and the approval
messages. Asked for a file, the model answers with a path and a sentence saying
it cannot attach anything — which is what the operator saw when asking for a
payslip that the assistant had just located on the NAS.

The Telegram Bot API `sendDocument` method uploads a file as
`multipart/form-data`. Every method the client has today posts
`application/x-www-form-urlencoded` through one `execute` helper, and the only
place that touches raw bytes is `DownloadFile`, which reads them.

## Goal

A user who asks for one of their own NAS files receives it as a Telegram
document in the same turn, alongside the model's reply. The file arrives only
when they asked for it, only from the roots that user is already granted, and
only to that user's own chat. Nothing on the NAS is changed by sending it, and a
file that cannot be sent produces a refusal the model can repeat truthfully
rather than a claim that it was sent.

## Decisions

- The tool belongs to the NAS connector, as `nas_send_file`. The connector
  already owns location parsing, root confinement, symlink refusal, and the
  per-principal grant; sending is a use of the read grant, not a new one. A
  general "send anything" tool would need its own answer to all four questions.
- Sending needs no confirmation. The file goes to the private chat of the same
  principal who is already granted to read it, so it crosses no boundary that
  `nas_read_file` does not already cross. The approval flow is for changing a
  system of record, which this does not do.
- The tool is contributed with the browse tools, not behind the saving switch.
  It is classed `tools.RiskRead`: nothing is created, changed, or removed.
- **Sending a file never requires reading it.** Finding a file is a matter of
  names and folders, and the bytes go from disk to Telegram without passing
  through the model, so the operator's rule — content is opened only when they
  asked what a file says — survives this change instead of being widened by it.
  The read guidance is hardened accordingly: locating, identifying, naming,
  listing, filing, or sending a file is never a reason to call `nas_read_file`.
- That boundary stays guidance rather than a confirmation button or a settings
  switch, which was decided explicitly. A button would cost a press on every
  reading the user did ask for, and a switch would turn "ask me" into "go and
  flip something first". Both remain available if guidance proves too weak.
- Search stays name- and folder-based. No file metadata, extended attribute, or
  content index is read to find a file.
- Unlike `nas_read_file` this is not restricted to text. A PDF is the common
  case, and the bytes are handed to Telegram rather than to the model, so no
  type can mislead the model or spend its context.
- A principal's private chat ID is their Telegram user ID. Telegram makes them
  equal in private chats, and `validatePrivateMessage` already refuses any
  message where they differ, so no new state is needed to know where to send.
  The mapping is read from `telegram_allowlist` through a new
  `ResolveActiveChat`, which applies the same allowlist and principal-status
  conditions as `ResolveActivePrincipal`: a revoked or disabled principal has no
  chat and is sent nothing, even mid-turn.
- A new `internal/delivery` package holds that resolution and the upload, and
  the NAS connector depends on its own narrow `Sender` interface, the way it
  already depends on `Proposer` and `Attachments`. `internal/telegram` imports
  the NAS connector for its settings section, so the connector cannot import the
  client; and delivering to a principal is core state, like approvals, not a
  connector concern. A nil sender leaves the tool out entirely.
- The file is streamed from disk into the multipart body rather than read into
  memory, and bounded at 20 MiB — the same order as the inbound limits and well
  under Telegram's 50 MB ceiling for a document a bot uploads.
- The broker's single 15-second call deadline is too short for an upload of that
  size on a household uplink, so `tools.Definition` gains an optional
  `Timeout`, defaulting to the current 15 seconds, and `nas_send_file` asks for
  60. The turn timeout still bounds the whole turn.
- An upload does not use the shared HTTP client's timeout, which is sized for
  long polling and would have cut every upload off at 35 seconds whatever the
  tool asked for. Uploads are bounded by the caller's context, with a two-minute
  ceiling that only catches a call arriving without a deadline at all.
- A principal holding two active allowlist rows resolves to no chat rather than
  to whichever row the query returned first. The column is not unique, and
  sending a household file to an account its owner has left is worse than not
  sending it.
- The document carries no caption. The model's own reply already explains the
  file in the same turn, Telegram shows the file name itself, and a caption
  would be a second rendering path for model text.
- File names stay out of logs and metrics, as with attachments. The delivery log
  line carries the principal and the byte count.
- The privacy reply gains a sentence: a NAS file the user asks for is uploaded
  to Telegram to reach them.

## Approach

1. `internal/telegram/client.go`: `SendDocument(ctx, chatID, name, body,
   size)`, posting `multipart/form-data` to `sendDocument` through a new upload
   path that shares the telemetry, envelope decoding, and `apiError` handling of
   `execute`. The file name is sanitized for the part header. Complexity:
   moderate.
2. `internal/store/sqlite/telegram.go`: `ResolveActiveChat(ctx, principalID)`
   returning the Telegram user ID of an enabled, allowlisted principal, mirroring
   the conditions of `ResolveActivePrincipal`. Complexity: simple.
3. New `internal/delivery`: `Service.SendFile(ctx, principalID, File)` resolving
   the chat, refusing a principal without one, and uploading through a `Documents`
   interface the Telegram client satisfies. `File` carries the name, the size,
   and an `io.ReadCloser`. Complexity: simple.
4. `internal/tools/broker.go`: `Definition.Timeout`, validated as non-negative
   and capped, applied per call instead of the constant. Complexity: simple.
5. `internal/connectors/nas`: `Session.Open` on the store and `localSession`
   (read-only, refusing a directory, a non-regular file, and a symlink escape
   exactly as `Read` does); a `Sender` interface and `sendFile` in a new
   `send.go`, which also holds the size refusal and its sentence; the definition
   and guidance in `tools.go`. Complexity: complex.
6. `readGuidance` and the `nas_read_file` description in `tools.go`: state the
   reading boundary as a rule rather than a preference, and name sending among
   the things that never justify a read. Complexity: simple.
7. `internal/app/app.go`: build the delivery service from the store and the
   Telegram client, and pass it to `nas.NewService`. Complexity: simple.
8. Documentation: the capability rows in `docs/02-scope-and-capabilities.md` and
   `docs/04-integrations.md`, the `/privacy` line in `internal/telegram/commands.go`
   and `service.go`, and this plan with its index row. Complexity: simple.

## Files affected

- `internal/telegram/client.go`, `internal/telegram/commands.go`,
  `internal/telegram/service.go`
- new `internal/delivery/delivery.go`, `internal/delivery/delivery_test.go`
- `internal/store/sqlite/telegram.go`, new
  `internal/store/sqlite/telegram_test.go`
- `internal/tools/broker.go`, new `internal/tools/broker_test.go`
- `internal/connectors/nas/`: `service.go`, `store.go`, `local.go`, `tools.go`,
  new `send.go`, `service_test.go`
- `internal/app/app.go`
- `internal/telegram/conversation_integration_test.go` (fake `sendDocument`),
  `internal/telegram/nas_integration_test.go`
- `docs/02-scope-and-capabilities.md`, `docs/04-integrations.md`,
  `docs/plans/README.md`

## Validation

- `gofmt -l`, `go mod tidy -diff`, `go test -race ./...`, `go vet ./...`,
  `go build -buildvcs=false ./...`, `scripts/check-plans`.
- Against real temporary directories: a file inside a granted root is sent with
  its own name and bytes; another principal's root, a path outside a root, a
  symlink to a file outside it, a directory, a missing file, an empty file, and
  a file over the limit are each refused with their own sentence and nothing is
  uploaded.
- The tool is absent for a principal with no roots and for a deployment without
  a sender, and present without the saving switch being on.
- The contributed guidance states the reading boundary and names sending as
  something that does not justify a read, asserted where the existing
  contribution tests assert what a principal is offered.
- `internal/delivery`: a principal whose allowlist entry is revoked or whose
  principal row is disabled is refused before any upload; an upload failure is
  returned rather than swallowed; the open file is closed on every path.
- `ResolveActiveChat`, against a real temporary database: an allowlisted active
  principal resolves their chat, and an unknown, revoked, disabled, or
  doubly-allowlisted principal resolves none.
- Integration, through the fake Telegram server and the Pi fixture: a turn in
  which the model calls `nas_send_file` produces a `sendDocument` upload with the
  right file name and bytes to that user's chat, followed by the model's reply;
  a refused send leaves the turn with a reply and no upload.
- Live check in the operator's own chat: asking for the payslip that prompted
  this plan returns the PDF.

## Risks and open questions

- The upload happens inside the tool call, so a slow uplink spends the turn's
  time on it. The 60-second tool deadline and the 20 MiB cap bound it, but a
  household uplink under about 3 Mbit/s cannot deliver a file at the cap.
- Sending is not confirmed, so a prompt injection carried in a file the model
  read could ask it to send another file — to the same user's own chat, which is
  the limit of the damage. Cross-user sending is impossible: the chat is derived
  from the principal running the turn.
- The reading boundary is enforced by guidance alone, so a model that ignores it
  opens a file the user did not ask about. Nothing in this change makes that
  more likely — sending needs no read — but it is worth watching in
  `tools.call.completed`, and confirmed reads or a reading switch are the
  answers if it happens.
- Only NAS files can be sent. Sending back an attachment the user supplied, or a
  file the model composed, needs a scratch area and its own type and size rules,
  and is deliberately left out.
- Telegram sees the file. That is already true of everything in the chat, and
  the privacy reply now says so, but it is a real difference from reading a file
  as text — the whole file leaves the NAS rather than a bounded window of it.
