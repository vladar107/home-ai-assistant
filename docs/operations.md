# Operations

Production changes are explicit and operator-driven from the Mac. Never paste
tokens, passwords, or keys into repository files, command arguments, chat, issues,
or CI settings: the scripts read secrets from standard input or macOS Keychain.
Clear the clipboard after any manual paste.

Telegram allows one `getUpdates` poller per bot. Never run the local server while
the NAS is polling with the same token.

## 1. Telegram bots and users

Create two bots with `@BotFather` and disable group joins (`/setjoingroups`) on both:

- **Hearthkeeper**: the assistant. The application registers its command menu.
- **Hearthwatch**: send-only Grafana alerts, with its own token.

Each authorized user opens a private chat with Hearthkeeper and sends one message.
List their numeric IDs (the token is read from `.env`, and message text is never
printed):

```sh
scripts/telegram-list-users main
cp deploy/telegram-users.example.json deploy/telegram-users.json
chmod 600 deploy/telegram-users.json   # fill in IDs, principal IDs, display names
```

The users file is allowlist configuration, not a secret, and holds one or two users.
For alerts:

```sh
scripts/keychain-set-secret grafana.telegram-alert-bot-token
scripts/telegram-list-users alerts
scripts/keychain-set-secret grafana.telegram-alert-chat-id
```

## 2. Local development

```sh
cp .env.example .env && chmod 600 .env
go run ./cmd/home-agent        # or run cmd/home-agent/main.go in GoLand from the repo root
curl --fail http://127.0.0.1:8080/healthz
curl --fail http://127.0.0.1:8080/readyz
```

`.env` is loaded from the working directory. Conversation needs the pinned Pi
version authenticated locally:

```sh
pi --version                                   # must match HOME_AGENT_PI_VERSION
pi auth check --provider openai-codex --json
```

If GoLand does not inherit the shell `PATH` (for example with `fnm`), set
`HOME_AGENT_PI_BINARY` and `HOME_AGENT_PI_NODE_BINARY` to absolute paths.

When upgrading Pi, change the version in the `Dockerfile`, the config default,
both env examples, the compose default, and the ignored `.env` and
`deploy/.env.nas` together, and upgrade the Mac's global install at the same
time; the app refuses to start on a mismatch.
Since 1.0, Pi ships codemode, `tool_search`, MCP, and llama.cpp as built-in
extensions. `--no-extensions` keeps them off, so check after each upgrade that
a turn only offers the app's own tools. Pi must never receive a TypeSafe key
(`TYPESAFE_API_KEY`): judgments go through the app's redactor.

Tests need no credentials, Node, or Pi. They use temporary SQLite databases, a
loopback Telegram simulator, and a Go subprocess that speaks Pi's RPC protocol:

```sh
go test -race ./...
```

Live smoke check after conversation changes: `/status` and `/privacy` answer without
the model; ordinary text gets a reply that remembers context; `/cancel` stops a slow
turn; `/new` forgets context; a second message during a turn gets a busy reply; an
unknown account and group chats are ignored; a restart mid-turn reports the
interruption without replaying it.

## 3. Prepare the NAS

Enable SSH on the NAS, verify its address, port, account, and host-key fingerprint
through a trusted interface, then install a dedicated encrypted key:

```sh
ssh-keygen -t ed25519 -a 64 -f ~/.ssh/home_nas_ed25519 -C "home-nas deployment"
ssh-copy-id -i ~/.ssh/home_nas_ed25519.pub <nas-user>@<nas-address>
```

```sshconfig
Host home-nas
    HostName <nas-address>
    User <nas-user>
    IdentityFile ~/.ssh/home_nas_ed25519
    IdentitiesOnly yes
    AddKeysToAgent yes
    UseKeychain yes
    HashKnownHosts yes
```

```sh
ssh-add --apple-use-keychain ~/.ssh/home_nas_ed25519
ssh -o BatchMode=yes home-nas true
scripts/nas-preflight home-nas     # x86_64, Docker, Compose, free space, runtime UID/GID
```

The NAS account has no Docker socket access. The scripts open one interactive
`ssh -tt` session and `sudo` prompts for the password. Stop if the host key ever
changes unexpectedly.

Create the project directories as the deployment account, not with `sudo`:

```sh
ssh home-nas 'umask 077; for d in deploy config data secrets; do install -d -m 700 /volume1/docker/home-ai-assistant/$d; done'
cp deploy/nas.env.example deploy/.env.nas && chmod 600 deploy/.env.nas
scripts/keychain-set-secret home-agent.telegram-bot-token
```

Set `NAS_RUNTIME_UID` and `NAS_RUNTIME_GID` in `deploy/.env.nas` to the values
`nas-preflight` printed. No secret value goes into that file.

## 4. Deploy

Deploy monitoring first, from the separate `home-monitoring` repository
(`scripts/deploy home-nas`). It creates the `home-observability` network the
application joins.

Once per NAS, log Docker in to GHCR with a `read:packages` token:

```sh
ssh -tt home-nas 'sudo docker login ghcr.io'
```

Push `main`, wait for the "Production image" workflow, stop the local poller, then
deploy:

```sh
scripts/image-tag
gh run list --workflow=image.yml --commit "$(git rev-parse HEAD)"
scripts/deploy-app-nas home-nas deploy/.env.nas
```

The script refuses uncommitted or unpushed state. It writes the Compose file and
`runtime.env`, copies the users and NAS roots files, and syncs secrets from
Keychain. It then pulls the commit's release tag, verifies the digest, runs
`docker compose up -d`, and appends tag, digest, and commit to `deployments.log`.

```sh
ssh -tt home-nas 'cd /volume1/docker/home-ai-assistant/deploy && sudo docker compose --env-file runtime.env -f compose.app.yaml ps'
ssh home-nas 'tail -1 /volume1/docker/home-ai-assistant/deployments.log'
ssh home-nas 'curl --fail http://127.0.0.1:18080/readyz'
```

Each user then connects their own model subscription from `/settings`.

## 5. Integrations

Every integration is off for the deployment until enabled in `deploy/.env.nas`
(or `.env` locally). Each one then appears in each user's `/settings`, where
that user turns it on or off for themselves. Optional Compose overlays
(`deploy/compose.app.*.yaml`) are applied only while their integration is enabled.

| Integration | Enable | Secret | Notes |
| --- | --- | --- | --- |
| Notion | `HOME_AGENT_NOTION_ENABLED=true` | per user, via `scripts/notion-set-token <principal> [home-nas deploy/.env.nas]` | Internal connection with read, update, insert; share only the wanted pages. `HOME_AGENT_NOTION_CHAT_CONNECT=true` allows pasting the token in chat (deleted before verification). Writes off until the user enables them. |
| NAS files | `HOME_AGENT_NAS_ENABLED=true` | none | Set the mount paths in `runtime.env` and copy `deploy/nas-roots.example.json` to `deploy/nas-roots.json`. Only inbox and shared-drop mounts are writable. The roots file is the access rule between users, so review it like one. |
| Google (Gmail, Calendar) | `HOME_AGENT_GOOGLE_ENABLED=true`, `HOME_AGENT_GOOGLE_CLIENT_ID` | `scripts/keychain-set-secret home-agent.google-client-secret` | Desktop-app OAuth client in a published Google project (no logo, so no verification) with the Gmail and Calendar APIs enabled and `gmail.readonly` and `calendar.readonly` on the consent screen. Each user connects any number of accounts by pasting the failed `localhost` redirect URL into the chat, then switches Mail and Calendar per account in `/settings`. |
| Web | `HOME_AGENT_WEB_ENABLED=true`, optional `HOME_AGENT_WEATHER_HOME` | none | Search runs on the user's own subscription through the vendored `pi-web-search` extension; weather uses Open-Meteo. Locally set `HOME_AGENT_PI_WEB_SEARCH_EXTENSION=<repo>/third_party/pi-web-search/src/index.ts`. Re-check live search on every Pi upgrade. |
| NS trains | `HOME_AGENT_NS_ENABLED=true` | Keychain `ns.api-key` (Ns-App product at apiportal.ns.nl) | |
| Home Assistant | `HOME_AGENT_HA_ENABLED=true`; locally `HOME_AGENT_HA_TOKEN`, optional `HOME_AGENT_HA_URL` (default `https://ha.home`) | Keychain `home-assistant.token`, synced as Docker secret `ha-token` | One household long-lived token; see the setup below. The overlay `deploy/compose.app.homeassistant.yaml` reaches Home Assistant at `http://homeassistant:8123` through host-gateway, because it uses host networking and the NAS cannot reach the Caddy macvlan edge. Per-user "Home" switch in `/settings`, on by default. |
| Grafana | `HOME_AGENT_GRAFANA_ENABLED=true`, `HOME_AGENT_GRAFANA_PRINCIPALS`; locally `HOME_AGENT_GRAFANA_TOKEN`, `HOME_AGENT_GRAFANA_URL` (default `http://grafana:3000`), `HOME_AGENT_GRAFANA_PRINCIPALS` | Keychain `grafana.home-agent-token`, synced as Docker secret `grafana-token` | Read-only dashboards, PromQL, and LogQL for the named principals only; nobody else sees it in `/settings`. See the setup below. The overlay `deploy/compose.app.grafana.yaml` reaches Grafana by service name over `home-observability`. Per-user "Monitoring" switch, on by default. |
| Buses | `HOME_AGENT_BUS_ENABLED=true`, `HOME_AGENT_BUS_HOME_STOP`, optional `HOME_AGENT_BUS_CONTACT` | none | Live data from OVapi (plain HTTP, no key); the stop list is read daily from OVapi's GTFS archive by range requests. The home stop is any timing point code of the stop (`stop_code` in GTFS `stops.txt`). Planned trips and boards for stops OVapi has no live data for go to Transitous; set the contact, which its terms ask for, to label those requests. Its use is private and non-commercial. |
| Shared locations | `HOME_AGENT_LOCATION_ENABLED=true`, optional `HOME_AGENT_LOCATION_CONTACT`; retention `HOME_AGENT_LOCATION_PIN_TTL` (1h, 5m–24h), `HOME_AGENT_LOCATION_LIVE_SILENCE` (1h, 5m–24h), `HOME_AGENT_LOCATION_LIVE_GRACE` (15m, 0–2h), `HOME_AGENT_LOCATION_PRUNE_INTERVAL` (10m, 1m–1h) | none | A pin, venue, or live location a user shares is acknowledged without a model call and named through Transitous reverse geocoding; while fresh, its coordinates go into that user's turns, and `nearby_places` queries OpenStreetMap's public Overpass API. Only the latest position per user is kept; it is hidden once expired and deleted at the next prune, with SQLite `secure_delete` on. Changing a duration needs only a restart. Set the contact, which both services' terms ask for. Users forget their location in `/settings`. |
| Background tasks | `HOME_AGENT_TASKS_ENABLED=true`, optional `HOME_AGENT_TASK_TIMEOUT` (60m, 1m–6h) | none | The model hands a long request to a sub-agent: a second Pi process on the user's own profile and subscription, beside the chat and outside its turn slot, with read-only tools, 100 tool calls, 150 model calls, and the timeout. One runs per user and three wait behind it. Its report arrives as a reply to the request; `/tasks` lists and cancels them. A restart resumes a running task once; a second interruption fails it. Rows and reports are pruned 30 days after a task finishes. Watch `home_agent_task_runs_total` by outcome and the NAS memory of a long run before raising limits. |
| Watches | `HOME_AGENT_WATCHES_ENABLED=true` | none | Requires conversation. Departure watches are offered with the NS or bus connector, disruption watches with the NS connector; route watches with saved routes. Each check runs under the owner's trains or buses switch, and a cycle asks NS or the bus services once per departure or station for all of a user's watches on it. Alerts reach only the owner's chat, with Mute today and Pause; `/jobs` pauses and deletes watches. Window states and finished alerts are pruned after 30 days, and an alert's text is cleared once it is sent or dropped. Watch `home_agent_watch_checks_total` by kind and outcome, `home_agent_watch_alerts_total` by type and outcome, and `home_agent_watch_upstream_errors_total`. |
| Saved routes | on with conversation and NS trains or buses | none | Migration 019 adds the `routes` table. Each user may save 20 routes, each confirmed, and deletes them in `/jobs`; station and stop names are looked up when a leg is proposed, so a renamed station or stop needs the route edited. A train or bus leg is checked only while the user's trains or buses switch is on; otherwise it is reported as a failed check. |
| TypeSafe checks | `HOME_AGENT_TYPESAFE_ENABLED=true` | `scripts/keychain-set-secret typesafe.api-key` | Each user opts in. Nothing is sent for users who have not. A failed check leaves the turn unchanged. |

After changing a secret, run `scripts/nas-sync-secrets` and redeploy.

### Home Assistant setup

1. Create a dedicated non-admin Home Assistant user ("Home Agent") and a
   long-lived access token for it.
2. Add the "Model Context Protocol Server" integration with the Assist API.
3. In Settings → Voice assistants → Expose, expose the everyday devices.
   Consider turning off "expose new entities" so nothing appears by accident.
4. Label sensitive devices `assistant-confirm` and keep them unexposed. They are
   then offered only through `ha_confirmed_devices` and `ha_service_call`, which
   always ask for approval.
5. Store the token in Keychain; `scripts/keychain-set-secret` does not cover it:
   `security add-generic-password -a home-ai-assistant -s home-assistant.token -U -w`.
6. Set `HOME_AGENT_HA_ENABLED=true` in `deploy/.env.nas`, run
   `scripts/nas-sync-secrets`, and deploy as for NS.

Locks, alarm panels, valves, sirens, cameras, firmware updates, and garage, gate,
or door covers count as needing confirmation. If one is exposed, or shares a name
with a labelled entity, all immediate actions are withheld, reads remain, and
`/settings` names the conflicting devices. Tools, exposure, and labels are cached
for 5 minutes.

To add a device, expose it (everyday) or label it `assistant-confirm` (confirmed).
No code change is needed.

### Grafana setup

1. In Grafana, Administration → Users and access → Service accounts, add
   `home-agent` with the `Viewer` role and create a token for it.
2. Store the token in Keychain:
   `security add-generic-password -a home-ai-assistant -s grafana.home-agent-token -U -w`.
3. Set `HOME_AGENT_GRAFANA_ENABLED=true` and `HOME_AGENT_GRAFANA_PRINCIPALS`
   (comma-separated principal IDs) in `deploy/.env.nas`, run
   `scripts/nas-sync-secrets`, and deploy as for NS.

The tools query the provisioned `prometheus` and `loki` data sources by UID.
To revoke access, delete the token in Grafana; the `claude-mcp` account is
separate.

## 6. Grafana and alerts

Grafana runs from `home-monitoring` on the NAS LAN address, port 3000. Log in as
`admin`, with the password copied without printing it:

```sh
security find-generic-password -a home-monitoring -s grafana.admin-password -w | pbcopy
pbcopy </dev/null   # after login
```

Check that the Prometheus and Tempo data sources are present, that the Home Agent
dashboards show activity after `/status`, and that Tempo finds traces for
`service.name=home-agent`. Dashboards are provisioned from `home-monitoring`; edit
them there.

Rare failure series (turn errors per error kind and safety check, and the
`timeout`, `cancelled`, and `failed` model-call outcomes) are created at zero at
startup, so `increase()` counts the first failure. `home_agent_model_calls`
counts every model call by outcome: a call that Pi retries and then recovers
counts once as `completed`, while `timeout` and `cancelled` mean the turn ended
while the model call was still open. Each turn's trace shows
`home_agent.model.call` spans (with `pi.auto_retry` events) and
`home_agent.tool.call` spans. A model call that Pi had to retry logs one
`pi.model_call.retried` warning with its `category`, `retries`, and `outcome`.
A turn that hears nothing from Pi for 45 seconds before any tool call or
reply text is stopped, its model call ending as `stalled`, and run once more
on its restored session. A silence after that point waits for the turn's own
deadline. Both are counted in `home_agent_model_stalls` by outcome
(`retried_ok`, `retried_failed`, `not_retried`) and log `pi.turn.stalled`.

Add a Telegram contact point `Hearthwatch` with the alert bot token and chat ID
from Keychain. Start with an alert on an absent `home_agent_application_running`
metric, pending for two minutes.

## 7. Logs and rollback

```sh
ssh -tt home-nas 'cd /volume1/docker/home-ai-assistant/deploy && sudo docker compose --env-file runtime.env -f compose.app.yaml logs --tail=200 home-agent'
```

Logs are JSON with trace and span IDs. They never contain message text,
usernames, Telegram IDs, or tokens, and rotate at 10 MB × 5 files.

To roll back, choose a tag from `deployments.log` (or `scripts/image-tag <commit>`)
and pin it:

```sh
ssh -tt home-nas
cd /volume1/docker/home-ai-assistant/deploy
sed -i 's|^HOME_AGENT_IMAGE_TAG=.*|HOME_AGENT_IMAGE_TAG=<tag>|' runtime.env
sudo docker compose --env-file runtime.env -f compose.app.yaml pull home-agent
sudo docker compose --env-file runtime.env -f compose.app.yaml up -d home-agent
```

Never start an older binary against a database with newer migrations. Keep a data
backup until the replacement is verified.

## 8. Backup and restore

For a consistent backup, stop `home-agent`, snapshot or copy the whole data
directory, and start it again. Application state matters more than telemetry, and
Prometheus, Tempo, and Alloy state can be recreated. A full recovery needs the
repository, the Keychain secrets, `deploy/.env.nas`, the users and roots files,
and the recorded image digest.

A backup counts only after a restore into an isolated directory passes the SQLite
integrity check, readiness, identity mapping, and polling-cursor check without
sending duplicate replies.

## 9. Rotation and incidents

- Telegram token: revoke in BotFather, update Keychain, run
  `scripts/nas-sync-secrets`, and restart only its consumer.
- NS, TypeSafe, and Home Assistant keys: regenerate (for Home Assistant, a new
  token for the Home Agent user), `security add-generic-password -U …`, sync, and
  redeploy.
- `grafana.admin-password`: rotate in Grafana and Keychain together.
  `grafana.secret-key` needs Grafana's re-encryption procedure.
- Lost Mac or SSH key: revoke the public key on the NAS.
- Treat an unexpected host-key change, plaintext registry credentials, broad NAS
  secret permissions, or a token in logs as an incident, and stop deploying.
