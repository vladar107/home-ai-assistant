# 0130 — Stop trusting mode bits on the NAS

Status: `done`

This is a retrospective record: the change was made to restore a stopped
service and written up alongside it.

## Context

The application refused to start, and scripts refused to deploy, whenever a
secret file had group or other permission bits, or a private directory was not
exactly `0700`. A NAS operating system update reset the deployment tree to mode
`0777` under an access control list. That list still admits only the owning
account, so nothing was exposed, but every mode check failed: the service
stopped at configuration with "must be a small regular file without group or
other permissions" and restarted in a loop, and the secret sync exited without
a message. Restoring the modes by hand lasts only until the next update.

## Goal

The application starts, and the secrets sync, with whatever mode bits the NAS
reports, so an operating system update no longer stops the assistant.

## Decisions

- Access to the deployment tree is enforced by the NAS, and every container
  runs as the account that owns the files. Mode bits are no longer a security
  signal the application checks or repairs.
- Files and directories the application creates keep their private modes
  (`0600` and `0700`); only checks and repairs of existing ones go.
- Secret files must still be small regular files. Private directories must
  still be real directories, not symbolic links, so a profile linked to another
  principal's still reads as not connected.

## Approach

1. Drop the mode checks from the secret file readers (NS, Home Assistant,
   Grafana, TypeSafe, Google client secret, Telegram token) and from
   integration credentials.
2. Drop the `0700` checks on existing data, outbox, attachment, model profile,
   integration credential, and Pi session and workspace directories, and the
   repairs of an existing export key's and database file's modes.
3. Drop the `700` check on the NAS secret directories from
   `scripts/nas-sync-secrets`.

## Files affected

- `internal/config`, `internal/connectors/google`, `internal/telegram`,
  `internal/integration`, `internal/connection`, `internal/outbox`,
  `internal/attachment`, `internal/runtime/pi`, `internal/export`,
  `internal/store/sqlite`
- `scripts/nas-sync-secrets`

## Validation

- A test loads each configuration secret file at mode `0777`; it fails before
  this change and passes after.
- The Pi test for a shared profile now links one principal's profile to
  another's and still expects the turn to be refused.
- `gofmt`, `go mod tidy -diff`, `go vet`, `go build`, `go test`, and
  `scripts/check-plans` pass.
- After deployment the service starts with the secret files at the modes the
  NAS left them.

## Risks and open questions

- On a host without such an access control list, a secret file placed with
  broad permissions is now used instead of refused. Deployment still creates
  every secret file at `0600`.
