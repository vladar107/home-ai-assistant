# 0101 — Scheduling latency, shutdown robustness and digest-pinned deploys

Status: `done`

## Context

An audit found five places where one slow or stopped part held up others:

- The scheduler lets two queued runs execute at once, and reminders count
  against that bound. A scheduled model turn waits for its owner's chat turn
  while holding a slot, so when one principal is chatting and two of their model
  jobs fire, both slots wait on that chat and another principal's reminder
  arrives minutes late, marked as overdue.
- A failed reverse geocode is not remembered. Every chat, scheduled, task, and
  agent-check turn names the user's position before the model starts, so while
  the geocoding service is down each of those turns waits out the five-second
  request timeout.
- Shutdown stops Telegram, the conversation services, the HTTP server, and the
  modules one after another within one budget. Each service cancels its own work
  only when its Shutdown is called, so one slow service can use up the budget
  before the next is told to stop, and the store then closes under them.
- A `/cancel` (or `/new`, or a disconnect) during a reply's delivery makes the
  conversation lane log a retry and sleep five seconds before it takes the next
  message.
- The NAS deployment pulls the image by tag and only reads the digest
  afterwards, so the digest it records is whatever the tag pointed at when the
  NAS pulled it.

## Goal

Reminders and other principals' scheduled runs are not held up by one
principal's waiting model turns; a geocoding outage costs at most one timeout a
minute; every service is told to stop at the start of shutdown and they all wind
down within the same budget; the message after a stopped delivery is taken at
once; and each deploy runs exactly the digest the Mac resolved from the commit's
tag.

## Decisions

- Job types say whether their runs ask the model. A reminder does not, and its
  runs skip the concurrent-run bound entirely. Model-using runs (scheduled model
  turns and Trips scans) keep the household bound of two and are also limited to
  one executing run per principal: a principal's scheduled turns wait behind
  their chat and each other anyway, so a second one would only hold a slot
  another principal could use. Taking the conversation's turn slot before a
  scheduler slot was not chosen, because it would reach into the conversation
  service's lane handling from the scheduler.
- A run's slot is freed as soon as it has executed, before its report is
  delivered. When the run produced a report, freeing its slot wakes the
  scheduler, so a run held back by the bound starts then rather than at the next
  tick. A run that failed to execute, or ended quiet or skipped, wakes nothing:
  an immediate tick after a store or chat lookup failure would retry the same
  run into the same failure in a tight loop.
- A reverse-geocode failure is remembered for one minute for every point, not
  only the failed grid cell: an outage affects all points, and a moving live
  location would otherwise pay the timeout again in each new cell. A failure
  caused by the caller's own cancellation is not remembered.
- Shutdown runs every service's Shutdown at once and waits for all of them, both
  at the top level and inside the conversation and module groups; the store
  still closes last. Every Shutdown cancels its own work before it waits and
  none closes anything another service calls (connector shutdowns only mark
  themselves not ready and close idle connections), so the old order bought
  nothing but serial waiting.
- The five-second wait after a failed delivery stays for real delivery failures
  but not for a cancelled one. `/cancel`, `/new`, and a disconnect each end the
  request in the store before they cancel its delivery, so the lane cannot find
  the stopped request again, and the wait guarded nothing.
- The deployment resolves the release tag to a digest on the Mac through the
  registry API, with a pull token obtained from the GitHub CLI's login (which
  needs the `read:packages` scope), before it touches the NAS. The NAS pulls and
  runs `repository@digest`: `runtime.env` carries `HOME_AGENT_IMAGE_DIGEST`
  beside `HOME_AGENT_IMAGE_TAG`, which still names the service version in
  telemetry. Tokens reach curl on stdin, not its command line. The deployment
  stays manual and tag-chosen, with the same interactive `sudo` session.

## Approach

1. Add a `Light` flag to job types, set it for reminders, and make the
   scheduler's dispatch skip the bounds for light runs, keep one bounded run per
   principal, free the slot after execution, and wake itself when a run
   reported.
2. Remember a failed reverse geocode for a minute in the places connector's
   cache.
3. Remove the delivery retry wait when the delivery was cancelled.
4. Run shutdowns together in the application, keeping the store last.
5. Resolve the digest on the Mac in `scripts/deploy-app-nas`, deploy by digest
   through `deploy/compose.app.yaml`, and update `docs/operations.md` (the
   one-time `gh` scope, the deploy description, rollback by tag and digest),
   `deploy/nas.env.example`, and the deployment row in `docs/architecture.md`.

## Files affected

- `internal/schedule/schedule.go`, `internal/schedule/reminder.go`,
  `internal/schedule/service.go`, and a new scheduler test.
- `internal/connectors/places/describe.go` and its tests.
- `internal/conversation/service.go` and a new conversation test.
- `internal/app/app.go` and a new shutdown test.
- `scripts/deploy-app-nas`, `deploy/compose.app.yaml`, `deploy/nas.env.example`,
  `docs/operations.md`, `docs/architecture.md`.

## Validation

- A scheduler test where one principal's model turns are held: another
  principal's reminder and model turn are delivered meanwhile, the held
  principal occupies one slot, and their second turn starts once the first
  frees its slot, with no tick due.
- A places test where a failed reverse geocode spares the next point a request
  and a request is made again after the minute.
- A conversation test where the message after a cancelled delivery is answered
  well within the old five-second wait.
- A shutdown test where a service behind a stuck one is told to stop and
  finishes within the budget.
- The usual formatting, module, vet, build, test, and plan checks; the
  deployment script's syntax and its registry resolution against a public image.
- The first real deployment after this change confirms the digest resolution
  with the operator's credentials and the NAS pull by digest.

## Risks and open questions

- The Mac's `gh` login needs the `read:packages` scope once; without it the
  script stops before touching the NAS and names the command that adds it.
- With one bounded run per principal, a principal's Trips scan, which does not
  wait for their chat, can now wait behind their own scheduled model turn. Both
  are catch-up jobs measured in hours, so this was accepted.
- A rollback now pins both the tag and the digest from `deployments.log`; a
  commit that was never deployed has no recorded digest and is deployed through
  the script instead.
