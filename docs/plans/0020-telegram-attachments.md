# 0020 — Photos and files in Telegram conversations

Status: `in-progress`

## Context

The Telegram poller reads `message.text` and nothing else. A message that
carries a photo or a document reaches `processUpdate`, fails the
`strings.TrimSpace(message.Text) == ""` check, and is recorded as
`telegram.update.ignored` with reason `unhandled_text`. The user sees no reply
at all. `conversation.EnqueueInput` has only a `Message` string,
`agentruntime.RunRequest` has only a `Message`, and the Pi adapter sends
`{"type":"prompt","message":...}`.

Pi 0.84.1 accepts images on the RPC prompt:
`{"type":"prompt","message":"...","images":[{"type":"image","data":"<base64>","mimeType":"image/png"}]}`
(`docs/rpc.md`). It accepts nothing else — no audio, no documents. The
`openai-codex` catalog shipped with that version declares `input: ["text",
"image"]` for every model except `gpt-5.3-codex-spark`, so images work with the
provider in use and audio is not reachable through the runtime at all.

`docs/02-scope-and-capabilities.md` already lists "Receive text/files" as an
initial Telegram capability, and `docs/04-integrations.md` lists "Photos and
files subject to Telegram size limits" as an interaction primitive. Neither is
implemented.

## Goal

A household user can send a photo or a file in the normal conversation, with or
without a caption, and get an answer about it. A photo is looked at by the
model in the same turn. A file stays available for the rest of the session, and
its content reaches the model only when the request actually needs it — asking
to file a document somewhere must not cost the tokens of reading it. Media the
system cannot handle is answered with a plain sentence instead of silence.

## Decisions

- Voice, audio, and video are out of scope. No model reachable through Pi
  accepts audio, and transcription would need either a local speech-to-text
  service on the NAS or a second per-user credential. Both are a separate plan.
  These messages get an explicit "not supported" reply rather than silence.
- Photos and image documents are passed inline on the prompt of the message
  that carried them, through a new `RunRequest.Images`. This is the only channel
  Pi offers, and "what is in this picture" is the common case. Pi keeps earlier
  images in its own session file, so a follow-up question about a photo sent one
  turn ago still works.
- Non-image files are never inlined. The turn message lists each one by ID,
  name, type, and size, and the model reads content through a new core tool.
  This is what keeps "put this document in Notion" cheap while "what does this
  document say" still works.
- Attachments are session-scoped, not turn-scoped: they stay readable for every
  turn of the conversation and are deleted when the session closes with `/new`.
  A restart therefore does not lose them, unlike the request message, which the
  schema already clears on delivery.
- Bytes live on disk under `<DataDir>/attachments/<principal>/`, named by
  attachment ID, with 0700 directories and 0600 files, reusing
  `runtime.ProfileDirectoryName` for the principal directory name. SQLite holds
  metadata only — no file content in the database, and no filename or MIME type
  in logs or metrics.
- A request and its attachment rows are written in one transaction. The bytes
  are staged on disk first and the rows travel into `EnqueueConversation`, so a
  worker that picks up the request between the two can never run a turn whose
  files are not yet stored. Bytes staged for a request the store did not create
  — a redelivered update, or one refused as busy — are removed immediately, and
  anything left behind by a crash is collected by the startup sweep.
- A new `internal/attachment` package owns download limits, storage, retrieval,
  and cleanup, and contributes its tools through the existing broker as a core
  provider. The connector pattern stays for external services; this is core
  state, like approvals.
- Caps are fixed constants, not configuration: 5 MiB per image, 10 MiB per file,
  10 attachments and 50 MiB per session. Telegram's own bot download limit is
  20 MiB. Anything larger is refused with a sentence naming the limit.
- `read_attachment` returns text only for text-shaped types (`text/*`,
  JSON, CSV, Markdown, XML, YAML) that are valid UTF-8, bounded per call with an
  offset for continuation, well under the broker's 16 KiB result limit. Any
  other type reports that its content cannot be read; the file still exists for
  a later connector tool to consume by ID.
- PDF text extraction is not part of this change. It needs a new runtime
  dependency in the image and is worth its own decision once attachments exist.

## Approach

1. Telegram client and message types: add `caption`, `photo`, `document`, and
   the unsupported media fields to `Message`; add `GetFile` and a bounded file
   download that uses the `/file/bot<token>/<path>` route with the same redirect
   refusal, path escaping, and telemetry as the API methods.
2. `internal/attachment`: the store (stage, list, read, discard, close a
   session's attachments), the type and size rules, the deterministic disk
   layout, and the `list_attachments` and `read_attachment` tools with their
   guidance. Migration `008_attachments.sql` adds `conversation_attachments`,
   unique on `(request_id, position)` so a request cannot hold two files in the
   same place.
3. Conversation and runtime: `EnqueueInput.Attachments`, a message that may be
   empty when attachments are present, the staged rows passed through
   `EnqueueConversation`, an attachment section appended by `turnMessage`,
   `RunRequest.Images`, and the Pi adapter encoding them into the prompt's
   `images` field. `ResetConversation` reports the session it closed so its
   attachments are removed; startup recovery sweeps orphans.
4. Telegram service: select the largest photo size within the cap, treat an
   image document as an image, download, enqueue with the caption as the
   message, answer unsupported media, and answer a refused download with the
   reason. Wire the attachment provider into the broker in `app.go`.
5. Update the `/privacy` reply to say that photos and files sent in a
   conversation are kept until `/new`, refresh the two capability rows in
   `docs/02` and `docs/04`, and record the plan and index row.

## Files affected

- `internal/telegram/client.go`, `service.go`, `commands.go`, and a new
  `media.go`
- new `internal/attachment/` (store, tools, tests)
- `internal/store/sqlite/migrations/008_attachments.sql`,
  `internal/store/sqlite/conversation.go`
- `internal/conversation/service.go`, `types.go`
- `internal/runtime/runtime.go`, `internal/runtime/pi/runtime.go`
- `internal/app/app.go`
- `internal/testutil/pifixture/fixture.go`
- `internal/telegram/conversation_integration_test.go`,
  `internal/runtime/pi/runtime_test.go`
- `docs/02-scope-and-capabilities.md`, `docs/04-integrations.md`,
  `docs/plans/README.md`

## Validation

- `gofmt -l`, `go mod tidy -diff`, `go test -race ./...`, `go vet ./...`,
  `go build -buildvcs=false ./...`, `scripts/check-plans`.
- Integration tests against the fake Telegram server and the Pi fixture: a photo
  with a caption reaches the runtime as an image; a photo without a caption
  still starts a turn; a document is listed in the turn message and its content
  arrives only through `read_attachment`; an oversized file is refused with the
  limit; a voice message gets the unsupported reply; `/new` removes the stored
  files from disk.
- Live check in the operator's own chat: a photo question, a text file the model
  reads on request, and a voice message that is refused.

## Risks and open questions

- An album is several updates sharing `media_group_id`, one per photo. Each
  becomes its own turn, so the second photo of an album gets the busy reply
  until a grouping window exists. Deferred deliberately; single photos are the
  common case.
- Images are always inlined for the message that carried them, so sending a
  photo purely to file it elsewhere still costs image tokens. Splitting that
  would need an intent guess before the turn.
- Disk growth is bounded per session but not globally; the startup sweep and
  `/new` cleanup are the only reclamation. A retention job belongs with the
  wider storage policy.
- Telegram file downloads bypass the JSON envelope, so the download path needs
  its own size and content handling rather than reusing `execute`.
