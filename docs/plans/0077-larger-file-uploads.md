# 0077 — Larger file uploads

Status: `done`

## Context

Files sent to the bot are refused above 10 MB. The user asked, through the
backlog, to raise the limit. Telegram's cloud Bot API lets a bot download
files up to 20 MB; voice and audio already use that ceiling.

## Goal

- Files up to 20 MB are accepted; larger ones get a reply naming the new
  limit.

## Decisions

- **20 MB, Telegram's ceiling.** Going further needs a self-hosted Bot API
  server, a separate container with its own Telegram API credentials; the
  user chose not to.
- **Images stay at 5 MB.** That bound comes from what the model accepts as
  image input, not from Telegram.
- **The session cap stays at 10 files and 50 MB.** Two full-size files still
  fit beside smaller ones.

## Approach

1. Raise `attachment.MaximumFileBytes` to 20 MB.
2. Update the reply for a file that is too large.

## Files affected

- `internal/attachment/attachment.go`
- `internal/telegram/media.go`
- `internal/telegram/attachment_integration_test.go`

## Validation

- The oversized-media integration test sends a file above 20 MB and expects
  the new reply.
- Formatting, module, test, vet, build, and plan checks.

## Risks and open questions

- A 20 MB PDF is parsed in memory; page and text limits in `pdftext` still
  bound what reaches the model.
