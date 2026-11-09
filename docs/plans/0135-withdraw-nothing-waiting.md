# 0135 — Withdrawing when nothing is waiting is not a failure

Status: `done`

## Context

The weekly review filed a backlog item: `proposal_withdraw` failed 5 times in a week. All five
returned the same text, "No proposal of yours is waiting for the user's confirmation; nothing was
withdrawn.", and each followed the same sequence: the model proposed, the user pressed Reject,
the user wrote again, and the model called `proposal_withdraw` to retract the proposal. Nothing
failed: the proposal was already resolved, which is the state withdrawal aims for. The tool still
marks the result as an error, so the review counts it as a failure.

The model was never told of the rejection. A Reject press resolves the bundle but, unlike a
Confirm press, an expiry, or a replacement, records no notice for the next turn, although the
notice text for a rejected proposal exists. This omission dates from the first approval state
machine and was found during implementation; the first draft of this plan assumed the notice
was delivered.

## Goal

A withdrawal that finds nothing waiting returns a plain, non-error result, so the review stops
reporting it, and the notice of a rejected or expired proposal says it is no longer waiting, so
the model stops making the call.

## Decisions

- "Nothing is waiting" (no pending bundle, or only one the application offered) is a non-error
  result: the end state the model asked for holds. Its text adds that a proposal the user
  confirmed has already been carried out and is taken back with the opposite change, so the model
  does not report a confirmed action as withdrawn.
- It stays an error when the turn already made direct actions, which the model may be trying to
  take back and cannot withdraw; that result already names them and how to undo them.
- Requests that could not be carried out stay errors: a bad item number, an item no longer
  waiting, and every chosen item resolved by the user meanwhile.
- `Withdrawer.Withdraw` returns whether the request was refused instead of whether anything was
  withdrawn, so the tool derives `IsError` from the right fact; doc comments follow.
- A Reject press records the bundle's notice, as a Confirm press already does, so the next
  turn tells the model its proposal was rejected.
- The rejected and expired proposal notices say the proposal is no longer waiting. The notice
  reaches the model at the moment it would otherwise withdraw, and leaves the cacheable system
  prompt unchanged.

## Approach

1. `internal/approval/withdraw.go`: refused is false for the nothing-waiting case without direct
   actions and after any withdrawal; true for the direct-actions case, bad items, items no longer
   waiting, and all chosen items resolved meanwhile. Extend the nothing-waiting text.
2. `internal/approval/approval.go`: record the notice on a Reject press; extend the rejected
   and expired notice texts.
3. `internal/approval/withdrawal/withdrawal.go`: rename the interface result and set `IsError`
   from it.
4. Integration test in `internal/telegram`, beside the existing withdraw tests; update tests that
   assert the changed texts or the old boolean.

## Files affected

- `internal/approval/withdraw.go`, `internal/approval/approval.go`
- `internal/approval/withdrawal/withdrawal.go`
- `internal/telegram/schedule_editing_integration_test.go`, `act_integration_test.go`, and any
  test asserting the notice texts
- `docs/plans/0135-withdraw-nothing-waiting.md`, `docs/plans/README.md`

## Validation

- Integration test: the model proposes, the user presses Reject, the next turn's message carries
  the notice saying the proposal is no longer waiting, and a `proposal_withdraw` call then
  returns a non-error result saying nothing was withdrawn. The direct-action withdrawal test
  sees `refused`, and partial withdrawal and its error cases still work.
- Format, vet, module, test (including `-race`), build, and `scripts/check-plans`.
- The backlog item is closed once the change is deployed.

## Risks and open questions

- A non-error result could hide a real withdrawal miss. It is returned only when no proposal of
  the model's is pending and the turn made no direct action, where a withdrawal had nothing to
  act on.
