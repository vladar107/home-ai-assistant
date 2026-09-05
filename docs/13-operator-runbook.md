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
5. Use `/setcommands` and enter:

   ```text
   status - Check whether the home agent is online
   privacy - Show the current privacy boundary
   cancel - Stop the current model response
   new - Start a fresh conversation
   ```

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

Set `HOME_AGENT_TELEGRAM_BOT_TOKEN` to the Hearthkeeper token. Leave
`HOME_AGENT_TELEGRAM_USER_ID` empty until the next section discovers it.

If a token is ever pasted elsewhere or committed, revoke it with BotFather and
replace the configured token before continuing.

## 2. Discover your numeric Telegram user ID

Open a private chat with Hearthkeeper, press **Start**, and send one message.
Stop any existing bot process first because Telegram permits only one active
`getUpdates` poller per bot.

Run:

```sh
scripts/telegram-list-users main
```

The script reads the token from `.env`, sends it to `curl` through standard
input, and prints only sender ID, username, and display name. It does not print
message text or the token. Put the positive numeric ID in `.env` as
`HOME_AGENT_TELEGRAM_USER_ID`. This ID is allowlist configuration, not a
secret, and is never stored in Keychain.

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
bodies after delivery. Neither location is committed. This local feature is
not enabled in the production Compose configuration because the current image
does not package Pi or its authentication profile.

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
dedicated deployment account. Docker access is effectively root-equivalent;
do not share this account or mount the Docker socket into a container.

Select a user-writable absolute data root on the real NAS volume. The example
below is intentionally a placeholder; do not copy it until the actual NAS OS
volume layout and backup coverage are known:

```sh
umask 077
install -d -m 700 /VALIDATED_VOLUME/home-ai-assistant
install -d -m 700 /VALIDATED_VOLUME/home-ai-assistant/releases
install -d -m 700 /VALIDATED_VOLUME/home-ai-assistant/home-agent/data
install -d -m 700 /VALIDATED_VOLUME/home-ai-assistant/home-agent/secrets
install -d -m 700 /VALIDATED_VOLUME/home-ai-assistant/alloy
install -d -m 700 /VALIDATED_VOLUME/home-ai-assistant/prometheus
install -d -m 700 /VALIDATED_VOLUME/home-ai-assistant/tempo
install -d -m 700 /VALIDATED_VOLUME/home-ai-assistant/grafana
install -d -m 700 /VALIDATED_VOLUME/home-ai-assistant/grafana-secrets
```

Run those commands while connected as the deployment account, so all
directories have that account's UID and GID. Do not solve permission errors
with `chmod 777`.

On the Mac, create the ignored deployment environment:

```sh
cp deploy/nas.env.example deploy/.env.nas
chmod 600 deploy/.env.nas
${EDITOR:-vi} deploy/.env.nas
```

Set `NAS_RUNTIME_UID` and `NAS_RUNTIME_GID` to the numeric values printed by
`nas-preflight`. Replace every placeholder path with the directories just
created. The Compose services intentionally run as this verified unprivileged
identity so their mode-`0600` secret files and mode-`0700` data directories do
not need broader permissions.

Set `HOME_AGENT_TELEGRAM_USER_ID` in `deploy/.env.nas`. Store the bot token and
remaining Grafana secrets in Keychain. Use a unique generated admin password
and at least 32 random characters for the Grafana secret key; paste each only
at its Keychain prompt:

```sh
scripts/keychain-set-secret home-agent.telegram-bot-token
scripts/keychain-set-secret grafana.admin-password
scripts/keychain-set-secret grafana.secret-key
```

Synchronize only the always-needed runtime copies:

```sh
scripts/nas-sync-secrets home-nas deploy/.env.nas
```

The script refuses broad local environment-file permissions, verifies both NAS
secret directories are mode `0700` and owned by the SSH account, and installs
each non-empty secret atomically with mode `0600`.

## 6. Create the private repository and image pipeline

Keep the GitHub repository private. The included workflows have separate
responsibilities:

- `CI` formats, validates module files, tests, vets, and builds;
- `Production image` builds only `linux/amd64`, publishes a commit-SHA tag to
  private GHCR, and records build provenance;
- neither workflow has NAS or runtime secrets, network access to the NAS, or a
  deployment step.

Publish only after reviewing and committing the complete change. If no private
remote exists yet, create one with the GitHub CLI when ready:

```sh
gh repo create vladar107/home-ai-assistant --private --source=. --remote=origin
git push -u origin main
```

Confirm the repository and GHCR package are private. On the Mac, authenticate
Docker to GHCR using a token with package read access. With Docker Desktop or
the `osxkeychain` credential helper configured, this stores the credential in
Keychain instead of plaintext Docker configuration:

```sh
gh auth token | docker login ghcr.io -u vladar107 --password-stdin
```

Check `~/.docker/config.json` for a credential helper such as `desktop` or
`osxkeychain`; stop if it contains a plaintext `auth` entry for GHCR. The NAS
does not log in to GHCR.

After the image workflow succeeds, select the exact manifest digest for the
`sha-<full-commit>` tag. A deployable reference has this shape:

```text
ghcr.io/vladar107/home-ai-assistant@sha256:64-lowercase-hex-characters
```

## 7. Deploy manually from the Mac

Ensure Docker Desktop is running, the repository is checked out at the exact
image revision, and the deployment inputs are committed. Then run:

```sh
scripts/deploy-nas \
  ghcr.io/vladar107/home-ai-assistant@sha256:REPLACE_WITH_REAL_DIGEST \
  home-nas \
  deploy/.env.nas
```

The script performs these gates:

1. validates an exact GHCR digest and a clean set of deployment inputs;
2. pulls `linux/amd64` on the Mac and verifies the image's revision label;
3. tags it locally and streams `docker image save` directly through SSH to
   `docker image load`, without an archive or NAS registry credential;
4. copies only version-controlled deployment configuration into a versioned
   NAS release directory;
5. refreshes runtime secret files from Keychain;
6. pulls and starts Alloy, Prometheus, Tempo, and Grafana first;
7. checks their private readiness endpoints, then starts home-agent;
8. requires application readiness plus an application metric and trace;
9. updates the `current` release symlink only after all gates pass.

If a gate fails and a previous release exists, Compose is reconciled back to
that release. The failed release and both images remain available for
diagnosis; cleanup is a separate explicit operation.

## 8. Open and verify Grafana

Grafana listens only on NAS loopback. Keep this terminal open:

```sh
ssh -N -L 3000:127.0.0.1:3000 home-nas
```

Open `http://127.0.0.1:3000` and log in as `admin`. To paste the admin password
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
3. **Explore → Tempo** can find recent traces for `service.name=home-agent`.
4. NAS container logs contain JSON events with matching trace and span IDs but
   no Telegram text, username, numeric ID, or token.

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
ssh home-nas 'cd /VALIDATED_VOLUME/home-ai-assistant/current/deploy && docker compose --env-file runtime.env ps'
ssh home-nas 'cd /VALIDATED_VOLUME/home-ai-assistant/current/deploy && docker compose --env-file runtime.env logs --tail=200 home-agent'
```

Use the exact validated NAS root in place of the placeholder. Container logs
rotate at 10 MB with five files per service. Loki is intentionally absent.

To roll back manually, identify a retained release, inspect its `source-image`
and `runtime.env`, and pair it with the data snapshot taken before that release
changed the database. An older binary must not be started against a database
that has newer migrations. Update the `current` symlink only after readiness
and telemetry are verified. Never delete the last known-good image, release,
or pre-upgrade snapshot during the same operation as a deployment.

`scripts/deploy-nas` stops the previous Home Agent immediately before starting
the new image and copies its clean data directory to
an attempt-specific `pre-upgrade-data-*` directory inside the new release. If
readiness or telemetry validation fails, the script stops the new container,
restores that attempt's snapshot, and starts the previous Compose release
before returning an error. Retained snapshots must be associated with their
deployment attempt before a later manual restore.

## 11. Backup and restore

Application state has higher recovery priority than telemetry. For a simple
transaction-consistent manual backup, stop only `home-agent`, take a NAS
snapshot or copy of its entire private data directory, and start it again:

```sh
ssh home-nas 'cd /VALIDATED_VOLUME/home-ai-assistant/current/deploy && docker compose --env-file runtime.env stop home-agent'
# Take the configured NAS snapshot or protected copy here.
ssh home-nas 'cd /VALIDATED_VOLUME/home-ai-assistant/current/deploy && docker compose --env-file runtime.env up -d home-agent'
```

Back up the Grafana data directory if alert configuration must survive a loss.
Prometheus metrics, Tempo traces, and Alloy queue state have lower recovery
priority and may be recreated. The source repository, Keychain secrets,
selected image digest, ignored NAS environment file, and documented NAS paths
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
- Treat unexpected host-key changes, a plaintext GHCR credential, broad NAS
  secret permissions, or token-bearing logs as an incident and stop deployment.
- Never paste diagnostic output that might contain message bodies or secrets
  into an issue without reviewing and redacting it locally.
