# 0022 — Monitoring stack in its own repository with a single deployment directory

Status: `in-progress`

## Context

The NAS monitoring stack (Alloy, Prometheus, Tempo, Grafana) lives inside this
repository under `deploy/`: `compose.monitoring.yaml`, the four service
configurations, the Grafana provisioning files and three dashboards, and
`scripts/deploy-monitoring-nas`. It shares `deploy/nas.env.example`,
`scripts/nas-sync-secrets`, and `scripts/keychain-set-secret` with the
application, each of which branches on a `monitoring|app` stage or on the
secret name.

The stack is not application code. It is deployed first and independently, it
already scrapes the photo library, and it is meant to serve future applications that join
the `home-observability` network. Its change cadence is upstream image bumps
and dashboard edits, which today land between application commits and share
the application's plan governance.

[0018](0018-tag-pinned-deployment.md) removed the release-directory layout for
the application and deliberately left monitoring on it. Every monitoring
deployment still creates `$MONITORING_ROOT/releases/<timestamp>-<sha16>/deploy`,
flips the `current` symlink, writes `current.release`, and never prunes. Every
runbook command for monitoring reaches through `current/deploy` and passes
`--env-file runtime.env -f compose.monitoring.yaml`.

## Goal

`~/src/home-monitoring` is a standalone Git repository that holds one
`compose.yaml`, the service configurations, the dashboards, and its own
deployment script. `scripts/deploy` replaces `$MONITORING_ROOT/deploy/` on the
NAS with the committed files and restarts the Compose project in place, with no
release directories and no symlinks. This repository keeps only the application
deployment and points to the monitoring repository wherever the runbook used to
describe monitoring.

## Decisions

- The repository is named `home-monitoring`. It joins `home-network` and
  `home-ai-assistant` as the third `home-*` repository, it is already the
  Compose project name the NAS runs (`-p home-monitoring`), and the NAS
  directory is `/volume1/docker/monitoring`. `home-observability` stays the
  name of the shared Docker network only; renaming it would touch the
  application Compose file for no gain.
- The new repository starts from a fresh `git init`. Monitoring history here
  is seven commits interleaved with application work; the first commit message
  in the new repository names the source commit (`b2bc1b4`) so the lineage is
  recorded without rewriting history.
- Repository layout: `compose.yaml`, `alloy/`, `prometheus/`, `tempo/`,
  `grafana/provisioning/`, `grafana/dashboards/`, `scripts/`, `nas.env.example`,
  `README.md`, `CLAUDE.md`, `.gitignore`. The ignored operator file is
  `.env.nas` at the repository root. Relative bind-mount sources in the Compose
  file do not change, so the archive extracts straight into the NAS deployment
  directory.
- `compose.yaml` declares `name: home-monitoring`, so the deploy script and the
  runbook stop passing `-p`. The running containers belong to that project
  already and are adopted, not renamed.
- The NAS environment file is installed as `deploy/.env`, which Compose reads
  without flags. Operator commands become
  `cd /volume1/docker/monitoring/deploy && sudo docker compose ps`. This
  differs from the application's `runtime.env`; the application keeps its
  name because its file is derived, not copied.
- `scripts/deploy [ssh-host] [env-file]` keeps the guards from
  `deploy-monitoring-nas`: mode-`0600` environment file, no secret values in
  it, numeric UID/GID, safe absolute paths below `MONITORING_ROOT`, and no
  uncommitted changes in `compose.yaml`, `alloy`, `prometheus`, `tempo`, or
  `grafana`. It does not require a Git remote.
- Deployment stages `git archive HEAD` of those five paths plus the
  environment file into `$MONITORING_ROOT/deploy.next`, then in the interactive
  NAS terminal repairs directory ownership as before, ensures the
  `home-observability` network, swaps `deploy.next` into `deploy/`, runs
  `pull` from the staging directory before the swap, then
  `up -d --force-recreate` and `ps`, and appends
  `<utc-timestamp> <commit>` to `$MONITORING_ROOT/deployments.log` (mode
  `0600`). `--force-recreate` preserves today's behaviour, where a fresh
  release path recreated every service; without it an edited Prometheus or
  Alloy configuration would not be picked up.
- Rollback is `git checkout <commit>` followed by `scripts/deploy`; the
  repository is the release history and `deployments.log` names the targets.
- The Alloy data directory is still owned as UID/GID 473 by the deploy script,
  and extracted configuration files keep mode `0644` inside the mode-`0700`
  root so Alloy can read its configuration through the bind mount.
- Keychain: the new repository uses account `home-monitoring` for
  `grafana.admin-password` and `grafana.secret-key`, the two secrets its
  `scripts/sync-secrets` materializes. A one-time operator step copies both
  values from the `home-ai-assistant` account. `grafana.telegram-alert-bot-token`
  and `grafana.telegram-alert-chat-id` stay under `home-ai-assistant`, because
  `scripts/telegram-list-users alerts` reads them and Grafana only receives
  them by paste.
- `scripts/nas-preflight` is copied into the new repository so it can be
  prepared and deployed without this one.
- The new repository does not adopt numbered plans; its `README.md` documents
  purpose, layout, deployment, verification, and rollback, and a short
  `CLAUDE.md` carries the operating rules that apply to it (manual tag-pinned
  deploys, Keychain secrets, private endpoints, `ssh -tt home-nas` with `sudo`).
- No GitHub remote is created by this plan. When wanted,
  `gh repo create vladar107/home-monitoring --private --source . --push` adds
  one; nothing in the deploy path depends on it.
- In this repository, `scripts/nas-sync-secrets` loses its stage argument and
  becomes application-only, `scripts/keychain-set-secret` drops
  `grafana.admin-password` and `grafana.secret-key`, `deploy/nas.env.example`
  drops the monitoring keys, and `scripts/deploy-app-nas` calls the trimmed
  secrets script. `scripts/deploy-app-nas` still requires the
  `home-observability` network to exist, which is now created by the other
  repository's deploy.
- Dashboards move with the stack. A metric change that needs a dashboard
  change now spans two repositories; the application commit references the
  monitoring commit in its message.
- The `home-ai-assistant` side of this plan is committed after
  [0021](0021-nas-file-access.md), which is in progress with uncommitted
  changes to `docs/13-operator-runbook.md` and `deploy/nas.env.example`.
  `scripts/check-plans` also requires 0021 to exist in history before 0022.
  Creating and deploying the new repository does not depend on that ordering.

## Approach

1. Create `~/src/home-monitoring`, `git init` with `main`, and add
   `.gitignore`, `compose.yaml` (renamed from `compose.monitoring.yaml`, with
   `name: home-monitoring`), `alloy/`, `prometheus/`, `tempo/`, `grafana/`,
   and `nas.env.example` holding only `NAS_RUNTIME_UID`, `NAS_RUNTIME_GID`,
   `MONITORING_ROOT`, the four data paths, `GRAFANA_SECRETS_PATH`,
   `GRAFANA_PORT`, and `GRAFANA_BIND_ADDRESS`. Complexity: simple.
2. Add `scripts/deploy`, `scripts/sync-secrets`, `scripts/keychain-set-secret`
   (Grafana secrets only, account `home-monitoring`), and
   `scripts/nas-preflight`. Complexity: complex.
3. Write `README.md` and `CLAUDE.md`. Commit as one signed conventional commit
   whose body names the source commit. Complexity: simple.
4. Operator, from the new repository: copy the two Keychain items to the
   `home-monitoring` account, create `.env.nas` from the monitoring keys of
   this repository's `deploy/.env.nas`, run `scripts/deploy home-nas`, verify,
   then remove `releases/`, `current`, and `current.release` under
   `/volume1/docker/monitoring` by hand.
5. In this repository, after 0021 is committed: delete
   `deploy/compose.monitoring.yaml`, `deploy/alloy`, `deploy/prometheus`,
   `deploy/tempo`, `deploy/grafana`, and `scripts/deploy-monitoring-nas`; trim
   `scripts/nas-sync-secrets`, `scripts/keychain-set-secret`,
   `scripts/deploy-app-nas`, and `deploy/nas.env.example`; update `CLAUDE.md`,
   `README.md`, `docs/07-deployment-and-network.md`,
   `docs/08-observability-and-operations.md`, `docs/09-technology-decisions.md`
   (new decision D-033), and `docs/13-operator-runbook.md` (§2 Hearthwatch
   commands unchanged, §5 directory list, §6 replaced by a pointer plus the
   verification commands, §8 dashboard path, §9 Keychain account for the
   admin password, §10 monitoring status command, §11 Grafana backup note).
   Mark this plan `done` and commit. Complexity: simple.

## Files affected

New repository `~/src/home-monitoring`:

- `compose.yaml`, `alloy/config.alloy`, `prometheus/prometheus.yml`,
  `tempo/tempo.yaml`, `grafana/provisioning/**`, `grafana/dashboards/*.json`
- `scripts/deploy`, `scripts/sync-secrets`, `scripts/keychain-set-secret`,
  `scripts/nas-preflight`
- `nas.env.example`, `.gitignore`, `README.md`, `CLAUDE.md`

This repository:

- removed: `deploy/compose.monitoring.yaml`, `deploy/alloy/`,
  `deploy/prometheus/`, `deploy/tempo/`, `deploy/grafana/`,
  `scripts/deploy-monitoring-nas`
- changed: `scripts/nas-sync-secrets`, `scripts/keychain-set-secret`,
  `scripts/deploy-app-nas`, `deploy/nas.env.example`, `CLAUDE.md`,
  `README.md`, `docs/07-deployment-and-network.md`,
  `docs/08-observability-and-operations.md`,
  `docs/09-technology-decisions.md`, `docs/13-operator-runbook.md`,
  `docs/plans/README.md`

## Validation

- `bash -n` and `shellcheck` on every script in the new repository.
- `docker compose -f compose.yaml --env-file <sample> config` renders the four
  services under project `home-monitoring` with the expected bind sources.
- `scripts/deploy` refuses a group-readable environment file, a file carrying
  `GRAFANA_ADMIN_PASSWORD`, a path outside `MONITORING_ROOT`, and a modified
  dashboard, all before opening SSH.
- Live: `scripts/deploy home-nas` from the new repository recreates the four
  services from `/volume1/docker/monitoring/deploy`, `sudo docker compose ps`
  there shows them running, Grafana still lists the Prometheus and Tempo data
  sources and the three Home Agent dashboards, a `/status` command in Telegram
  appears on the Home Agent overview, and `deployments.log` holds the commit.
- Live: after removing `releases/`, `current`, and `current.release`, the
  services keep running and a second `scripts/deploy` is clean.
- In this repository: `gofmt`, `go mod tidy` diff, `go vet`, `go build`,
  `go test ./...`, `bash -n` on the trimmed scripts, `scripts/check-plans`,
  and `scripts/deploy-app-nas` still passing its pre-SSH checks.

Outcome of stages 1 to 4 (2026-09-09): the new repository's first commit is
`c2b8db3`, and the operator deployed it twice from
`/volume1/docker/monitoring/deploy` (19:28Z and 19:35Z in `deployments.log`).
Grafana 13.2.1 reports a healthy database, admin login works, the Prometheus
and Tempo data sources and the three Home Agent dashboards are provisioned,
and the alloy, grafana, prometheus, and tempo scrape targets are up; the photo-library
target was already down before this change. `releases/`, `current`, and
`current.release` are removed. During the Keychain migration the operator
re-entered both secrets under `home-monitoring` and deleted the
`home-ai-assistant` copies: `grafana.admin-password` is unchanged, while
`grafana.secret-key` is a new 64-character value and the original was not
retained. No contact point or other encrypted Grafana secret existed under the
old key, so nothing became unreadable. The Hearthwatch Keychain items
(`grafana.telegram-alert-bot-token`, `grafana.telegram-alert-chat-id`) exist
under neither account and must be stored before runbook §9. Stage 5 waits for
0021 as decided above.

## Risks and open questions

- Every deployment recreates all four services while the application keeps
  running. Alloy's OTLP export queue is in memory, so telemetry the application
  sends during the restart window is dropped and one scrape interval is
  missed. Counters recover on the next export; those traces are gone.
- Removing the `releases/` tree is a manual step after verification, as it
  was for the application in 0018. Until it runs, the old tree is inert.
- `--force-recreate` restarts Grafana on every dashboard change even though
  provisioning would have picked the change up without a restart. Accepted for
  a single-operator home deployment; a per-service restart rule can follow if
  it becomes annoying.
- Adding a GitHub remote, CI (compose config and shellcheck), and the
  `jbcontext` repository list entry are follow-ups outside this plan.
