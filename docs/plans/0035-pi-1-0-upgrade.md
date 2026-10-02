# 0035 — Upgrade Pi to 1.0.0

Status: `done`

## Context

The image and the operator's Mac pin Pi `0.84.1`. Pi `1.0.0` was released on 2026-10-01, after
0.84.2–0.99.2. Every turn starts a fresh `pi --mode rpc` process with only the application's tool
bridge and, for some users, the vendored `pi-web-search` extension. Built-in tools and every
discovered extension are turned off, and each principal has its own agent directory.

What 1.0 changes for this application, from its changelog, docs and `npm pack` contents:

- **Prompt acknowledgement:** the RPC `prompt` response now carries `data.disposition`
  (`started`, `queued` or `handled`). With `handled`, no run starts and no `agent_settled` follows.
- **Built-in extensions:** codemode, `tool_search`, MCP and llama.cpp now ship as built-in
  extensions. The docs say `--no-extensions` disables them, while explicit `-e` paths still load.
- **Classifier models:** Pi includes TypeSafe's `jev-latest` behind `TYPESAFE_API_KEY`.
- **Openai-codex provider:** now labelled "legacy", but still uses the same client id and
  device-code endpoints as `internal/connection/device.go`.
  `gpt-5.6-luna` is still in its catalog.
- **Startup:** the CLI calls `module.enableCompileCache()`. With our environment the cache lands in
  `/tmp/node-compile-cache`, which is a 64 MB tmpfs in the container.
- **Cache warming** defaults to `streaming`. It runs only for models that declare a prompt-cache
  lifetime, and no openai-codex model does.
- **`--provider` without `--model`** now fails. `HOME_AGENT_PI_MODEL` is already required.
- **Event shapes:** `turn_end` and other extension events gained fields, and retry and compaction
  handling changed. The adapter and the Pi fixture rely on the 0.84.1 event order for parallel tool
  batches and retries.
- **Extension APIs:** the APIs used by `pi-web-search` 1.6.0 still exist (`modelRegistry.find`,
  `getAvailable`, `getApiKeyAndHeaders`). No newer `pi-web-search` release exists.
- **Session files:** still version 3, but since 0.86 the first request stores the full prompt and
  tool declarations as a `system` message, and later prompt or tool changes are stored as patches.
  Providers that cannot represent a change get a full transcript checkpoint, which can invalidate
  the cached prefix. Our system prompt and tool set vary between turns. 0.84.1 has never seen these
  entries, so a rollback after 1.0 has written sessions may not resume them. Files written by
  0.84.1 must still resume (plan 0015).
- **Failed first turns:** the session file is now created when the first user message is sent, so
  a turn that fails before any reply may leave a dangling user message for the next turn to resume.
- **Offline and retry settings:** `PI_OFFLINE` and `PI_SKIP_VERSION_CHECK` stop catalog refreshes
  and the `pi.dev` version request. `retry.maxRetries` and `retry.maxAgentDelayMs` (60 s by
  default) bound Pi's own retries.
- **Runtime and login:** Node `>=22.19.0` is required, and the image uses 24.19.0. The openai-codex
  `auth.json` record is unchanged.

## Goal

- Every turn runs on Pi 1.0.0, locally and on the NAS, with no change users notice beyond the
  upgrade itself.
- Existing sessions, logins, tools, attachments, web search, retries and model-call metrics keep
  working.
- A prompt that Pi handles without starting a run fails at once instead of waiting for the turn
  timeout.
- Turn start time (process start to prompt acknowledgement) is not worse than on 0.84.1.
- A rollback to the previous image restores working sessions.

## Decisions

- **Keep `internal/judgment` and call TypeSafe from Go.** Pi's classifier runs inside the
  per-turn Node process, but the intent check runs after the turn and screening and hints rely on
  Go-side redaction, opt-in and exposure state. Pi's TypeSafe catalog only offers `jev-latest`,
  while 0029 pins `jev-1.13.0`.
- **Never give Pi a TypeSafe key.** The restricted child environment stays as it is, so
  household data can reach TypeSafe only through the redactor.
- **Keep built-in extensions off** with the existing `--no-extensions`, and verify it live instead
  of trusting the docs.
- **Adopt from 1.0 only what is free or defensive in this plan:** the compile cache, the
  disposition guard, and, if C3 shows a gain, `PI_OFFLINE` and `PI_SKIP_VERSION_CHECK` in the
  restricted child environment. Deferred tool loading (`exposure: "deferred"` with
  `tool_search`) and a model change are separate decisions, listed below.
- **Stay on the openai-codex provider.** The ChatGPT sign-in on the `openai` provider needs a
  browser callback and a device id, and has no headless device-code flow that fits `/connect`.

## Approach

1. **C1 Version pins** (Jev: simple 0.99 → Haiku):
   - Change `0.84.1` to `1.0.0` in the `Dockerfile` install line and its `--version` check, in
     `defaultPiVersion`, `.env.example`, `deploy/nas.env.example`, and the
     `deploy/compose.app.yaml` default.
   - Done plans are immutable and keep their version mentions.
2. **C2 Adapter: prompt disposition** (Jev: complex 0.96 → Sonnet):
   - In `internal/runtime/pi/runtime.go`, read `data.disposition` from the `prompt` response.
     Anything other than `started` ends the turn at once with an error that contains no message
     text. A missing field (older Pi) is accepted.
   - The fixture's acknowledgement sends `data.disposition: "started"`, as 1.0 does.
   - If C3 shows the cache does not fit in `/tmp`, or is lost too often to help, pass
     `NODE_COMPILE_CACHE` pointing under the data directory through `restrictedEnvironment`.
     Otherwise leave the environment alone. Add `PI_OFFLINE` and `PI_SKIP_VERSION_CHECK` the same
     way if C3 shows they help.
   - Correct the fixture's event order where C3 found differences. C1 and C2 land in one commit,
     because the app checks the exact Pi version.
3. **C3 Live compatibility check** (Jev: complex 0.79; done by the orchestrator, because it needs
   the operator's subscription, a local Pi 1.0 install and coordination with the GoLand server):
   - Install 1.0.0 into a scratch `npm --prefix`, never globally, so the GoLand server keeps
     running on 0.84.1.
   - Drive `pi --mode rpc` 1.0.0 directly, with a copy of a principal's agent and session
     directories and the application's real arguments, and record the event streams for:
     - a plain reply;
     - a parallel tool batch through a bridge extension;
     - an image prompt through the real attachment path;
     - resuming a session file written by 0.84.1;
     - resuming across a tools-to-no-tools turn and a web-search toggle, checking that the request
       carries the current prompt and tools and comparing `cacheRead` before and after;
     - a first turn that fails (for example a rejected credential), followed by a normal turn,
       to see what the failed turn leaves in the session;
     - a forced retry, through a local stub provider returning 529, to observe the retry order.
   - Compare them with the fixture's assumptions: the parallel batch order and the retry order.
     Correct the fixture where 1.0 differs.
   - Confirm the provider request offers only the bridge and web-search tools: no `codemode`,
     `tool_search` or MCP.
   - Re-run the live web-search check from plan 0026.
   - Time process start to the prompt acknowledgement on 0.84.1 and 1.0.0, with a cold and a warm
     compile cache, with and without `PI_OFFLINE`, and note the cache size.
   - Note how much larger a session file gets, and confirm the stored system prompt holds no
     household data.
   - Run `pi auth check --provider openai-codex` against an existing login.
4. **C4 Docs and deployment** (Jev: simple 0.63, below the threshold; done by the orchestrator
   with the operator, because NAS `sudo` is interactive):
   - Note in `docs/operations.md` that Pi 1.0 must keep built-in extensions off and must not
     receive a TypeSafe key.
   - Upgrade the Mac's global Pi together with a coordinated GoLand restart.
   - Before deploying, record the running image digest and copy the principals' Pi agent and
     session directories on the NAS. Rollback is the previous image plus that copy, which loses
     only conversation since the deploy.
   - Tag, build and deploy a pinned image, and record its digest.
   - Compare the 24 hours after the deploy with the 24 hours before in Grafana. Pass: no new
     failure outcomes in `home_agent.model.calls`, no unfinished model calls (0034), turn-latency
     p95 no more than 10% worse, and container `/tmp` usage under 50%.

Waves: C3 first, then C1 and C2 together with C3's fixture corrections, then C4.

## Files affected

- `Dockerfile`, `deploy/compose.app.yaml`, `deploy/nas.env.example`, `.env.example`
- `internal/config/conversation.go`
- `internal/runtime/pi/runtime.go`
- `internal/testutil/pifixture/fixture.go`
- `docs/operations.md`, `docs/plans/README.md`, this plan

## Validation

- `gofmt`, the module check, `go vet`, `go test ./...`, `go build`, `scripts/check-plans`.
- The live compatibility results above, recorded here.
- After deployment: one chat turn, one tool turn, one image turn, one web search, and one
  scheduled turn on the NAS, then the 24-hour Grafana comparison in C4.

## Risks and open questions

- **Rollback** is the previous image, with the pre-deploy copy of the session directories as a
  precaution.
- **Prompt changes now accumulate in the session.** If turn tokens grow
  noticeably, a follow-up could keep the system prompt identical across tool sets.
- **The openai-codex provider is now "legacy"** and may be dropped in a later Pi release. Moving
  `/connect` to another login is a separate plan.
- **No test for the disposition guard,** decided 2026-10-02 under the rule not to add tests unless
  asked. The fixture's acknowledgement carries `started`, so existing tests cover the normal path.
- **Follow-ups, not in this plan:**
  - Deferred tool loading for the roughly 25 broker tools, which interacts with the Jev tool hint
    and prompt caching.
  - Moving to a newer Codex model, such as `gpt-6-luna`.
  - Lining up `retry.maxRetries` and `retry.maxAgentDelayMs` with the turn timeout.
  - Returning `isError` results from the bridge instead of throwing.
