# 0065 — Confine NAS file access with os.Root

Status: `done`

## Context

The NAS connector checks every path before touching it: segments are validated,
the joined path must stay under the root, and `confinedToRoot` follows symlinks
and checks the real path again. The file operation then resolves the path a
second time through `os.Open`, `os.OpenFile`, or `os.Mkdir`. Anyone who can write
to a share directly, such as over SMB, could swap a directory in the path for a
symlink between the check and the open and reach a file outside the root.
CodeQL reports this as path injection.

## Goal

Every file operation of the local NAS session resolves its path inside the root
in the same step that opens it, so no symlink, planted early or late, leads out
of a root.

## Decisions

- Use the standard library's `os.Root`: each operation opens the root and acts on
  the path relative to it. The kernel keeps the lookup inside the root, symlinks
  included.
- Keep the existing segment validation, prefix check, and `confinedToRoot` as a
  first layer; they give the model its specific refusals.
- An escape that only `os.Root` catches, which needs a race, is reported as the
  generic unavailable error; the standard library does not export that error.

## Approach

1. Let `resolve` open the root and return it with the path relative to it.
2. Open the root per operation and use its `Open`, `OpenFile`, `Lstat`, and
   `Mkdir`; list a directory through the opened handle, sorted by name as
   `os.ReadDir` did.

## Files affected

- `internal/connectors/nas/local.go`

## Validation

- The existing NAS tests, including the symlink escape tests, pass unchanged.
- Formatting, module, test, vet, build, and plan checks pass.

## Risks and open questions

- CodeQL may not recognise `os.Root` as a sanitiser; if the alert stays, dismiss
  it with a reference to this plan.
