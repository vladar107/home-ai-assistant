# 0021 — NAS files through mounted per-user roots

Status: `done`

## Context

The assistant reaches Notion but not the household's own files, which live on
the NAS that also hosts the application container.

[0020](0020-telegram-attachments.md) landed the attachment store, so a photo or
document the user sends in Telegram is already on disk under that principal's
directory. That is what makes "add this file to the NAS" a small step.

`docs/04-integrations.md` prescribed read-only bind mounts for selected roots.
That was set aside first, because the product direction makes integrations
user-scoped and a mount is one filesystem seen by one process as one UID. The
work went to per-user SMB instead, and came back.

## The route not taken

Per-user SMB with a dedicated NAS account per principal was designed, built,
and abandoned. It is recorded here because the reason is a property of the
appliance and will outlive this plan.

A probe on 2026-09-09 found that the NAS vendor publishes no file API, that WebDAV is
installed but switched off, and that SFTP cannot serve a second user, whose shell is
`/usr/sbin/nologin`. SMB was the only standard per-user protocol, and it looked
ideal: `smbshare.conf` already scopes `shared` to an admin group and a family group, and
a private backup share to the second user, excluding the operator via `invalid users`, so the NAS itself
would have answered "which files may this user see".

It cannot answer the question that matters. SMB is share-scoped, and the
personal-folder share is defined as `path = %H`, which Samba resolves to the
home of whoever authenticates. A dedicated `agent-user` account reaches
`agent-user`'s home and nothing else; no share exposes a person's home to
another account, and the NAS OS creates shares at volume root rather than inside
`@home`. The household's own documents live in those personal folders, so the
only way to reach them over SMB is to authenticate as the person — storing two
real NAS passwords in a file on the NAS, held by a process that could then
delete anything either of them can reach. That is a worse position than the one
it was meant to improve, and it was refused.

## Goal

Each authorized user can ask, in ordinary conversation, what is in their own NAS
folders and what one of those files says, and can have a file they sent in
Telegram filed onto the NAS after an explicit confirmation. No NAS account or
password exists anywhere in the system. Nothing already on the NAS can be
modified or removed through the assistant.

## Decisions

- Reach the files through directories mounted into the container. The container
  already runs as `NAS_RUNTIME_UID=1000`, which is the household's own user, so
  this needs no credential of any kind. There is nothing to store, leak, or
  revoke.
- Every mount is read-only except the inbox directories a confirmed save writes
  to. This is the outermost guarantee and the strongest one available: a write
  to a read-only mount fails in the kernel whatever this application believes.
- Each principal is granted a named set of roots in a JSON roots file, deployed
  like the Telegram allowlist. A principal absent from it has no NAS access, and
  the connector contributes nothing for them.
- **The per-user boundary is that grant, not the NAS.** One process serves the
  whole household, so unlike the SMB design nothing outside this code separates
  the two people's folders. Roots are therefore mounted narrowly, named per
  principal, and the file is documented as an access rule to be reviewed.
- Saving takes a folder the user names, such as one for finances, and falls back
  to that principal's own `default_write` root. Only a root marked writable can
  receive a save, checked before the user is ever shown a confirmation.
- Writes are switched per user exactly as Notion's are: an
  `integration.Connection.WritesEnabled` flag behind a `/settings` button, with
  the tool left out of the contribution entirely while the switch is off. The
  metadata row holds that switch and nothing else; it carries no credential.
- The settings section has no Connect or Disconnect. Access is a set of mounted
  directories, which only the operator can change, so a button implying
  otherwise would be a lie.
- Browsing and reading are separate. Listing and name search are always
  available to a principal with roots; `nas_read_file` is contributed with
  guidance that it may be called only when the user asked about that file's
  content, the boundary `read_attachment` already uses.
- Every path is validated segment by segment, resolved inside its root, and then
  checked again against what the filesystem says the path really is, so a
  symlink planted inside a root cannot reach outside it. Symlinks are omitted
  from listings rather than followed.
- Writes use exclusive create. An existing name is refused and reported, never
  replaced.

## Approach

1. `internal/config/nas.go`: `HOME_AGENT_NAS_ENABLED` and a roots file read like
   the Telegram users file, validating principals, root names, absolute paths,
   and the `default_write` reference. Complexity: moderate.
2. `internal/connectors/nas`: `paths.go` for location parsing and root-confined
   resolution, `local.go` for the mounted-directory store, `store.go` for the
   session contract and the bounded search, `service.go` for the module and the
   saving switch. Complexity: complex.
3. Read tools and guidance in `tools.go`: `nas_list_files` (no path lists the
   user's roots, marking which can be saved to), `nas_search_files`, and
   `nas_read_file` (text only, byte-capped, resumable by offset).
   Complexity: moderate.
4. `nas_save_attachment` in `writes.go` as an `approval.Executor`, resolving
   folder and name, refusing read-only roots and names in use, and rendering a
   confirmation that names the destination and any folder it would create.
   Complexity: complex.
5. The `/settings` section and `NASController` in `internal/telegram`, plus
   `internal/app/app.go` wiring behind `cfg.NAS.Enabled`. Complexity: moderate.
6. `deploy/compose.app.nas.yaml` carrying the mounts, applied by
   `scripts/deploy-app-nas` only while the feature is enabled, with the roots
   file copied to the NAS like the Telegram allowlist. Complexity: moderate.
7. Documentation: supersede the bind-mount section of
   `docs/04-integrations.md` with this design and its cost, and give the
   operator the folder, mount, and roots-file steps in
   `docs/13-operator-runbook.md`. Complexity: simple.

## Files affected

- `internal/connectors/nas/`: `service.go`, `local.go`, `store.go`, `paths.go`,
  `tools.go`, `writes.go`, `render.go`, `errors.go`, `service_test.go`,
  `paths_test.go`
- `internal/config/config.go`, `internal/config/nas.go`
- `internal/attachment/attachment.go` (`Describe` and `Content` accessors)
- `internal/telegram/service.go`, `internal/telegram/settings.go`,
  `internal/telegram/nas_integration_test.go`,
  `internal/telegram/conversation_integration_test.go`
- `internal/app/app.go`
- `deploy/compose.app.yaml`, `deploy/compose.app.nas.yaml`,
  `deploy/nas-roots.example.json`, `deploy/nas.env.example`,
  `scripts/deploy-app-nas`
- `docs/04-integrations.md`, `docs/13-operator-runbook.md`

## Validation

- `gofmt`, `go mod tidy -diff`, `go vet ./...`, `go test ./...`, and
  `scripts/check-plans` pass. The connector adds no dependency.
- Against real temporary directories, which is what production reads: roots are
  scoped per principal, another principal's root is not reachable, a principal
  with no roots is offered no tools, reads are text-only and resumable, an empty
  file and a folder are refused, and a search matches names under a folder.
- Symlink escapes: a link to a directory outside a root and a link to a file
  outside a root are both absent from listings and refused when addressed
  directly.
- Saving: the tool is absent until the switch is on, a save defaults to the
  user's own inbox, a named folder is honoured with its missing folders listed
  in the confirmation, a read-only root and a name in use are refused before any
  proposal is recorded, a confirmed save writes once, and switching the grant off
  between proposal and confirmation stops it.
- A directory made read-only on disk refuses a confirmed save, which is the test
  standing in for a read-only mount.
- The operator's roots file was parsed by the configuration loader itself: two
  principals, four roots each, two of them writable, defaulting to the user's
  own inbox. The three inbox and drop folders exist on the NAS.
- Live behaviour is exercised by the first deployment carrying this change:
  each user sees only their own folders, filing a photo lands it in their
  inbox, and a save into a read-only folder is refused.

## Risks and open questions

- Cross-user separation is enforced by this code and the roots file, not by the
  NAS. A wrong `principal_id` in that file grants one person's folders to the
  other. This is the deliberate cost of reaching personal folders without
  passwords, and the reason the runbook treats the file as an access rule.
- The read gate is model-enforced. Guidance can be misjudged, and the connector
  cannot tell an unprompted read from a prompted one. The cost is bounded to one
  file the user could already read, and the call is logged; a per-conversation
  reading grant remains the fallback.
- Mounting a whole home exposes everything in it, including anything private,
  to a read that the model decides to make. Narrower mounts remain available
  without a code change, since roots are configuration.
- File names and content are the user's data reaching the model, so a listing
  already discloses more than the strict-local phrasing in
  `docs/04-integrations.md` reserved. That is a deliberate departure.
- A save writes into a directory the household also uses, so a confirmed save
  can collide with a file created a moment earlier. Exclusive create turns that
  into a refusal rather than a loss.
