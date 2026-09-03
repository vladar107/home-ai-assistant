# 0002 — Private repository and plan governance

Status: `done`

## Context

The completed local foundation has no Git remote. Future work needs a private,
recoverable source of truth, signed linear history, private image storage, and
immutable implementation records without introducing a pull-request workflow.

## Goal

Publish the existing project to a private `vladar107/home-ai-assistant` GitHub
repository and establish an enforced plan-first workflow for every subsequent
non-trivial change.

## Decisions

- Use `CLAUDE.md` as canonical project guidance and expose it through the `AGENTS.md` symlink.
- Work directly on `main` without pull requests or automated deployments.
- Use signed conventional commits and preserve a flat history without merge commits.
- Allow plan edits only before a terminal status; terminal plans and their existing index rows are permanent records.
- Validate plan structure locally and in both CI and image-publishing workflows.
- Treat the existing foundation as a clearly labeled retrospective exception rather than rewriting its history to look plan-first.
- Store BuildKit provenance with the private GHCR image; GitHub repository attestations are unavailable for user-owned private repositories.

## Approach

1. Add canonical project guidance, the plan index and template, and the retrospective foundation record.
2. Add a repository-local validator for numbering, index consistency, statuses, and terminal-plan immutability.
3. Run the validator from CI before source validation or image publication.
4. Amend plan 0001 into the existing unpublished foundation commit and commit this plan with the governance change.
5. Create the private GitHub repository, push `main`, and verify privacy, signatures, linear history, workflows, and the GHCR package.

## Files affected

- `CLAUDE.md` and `AGENTS.md`
- `docs/plans/`
- `scripts/check-plans`
- `.github/workflows/ci.yml`
- `.github/workflows/image.yml`
- Local Git remote configuration and the private GitHub repository

## Validation

- Run `scripts/check-plans` against the working tree and the previous commit.
- Run the repository's configured formatting, module, test, vet, and build checks.
- Confirm the GitHub repository is private and its default branch is `main`.
- Confirm local and remote SHAs match, commits are verified, and history contains no merge commits.
- Confirm CI and production-image workflows succeed and the GHCR package is private.

## Risks and open questions

- GHCR package visibility cannot be queried with the current GitHub CLI token unless it receives `read:packages`; verify privacy through unauthenticated pull denial or explicitly authorize that additional scope.
- Natural-language Telegram conversation and per-user integration enablement belong to the next implementation plan after this bootstrap is verified.
