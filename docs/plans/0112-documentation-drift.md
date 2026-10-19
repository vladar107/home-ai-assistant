# 0112 — Documentation and plan drift

Status: `done`

## Context

An architecture audit (finding F8) found the README repository layout and the
component diagram in `docs/architecture.md` listing only part of `internal/`,
the unauthenticated backlog API described only by its mechanism, and two plans
whose status no longer matched the code.

## Goal

- The README layout and the architecture components match `internal/`.
- The backlog API's missing authentication is stated as a deliberate decision
  and trust boundary.
- Plan statuses reflect what is implemented.

## Decisions

- Plan 0074 is marked `done`: the `agent` watch kind, its evaluator, the
  `RunCheck` turn, the limits, and the migration all ship.
- Plan 0078 stays `proposed`: it is the design for three mechanisms, and only
  the first (plan 0080) is implemented. Learned procedures and the weekly
  reflection are not built.
- The architecture text names the backlog API's trust model without describing
  the network layout.

## Approach

1. Update the README layout and the architecture component diagram and helper
   list against `internal/`.
2. Rewrite the backlog bullet in the security model.
3. Set the status of plan 0074 and its index row.

## Files affected

- `README.md`, `docs/architecture.md`
- `docs/plans/0074-watch-anything.md`, `docs/plans/README.md`

## Validation

- `scripts/check-plans HEAD^`.

## Risks and open questions

- Plan 0078's remaining mechanisms stay open until they get their own plans.
