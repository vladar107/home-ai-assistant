# 0008 — NAS deployment of monitoring and the application

Status: `proposed`

## Context

[0001](0001-telegram-observability-nas-foundation.md) delivered the Compose
stack, the Mac-driven deployment script, private image publishing, and the
operator runbook, but nothing has been deployed. On 2026-09-06 the NAS
refused SSH, no `deploy/.env.nas` exists, the Grafana secrets are not in
Keychain, and Docker Desktop was not running. CI and the production image
workflow succeed on every push to `main`, so an immutable `linux/amd64` image
tagged with the commit exists in private GHCR for the current head.

The product has moved on since 0001: ordinary text goes to a locked-down Pi
runtime under each user's own subscription, connected through `/settings`.
The production image does not contain Pi and the Compose file does not
enable conversation, so deploying as-is would ship only `/status` and
`/privacy`. `scripts/deploy-nas` also starts monitoring and the application
in one run, while the operator wants the monitoring stack verified first and
the application deployed as a second step.

Verified on 2026-09-06: Pi 0.84.1 requires Node 22.19 or later;
`node:24.19.0-alpine3.24` exists on Docker Hub; the pinned observability
image tags exist; Compose runs the application as the NAS account's UID and
GID, which has no passwd entry in the image, so the Pi adapter's home
directory must be set explicitly.

## Goal

Alloy, Prometheus, Tempo, and Grafana run on the NAS and are verified
through the SSH tunnel. Then the application runs on the NAS from an exact
GHCR digest with conversation enabled, the operator connects their
subscription through the NAS bot's `/settings`, and the local GoLand poller
is stopped for that token. Every step is an explicit operation from the
Mac, and rollback and backup paths exist for the application data.

## Decisions

- Deployment stays manual from the Mac using GHCR digests; no local image
  build path is added, because CI already publishes immutable images and the
  script verifies the image's revision label against the checked-out commit.
- `scripts/deploy-nas` gains stages. `scripts/deploy-nas monitoring`
  prepares a release directory, syncs secrets, pulls, starts, and verifies
  the four observability services, and points `current` at the release.
  `scripts/deploy-nas app <digest>` does the same and then streams the
  image, snapshots the data directory, starts the application, and requires
  readiness plus a metric and a trace before updating `current`. Rollback
  restarts the previous release's application only when that release
  recorded an image; otherwise it restores monitoring alone.
- The image packages Pi. The Go build stage is unchanged; the runtime stage
  becomes `node:24.19.0-alpine3.24` with
  `@earendil-works/pi-coding-agent@0.84.1` installed globally at build time
  and `pi --version` asserted during the build. Non-root user, read-only
  root filesystem, dropped capabilities, and the health check stay.
- Compose enables conversation with the pinned provider, model, and Pi
  version, passes the Node and Pi binary paths, sets `HOME` to the data
  directory so Pi's per-principal directories and caches stay on the mounted
  volume, raises the application tmpfs to 64m for Node, and raises the
  memory limit to 1g and the pids limit to 256 for the Node child process.
  These limits are starting points to be tuned from Grafana after the first
  day of measurements.
- The first deployment keeps the single-user allowlist through
  `HOME_AGENT_TELEGRAM_USER_ID`; the second household user is added later
  through the users file mounted read-only.
- NAS state starts fresh. No local data, sessions, or credentials are copied;
  the operator connects the subscription again on the NAS through
  `/settings`. Only one poller may hold the bot token, so the GoLand run is
  stopped before the application stage; a separate development bot token for
  local work is a later decision.
- Grafana admin password and secret key are created in Keychain before the
  monitoring stage. The Hearthwatch alert bot and the first alert rule follow
  once a baseline is visible, as the runbook already describes.
- The runbook is updated for the stages, the SSH enablement precondition,
  Pi in the image, and command registration by the application, which
  replaced the manual BotFather command list in [0005](0005-bot-commands-and-provider-failures.md).

## Approach

1. Operator prerequisites outside the repository: enable SSH on the NAS and
   verify its host key, run `scripts/nas-preflight`, create the NAS
   directories, write `deploy/.env.nas`, store the Grafana secrets in
   Keychain, start Docker Desktop, and log Docker in to GHCR. Complexity:
   simple, but only the operator can do it.
2. Repository changes: staged deployment script, Dockerfile with Pi, Compose
   conversation settings, environment example, runbook, and README.
   Complexity: complex. Validated locally by building the image for
   `linux/amd64`, running `pi --version` and an RPC `get_state` round trip
   inside the container as the non-root user with a read-only root
   filesystem, and rendering the Compose file with a sample environment.
3. Push, let CI publish the image, and run the monitoring stage; verify
   Grafana data sources and the dashboard through the tunnel.
4. Run the application stage with the digest of the commit that carries
   these changes; run the Telegram smoke test; record memory and CPU for the
   container during a turn.
5. Record results, measurements, and the rollback and backup state.

## Files affected

- `Dockerfile`, `deploy/compose.yaml`, `deploy/nas.env.example`
- `scripts/deploy-nas`
- `docs/13-operator-runbook.md`, `docs/07-deployment-and-network.md`,
  `README.md`

## Validation

- The image builds for `linux/amd64`, reports Pi 0.84.1, and answers an RPC
  `get_state` as the non-root user with a read-only root filesystem.
- Monitoring stage: the four services report ready, Grafana shows the
  provisioned Prometheus and Tempo data sources and the Home Agent
  dashboard, and `current` points at the release.
- Application stage: readiness, metric, and trace gates pass; `/status`,
  `/privacy`, and `/settings` reply; the Connect button completes the
  device-code ceremony from the NAS; an ordinary message receives a model
  reply; `/cancel` and `/new` behave; the dashboard shows the turn; container
  logs contain no Telegram text, token, or credential.
- `docker stats` during a turn stays inside the configured limits with
  headroom; the pre-upgrade snapshot and the `current` symlink exist on the
  NAS.
- The runbook steps match what was actually run.

## Risks and open questions

- SSH is currently refused by the NAS; nothing proceeds until it is enabled
  through the trusted NAS interface.
- Node inside the application container adds a few hundred megabytes during
  a turn on a small NAS shared with other apps; the limits are estimates until
  measured.
- Pi on Alpine's musl libc is unverified; if a dependency fails there, the
  fallback is the Debian slim Node image at the cost of image size.
- The device-code ceremony and token refresh from the NAS use outbound
  HTTPS only, but have not run headless outside the Mac.
- Deploying the NAS bot ends local GoLand testing with the same token; a
  development bot is the likely follow-up.
