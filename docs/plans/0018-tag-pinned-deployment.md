# 0018 — Tag-pinned deployment without release directories

Status: `done`

## Context

[0008](0008-nas-deployment.md) introduced the NAS release layout and
[0012](0012-ghcr-image-deployment.md) moved the application image to GHCR.
Together they produce a deployment that is heavier than this project needs.

Every run of `scripts/deploy-app-nas` creates
`$HOME_AGENT_NAS_ROOT/releases/<utc-timestamp>-<sha16>/deploy/`, copies
`compose.app.yaml` and a derived `runtime.env` into it, flips the `current`
symlink and the `current.release` file, and never prunes. The directories hold
no state — the database, configuration, and secrets live in their own paths —
so the accumulating tree only records which Compose file and image reference
were used, and it makes every runbook command reach through `current/deploy`.

The published image tag is equally awkward. CI sets `flavor: latest=false` and
publishes exactly one tag per commit, `sha-<40-character commit>`. There is no
moving tag and no readable tag, so a rollback target can only be named by a
full commit hash found by hand. The repository has no Git tags at all.

Rollback today means locating a retained release directory, reading its
`runtime.env`, starting it, and re-pointing the symlink.

## Goal

The NAS holds one application deployment directory. A deployment is identified
by a readable, immutable image tag, and a rollback is one edited line in
`runtime.env` followed by `docker compose up -d` inside that directory.
Deployment stays a manual, operator-triggered step from the Mac.

## Decisions

- Deployment remains manual. No `latest`-following watcher, scheduled pull, or
  other unattended update runs on the NAS. `latest` is published for
  orientation and for a deliberate operator-side "take whatever main last
  built" pull, not as the deployed reference.
- CI publishes two tags for every push to `main`: `latest` and an immutable
  release tag `<commit date UTC>-<short commit>`, such as
  `2026.09.09-cdab3a9`. The `sha-<full commit>` tag is dropped. Tags published
  by earlier commits stay pullable and are unaffected.
- One shell helper, `scripts/image-tag [revision]`, defines the release tag
  format. CI, `scripts/deploy-app-nas`, and the operator all call it, so the
  Mac can name the image for a commit without querying the registry — the
  operator's `gh` token still has no `read:packages` scope.
- `deploy/compose.app.yaml` builds the image reference from
  `${HOME_AGENT_IMAGE_REPOSITORY}:${HOME_AGENT_IMAGE_TAG}` and reports
  `service.version` from the same tag. `HOME_AGENT_IMAGE` and
  `HOME_AGENT_VERSION` are retired, so a rollback edits one line and both the
  running image and the version reported to Grafana follow it.
- The NAS layout becomes `$HOME_AGENT_NAS_ROOT/deploy/` holding
  `compose.app.yaml` and `runtime.env`. The `releases/` tree, the `current`
  symlink, and `current.release` are removed. Data, configuration, and secret
  paths are untouched.
- The deployment stops being digest-selected and becomes tag-pinned. The NAS
  still resolves and verifies the pulled digest, and appends
  `<timestamp> <tag> <digest> <commit>` to `$HOME_AGENT_NAS_ROOT/deployments.log`
  (mode `0600`), which also gives the operator the list of rollback targets
  without registry access. This amends the "digest-selected" wording in
  `CLAUDE.md`; the guarantee that the deployed artifact is identified and
  recorded is kept, the guarantee that Compose references it by digest is not.
- `scripts/image-tag` joins `deploy/compose.app.yaml` in the deployment-input
  cleanliness check, because a locally modified tag format would compute a tag
  that CI never published.
- Removing the existing `releases/` tree is a one-time operator step in the
  runbook, not an automatic deletion by the script.
- `scripts/deploy-monitoring-nas` keeps its release layout. Monitoring runs
  pinned public upstream images and gains nothing from this change; aligning it
  is a later plan.

## Approach

1. Add `scripts/image-tag`: print `<commit date UTC>-<short commit>` for a
   revision, defaulting to `HEAD`, and fail on an unknown revision.
   Complexity: simple.
2. Rewrite the image tagging in `.github/workflows/image.yml` to call
   `scripts/image-tag`, publish `latest` on the default branch and the release
   tag always, and keep the existing labels, provenance, and cache settings.
   Complexity: simple.
3. Change `deploy/compose.app.yaml` to compose the image reference and
   `service.version` from `HOME_AGENT_IMAGE_REPOSITORY` and
   `HOME_AGENT_IMAGE_TAG`, and update `deploy/nas.env.example`.
   Complexity: simple.
4. Rewrite `scripts/deploy-app-nas`: extend the cleanliness check, drop the
   release path, extract the archive into `$HOME_AGENT_NAS_ROOT` so the
   Compose file lands in `deploy/`, install `runtime.env` atomically with
   `HOME_AGENT_IMAGE_TAG` appended, and in the interactive NAS terminal pull
   the tag, resolve and verify the digest, append the deployment log line, and
   run `up -d` and `ps` from the single directory. Remove the symlink and
   `current.release` bookkeeping. Complexity: complex.
5. Update `docs/13-operator-runbook.md` (§6 directory preparation, §7 deploy
   and verification, §10 checks and rollback, §11 backup commands) with the new
   paths, add the one-time `releases/` cleanup, and document rollback as
   editing `HOME_AGENT_IMAGE_TAG` and running `pull` and `up -d`. Update the
   application deployment flow and the volume listing in
   `docs/07-deployment-and-network.md`, and the deployment principle in
   `CLAUDE.md`. Correct the two Grafana "Version" panel descriptions that call
   the value a Git commit; the panels themselves are unaffected because they
   only display the `service_version` label. Complexity: simple.
6. Operator: push `main`, confirm the workflow published both tags, run
   `scripts/deploy-app-nas`, verify, perform a rollback drill to the previous
   release tag and back, then remove the old `releases/` tree.

## Files affected

- `scripts/image-tag` (new), `scripts/deploy-app-nas`
- `.github/workflows/image.yml`
- `deploy/compose.app.yaml`, `deploy/nas.env.example`
- `docs/13-operator-runbook.md`, `docs/07-deployment-and-network.md`,
  `CLAUDE.md`, `README.md`
- `deploy/grafana/dashboards/home-agent.json`,
  `deploy/grafana/dashboards/home-agent-runtime.json` (panel description only;
  the corrected text reaches Grafana on the next monitoring deployment)

## Validation

- `bash -n` on both scripts; `scripts/image-tag` returns the expected tag for
  `HEAD` and for a named older commit, and fails on an unknown revision.
- `docker compose -f deploy/compose.app.yaml --env-file <sample> config`
  renders the repository and tag as one image reference and the matching
  `service.version`.
- The deployment script still refuses an unpushed `HEAD`, a modified
  `deploy/compose.app.yaml` or `scripts/image-tag`, an unsafe path, and a
  secret-bearing environment file, all before touching the NAS.
- The image workflow log shows both published tags for the pushed commit.
- Live: `scripts/deploy-app-nas` deploys from `$HOME_AGENT_NAS_ROOT/deploy`,
  `docker compose ps` shows the service healthy, `/healthz` and `/readyz`
  answer, `deployments.log` records the tag and digest, Grafana reports
  `service.version` as the release tag, and `/status` replies in Telegram.
- Live rollback drill: edit `HOME_AGENT_IMAGE_TAG` to the previous release tag,
  `pull` and `up -d`, confirm the reported version changes, then return to the
  current tag.
- `scripts/check-plans` passes.

Outcome: three deployments on 2026-09-09 succeeded from the single deployment
directory, each running the pulled release tag, confirmed by the behavior every
image carried reaching Telegram. The live rollback drill was skipped by the
operator's decision and has not been run on this NAS, so the rollback path
documented in `docs/13-operator-runbook.md` is correct by construction but
unproven here. The one-time removal of the superseded `releases/` tree remains
an operator step.

## Risks and open questions

- A rollback started this way runs an older binary against the current
  database. The migration warning stays in the runbook; this plan does not add
  a schema-compatibility check.
- The release tag depends on the committer date, so two commits authored on the
  same UTC day are distinguished only by their short hash. A short-hash
  collision in this repository is not a practical concern, and the deployment
  log records the full commit.
- Removing `sha-<full commit>` tags means future tooling cannot assume a
  commit-addressable tag exists for commits published after this change;
  `scripts/image-tag` is the only supported way to name one.
- The first deployment after this change recreates the container because the
  image reference form changes. Redeploying the same commit afterwards is a
  no-op for Compose instead of a restart.
