# Home AI Assistant Project Instructions

## Product Direction

- Build a private, production-like home assistant for explicitly authorized household users.
- Keep identity, authorization, secrets, observability, and recovery as first-class product boundaries.
- Prefer narrow, typed integrations and least-privilege access over general-purpose tool authority.
- Make ordinary natural-language conversation the primary Telegram experience; keep commands as deterministic shortcuts and operational controls.
- Make integrations user-scoped: each authorized user must be able to discover, configure, enable, and disable their own integrations independently.
- Require each user to connect their own model subscription through a principal-bound authentication profile. Never share or fall back to another user's subscription; separate conversation sessions alone do not satisfy this boundary.

## Immutable Plans

- Store implementation plans in `docs/plans/` using sequential names such as `0003-model-conversation.md`.
- Create a plan before starting any non-trivial change. Update it as decisions are made and implementation progresses.
- Keep a plan editable only while its status is `proposed` or `in-progress`.
- Never modify or delete a plan, or its existing index row, after its status becomes `done`, `superseded`, or `abandoned`.
- Commit the completed plan together with the implementation it describes.
- Clearly label retrospective plans; do not present reconstructed history as plan-first work.
- Add every plan to `docs/plans/README.md`, never reuse a plan number, and run `scripts/check-plans` before committing.

## Development Workflow

- Work directly on `main`; this private project does not use pull requests.
- Keep history linear with no merge commits.
- Use signed conventional commits, with one coherent change per commit.
- Amend the relevant commit when correcting work that is already committed instead of adding a follow-up repair commit.
- Match existing structure and dependencies, and avoid unrelated refactoring.
- Run the configured formatting, module, test, vet, build, and plan checks after code changes.
- Local development uses the operator's GoLand server. Coordinate restarts and do not start a competing Telegram poller with the same bot token.

## Testing Strategy

- Prefer integration tests and end-to-end tests that verify observable behavior across real application components. Use unit tests selectively for isolated logic that is difficult to cover meaningfully at those boundaries; avoid mock-heavy tests, implementation-mirroring assertions, and coverage targets for their own sake.
- Use real temporary SQLite databases and isolate external services at their HTTP or process boundaries. Keep routine tests deterministic and credential-free; run live Telegram/model checks explicitly without competing with the operator's GoLand server.

## Security and Operations

- Never commit tokens, passwords, private keys, runtime `.env` files, or NAS secret copies.
- Keep local development secrets in the ignored `.env` file and production source secrets in macOS Keychain.
- Treat Telegram numeric user IDs and other allowlist identifiers as configuration, not secrets.
- Keep deployments manual and tag-pinned from the operator's Mac, recording the resolved image digest; CI must not receive NAS access or deployment authority.
- Keep NAS and observability endpoints private and reach administrative interfaces through the configured SSH alias and tunnels.
- Use the existing NAS `/volume1/docker` shared folder as the deployment parent; keep `monitoring` and `home-ai-assistant` as separate sibling projects beside Immich.
- Run NAS Docker commands through `ssh -tt home-nas` with `sudo`; the NAS account has no Docker socket access and `sudo` always prompts for a password, so `sudo -n` and non-interactive sessions fail.
- Prometheus is not published on the LAN. Verify metrics from the Mac through Grafana's data source proxy at `http://nas.local:3000/api/datasources/proxy/uid/prometheus/api/v1/...`, authenticating as `admin` with the Keychain item `grafana.admin-password`.
