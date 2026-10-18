# 0109 — One ordered lifecycle and a module registry without unused parts

Status: `done`

## Context

An architecture audit found that `internal/module` carries surface nothing
uses, and that the start and stop of the services is written out by hand in
several places:

- `module.Descriptor.Dependencies` is never declared, so `Registry.Ordered`'s
  dependency validation and topological sort have never had work: the order
  is always the registration order. `KindAutomation` and `KindEdgeWorker` mean
  nothing to the application; only the trip loop and the watch service use
  `KindAutomation`.
- Since plan 0107, `wiring.Assembly.Start` starts the model connections,
  approvals, conversation, scheduler, and background tasks, and on a failure
  shuts down the ones before it with a hand-written list per step.
  `internal/app` starts the modules, the assembly, and the Telegram service,
  and repeats the unwinding after each later stage and once more at the end of
  the run.

Shutdown is deliberately concurrent (`wiring.ShutdownTogether`); start is a
short data-flow order.

## Goal

The module registry keeps only what the application uses: registration,
validation, readiness through the modules, and the enabled module list in
registration order. The application starts its services from one ordered list
of steps, and whatever has started, at any point, stops together with one
concurrent shutdown. Start order, error texts, and the lifecycle telemetry
spans stay as they were.

## Decisions

- `KindAutomation`, `KindEdgeWorker`, `Descriptor.Dependencies`, the
  dependency validation, and the topological sort are deleted. The trip loop
  and the watch service register as `KindSystem`. `Registry.Ordered` keeps its
  name, returns the modules in registration order, and no longer returns an
  error, since nothing can fail any more.
- The lifecycle lives in `internal/wiring`, beside `ShutdownTogether`, because
  plan 0107 moved the core services' start and stop there and the Telegram
  test harness starts them through `Assembly.Start`. The design placed it in
  `internal/app`; the step list itself is unchanged.
- A `wiring.Step` names a service with its start and shutdown. A
  `wiring.Lifecycle` starts steps in order and remembers those that started;
  its `Shutdown` stops all of them at once through `ShutdownTogether`, together
  with any other shutdowns passed in. A step whose start failed is not counted
  as started and is not shut down, as `Assembly.Start` already did for the core
  services.
- `Assembly.Steps` lists the core services in their start order: model
  connections, approvals, conversation service, scheduler, and, when
  configured, background tasks. `Assembly.Start` and `Assembly.Shutdown` stay
  for the Telegram test harness and are written on the steps.
- `App.Run` keeps one `Lifecycle` for the run and starts three groups into it,
  each under its existing telemetry span: the modules (`modules_start`), the
  core steps (`conversation_start`), and the Telegram service
  (`telegram_start`). Modules still start before the conversation. A failure
  in any group, or of the HTTP listener, stops everything started so far with
  one shutdown under the configured shutdown budget and closes the store. The
  end of the run is one `Lifecycle.Shutdown` that also shuts down the HTTP
  server. The step labels keep the error texts: `start module "id"`,
  `start Telegram service`, `shutdown approvals`, and so on.
- The Telegram service is no longer shut down after its own start failed: its
  start returns before it launches the poller, so there is nothing to stop.
- Modules are no longer shut down in reverse start order; shutdown is
  concurrent, so the order had no effect.

## Approach

1. Delete the unused module kinds, the dependency field and checks, and the
   sort; move the two descriptors to `KindSystem`.
2. Add `wiring.Step` and `wiring.Lifecycle`; write `Assembly.Steps`, `Start`,
   and `Shutdown` on them.
3. Replace the hand-written start and unwind code in `internal/app` with the
   lifecycle: module, core, and Telegram steps, one abort path, and one final
   shutdown.
4. Add an application test: a module whose start fails leaves the module
   started before it shut down, and is not shut down itself.

## Files affected

- `internal/module/module.go`, `internal/module/registry.go`
- `internal/trip/loop.go`, `internal/watch/service.go`
- `internal/wiring/wiring.go`
- `internal/app/app.go`, `internal/app/app_test.go`

## Validation

- `gofmt`, `go mod tidy -diff`, `go vet`, `go build`, the race-enabled test
  suite, and `scripts/check-plans`.
- `TestShutdownTogetherStopsEveryServiceWithinTheBudget` keeps shutdown
  concurrent; the Telegram integration tests start and stop the assembly
  through the steps.

## Risks and open questions

- The turn-engine split from the same audit is a separate, later plan; this
  plan does not touch the conversation package.
