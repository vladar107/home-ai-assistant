# 0050 — Scrub private data before making the repository public

Status: `done`

## Context

The repository is about to become public. Its history described the operator's
own home area, daily trips, home network, hardware, and household, both in
documentation and in test fixtures recorded from live transport APIs. No
secrets were committed, but those details identify the household.

## Goal

No revision or commit message identifies the household's location, network,
hardware, or members, every commit stays signed, and the current tree passes
all checks. A rule in `CLAUDE.md` keeps it that way.

## Decisions

- Tests and documentation use another real town as their example. Transport
  fixtures were re-recorded from the live APIs for that town; scenarios that
  could not be captured live were derived from real responses.
- Error messages, comments, and tool descriptions carry no household-specific
  examples; neutral ones such as large stations stay. Anything specific to the
  household comes from configuration.
- The NAS mount for the second user is named `second` rather than after a
  relationship; its settings are `HOME_AGENT_NAS_SECOND_*`.
- Metric and container names of monitored software stay as they are; they name
  software, not the household.
- History is rewritten, not just the tip: each change is folded into every
  revision that carried the old text, a deleted binary document is dropped, and
  commits are re-signed with their original dates. Only the tip is guaranteed to
  pass tests; earlier commits still build.
- Done plans are edited as part of this scrub, by explicit operator decision,
  without changing their technical content.

## Approach

1. Re-record fixtures and update tests; remove examples from non-test code.
2. Rename the second user's NAS settings and mounts, and mirror the rename into
   the local environment files.
3. Neutralise prose in documentation and plans line by line.
4. Rewrite history with `git filter-repo` from a per-path map derived from the
   scrub diff plus rules for text only found in older revisions; re-sign.
5. Force-push, ask GitHub Support to purge cached and unreferenced commits, and
   only then make the repository public.
6. Redeploy the application on the NAS so it uses the renamed settings.
7. When the repository becomes public, turn on GitHub private vulnerability
   reporting, which `SECURITY.md` (plan 0052) points reporters to.

## Files affected

- Bus, NS, day card, web, location, memory, NAS, and Notion tests and fixtures.
- `internal/config/bus.go`, bus and NS tool descriptions, memory tool
  description, bus stop comments.
- `deploy/compose.app.nas.yaml`, `deploy/nas.env.example`,
  `deploy/nas-roots.example.json`, `scripts/deploy-app-nas`.
- `README.md`, `CLAUDE.md`, `docs/architecture.md`, and several plans.

## Validation

- The rewritten tip must be byte-identical to the scrubbed working tree.
- A pattern scan over every revision and commit message, in Latin and Cyrillic
  spellings, must find no private term; binary blobs are checked for metadata;
  gitleaks may report only the known fake tokens in the redaction tests.
- Formatting, module, test, vet, build, and plan checks must pass at the tip;
  every commit must build and carry a signature with its original dates.

## Risks and open questions

- Every commit ID changes; deployed images keep their digests but no longer
  match a commit. Tags for the new commits exist only after the image workflow
  builds them, so deploy after it finishes.
- Old images and Actions runs still carry the old text and commit IDs: keep the
  image package private or prune old versions, and delete old workflow runs.
- `ns/testdata/departures.json` is synthetic because the live endpoint was
  unavailable; re-record it when it recovers.
