# Home AI Assistant Project Instructions

## Product Direction

- Build a private, production-like home assistant for explicitly authorized household users.
- Keep identity, authorization, secrets, observability, and recovery as first-class product boundaries.
- Prefer narrow, typed integrations and least-privilege access over general-purpose tool authority.
- Make ordinary natural-language conversation the primary Telegram experience; keep commands as deterministic shortcuts and operational controls.
- Make integrations user-scoped: each authorized user must be able to discover, configure, enable, and disable their own integrations independently.

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

## Security and Operations

- Never commit tokens, passwords, private keys, runtime `.env` files, or NAS secret copies.
- Keep local development secrets in the ignored `.env` file and production source secrets in macOS Keychain.
- Treat Telegram numeric user IDs and other allowlist identifiers as configuration, not secrets.
- Keep deployments manual and digest-selected from the operator's Mac; CI must not receive NAS access or deployment authority.
- Keep NAS and observability endpoints private and reach administrative interfaces through the configured SSH alias and tunnels.
