# 0012 — Pull the application image from GHCR by digest

Status: `done`

## Context

[0008](0008-nas-deployment.md) moved the application build onto the NAS:
`scripts/deploy-app-nas` copies the Go sources and Dockerfile into a release
directory and runs `docker compose build` there. Both stages were deployed on
2026-09-06, but the NAS build is slow, duplicates what CI already does, and
produces an image that cannot be identified or verified by digest.

`.github/workflows/image.yml` already validates the source and publishes a
`linux/amd64` image to the private package
`ghcr.io/vladar107/home-ai-assistant` with the tag `sha-<full commit>` on every
push to `main`. The operator wants the NAS to run that CI-built image instead
of building locally, which restores the "digest-selected" boundary from
[0001](0001-telegram-observability-nas-foundation.md).

Verified on 2026-09-06: the package is private; the operator has already
logged the NAS Docker daemon into GHCR (root's Docker configuration, used by
`sudo docker`; no user-level configuration exists); the operator's `gh` OAuth
token has no `read:packages` scope, so the Mac cannot query the registry; the
NAS requires an interactive `sudo` password for Docker.

## Goal

`scripts/deploy-app-nas` deploys the CI-published image for the checked-out
commit without building on the NAS and without streaming an image from the
Mac. The release records the pulled image by registry digest so the running
container and every retained release can be identified and rolled back.

## Decisions

- Registry access is the existing root Docker login on the NAS. No registry
  token is stored in Keychain or synchronized by the deployment scripts. If
  the login is missing or expired, the pull fails and the script prints the
  `sudo docker login ghcr.io` instruction.
- The image repository is a non-secret setting in `deploy/.env.nas`:
  `HOME_AGENT_IMAGE_REPOSITORY=ghcr.io/vladar107/home-ai-assistant`.
  `HOME_AGENT_IMAGE` and `HOME_AGENT_VERSION` are derived by the script, as
  today, and are ignored if present in the environment file.
- The script requires the checked-out `HEAD` to be contained in
  `origin/main` after a fetch, because only pushed commits have a CI image.
  The application sources no longer need to be clean; only
  `deploy/compose.app.yaml` is copied into the release and must be committed.
- The NAS selects and records the digest: inside the existing interactive
  `sudo` terminal the script pulls `<repository>:sha-<HEAD>`, reads the
  matching `RepoDigests` entry, rewrites `HOME_AGENT_IMAGE` in the release's
  `runtime.env` to `<repository>@sha256:<digest>`, and only then runs
  `docker compose up -d` and `ps`. A pull failure for an unknown manifest
  prints the `gh run list --workflow=image.yml --commit <HEAD>` hint.
- `deploy/compose.app.yaml` drops the `build` block and `pull_policy: never`;
  the `image` reference keeps `platform: linux/amd64`.
- The Dockerfile, CI workflow, and monitoring stage are unchanged. Plan 0008
  stays `in-progress`; its build-on-NAS decision is superseded by this plan.

## Approach

1. Rewrite the image selection in `scripts/deploy-app-nas`: push check,
   minimal archive, `runtime.env` with the tag reference, remote pull, digest
   capture, `runtime.env` rewrite, `up -d`. Complexity: complex.
2. Update `deploy/compose.app.yaml` and `deploy/nas.env.example`; add the
   repository setting to the operator's local `deploy/.env.nas`.
3. Update `docs/13-operator-runbook.md` §7, `docs/07-deployment-and-network.md`
   build flow, README where it describes the NAS build, and add the
   cross-reference to 0008.
4. Operator: push `main`, wait for the image workflow, run
   `scripts/deploy-app-nas`, and verify the running container reports the
   selected digest and commit.

## Files affected

- `scripts/deploy-app-nas`
- `deploy/compose.app.yaml`, `deploy/nas.env.example`
- `docs/13-operator-runbook.md`, `docs/07-deployment-and-network.md`,
  `README.md`, `docs/plans/0008-nas-deployment.md`

## Validation

- `bash -n` on the script; `docker compose config` renders
  `deploy/compose.app.yaml` with a sample environment and a digest reference.
- With an unpushed commit the script fails before touching the NAS.
- Live: `scripts/deploy-app-nas` pulls the image, `docker compose ps` shows the
  service healthy, the release `runtime.env` holds the digest reference,
  Grafana shows `service.version` equal to the deployed commit, and `/status`
  replies.
- `scripts/check-plans` passes.

## Risks and open questions

- The root Docker login on the NAS is outside the repository; token rotation
  and expiry are operator tasks.
- GHCR pulls need outbound HTTPS from the NAS to `ghcr.io` and
  `pkg-containers.githubusercontent.com`.
- Rollback to a previous release re-pulls by digest if the image was pruned,
  which also needs the login.
