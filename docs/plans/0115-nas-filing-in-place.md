# 0115 — NAS filing in place

Status: `done`

## Context

Plan 0021 lets a confirmed save add a file to a principal's writable roots, and
those are only the personal and shared inbox mounts. Home and shared folders
are mounted read-only. The save guidance tells the model to give a folder only
when the user names one, so a request such as "put this where it belongs" ends
up at the top of the inbox, and no tool shows the model the shape of a folder
tree beyond one listing at a time.

A recent save meant for the user's own inbox landed in the shared inbox. The
model left nothing to the default: it passed the shared root as the folder. The
roots listing shows bare names with no hint of which folder belongs to the user
and which to the household, and the confirmation text does not say it either,
so the wrong destination was easy to choose and easy to confirm.

## Goal

- Asked to file something where it belongs, the agent reads the folder
  structure of the user's own folders and proposes the best-fitting existing
  folder, including folders in the user's personal home.
- "Inbox" means the user's own inbox. A shared folder is used only when the user
  says so, and every listing and confirmation makes ownership visible.

## Decisions

- Each principal's personal home becomes writable for confirmed saves. This
  supersedes the decision in plans 0021 and 0065 that only inboxes are
  writable. Shared folders stay read-only. Saves still only add new files with
  exclusive create: nothing on the NAS is replaced, renamed, or removed.
- The roots file gains two per-root flags, both false by default so a missing
  flag fails closed:
  - `shared` marks a folder that belongs to the household rather than to this
    user. A directory reachable by more than one principal, granted to both or
    nested inside another principal's root, must be `shared` in every grant
    involved, and `default_write` cannot name a shared root.
  - `create_folders` lets a save create missing folders there. It is valid only
    on a writable root and is set on the inboxes, not on the homes.
- Folder names are matched against the disk exactly. A save into a root
  without `create_folders` must name folders that exist; the connector rewrites
  each folder to the case found on disk before the proposal is recorded, so the
  confirmation and the stored path agree with the disk. A missing folder is
  refused with a message that points the model to `nas_list_folders` and the
  default folder. A folder or file name starting with `.`, `@`, or `#` is
  refused as a destination, including the name the file was sent with.
- At confirmation, a root without `create_folders` (read from the live
  session, not the proposal) gets no folder created at all; a folder that
  disappeared in between fails as a missing folder.
- A new read-only tool, `nas_list_folders`, returns the folder tree under one
  required path, folders only, breadth-first to a default depth of 3 and a
  maximum of the search depth, within the search entry budget and the listing
  byte budget, reporting truncation. File names stay out of it, and system
  folders whose names start with `.`, `@`, or `#` are skipped. When the byte
  budget cuts the list, the deepest folders go first.
- Placement is chosen from folder names and from the file the user sent; the
  agent does not open other NAS files to decide where one belongs.
- The roots listing labels each root as the user's own or shared with the
  household, and the save confirmation names a shared destination as shared.
- Default destination stays the principal's `default_write` root.
- The settings line that lists NAS roots stays as it is.

## Approach

1. Config and connector plumbing: parse and validate `shared` and
   `create_folders`, and carry them through `app` into `nas.RootPath`,
   `nas.Root`, and `Status`.
2. Save guard in `writes.go`: exact-case resolution of existing folders,
   refusal of missing folders and system names in roots without
   `create_folders`, and the confirmation-time re-check.
3. Ownership and guidance text: `renderRoots`, `saveSummary`, the `folder`
   schema description, and the write guidance, which says inbox means the
   user's own, a shared root is used only when the user says so, and filing
   reads the tree of the user's own writable roots, picks an existing folder,
   and falls back to the default folder with a note when nothing fits.
4. Folder tree tool in a new `folders.go`: definition, walk, renderer, and a
   line in the read guidance.
5. Deployment: the operator gives the container's NAS account write access to
   both homes, then the compose overlay mounts the homes read-write and the
   example and operator roots files gain the new flags.
6. Documentation: the NAS isolation bullet in `docs/architecture.md` and the
   NAS row in `docs/operations.md`.

## Files affected

- `internal/config/nas.go`, `internal/app/app.go`
- `internal/connectors/nas/`: `local.go`, `store.go`, `writes.go`,
  `render.go`, `tools.go`, a new `folders.go`, and tests
- `deploy/compose.app.nas.yaml`, `deploy/nas-roots.example.json`, the ignored
  `deploy/nas-roots.json`
- `docs/architecture.md`, `docs/operations.md`, `docs/plans/README.md`

## Validation

- Formatting, module, vet, build, race and non-race tests, and plan checks.
- Integration tests on real temporary directories: config validation of the new
  flags; ownership labels in the listing and confirmation; a save into an
  existing home folder; a save into a missing home folder refused before any
  proposal; a differently cased folder saved under the case on disk with no new
  folder created; a confirmation in a root without `create_folders` creating no
  folder; the inbox still creating folders; and the tree tool's depth, budget,
  folders-only output, and skipped system folders.
- After a manual deploy: ask the assistant to put a file in the inbox and see the
  personal inbox in the confirmation; ask it to file a document where it belongs
  and see an existing home folder proposed.

## Risks and open questions

- One container now holds kernel write access to both homes. Separation between
  them rests on the roots file and the `os.Root` confinement from plan 0065, as
  it already does for the inboxes. The blast radius grows, but stays limited to
  adding new files.
- Files saved into a home are owned by the container's NAS account; whether the
  home's owner can later rename or remove them depends on inherited
  permissions, which the operator checks when granting write access.
- A planted file or folder name can steer placement toward a wrong but
  plausible folder; the confirmation is the guard, so it shows the full
  destination and its ownership.
- An explicit request for a new folder inside a home is refused under these
  rules; the user can save to the inbox instead or the operator can enable
  `create_folders` for that root.
