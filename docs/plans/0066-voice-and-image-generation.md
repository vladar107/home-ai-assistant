# 0066 — Voice messages and image generation through the user's ChatGPT subscription

Status: `done`

## Context

Voice and audio messages are refused with "I cannot listen to voice messages
or audio yet", and the assistant cannot draw. Pi has neither speech-to-text
nor image generation, and the oh-my-pi fork adds image generation only as part
of a much broader coding-agent tool surface, so migrating was rejected.

Each principal already holds their own `openai-codex` OAuth record in
`data/pi/<principal>/auth.json` ([0004](0004-per-user-model-subscriptions.md)).
The same ChatGPT login reaches two undocumented ChatGPT backend endpoints:

- `POST https://chatgpt.com/backend-api/transcribe`: a multipart `file` field
  (plus an optional `language`), answered with `{"text": "..."}`. The Codex
  desktop app uses it for dictation.
- `POST https://chatgpt.com/backend-api/codex/responses`: the endpoint Pi
  already talks to. A streamed request carrying the hosted
  `image_generation` tool with `tool_choice` forced to it returns the picture
  as base64 in an `image_generation_call` output item. Codex CLI's `$imagegen`
  and oh-my-pi's `generate_image` use this path, and it counts against the
  ChatGPT plan's usage limits.

Pi refreshes the OAuth record and rewrites `auth.json` while holding a
`proper-lockfile` lock: a directory named `auth.json.lock`, considered stale
when its modification time is older than 30 seconds.

## Goal

A voice note or audio file in Telegram becomes an ordinary conversation turn
built from its transcript, and the model can draw a picture (optionally from
photos the user sent) that arrives in the chat as a photo. Both run on the
requesting principal's own subscription only.

## Decisions

- Use the undocumented endpoints directly, without a fallback. If either
  changes, the feature fails with a clear reply until it is fixed.
- Never borrow another principal's credential. A principal without a model
  connection gets the existing connect guidance.
- The connection service owns credential access. A new `Credential` method
  returns a current access token and account ID, refreshing an expiring token
  through the same token endpoint and lock protocol Pi uses, and writes the
  record back preserving any other provider in the file.
- A new `internal/chatgpt` package is the HTTP client for both endpoints. It
  takes the credential per call and keeps no token state.
- Requests carry the application's own User-Agent. Cloudflare challenges Go's
  default User-Agent on the transcription path before the credential is read,
  and such a block is reported as its own error rather than as a refused
  credential.
- Transcription runs in the Telegram update handler before the turn is queued,
  bounded by its own timeout. The turn's message is the transcript, marked as
  coming from a voice message, followed by any caption. An empty or failed
  transcription is answered directly and the update is not retried.
- Voice notes and audio files are both transcribed, up to Telegram's 20 MB bot
  download limit; video notes stay unsupported.
- Image generation is a `generate_image` read-risk tool in a new
  `internal/imagegen` provider, offered while the conversation is enabled. It
  accepts a prompt and optional ids of images the user sent in the session as
  references. The image goes straight to the principal's own chat through the
  delivery service, and the model receives only a confirmation.
- The image request uses the configured Pi model, without its thinking-level
  suffix, as the carrier model, and asks for PNG output.
- Generated images are not stored as attachments, so iterating means
  generating again from a revised prompt or from a photo the user sends back.

## Approach

1. Add `Credential` to the connection service with Pi-compatible locking and
   refresh.
2. Add the `internal/chatgpt` client for transcription and hosted image
   generation, including SSE parsing of the Responses stream.
3. Transcribe voice and audio in the Telegram service and enqueue the
   transcript as the turn's message.
4. Add photo delivery to the delivery service and the `generate_image` tool
   provider with its activity label, and register it in the application.
5. Update operations documentation for the new behaviour and its dependency on
   undocumented endpoints.

## Files affected

- `internal/connection/`
- `internal/chatgpt/` (new)
- `internal/imagegen/` (new)
- `internal/delivery/delivery.go`
- `internal/telegram/` (media classification, service, client, commands)
- `internal/tools/broker.go`, `internal/conversation/activity.go`
- `internal/app/app.go`
- `docs/operations.md`, `README.md`

## Validation

- Integration tests against simulated ChatGPT endpoints: a voice note becomes
  a queued turn with the transcript, a failed transcription is answered, the
  image tool delivers a photo and reports refusals, and an expired token is
  refreshed under the lock.
- Format, vet, test, build, and plan checks.
- A live voice note and a live image request through the operator's bot.

## Risks and open questions

- The endpoints are undocumented and may change or be restricted without
  notice; there is deliberately no fallback.
- Image generation can take most of a minute. The tool's timeout is shorter
  than the turn's, but a turn that also does other slow work may hit the turn
  timeout.
- Image requests spend the user's ChatGPT plan limits.
