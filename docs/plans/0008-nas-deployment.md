# 0008 — NAS deployment of monitoring and the application

Status: `in-progress`

## Context

[0001](0001-telegram-observability-nas-foundation.md) delivered the Compose
stack, the Mac-driven deployment script, private image publishing, and the
operator runbook, but nothing has been deployed. On 2026-09-06 the NAS
refused SSH, no `deploy/.env.nas` exists, the Grafana secrets are not in
Keychain, and Docker Desktop was not running. The operator now wants the NAS
to be self-contained: the repository build context and Dockerfiles should be
placed on the NAS and built there, with no application image stream from the
Mac.

The product has moved on since 0001: ordinary text goes to a locked-down Pi
runtime under each user's own subscription, connected through `/settings`.
The production image does not contain Pi and the Compose file does not
enable conversation, so deploying as-is would ship only `/status` and
`/privacy`. Monitoring and the application were incorrectly coupled in one
Compose project, and the deployment plan enabled only a single Telegram user
even though the application already supports a validated one- or two-user
configuration file. The NAS build must include the Node/Pi runtime and the
complete Go build context.

Verified on 2026-09-06: Pi 0.84.1 requires Node 22.19 or later;
`node:24.19.0-alpine3.24` exists on Docker Hub; the pinned observability
image tags exist; Compose runs the application as the NAS account's UID and
GID, which has no passwd entry in the image, so the Pi adapter's home
directory must be set explicitly. The NAS is `x86_64` with Docker and Compose available, enough CPU and memory,
and free space on the NAS volume. Docker stores its
internal state below `/volume1/@docker`. The deployment data root will use the
existing NAS OS `docker` shared folder (for example `/volume1/docker`) alongside
other apps, rather than creating another top-level share, using the operator's
personal home directory, or writing inside Docker's internal directory.

## Goal

Alloy, Prometheus, Tempo, and Grafana run as an independent monitoring project
on the NAS and Grafana is verified on the trusted LAN. The NAS builds the
application image from checked-out source in a separate application project,
which joins a shared observability network and runs with conversation enabled
for all configured household users. The operator connects each user's own
subscription through the NAS bot's `/settings`, and the local GoLand poller is
stopped for that token. Every step is an explicit operation from the Mac, and
rollback and backup paths exist independently for monitoring and application
state.

## Decisions

- Deployment stays manually initiated from the Mac, but the NAS owns the
  application build. The app script copies the checked-out Dockerfile, Go
  sources, and deployment files into a new NAS release directory and runs the
  `linux/amd64` Docker build there. No application image is streamed to or
  pulled from GHCR during deployment.
- `scripts/deploy-monitoring-nas` manages an independent release root (for
  example `/volume1/docker/monitoring`). It copies only monitoring
  configuration, syncs the Grafana secrets, opens one interactive SSH terminal
  for the four pinned image pulls and `docker compose up -d`, and updates the
  monitoring `current` symlink. It has no application image, Telegram
  settings, or application secrets.
- `scripts/deploy-app-nas` manages a separate release root (for example
  `/volume1/docker/home-ai-assistant`). It copies the application build
  context and multi-user configuration, syncs the Telegram secret, opens one
  interactive SSH terminal for the NAS-local build and `docker compose up -d`,
  and updates the application `current` symlink. Application deployment never
  changes the monitoring Compose project.
- The image packages Pi. The Go build stage is unchanged; the runtime stage
  becomes `node:24.19.0-alpine3.24` with
  `@earendil-works/pi-coding-agent@0.84.1` installed globally at build time
  and `pi --version` asserted during the build. Non-root user, read-only
  root filesystem, dropped capabilities, and the health check stay.
- Alloy, Prometheus, Tempo, and Grafana remain pinned upstream images pulled by
  Docker on the NAS. They do not need custom Dockerfiles; maintaining local
  copies of those vendor images would add no control boundary and would make
  security updates harder to consume. Alloy runs as the pinned image's UID/GID
  473 and its bind-mounted data directory is repaired to that ownership because
  all Linux capabilities are dropped; Prometheus, Tempo, and Grafana use the
  verified NAS UID/GID. Tempo 3 retention is configured under
  `backend_worker.compaction`.
- The Compose definitions allow the monitoring stage to run without starting
  or building `home-agent`; the application stage supplies its locally built
  image and Telegram secret afterward. The stages share the private Docker
  network and persistent observability directories. Use separate
  `deploy/compose.monitoring.yaml` and `deploy/compose.app.yaml` files so the
  monitoring stage does not need an application image or Telegram secret.
- The deployment script ensures the private `home-observability` network
  exists, and both Compose projects declare it as external. There is no
  cross-project `depends_on`; Alloy receives generic OTLP signals keyed by
  `service.name`, so future applications can send metrics and traces without
  changing the monitoring stack.
- Compose enables conversation with the pinned provider, model, and Pi
  version, passes the Node and Pi binary paths, sets `HOME` to the data
  directory so Pi's per-principal directories and caches stay on the mounted
  volume, raises the application tmpfs to 64m for Node, and raises the
  memory limit to 1g and the pids limit to 256 for the Node child process.
  These limits are starting points to be tuned from Grafana after the first
  day of measurements.
- The first deployment uses `HOME_AGENT_TELEGRAM_USERS_FILE` with one or two
  entries immediately. Each entry has a distinct Telegram user ID, principal
  ID, and display name. The file is mounted read-only from the application
  config directory; the legacy single-user environment variable is not used
  in the NAS deployment.
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
   verify its host key, run `scripts/nas-preflight` with its interactive sudo
   prompt, create the NAS directories, write `deploy/.env.nas`, create the
   multi-user Telegram users file, and store Grafana secrets in Keychain. GHCR
   login is not needed for the NAS-native application build; Docker Hub access
   is needed for the pinned monitoring images. Complexity: simple, but only
   the operator can do it.
2. Repository changes: separate deployment scripts that copy source and build
   on the NAS, independent monitoring and application Compose definitions,
   generic Alloy configuration, Dockerfile with Pi, multi-user environment
   example, runbook, and README. Complexity:
   complex. Validate the Docker build and runtime checks locally where
   possible, render both Compose stages with a sample environment, and verify
   the NAS build records the checked-out revision.
3. Push the source changes and run only `scripts/deploy-monitoring-nas`.
   Verify Alloy, Prometheus, Tempo, and Grafana readiness, provisioned data
   sources, dashboards, the external network, and the LAN Grafana URL before
   touching the application.
4. Stop the GoLand poller for the production bot token, run
   `scripts/deploy-app-nas`, and let the NAS build the checked-out revision.
   Verify both configured users can reach `/status`, `/privacy`, and
   `/settings`, connect each subscription independently, and complete the
   ordinary-message, `/cancel`, and `/new` smoke tests.
5. Record measurements, the selected source revision, release symlink,
   rollback snapshot, and backup/restore state.

## Files affected

- `Dockerfile`, `deploy/compose.monitoring.yaml`, `deploy/compose.app.yaml`,
  `deploy/nas.env.example`, `deploy/telegram-users.example.json` (replace the
  combined `deploy/compose.yaml`)
- `scripts/deploy-monitoring-nas`, `scripts/deploy-app-nas`
- `docs/13-operator-runbook.md`, `docs/07-deployment-and-network.md`,
  `README.md`

## Validation

- The NAS builds the image for `linux/amd64`, reports Pi 0.84.1, and answers an
  RPC `get_state` as the non-root user with a read-only root filesystem.
- The monitoring Compose project starts and verifies without any application
  image, Telegram user ID, Telegram token, or application data directory.
- A second application can join `home-observability` and emit OTLP metrics and
  traces without changing the monitoring Compose project.
- Monitoring stage: the four services report ready, Grafana shows the
  provisioned Prometheus and Tempo data sources and the Home Agent
  dashboard, and `current` points at the release.
- Application stage: `/status`, `/privacy`, and `/settings` reply; the Connect
  button completes the device-code ceremony from the NAS; an ordinary message
  receives a model reply; `/cancel` and `/new` behave; the dashboard shows the
  turn; container logs contain no Telegram text, token, or credential.
- `docker stats` during a turn stays inside the configured limits with
  headroom; the `current` symlink and retained release exist on the NAS.
- The runbook steps match what was actually run.

## Implementation status

Repository implementation is complete and locally validated: both Compose
files render, the deployment scripts pass shell syntax checks, and Go tests,
race tests, vet, build, and plan validation pass. The NAS image build and live
monitoring/application smoke tests remain pending because they must run on
`home-nas` with the operator's Keychain secrets and Telegram accounts.

## Risks and open questions

- SSH credentials and the `home-nas` alias are configured, but the latest
  preflight received `connection refused` from `<nas-address>:22`; enable or
  restart the NAS OS SSH service before deployment.
- A NAS-native build needs enough temporary disk, memory, and outbound access
  to Docker Hub for Go modules, npm packages, and the pinned monitoring
  images. The runbook must show how to inspect and clean build cache without
  touching release data.
- Grafana dashboards that are specific to Home Agent must remain optional
  content; the monitoring project itself must stay usable when no application
  is running.
- Node inside the application container adds a few hundred megabytes during
  a turn on a small NAS shared with other apps; the limits are estimates until
  measured.
- Pi on Alpine's musl libc is unverified; if a dependency fails there, the
  fallback is the Debian slim Node image at the cost of image size.
- The device-code ceremony and token refresh from the NAS use outbound
  HTTPS only, but have not run headless outside the Mac.
- Deploying the NAS bot ends local GoLand testing with the same token; a
  development bot is the likely follow-up.
