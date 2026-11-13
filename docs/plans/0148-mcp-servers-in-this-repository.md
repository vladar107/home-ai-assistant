# 0148 — The MCP servers' Compose project lives in this repository

Status: `done`

## Context

The application reaches Model Context Protocol servers over HTTP and never starts one. A server
without a hosted endpoint runs as a sidecar in a Compose project of its own, which until now lived
in a separate private repository. The application is that project's only client, its catalog names
the project's servers, and the two change together, so keeping them apart only split one concern
over two repositories.

## Goal

The sidecar project is a folder of this repository, with the history it had, and deploys from
there exactly as before.

Success: a deploy from `mcp-servers/` leaves the same three containers per server running under
the same Compose project name, and the smoke check passes.

## Decisions

- **The project stays a separate Compose project.** It keeps its name, its directory on the NAS,
  its networks, and its own deploy script. The application's deploy does not start, stop, or build
  it, and the application's image does not contain it.
- **Its history comes with it.** The repository's commits are replayed under `mcp-servers/` with
  their authors and dates, on top of `main`, without a merge commit.
- **Its scripts resolve paths from their own folder,** not from the repository root, so they work
  wherever the folder sits.
- **Its instructions stay beside it** in `mcp-servers/CLAUDE.md` and add to the repository's. The
  rules they shared with this repository are dropped from the folder's copy.
- **Nothing private comes with it.** The folder holds no address, path, or secret; the NAS path
  stays in the ignored `mcp-servers/.env.nas`.

## Approach

1. Replay the commits under `mcp-servers/`.
2. Point `scripts/deploy` and `scripts/smoke` at their own folder, and drop the folder's ignore
   file, which the repository's covers.
3. Keep the folder out of the application image's build context.
4. Say where the sidecar project is in the operations guide, the MCP overlay, the repository
   layout, and the project instructions.
5. Deploy from the new location, run the smoke check, and archive the old repository.

## Files affected

- `mcp-servers/`
- `.dockerignore`
- `README.md`, `CLAUDE.md`, `docs/operations.md`, `deploy/compose.app.mcp.yaml`

## Validation

- In `mcp-servers/`: `bash -n scripts/*`, `shellcheck scripts/*`, and
  `REVISION=dev docker compose config --quiet`.
- `scripts/check-plans`.
- On the NAS: `scripts/deploy` and `scripts/smoke` from `mcp-servers/`.

## Risks and open questions

- Images are tagged with the commit, and the commit differs from the old repository's, so the
  first deploy builds the images again. The previous images stay on the NAS.
- A deploy needs an interactive `sudo` on the NAS, so step 5 is the operator's.
