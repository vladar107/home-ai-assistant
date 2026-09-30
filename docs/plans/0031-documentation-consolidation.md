# 0031 — Consolidate documentation into a README, C4 diagrams, and an operations guide

Status: `done`

## Context

`docs/` holds thirteen numbered design documents (about 4,300 lines), a research
note, a spike module, and the original `.docx` brief. They were written before most
of the code and now describe a future that diverged from it: the photo library, the network controller, GitHub,
automation packs, and edge workers are documented but not built, while Gmail, NS,
the web tools, day cards, and TypeSafe checks are only described in plans and the
runbook. The same material is repeated across the vision, scope, architecture,
security, and roadmap documents, and nobody reads them any more.

## Goal

Three current documents that match the code:

- `README.md`: what the assistant is, what it can do, how to run it locally, and
  where to read further.
- `docs/architecture.md`: C4 context, container, and component diagrams of the
  application as built, with the security model and the key decisions beside them.
- `docs/operations.md`: the operator runbook condensed to setup, integrations,
  deployment, rollback, backup, and rotation.

## Decisions

- **Plans stay.** `docs/plans/` and the immutability rule are unchanged; plans are
  the design history. Links from terminal plans to deleted documents stay broken
  rather than editing those plans.
- **Delete** `docs/01`–`docs/13`, `docs/research/`, `spikes/`, and
  `home_agent_platform_brief.docx`. Git history keeps them.
- **C4 in Mermaid flowcharts** rather than Mermaid's experimental `C4Context`
  syntax, which renders poorly on GitHub and in GoLand.
- **Describe only what is built.** Unbuilt integrations appear nowhere except a
  one-line "not built" note, so the documents cannot drift ahead of the code again.
- **No personal network detail.** The operations guide names the NAS through the
  `home-nas` SSH alias and `<nas-address>` rather than addresses, VLANs, or account
  names.

## Approach

1. Write `docs/architecture.md` from `internal/app/app.go` wiring and the package
   layout.
2. Condense `docs/13-operator-runbook.md` into `docs/operations.md`, dropping
   one-off migration steps.
3. Rewrite `README.md` around the new documents.
4. Delete the superseded files and confirm nothing outside `docs/plans/` links to
   them.

## Files affected

- `README.md`
- `docs/architecture.md`, `docs/operations.md` (new)
- `docs/01-*.md` … `docs/13-*.md`, `docs/research/`, `spikes/`,
  `home_agent_platform_brief.docx` (deleted)
- `docs/plans/README.md`, this plan

## Validation

- `scripts/check-plans` passes.
- `git grep` finds no link to a deleted file outside `docs/plans/`.
- `go build ./...` and `go vet ./...` still pass with `spikes/` gone.
- The Mermaid diagrams use plain flowchart syntax that GitHub and GoLand render.

## Risks and open questions

- Terminal plans link to `docs/13-operator-runbook.md` and other deleted documents;
  those links resolve only through history.
