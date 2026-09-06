# Operator Runbook

This runbook keeps production changes explicit and operator-driven. Complete
the Telegram smoke test on the Mac first. Prepare and inspect the NAS next.
Deploy the observability services before the home-agent container.

Do not paste tokens, passwords, or private keys into repository files, shell
commands, chat, issues, or CI settings. Commands below that retrieve a secret
send it through standard input or the macOS clipboard, never a command
argument. Clear the clipboard immediately after a manual paste.

## 1. Create the Telegram bots

Open Telegram and use the verified `@BotFather` account.

For the main interaction bot:

1. Send `/newbot`.
2. Use `Hearthkeeper` as the display name.
3. Choose an available username ending in `bot`, for example
   `HearthkeeperHomeBot` or `MyHearthkeeperBot`.
4. Use `/setjoingroups` and disable group addition. The application rejects
   non-private chats as a second boundary.

The application registers the command menu at startup, including `/status`,
`/privacy`, `/cancel`, `/new`, and `/settings` when those features are enabled.

For infrastructure alerts, repeat only the creation and group-disable steps
with a distinct bot; it needs no command menu. The preferred display name is
`Hearthwatch`; candidate usernames include
`HearthwatchAlertsBot`, `HearthwatchHomeBot`, and `MyHearthwatchBot`. This bot
will be connected directly to Grafana and must not share the Hearthkeeper
token.

For the local Hearthkeeper test, create the ignored environment file:

```sh
cp .env.example .env
chmod 600 .env
${EDITOR:-vi} .env
```

Set `HOME_AGENT_TELEGRAM_BOT_TOKEN` to the Hearthkeeper token. Leave the
production users file for the next section; local development may continue to
use `HOME_AGENT_TELEGRAM_USER_ID` when only one test account is needed.

If a token is ever pasted elsewhere or committed, revoke it with BotFather and
replace the configured token before continuing.

## 2. Discover and configure the authorized Telegram users

Open a private chat with Hearthkeeper, press **Start**, and send one message.
Stop any existing bot process first because Telegram permits only one active
`getUpdates` poller per bot.

Run this once for each authorized household user:

```sh
scripts/telegram-list-users main
```

The script reads the token from `.env`, sends it to `curl` through standard
input, and prints only sender ID, username, and display name. It does not print
message text or the token. Record each positive numeric ID in a local copy of
`deploy/telegram-users.example.json`, replacing the placeholder principal IDs
and display names. The users file is allowlist configuration, not a secret,
and is never stored in Keychain. Keep one entry for each authorized user; the
application accepts one or two entries and rejects duplicate IDs or principals.

Create the ignored production copy:

```sh
cp deploy/telegram-users.example.json deploy/telegram-users.json
chmod 600 deploy/telegram-users.json
${EDITOR:-vi} deploy/telegram-users.json
```

The deployment command copies this file to the NAS application configuration
directory. Do not put the bot token in the JSON file.

For Hearthwatch, store its separate token in Keychain, send `/start` to the
alert bot, and run:

```sh
scripts/keychain-set-secret grafana.telegram-alert-bot-token
scripts/telegram-list-users alerts
```

Store the intended recipient's numeric ID as the alert chat ID:

```sh
scripts/keychain-set-secret grafana.telegram-alert-chat-id
```

## 3. Test Hearthkeeper locally

In GoLand, open `cmd/home-agent/main.go` and run the `main` package. Set the run
configuration's working directory to the repository root. The application
loads `.env` from that directory automatically, so no variables need to be
copied into the GoLand configuration.

The command-line equivalent is:

```sh
go run ./cmd/home-agent
```

Open another terminal to check health:

```sh
curl --fail http://127.0.0.1:8080/healthz
curl --fail http://127.0.0.1:8080/readyz
```

Perform this smoke test:

1. Send `/status` and verify the deterministic online reply.
2. Send `/privacy` and verify the strict-local policy.
3. With conversation disabled, send ordinary text and verify there is no reply.
4. From another account, send `/status` and verify there is no reply.
5. If group addition was not disabled, add the bot temporarily and verify a
   group command receives no reply, then remove it.
6. Stop with Control-C, start the script again, and verify old commands are not
   replayed.

Do not run the local bot while the NAS bot is polling. The same token cannot be
used reliably by two long-polling processes at once.

### Automated conversation checks

Run the integration suites from the repository root:

```sh
go test ./...
go test -race ./...
```

The suites use real temporary SQLite databases, a loopback Telegram API
simulator, and a Go subprocess that implements the Pi RPC test protocol. They
require no bot token, model subscription, Node, or Pi installation. They do not
load the repository `.env` or use the running GoLand server's data. CI runs the
same suites with race detection.

These checks exercise application behavior across HTTP, persistence, and
process boundaries. They cannot establish real subscription access, model
quality, or Telegram service compatibility; complete the live check below
before recording the conversation plan as done.

### Test local model conversation

Keep the NAS bot stopped. Confirm the pinned local Pi installation and its
subscription authentication without printing credentials:

```sh
pi --version
pi auth check --provider openai-codex --json
command -v pi
```

Pi must report version `0.84.1`. Add the following to the ignored `.env` file;
use the absolute path from `command -v pi` if GoLand does not inherit the shell
PATH:

```dotenv
HOME_AGENT_CONVERSATION_ENABLED=true
HOME_AGENT_PI_BINARY=pi
HOME_AGENT_PI_NODE_BINARY=
HOME_AGENT_PI_PROVIDER=openai-codex
HOME_AGENT_PI_MODEL=gpt-5.6-luna:low
HOME_AGENT_PI_VERSION=0.84.1
HOME_AGENT_CONVERSATION_TURN_TIMEOUT=2m
```

For a GUI-launched GoLand instance using `fnm`, point
`HOME_AGENT_PI_BINARY` at the stable versioned `bin/pi` path and
`HOME_AGENT_PI_NODE_BINARY` at the matching `bin/node` path. This avoids
depending on a shell-specific `fnm_multishells` PATH entry.

Start `cmd/home-agent/main.go` from GoLand with the repository root as its
working directory, then verify:

1. An ordinary sentence receives a model response.
2. A follow-up referring to that response retains the conversation context.
3. `/status` and `/privacy` respond without waiting for or invoking the model.
4. Send a slow request followed by `/cancel`; cancellation is acknowledged and no later model reply arrives.
5. `/new` starts a fresh session whose next message does not retain the previous context.
6. A second ordinary message while a turn is active receives a busy response.
7. An unauthorized Telegram user remains ignored.
8. If the process is stopped during a turn, restart it and verify the bot reports
   the interruption without replaying the prompt; the next message starts a
   fresh session.

Conversation history is stored by Pi below `data/pi-sessions/`. SQLite stores
principal ownership and durable request state, but clears message and response
bodies after delivery. Neither location is committed. The NAS-built production
image packages the pinned Pi runtime; each user must still connect that user's
own subscription through `/settings` after deployment.

### Connect Notion

Notion is opt-in with `HOME_AGENT_NOTION_ENABLED=true`. Create an internal
connection in Notion's developer portal with read, update, and insert content
capabilities, share the wanted pages and data sources with it, and install its
token for the principal without printing it:

```sh
security find-generic-password -a home-ai-assistant -s notion.token -w | scripts/notion-set-token primary
```

Any command that writes the token to standard input works; the script accepts a
pasted token followed by Enter and Ctrl-D as well. The file is written to
`data/integrations/<principal>/notion.json` with mode `0600`. Then verify:

1. `/settings` shows the workspace, bot name, and token kind, and Notion's
   `users/me` was called once.
2. Ask for a search, a page, and a data source query; confirm the model used
   the tools and the answers match the shared content.
3. With `HOME_AGENT_NOTION_CHAT_CONNECT=true`, press Connect Notion, paste a
   token, and confirm the message disappears and the connection is reported.
4. Turn Notion writes on in `/settings`, ask the model to add a task to a
   shared data source, confirm the proposal, and verify the page in Notion;
   reject a second proposal and verify nothing changed; turn writes off and
   confirm the model no longer offers to write.
5. Disconnect from `/settings` and confirm the file and the row are gone.
6. Confirm the token appears in neither the logs nor the database.

## 4. Configure passwordless SSH safely

The NAS needs the public key. It must never receive the private key. First
enable SSH in the trusted NAS administration interface and confirm the NAS
address, SSH port, account name, and host-key fingerprint through that trusted
interface or local console.

Create a dedicated encrypted key on the Mac:

```sh
ssh-keygen -t ed25519 -a 64 -f ~/.ssh/home_nas_ed25519 -C "home-nas deployment"
```

Use a strong passphrase. Install only the public key; this is the one setup
step that normally asks for the NAS account password:

```sh
ssh-copy-id -i ~/.ssh/home_nas_ed25519.pub NAS_USER@10.0.0.10
```

Add this entry to `~/.ssh/config`, replacing `NAS_USER`, address, and port with
the verified values:

```sshconfig
Host home-nas
    HostName 10.0.0.10
    User vladar107
    Port 22
    IdentityFile ~/.ssh/home_nas_ed25519
    IdentitiesOnly yes
    AddKeysToAgent yes
    UseKeychain yes
    HashKnownHosts yes
    ServerAliveInterval 60
```

Protect the local files, load the encrypted key, and verify non-interactive
access:

```sh
chmod 700 ~/.ssh
chmod 600 ~/.ssh/config ~/.ssh/home_nas_ed25519
chmod 644 ~/.ssh/home_nas_ed25519.pub
ssh-add --apple-use-keychain ~/.ssh/home_nas_ed25519
ssh -o BatchMode=yes home-nas true
```

The connection string is not a secret; the alias removes the need to type it.
The private key remains encrypted on the Mac, its passphrase is protected by
Keychain, and `known_hosts` pins the NAS identity. Stop if the SSH host key ever
changes unexpectedly.

## 5. Inspect and prepare the NAS

Run the read-only inventory:

```sh
scripts/nas-preflight home-nas
```

Do not continue until it confirms Linux `x86_64`, working Docker Engine,
working Docker Compose, sufficient free space, and Docker access for the
dedicated deployment account. `nas-preflight` opens an interactive SSH
terminal and asks for the NAS account password once. The deployment scripts do
the same for their Compose command; no sudo timestamp needs to persist between
SSH sessions, and no Docker group is required. Docker access is effectively
root-equivalent; do not share this account or mount the Docker socket into a
container.

Use the existing `docker` shared folder. It is the parent for the two projects;
Immich remains a separate sibling. Connect to the NAS and create only the
directories that the deployment account owns:

```sh
ssh home-nas
umask 077
install -d -m 700 /volume1/docker/monitoring/releases
install -d -m 700 /volume1/docker/monitoring/alloy
install -d -m 700 /volume1/docker/monitoring/prometheus
install -d -m 700 /volume1/docker/monitoring/tempo
install -d -m 700 /volume1/docker/monitoring/grafana
install -d -m 700 /volume1/docker/monitoring/grafana-secrets
install -d -m 700 /volume1/docker/home-ai-assistant/releases
install -d -m 700 /volume1/docker/home-ai-assistant/config
install -d -m 700 /volume1/docker/home-ai-assistant/data
install -d -m 700 /volume1/docker/home-ai-assistant/secrets
exit
```

Run those commands as `vladar107`, not with `sudo`, so all directories have the
deployment account's UID and GID. Do not use a literal `'$NAS_ROOT'` or solve
permission errors with `chmod 777`. Confirm the resulting layout with:

```sh
ssh home-nas 'find /volume1/docker -maxdepth 2 -type d -print'
```

On the Mac, create the ignored deployment environment:

```sh
cp deploy/nas.env.example deploy/.env.nas
chmod 600 deploy/.env.nas
${EDITOR:-vi} deploy/.env.nas
```

Set `NAS_RUNTIME_UID` and `NAS_RUNTIME_GID` to the numeric values printed by
`nas-preflight`. Replace the path placeholders with the directories just
created: monitoring uses `/volume1/docker/monitoring`; the application uses
`/volume1/docker/home-ai-assistant`. Prometheus, Tempo, and Grafana run as this
verified unprivileged identity. The monitoring deploy script owns the Alloy
data directory as UID/GID 473, matching the pinned Alloy image, because all
Linux capabilities are dropped. Their mode-`0600` secret files and
mode-`0700` data directories do not need broader permissions.
Set `GRAFANA_BIND_ADDRESS` to the NAS LAN address (`10.0.0.10` here). This
publishes only Grafana on the internal network; the Home Agent health endpoint
remains bound to NAS loopback.

Store the bot token and Grafana secrets in Keychain. Use a unique generated
admin password and at least 32 random characters for the Grafana secret key;
paste each only at its Keychain prompt:

```sh
scripts/keychain-set-secret home-agent.telegram-bot-token
scripts/keychain-set-secret grafana.admin-password
scripts/keychain-set-secret grafana.secret-key
```

The stage-specific deployment commands below read these Keychain entries and
install only the secret needed by that stage. No runtime secret is written to
`deploy/.env.nas`.

## 6. Deploy monitoring first

Monitoring is independent and can run without the application image or a
Telegram token. Ensure Docker Hub is reachable from the NAS, then deploy it
from the checked-out repository:

```sh
scripts/deploy-monitoring-nas home-nas deploy/.env.nas
```

The command copies the pinned monitoring configuration to a new release under
`/volume1/docker/monitoring`, syncs the Grafana secrets from Keychain, and opens
one interactive NAS terminal for `sudo docker compose pull` and `up -d`. It does
not deploy or start `home-agent`.
Verify the stage before deploying the application:

```sh
ssh -tt home-nas 'cd /volume1/docker/monitoring/current/deploy && sudo docker compose --env-file runtime.env -f compose.monitoring.yaml ps'
ssh -tt home-nas 'sudo docker network inspect home-observability >/dev/null'
```

Open `http://10.0.0.10:3000` from a device on the trusted LAN and verify the
provisioned Prometheus and Tempo data sources. The Home Agent dashboard may be
empty until the application is deployed; that does not mean the monitoring
stage failed. Keep the NAS firewall limited to the trusted LAN.

## 7. Deploy the application on the NAS

The NAS runs the image that CI publishes; nothing is built or streamed from the
Mac. Once per NAS, log the Docker daemon into the private package registry with
a GitHub token that has the `read:packages` scope:

```sh
ssh -tt home-nas 'sudo docker login ghcr.io'
```

Before every deployment, push `main` and wait for the "Production image"
workflow to publish `ghcr.io/vladar107/home-ai-assistant:sha-<commit>`:

```sh
gh run list --workflow=image.yml --commit "$(git rev-parse HEAD)"
```

Stop the GoLand poller for the production bot token first. Keep
`deploy/telegram-users.json` beside the repository (it is ignored by Git), and
ensure it contains both authorized users. Then run:

```sh
scripts/deploy-app-nas home-nas deploy/.env.nas
```

The command refuses to run unless `HEAD` is contained in `origin/main`. It
copies only the application Compose file and the derived `runtime.env` to a new
release under `/volume1/docker/home-ai-assistant`, copies the users file to the
protected application config directory, syncs only the Hearthkeeper token from
Keychain, and opens one interactive NAS terminal that pulls
`$HOME_AGENT_IMAGE_REPOSITORY:sha-<commit>`, rewrites `HOME_AGENT_IMAGE` in the
release's `runtime.env` to the pulled `sha256` digest, and runs
`sudo docker compose up -d`.

Check the application, the recorded image digest, and its independent monitoring
project:

```sh
ssh -tt home-nas 'cd /volume1/docker/home-ai-assistant/current/deploy && sudo docker compose --env-file runtime.env -f compose.app.yaml ps'
ssh home-nas 'grep ^HOME_AGENT_IMAGE= /volume1/docker/home-ai-assistant/current/deploy/runtime.env'
ssh home-nas 'curl --fail http://127.0.0.1:18080/healthz && curl --fail http://127.0.0.1:18080/readyz'
```

Use `/status`, `/privacy`, and `/settings` from each configured Telegram user.
Connect each user's own model subscription through `/settings`; profiles are
principal-bound and are not shared. Then perform the ordinary-message,
`/cancel`, and `/new` smoke checks.

To connect a user's Notion workspace on the NAS, install the token through
the SSH alias; the script writes it below `HOME_AGENT_DATA_PATH/integrations`
as the runtime user and the bot adopts it on that user's next `/settings`:

```sh
security find-generic-password -a home-ai-assistant -s notion.token -w | scripts/notion-set-token <principal> home-nas deploy/.env.nas
```

## 8. Open and verify Grafana

Grafana listens on the configured NAS LAN address. Open
`http://10.0.0.10:3000` and log in as `admin`. To paste the admin password
without printing it:

```sh
security find-generic-password \
  -a home-ai-assistant \
  -s grafana.admin-password \
  -w | pbcopy
```

After login, clear the clipboard:

```sh
pbcopy </dev/null
```

Verify:

1. **Connections → Data sources** shows provisioned Prometheus and Tempo data
   sources.
2. **Dashboards → Home Agent → Home Agent Overview** shows the running metric
   and Telegram activity after a `/status` command.
3. **Dashboards → Home Agent → Home Agent Runtime** shows goroutines, container
   memory against the 1 GiB limit, and the SQLite connection pool.
4. **Dashboards → Home Agent → Go Runtime** with the `service` dropdown set to
   `home-agent` shows GC pauses and goroutines.
5. **Explore → Tempo** can find recent traces for `service.name=home-agent`.
6. NAS container logs contain JSON events with matching trace and span IDs but
   no Telegram text, username, numeric ID, or token.

The dashboards are provisioned from files. Grafana rereads
`deploy/grafana/dashboards` every 30 seconds, so a dashboard change appears
within half a minute of a monitoring deploy without a Grafana restart. The
dashboards are read-only in the interface; edit the JSON and deploy again.

Grafana does not include Prometheus, Tempo, Loki, or Alloy under the hood; the
Compose file operates each as a separate, resource-bounded service. Loki and a
host exporter are deferred until live NAS measurements and permissions justify
them.

## 9. Connect Hearthwatch to Grafana

In Grafana, open **Alerts & IRM → Alerting → Contact points**, add a Telegram
contact point named `Hearthwatch`, and use the alert bot token plus chat ID from
Keychain. Retrieve one value at a time with `security find-generic-password
-a home-ai-assistant -s SERVICE -w | pbcopy`, paste it, and immediately clear
the clipboard.

Send Grafana's built-in test notification. It must arrive through Hearthwatch
and contain only operational test text. Grafana stores the bot token encrypted
in its persistent database using `grafana.secret-key`; Keychain remains the
canonical recovery copy. Do not reuse the Hearthkeeper token.

Create the first alert only after a baseline is visible. Start with a
two-minute pending alert for an absent `home_agent_application_running` metric,
route it to Hearthwatch, stop the application briefly to test it, and restore
the application. Tune the pending period after observing real restarts and
maintenance behaviour.

## 10. Routine checks, logs, and rollback

Inspect current state without exposing secrets:

```sh
ssh -tt home-nas 'cd /volume1/docker/monitoring/current/deploy && sudo docker compose --env-file runtime.env -f compose.monitoring.yaml ps'
ssh -tt home-nas 'cd /volume1/docker/home-ai-assistant/current/deploy && sudo docker compose --env-file runtime.env -f compose.app.yaml ps'
ssh -tt home-nas 'cd /volume1/docker/home-ai-assistant/current/deploy && sudo docker compose --env-file runtime.env -f compose.app.yaml logs --tail=200 home-agent'
```

Container logs rotate at 10 MB with five files per service. Loki is intentionally
absent.

To roll back manually, identify a retained release and inspect its
`deploy/runtime.env`. Start it with the matching Compose file and then update
the `current` symlink. An older binary must not be started against a database
that has newer migrations. Keep at least one known-good release and its data
backup until the replacement has been verified.

## 11. Backup and restore

Application state has higher recovery priority than telemetry. For a simple
transaction-consistent manual backup, stop only `home-agent`, take a NAS
snapshot or copy of its entire private data directory, and start it again:

```sh
ssh -tt home-nas 'cd /volume1/docker/home-ai-assistant/current/deploy && sudo docker compose --env-file runtime.env -f compose.app.yaml stop home-agent'
# Take the configured NAS snapshot or protected copy here.
ssh -tt home-nas 'cd /volume1/docker/home-ai-assistant/current/deploy && sudo docker compose --env-file runtime.env -f compose.app.yaml up -d home-agent'
```

Back up the Grafana data directory if alert configuration must survive a loss.
Prometheus metrics, Tempo traces, and Alloy queue state have lower recovery
priority and may be recreated. The source repository, Keychain secrets,
recorded image digest, ignored NAS environment file, and documented NAS paths
are all required for full recovery.

A backup is not accepted until it has been restored into an isolated directory
and the SQLite integrity check, application readiness, identity mapping, and
polling cursor have been verified without sending duplicate Telegram replies.

## 12. Rotation and incident rules

- Rotate a Telegram token in BotFather, update Keychain, synchronize secrets,
  and restart only its consumer.
- Rotate `grafana.admin-password` through Grafana and Keychain together.
- Rotating `grafana.secret-key` requires Grafana's documented re-encryption
  procedure; replacing the file alone can make stored alert credentials
  unreadable.
- Revoke the SSH public key on the NAS if the Mac or private key is lost.
- Treat unexpected host-key changes, plaintext registry credentials, broad NAS
  secret permissions, or token-bearing logs as an incident and stop deployment.
- Never paste diagnostic output that might contain message bodies or secrets
  into an issue without reviewing and redacting it locally.
