# 0052 — Supply-chain pins and public-repository hygiene

Status: `done`

## Context

The 2026-10-04 security audit found that the image installs the Pi runtime with
`npm install --global`. Pi 1.0.0 ships an `npm-shrinkwrap.json` that pins its
dependency tree. However, the top-level tarball is not checked against a known
integrity hash, and install scripts run (`esbuild`, `@google/genai`,
`protobufjs`). Pi holds every user's model OAuth refresh token. Both base
images are pinned by tag only.

Before the repository goes public (plan 0050), the audit also flagged:

- machine-specific operator details in `CLAUDE.md`;
- a missing `SECURITY.md`;
- no alerts for vulnerable dependencies.

## Goal

- The image installs a Pi tarball with a known integrity hash, with no install
  scripts, on digest-pinned base images.
- The public tree carries no machine-specific operator details.
- The repository tells reporters how to report a vulnerability privately.
- GitHub raises alerts for vulnerable dependencies.

## Decisions

- **Pi from a committed lockfile.** `third_party/pi-runtime/` holds a
  `package.json` that depends on the pinned Pi version, and its
  `package-lock.json`. The lockfile is generated inside the runtime base image
  with the npm version it ships.
  - The image runs `npm ci --omit=dev --ignore-scripts` there and links the
    `pi` binary onto `PATH`.
  - The vendored web-search extension must still resolve Pi's packages.
  - The startup version check stays.
- **Base images pinned by digest.** Both `FROM` lines keep their tag for
  readability and add `@sha256:` for the multi-arch index.
- **Operator details move to an ignored `CLAUDE.local.md`.** This covers the
  NAS host name, the SSH and `sudo` routine, and the Grafana proxy check with
  its Keychain item.
  - `CLAUDE.md` keeps generic rules and says where the local notes live.
    Agents that read `AGENTS.md`, the symlink to it, can then find them.
  - `docs/operations.md`, the scripts' default SSH alias, and the Keychain
    item names stay. They are names the operator chooses, not addresses, and
    the scripts need them.
- **Dependabot alerts only.** Turn on the dependency graph and vulnerability
  alerts in the repository settings. There is no `dependabot.yml`, because
  version updates arrive as pull requests and this project works directly on
  `main`.
- **No `CODEOWNERS`.** With one owner and no pull requests it does nothing.
- **`SECURITY.md`** asks reporters to use GitHub private vulnerability
  reporting. Plan 0050 gets a step to enable that setting when the repository
  goes public.

## Approach

1. Add `third_party/pi-runtime/` and update the `Dockerfile` with the digest
   pins.
2. Move the operator details into `CLAUDE.local.md`, ignore it, update
   `CLAUDE.md`, and add `SECURITY.md`.
3. Enable the dependency graph and Dependabot alerts on GitHub, and add the
   private-reporting step to plan 0050.

## Files affected

- `Dockerfile`, `third_party/pi-runtime/`
- `CLAUDE.md`, `CLAUDE.local.md` (ignored), `.gitignore`, `SECURITY.md`
- `docs/plans/0050-public-release-scrub.md`

## Validation

- `docker buildx build --platform linux/amd64` succeeds.
- In the built image, `pi --version` prints the pinned version, and Pi starts
  with the vendored web-search extension loaded.
- Formatting, module, vet, test, build, and plan checks.
- `gh api repos/vladar107/home-ai-assistant/vulnerability-alerts` returns
  204.

## Risks and open questions

- Digest pins and the lockfile need manual bumps. Without version-update pull
  requests, the alerts are the only signal.
- If Pi needs an install script, `--ignore-scripts` breaks it. The build check
  catches that, and the fix would then be an explicit allowlisted rebuild.
