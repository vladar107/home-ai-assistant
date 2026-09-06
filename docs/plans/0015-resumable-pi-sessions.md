# 0015 — Resumable Pi sessions across turns

Status: `done`

## Context

After the NAS deployment the assistant answered "this is your first message"
to a question about the current dialogue. Verified on 2026-09-06: the SQLite
store keeps one active conversation session per principal and passes the same
runtime session id to every turn, but the NAS session directory held one Pi
session file per turn, all with that id and each with a different `cwd`
(`/tmp/home-agent-pi-*`). Pi 0.84.1 resumes `--session-id` only among the
sessions whose recorded working directory equals the current one when a custom
`--session-dir` is used (`SessionManager.list` filters by `cwd`); otherwise it
warns and creates a new session with the same id. The runtime created a fresh
temporary working directory for every turn, so no turn ever resumed. Local
check with a fabricated session file: the same working directory resumes the
file without a warning, another directory creates a second file.

## Goal

Every turn of an active conversation resumes the same Pi session file, so the
model keeps the dialogue context until `/new`.

## Decisions

- The Pi working directory is a stable, private `workspace` directory inside
  the principal's own agent directory, created with the same checks as the
  session directory. It stays empty; tools, extensions, and context files are
  disabled, so the directory only scopes session lookup.
- Existing session files recorded with temporary working directories are not
  migrated. They no longer match, so the next turn creates a fresh session
  file for the active id and later turns resume it; `/new` remains the
  explicit reset.
- The Pi fixture used by tests resumes by id regardless of the working
  directory, which is why the routine tests did not catch this. Making the
  fixture mirror Pi's directory scoping is a separate decision.

## Approach

1. Return the workspace directory from `principalDirectories` and run Pi
   there instead of a temporary directory. Complexity: simple.
2. Deploy the application and verify a two-message dialogue on the NAS keeps
   context and that the session directory gains no extra file per turn.

## Files affected

- `internal/runtime/pi/runtime.go`

## Validation

- `gofmt`, `go vet ./...`, `go test -race ./...`, `go build ./...`,
  `scripts/check-plans`.
- Live: after deployment, ask a question, then ask what was discussed; the
  reply references the first question; `ls` of the principal's session
  directory shows one file for the active session id across both turns.

## Risks and open questions

- The workspace path is part of every session header; moving the data
  directory would orphan sessions the same way the temporary directories did.
