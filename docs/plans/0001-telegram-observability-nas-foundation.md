# 0001 — Telegram, observability, and NAS foundation

Status: `done`

## Context

This is a retrospective record. The project adopted immutable, plan-first
artifacts after this implementation was complete, so this plan documents the
work without claiming it preceded the changes.

The architecture and Pi subscription spike existed, but the service needed a
safe walking skeleton before model-driven behavior or external integrations
could be enabled.

## Goal

Deliver a deterministic one-user Telegram shell with durable identity and
polling state, OpenTelemetry-based local observability, private-image CI, and
explicit manual NAS operations.

## Decisions

- Use one explicitly allowlisted Telegram user initially; numeric Telegram IDs are configuration, not secrets.
- Keep `/status` and `/privacy` deterministic and independent of a model runtime.
- Load an ignored `.env` file for local GoLand development and use protected files for production secrets.
- Export metrics and traces with OpenTelemetry through Grafana Alloy to separate Prometheus and Tempo services.
- Keep structured logs in bounded container output; defer Loki until local log search justifies it.
- Publish immutable `linux/amd64` images to private GHCR, but deploy only through an explicit Mac-to-NAS operation.
- Store source secrets in macOS Keychain and copy only required runtime values to protected NAS files.

## Approach

1. Add SQLite migrations and storage for principals, Telegram identities, capability grants, and polling cursors.
2. Add validated Telegram configuration, Bot API long polling, private-chat and allowlist boundaries, and deterministic commands.
3. Add OpenTelemetry instrumentation and a local Grafana, Alloy, Prometheus, and Tempo deployment stack.
4. Add a non-root image, CI validation, private GHCR publishing, and manual digest-selected NAS deployment scripts.
5. Document Telegram, SSH, secret, monitoring-first deployment, rollback, backup, and recovery procedures.

## Files affected

- `cmd/home-agent/` and `internal/`
- `deploy/` and `Dockerfile`
- `scripts/`
- `.github/workflows/`
- `README.md` and `docs/`

## Validation

- The application formatting, module, test, vet, and build checks passed locally.
- A real private Telegram chat returned the expected `/status` and `/privacy` responses from a GoLand run.
- The Telegram token remained in the ignored local `.env` file and the numeric user ID remained ordinary configuration.
- Image publication, NAS deployment, and the monitoring stack still require verification in their real environments.

## Risks and open questions

- NAS volume paths, resource headroom, SSH access, and backup coverage require inspection on the actual device.
- Grafana alert delivery requires the separate Hearthwatch bot and production secret setup.
- Natural-language conversation and user-controlled integrations intentionally remain outside this deterministic foundation.
