# 0094 — Scrub private details from two done plans

Status: `done`

## Context

A security audit on 2026-10-18 found household details in two done plans in
this public repository. Plan 0093 named the robot vacuum's make and
integration, linked its upstream projects, and carried the schedule format
calibrated on the household's robot. Plan 0076 described which network
segments and VPN clients can reach the home edge, and the firewall behind it.
`CLAUDE.md` already forbids device details in the repository, but nothing
said plans in particular must follow it.

## Goal

- No revision of either plan in the repository's history carries those
  details.
- Future plans keep them out from the start.

## Decisions

- **Rewrite history, as plan 0050 did**, by explicit operator decision. Done
  plans are edited only through this rewrite, so the plan check keeps
  passing on every commit.
- **Only the commits since the earlier of the two plans change.** Each plan
  had a single revision. The rewrite replaces those two blobs in every later
  tree and re-signs each rewritten commit with its original author,
  committer, dates, and message. Earlier commits keep their IDs and
  signatures.
- **Technical content stays.** The plans keep their decisions and approach.
  The vacuum's calibrated layout moves to the operator's local Home
  Assistant folder, and the network wording becomes "the trusted home
  network".
- **`CLAUDE.md` names plans explicitly** in the rule against private data,
  and states that a terminal plan is scrubbed only by such a rewrite.

## Approach

1. Keep a local copy of plan 0093's calibration outside the repository.
2. Neutralise both plans' prose in a scratch clone.
3. Rebuild every commit from the earlier plan's commit to the tip with the
   two replacement blobs and re-sign them with their original metadata.
4. Add the `CLAUDE.md` rule and this plan on top.
5. Force-push `main`, and ask GitHub Support to purge the old commits.
6. Reset local checkouts and rebase work in progress onto the new history.

## Files affected

- `docs/plans/0076-backlog-api.md`, `docs/plans/0093-vacuum-schedule-tools.md`
  (in history)
- `CLAUDE.md`, `docs/plans/0094-plan-privacy-scrub.md`, `docs/plans/README.md`

## Validation

- The rewritten tip differs from the old tip only in the two plans.
- Every rewritten commit has the same metadata, message, and tree as its
  original apart from the two plans, and carries a valid signature.
- A pattern scan over every revision of both plans finds none of the
  removed terms.
- Plan checks pass at the tip.

## Risks and open questions

- The commit IDs of the rewritten commits change. Deployed images keep their
  digests but their version labels no longer match a commit.
- The old commits stay reachable on GitHub until Support purges them, and
  old Actions runs reference the old IDs.
- Other clones, including the operator's GoLand checkout, must reset to the
  new `main`.
