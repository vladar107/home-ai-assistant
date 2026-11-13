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
a model turn (`/status` reads the subscription usage); ordinary text gets a reply that remembers context; `/cancel` stops a slow
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

Once per Mac, let the GitHub CLI read packages, so the deployment can resolve a
tag to its digest:

```sh
gh auth refresh -h github.com -s read:packages
```

Push `main`, wait for the "Production image" workflow, stop the local poller, then
deploy:

```sh
scripts/image-tag
gh run list --workflow=image.yml --commit "$(git rev-parse HEAD)"
scripts/deploy-app-nas home-nas deploy/.env.nas
```

The script refuses uncommitted or unpushed state. Before it touches the NAS, it
resolves the commit's release tag to its digest in GHCR from the Mac and stops if
the tag is not published. It writes the Compose file and `runtime.env`, which pins
`HOME_AGENT_IMAGE_DIGEST`, copies the users, NAS roots, and MCP catalog files, and
syncs secrets from Keychain. It then pulls that digest, runs `docker compose up -d`,
and appends tag, digest, and commit to `deployments.log`. A tag that moves after the
resolution does not change what runs.

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
| NAS files | `HOME_AGENT_NAS_ENABLED=true` | none | Set the mount paths in `runtime.env` and copy `deploy/nas-roots.example.json` to `deploy/nas-roots.json`. The shared folder is mounted read-only; homes and inboxes are writable, saves only add new files, and only roots marked `create_folders` get new folders. Mark every root granted to more than one user `shared`. The roots file is the access rule between users, so review it like one. |
| Google (Gmail, Calendar) | `HOME_AGENT_GOOGLE_ENABLED=true`, `HOME_AGENT_GOOGLE_CLIENT_ID` | `scripts/keychain-set-secret home-agent.google-client-secret` | Desktop-app OAuth client in a published Google project (no logo, so no verification) with the Gmail and Calendar APIs enabled and `gmail.readonly` and `calendar.readonly` on the consent screen. Each user connects any number of accounts by pasting the failed `localhost` redirect URL into the chat, then switches Mail and Calendar per account in `/settings`. |
| Web | `HOME_AGENT_WEB_ENABLED=true`, optional `HOME_AGENT_WEATHER_HOME` | none | Search runs on the user's own subscription through the vendored `pi-web-search` extension; weather uses Open-Meteo. Locally set `HOME_AGENT_PI_WEB_SEARCH_EXTENSION=<repo>/third_party/pi-web-search/src/index.ts`. Re-check live search on every Pi upgrade. Each query is checked by the application first (see the architecture's security model); `home_agent_search_checks` counts decisions by `verdict` (`allow`, `confirm`, `deny`) and `reason`. |
| NS trains | `HOME_AGENT_NS_ENABLED=true` | Keychain `ns.api-key` (Ns-App product at apiportal.ns.nl) | The same subscription must also include OV-fiets (the places API) for `ns_ovfiets`; without it, that tool answers that the subscription lacks OV-fiets while the other NS tools keep working. |
| Home Assistant | `HOME_AGENT_HA_ENABLED=true`; locally `HOME_AGENT_HA_TOKEN`, optional `HOME_AGENT_HA_URL` (default `https://ha.home`) | Keychain `home-assistant.token`, synced as Docker secret `ha-token` | One household long-lived token; see the setup below. The overlay `deploy/compose.app.homeassistant.yaml` reaches Home Assistant at `http://homeassistant:8123` through host-gateway, because it uses host networking and the NAS cannot reach the Caddy macvlan edge. Per-user "Home" switch in `/settings`, on by default. |
| Grafana | `HOME_AGENT_GRAFANA_ENABLED=true`, `HOME_AGENT_GRAFANA_PRINCIPALS`; locally `HOME_AGENT_GRAFANA_TOKEN`, `HOME_AGENT_GRAFANA_URL` (default `http://grafana:3000`), `HOME_AGENT_GRAFANA_PRINCIPALS` | Keychain `grafana.home-agent-token`, synced as Docker secret `grafana-token` | Read-only dashboards, PromQL, and LogQL for the named principals only; nobody else sees it in `/settings`. See the setup below. The overlay `deploy/compose.app.grafana.yaml` reaches Grafana by service name over `home-observability`. Per-user "Monitoring" switch, on by default. |
| Buses | `HOME_AGENT_BUS_ENABLED=true`, `HOME_AGENT_BUS_HOME_STOP`, optional `HOME_AGENT_BUS_CONTACT` | none | Live data from OVapi (plain HTTP, no key); the stop list is read daily from OVapi's GTFS archive by range requests. The home stop is any timing point code of the stop (`stop_code` in GTFS `stops.txt`). Planned trips and boards for stops OVapi has no live data for go to Transitous; set the contact, which its terms ask for, to label those requests. Its use is private and non-commercial. |
| Shared locations | `HOME_AGENT_LOCATION_ENABLED=true`, optional `HOME_AGENT_LOCATION_CONTACT`; retention `HOME_AGENT_LOCATION_PIN_TTL` (1h, 5m–24h), `HOME_AGENT_LOCATION_LIVE_SILENCE` (1h, 5m–24h), `HOME_AGENT_LOCATION_LIVE_GRACE` (15m, 0–2h), `HOME_AGENT_LOCATION_PRUNE_INTERVAL` (10m, 1m–1h) | none | A pin, venue, or live location a user shares is acknowledged without a model call and named through Transitous reverse geocoding; while fresh, its coordinates go into that user's turns, and `nearby_places` queries OpenStreetMap's public Overpass API. Only the latest position per user is kept; it is hidden once expired and deleted at the next prune, with SQLite `secure_delete` on. Changing a duration needs only a restart. Set the contact, which both services' terms ask for. Users forget their location in `/settings`. Places a user saves by name are kept until they delete them and 24 hours more for Undo, after which the prune removes them (see Saved places and arrivals). |
| Background tasks | `HOME_AGENT_TASKS_ENABLED=true`, optional `HOME_AGENT_TASK_TIMEOUT` (60m, 1m–6h) and `HOME_AGENT_PI_TASK_MODEL` (`<model>[:<effort>]`, default `gpt-6.1-sol:medium`) | none | The model hands a long request to a sub-agent: a second Pi process on the user's own profile and subscription, beside the chat and outside its turn slot, with read-only tools, 100 tool calls, 150 model calls, and the timeout. One runs per user and three wait behind it. Its report arrives as a reply to the request; `/tasks` lists and cancels them. A restart resumes a running task once; a second interruption fails it. Rows and reports are pruned 30 days after a task finishes. Watch `home_agent_task_runs_total` by outcome and the NAS memory of a long run before raising limits. |
| Watches | `HOME_AGENT_WATCHES_ENABLED=true` | none | Requires conversation. Departure watches are offered with the NS or bus connector, disruption watches with the NS connector; route watches with saved routes, home watches with the Home Assistant connector, weather watches with the web connector, and agent watches always. An agent watch runs one read-only model turn per check on its owner's subscription, every 30 to 1440 minutes (60 by default), at most one per user and two in all; five agent watches at hourly checks are about 120 turns a day, counted in the usual turn metrics. Each check runs under the owner's trains or buses switch, or Home switch for home watches, and a cycle asks NS or the bus services once per departure or station for all of a user's watches on it. Alerts reach only the owner's chat, with Mute today and Pause; `/jobs` pauses and deletes watches. Window states and finished alerts are pruned after 30 days, and an alert's text is cleared once it is sent or dropped. A watch with a language has each message rewritten by one tool-less model call on the owner's subscription, falling back to English after 30 seconds or when a number would change. Watch `home_agent_watch_checks_total` by kind and outcome, `home_agent_watch_alerts_total` by type and outcome, `home_agent_watch_rewrites_total` by outcome, and `home_agent_watch_upstream_errors_total`. |
| Saved routes | on with conversation and NS trains or buses | none | Migration 019 adds the `routes` table. Each user may save 20 routes, saved at once with Undo in a clean turn and confirmed after outside content, and deletes them in `/jobs`; station and stop names are looked up when a leg is proposed, so a renamed station or stop needs the route edited. A train or bus leg is checked only while the user's trains or buses switch is on; otherwise it is reported as a failed check. |
| Model and usage | on with conversation; `HOME_AGENT_PI_MODEL` (`<model>[:<effort>]`) is the chat default | none | `/model` lists the models in the user's own Pi catalog (`models-store.json`, refreshed by Pi) with their effort levels and changes the pair of one conversation until `/new`; background tasks use `HOME_AGENT_PI_TASK_MODEL`, not that choice; the Heads-up job runs on `HOME_AGENT_PI_HEADSUP_MODEL` (`<model>[:<effort>]`, default `gpt-6.1-sol:high`) unless it has its own model, and any model-turn job may have one; a model the user's catalog or usage does not offer falls back to the chat default. Scheduled model turns have their own limit, `HOME_AGENT_SCHEDULED_TURN_TIMEOUT` (3m, 5s–10m), and still hold the user's turn lane, so a chat message sent during a run waits for it. `/status` reads the user's subscription usage live from an undocumented ChatGPT backend endpoint (`/backend-api/wham/usage`) with that user's own login, bounded to two seconds and cached for about a minute; the plan, the 5-hour and weekly percent used, and the reset times are shown, and account identifiers are never decoded, logged, or shown. When OpenAI changes the endpoint `/status` says usage is unavailable and nothing else stops. |
| Voice and pictures | on with conversation | none | Each user's own ChatGPT login, read from their Pi profile and refreshed under Pi's `auth.json` lock, reaches two undocumented ChatGPT backend endpoints: `/backend-api/transcribe` for voice notes and audio files (up to 20 MB; the transcript becomes the turn), and `/backend-api/codex/responses` with the hosted `image_generation` tool for `generate_image`, carried by `HOME_AGENT_PI_MODEL` without its thinking level. Pictures arrive as photos in the user's chat and spend that user's ChatGPT limits. There is no fallback: when OpenAI changes either endpoint, `telegram.voice.failed` or `imagegen.failed` logs show the status and the feature stops until the client in `internal/chatgpt` is updated. |
| TypeSafe checks | `HOME_AGENT_TYPESAFE_ENABLED=true` | `scripts/keychain-set-secret typesafe.api-key` | Each user opts in. Nothing is sent for users who have not. A failed check leaves the turn unchanged. |
| MCP servers | On the NAS `HOME_AGENT_MCP_SERVERS_FILE_PATH`, `HOME_AGENT_MCP_NETWORK`, optional `HOME_AGENT_MCP_SERVERS_SOURCE` (default `deploy/mcp-servers.json`); locally `HOME_AGENT_MCP_SERVERS_FILE` | none for the deployment; a `key` server takes each user's own key, connected in `/settings` | The servers the operator lists in a catalog, each with a per-user switch that is off by default; only allowlisted read tools are offered, and their results count as outside content. The overlay `deploy/compose.app.mcp.yaml` mounts the catalog and joins the sidecar project's client network, which may hold only the application and the operator's relays. See MCP servers below. |

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
5. Label a script that only reads `assistant-read`. The assistant then treats it
   as a read: no confirmation, and available to scheduled runs. Only label scripts
   without side effects, since a read may be retried, and expose them to Assist. A
   script that also carries `assistant-confirm` stays confirmed. Without the
   label every script is a write, whatever its name; only the live context,
   to-do item, and calendar event reads are reads by name.
6. Store the token in Keychain; `scripts/keychain-set-secret` does not cover it:
   `security add-generic-password -a home-ai-assistant -s home-assistant.token -U -w`.
7. Set `HOME_AGENT_HA_ENABLED=true` in `deploy/.env.nas`, run
   `scripts/nas-sync-secrets`, and deploy as for NS.

Locks, alarm panels, valves, sirens, cameras, firmware updates, and garage, gate,
or door covers count as needing confirmation. If one is exposed, or shares a name
with a labelled entity, all immediate actions are withheld, reads remain, and
`/settings` names the conflicting devices. Tools, exposure, and labels are cached
for 5 minutes.

To add a device, expose it (everyday) or label it `assistant-confirm` (confirmed).
No code change is needed.

Home watches (when watches are enabled) read `/api/states` once a minute with the
household token, and only for entities exposed to Assist or labelled
`assistant-confirm`. An appliance without a clean "finished" state, such as a
washing machine that only reports power, may need a template sensor in Home
Assistant to watch.

### Grafana setup

1. In Grafana, Administration → Users and access → Service accounts, add
   `home-agent` with the `Viewer` role and create a token for it.
2. Store the token in Keychain:
   `security add-generic-password -a home-ai-assistant -s grafana.home-agent-token -U -w`.
3. Set `HOME_AGENT_GRAFANA_ENABLED=true` and `HOME_AGENT_GRAFANA_PRINCIPALS`
   (comma-separated principal IDs) in `deploy/.env.nas`, run
   `scripts/nas-sync-secrets`, and deploy as for NS.

The tools query the provisioned `prometheus` and `loki` data sources by UID.
The same `Viewer` account also reads alert rules and alert annotations for
`grafana_alerts`; no further permission is needed.
To revoke access, delete the token in Grafana; the `claude-mcp` account is
separate.

### MCP servers

The assistant can offer the read tools of Model Context Protocol servers the
operator chose. It reaches them over streamable HTTP only and never starts a
server process: a hosted server is called at its HTTPS address, and a server that
only speaks stdio runs as a sidecar in a Compose project of its own. The
[`mcp-servers`](../mcp-servers/README.md) folder holds such a project, with each
server isolated behind an egress proxy and a relay; it is deployed apart from the
application.

**The catalog.** Servers are listed in a JSON file, kept outside Git as
`deploy/mcp-servers.json` (ignored) and copied to the NAS on each deploy. Start
from the example:

```sh
cp deploy/mcp-servers.example.json deploy/mcp-servers.json
```

```json
{
  "servers": [
    {
      "name": "listings",
      "title": "Listings",
      "description": "Search classified listings and read a listing's details.",
      "url": "http://example-sidecar:8000/mcp",
      "auth": "none",
      "tools": [
        { "name": "search_listings" },
        { "name": "get_listing", "description": "Read one listing by its ID." }
      ],
      "guidance": "Use for second-hand items; prices are in the listing's currency."
    },
    {
      "name": "prices",
      "title": "Price tracker",
      "description": "Look up a product's current price and price history.",
      "url": "https://mcp.example.com/mcp",
      "auth": "key",
      "key_pattern": "[A-Za-z0-9_-]{32,64}",
      "tools": [{ "name": "lookup_price" }, { "name": "price_history" }],
      "principals": ["person-one", "person-two"]
    }
  ]
}
```

| Field | Rule |
| --- | --- |
| `name` | Required, unique, `^[a-z][a-z0-9]{0,15}$`. Names the tools (`mcp_<name>_<tool>`), the per-user switch, and the key files (`mcp-<name>`); never rename a server in use |
| `title`, `description` | Required single lines, at most 48 and 160 characters; shown in `/settings`, and the title prefixes each tool's description |
| `url` | Required absolute `http` or `https` URL without user info, query, or fragment; its origin (scheme, host, port) at most 128 characters |
| `auth` | `none` (shared, no credential) or `key` (each user's own key). A `key` server must use `https` unless its host is `localhost` or a loopback address |
| `key_header`, `key_prefix` | Only with `key`; default `Authorization` and `Bearer `. An explicit `""` prefix sends the bare key. The header cannot be hop-by-hop, `Host`, `Content-*`, `Accept`, `Mcp-Session-Id`, or `Mcp-Protocol-Version`; the prefix is at most 32 characters without control characters |
| `key_pattern` | Only with `key`, optional; a Go regular expression of 1 to 256 characters that the whole key must match |
| `tools` | 1 to 16 remote tool names, unique, at most 128 characters without whitespace. An optional `description` (at most 1024 characters) pins the text the model reads instead of the server's |
| `guidance` | Optional single line of at most 300 characters, added to the prompt while the server's tools are offered |
| `principals` | Optional; omit it to offer the server to every user. When present it must list at least one principal ID, each once |

A file may list at most 12 servers; unknown fields, a missing field, or a broken
rule stop startup with a message that names the server and the field. The
catalog is read at startup, so a change takes effect when `home-agent` next
starts: a deploy of a new image restarts it, and after a deploy that only copied
a changed catalog, restart it:

```sh
ssh -tt home-nas 'cd /volume1/docker/home-ai-assistant/deploy && sudo docker compose -p home-ai-assistant --env-file runtime.env -f compose.app.yaml -f compose.app.mcp.yaml restart home-agent'
```

Add the other overlays the deployment applies to that command, as
`scripts/deploy-app-nas` does. Locally, set `HOME_AGENT_MCP_SERVERS_FILE` in
`.env` to the catalog's path.

**Deployment.** In `deploy/.env.nas`, set `HOME_AGENT_MCP_SERVERS_FILE_PATH` to
a path below `HOME_AGENT_NAS_ROOT`, such as the config directory, and
`HOME_AGENT_MCP_NETWORK` to the client network the sidecar project creates. The
deploy script applies `deploy/compose.app.mcp.yaml` only while
`HOME_AGENT_MCP_SERVERS_FILE_PATH` is set: it checks both values, copies
`HOME_AGENT_MCP_SERVERS_SOURCE` (default `deploy/mcp-servers.json`) to that path
with mode 600, and stops before pulling the image when the network does not
exist. The overlay mounts the catalog read-only at
`/etc/home-agent/mcp-servers.json` and adds the client network to
`home-observability`. That network may hold only the application and relays the
operator controls, never a server's own code (see the hardened sidecar below).
Deploy the sidecar project first. The overlay needs the
network even when every server is hosted; then create an empty internal network
for it (`sudo docker network create --internal <name>`).

**Turning a server on.** Each user sees the servers they may use in `/settings`
under Services. Every server is off until the user turns it on, because its tools
send what the user asks to a service outside the household. For a `key` server
the user connects their own key there: the next message is taken as the key when
it is a single token of 16 to 512 characters without whitespace that matches
`key_pattern` and is not a URL, within 10 minutes; any other message ends the
wait and reaches no server. Either way the message is deleted from the chat and
never reaches the model. Declare a `key_pattern` where the service's keys have a
recognisable shape: a message of that shape is then deleted and kept from the
model even when no wait is open, and another connector's secret pasted during
the wait is not sent to the server. The key is checked with `initialize` and `tools/list`; a 401 or 403
refuses it, another failure leaves it unverified, and in both cases nothing is
stored and the user can paste it again. Connecting a key turns the server on;
disconnecting removes the key and turns the server off. There is no household
key and no fallback to another user's.

**A hardened sidecar.** Third-party server code never joins the network named by
`HOME_AGENT_MCP_NETWORK`. `home-agent` listens on all its interfaces, and
anything on that network can reach its HTTP listener, including the
unauthenticated backlog API, so the network may hold only the application and
relays the operator controls. Each stdio-only server therefore runs as three
containers in a Compose project of its own, outside this repository:

- **the server**, running the third-party code behind a stdio-to-HTTP proxy, in
  an image the operator builds from a pinned commit of the server and a pinned
  version of the proxy and deploys by digest. It sits only on an internal
  network of its own, with no route to the local network or the internet. On
  Docker Engine 28 or later, that network's gateway is isolated
  (`com.docker.network.bridge.gateway_mode_ipv4: isolated`), so the host gets no
  address on it; without that, the host's own listening ports stay reachable
  from the server through the bridge gateway.
- **an ingress relay**, a TCP relay attached to the server's network and to the
  client network, which forwards the one port clients use. The server never
  shares a network with a client, and the application's catalog URL names the
  relay (`http://example-sidecar:8000/mcp`). A server that checks the `Host`
  header must accept that name.
- **an egress proxy**, only when the server must reach its own service on the
  internet: a filtering forward proxy that allows only the hosts the server
  needs, attached to the server's network and to an outbound network. The server
  is pointed at it with `HTTPS_PROXY`.

The relay and the proxy are operator-built and minimal: non-root, read-only,
capabilities dropped, small limits, and pinned by digest like the server. A
sketch, where the relay and proxy settings stand for whatever the chosen images
take:

```yaml
name: example-sidecars

x-hardened: &hardened
  restart: unless-stopped
  init: true
  read_only: true
  user: "65534:65534"
  cap_drop:
    - ALL
  security_opt:
    - no-new-privileges:true
  logging:
    driver: local
    options:
      max-size: 10m
      max-file: "3"

services:
  example-sidecar-server:
    <<: *hardened
    image: registry.example.com/listings-mcp@sha256:<digest>
    environment:
      HTTPS_PROXY: http://example-sidecar-egress:3128
    tmpfs:
      - /tmp:size=16m,mode=1777
    mem_limit: 256m
    cpus: 0.5
    pids_limit: 64
    networks:
      - server

  example-sidecar:
    <<: *hardened
    image: registry.example.com/tcp-relay@sha256:<digest>
    environment:
      RELAY_LISTEN: 0.0.0.0:8000
      RELAY_TARGET: example-sidecar-server:8000
    mem_limit: 32m
    cpus: 0.25
    pids_limit: 16
    networks:
      - clients
      - server

  example-sidecar-egress:
    <<: *hardened
    image: registry.example.com/egress-proxy@sha256:<digest>
    environment:
      PROXY_LISTEN: 0.0.0.0:3128
      PROXY_ALLOWED_HOSTS: api.example.com
    mem_limit: 64m
    cpus: 0.25
    pids_limit: 32
    networks:
      - server
      - outbound

networks:
  clients:
    name: example-sidecars
    internal: true
  server:
    internal: true
    driver_opts:
      com.docker.network.bridge.gateway_mode_ipv4: isolated
  outbound: {}
```

Here `HOME_AGENT_MCP_NETWORK=example-sidecars`. Nothing publishes a port, and
nothing mounts anything: never the application's data, configuration, secrets,
or the Docker socket. The server has no network path to the application: its
only network holds the relay, which forwards to the server and nowhere else, and
the egress proxy. Drop the proxy and the outbound network for a server that needs
no outside service. The outbound network is an ordinary bridge, which routes to
the local network as well as the internet: container networking does not keep
outbound traffic off the household's devices, so the proxy's host list and the
operator's network configuration, outside both repositories, do. Since a plain
HTTP URL cannot carry a key, a sidecar is always a `none` server; a credential
the server needs for its own service is the sidecar project's to hold.

**What may be allowlisted.** Allowlist only tools that read, and only those the
household needs: every allowlisted tool is offered as a read, so it also runs in
scheduled runs, background tasks, and agent watches without confirmation. Never
allowlist a tool that takes an address or fetches on the caller's behalf, since
after outside content a call can carry conversation text to wherever the
argument points. Tool descriptions come from the server and reach the model as
trusted tool definitions; for a server you do not control, pin each tool's
`description` in the catalog. A server's tools beyond the allowlist, its tool
titles and annotations, and its `instructions` are ignored, and every result is
outside content: a call marks the conversation exposed, and its result is
screened like a web page.

**Removing a server.** Delete its entry from the catalog and deploy (or restart
as above). Its tools and its row in `/settings` are gone once `home-agent`
restarts, but each user's key and switch stay behind, and would apply again to
a server later added under the same name. With `home-agent` stopped and a data
backup taken, remove them for the server's `<name>`:

- the key files `<HOME_AGENT_DATA_PATH>/integrations/*/mcp-<name>.json`;
- the rows of `integration_connections` and `integration_switches` in
  `<HOME_AGENT_DATA_PATH>/home-agent.db` whose `integration` is `mcp-<name>`:

```sql
DELETE FROM integration_connections WHERE integration = 'mcp-<name>';
DELETE FROM integration_switches WHERE integration = 'mcp-<name>';
```

Until then a user's `/export` still carries the server's switch; restoring it
grants nothing while the server is not in the catalog.

**Troubleshooting.** `/settings` shows each server in one of these states for the
user:

| State | Meaning | Fix |
| --- | --- | --- |
| Off | The user's switch is off | Turn it on |
| On | Tools are offered while the server lists them | — |
| Needs a key | A `key` server is on without a key file and connection record | Connect a key |
| Needs reconnection | The catalog's URL moved to another origin since the key was connected, so the key is never sent there | Connect the key again, which binds it to the new origin |
| Problem | The server refused the connected key (401 or 403) on a call or a list refresh; its tools stay offered | Usually a revoked or expired key: connect a new one. It clears after a call succeeds, on reconnecting, or on restart |

A server that is down shows no state of its own. A list it served in the last
hour keeps being offered, so its tools stay in turns and calls to it fail with
"The service is unavailable right now." or a timeout; once no list from the
last hour is left, its tools are not offered until a refresh succeeds, retried
in the background from 30 seconds up to every five minutes. A turn waits only
for a server with no list it may serve whose last refresh did not fail, such as
one nobody used for over an hour; all such lists of the user are refreshed at
once, so the wait is about three seconds in total.

The logs never carry a key, an argument, or what the server sent. Every event
names the server; all but `mcp.tools.dropped`, and `mcp.refresh.failed` for a
`none` server, name the principal too:

| Event | Meaning |
| --- | --- |
| `mcp.refresh.failed` | A tool list refresh failed; `class` says why, and `stale` whether a list from the last hour still serves |
| `mcp.tools.dropped` | An allowlisted tool the server listed was left out, with a `count`: an empty description, a schema over 8 KiB or not describing an object, an offered name over 64 characters, two names that become the same offered name, or a tool the server listed twice |
| `mcp.call.failed` | A tool call failed, with its `class` |
| `mcp.key.refused` | The server refused a connected key (401 or 403) on a call or refresh; logged once until the problem clears |
| `mcp.connection.connected` | A key was verified and stored |
| `mcp.connection.rejected` | The server refused a key being connected (401 or 403); nothing was stored |
| `mcp.connection.verify_failed` | A key could not be checked because the server failed otherwise, with its `class`; nothing was stored |
| `mcp.connection.switch_failed` | A key was stored but the server could not be turned on |
| `mcp.connection.disconnected` | A key or its record was removed |
| `mcp.connection.unreadable` | The connection record could not be read while offering tools; the server offered nothing in that turn |
| `mcp.switch.changed` | A user turned a server on or off in `/settings`, with `enabled` |
| `mcp.intake.started` | A user pressed Connect and the wait for a key began |
| `mcp.intake.failed` | A key the server accepted could not be stored; the wait stays open |
| `mcp.intake.yielded` | A wait ended because the message was another connector's secret; nothing was sent to the server |
| `mcp.credentials.unreadable` | A key file could not be read, logged once per user and server; the key counts as missing |

The classes `not_found` (a wrong path), `redirect_refused` (redirects are never
followed: use the final URL), `unsupported_version` (a server that does not
speak a revision from `2024-11-05` to `2025-11-25`), `timeout`, and
`unavailable` point at the server or the URL; `protocol` is a request the server
rejected, `malformed` an answer the client could not read, and `too_large` an
answer over 4 MiB or a list over 20 pages or 1000 tools. Calls appear in the
usual tool metrics and `home_agent.tool.call` spans under their `mcp_` names, and
retried requests in `home_agent.http.retries` under service `mcp`.

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
while the model call was still open. The model call duration and token series carry the
requested `effort` beside the answering `model`. Each turn's trace shows
`home_agent.model.call` spans (with `pi.auto_retry` events) and
`home_agent.tool.call` spans. A model call that Pi had to retry logs one
`pi.model_call.retried` warning with its `category`, `retries`, and `outcome`.
A turn that hears nothing from Pi for 45 seconds before any tool call or
reply text is stopped, its model call ending as `stalled`, and run once more
on its restored session. A silence after that point waits for the turn's own
deadline. Both are counted in `home_agent_model_stalls` by outcome
(`retried_ok`, `retried_failed`, `not_retried`) and log `pi.turn.stalled`.
`home_agent_model_processes` shows how many Pi processes are running and how
many are waiting for one of the 5 slots, by `kind` and `state` (`running`,
`waiting`). The kinds are `chat` for chat turns, `task` for background tasks
(at most 2 at a time), and `background` for everything else (also at most 2).
If chat turns keep waiting while memory stays well below the container limit,
raise the limits in `internal/runtime/pi/limit.go`. When Pi exits or closes its
output before a turn completes, `pi.process.exited_early` logs the exit code
and the scrubbed last kilobyte of its stderr. A conversation's photos are
stripped from its Pi session file when it closes. The scheduler's hourly
session prune deletes closed conversations and their files 90 days after
they close.

Add a Telegram contact point `Hearthwatch` with the alert bot token and chat ID
from Keychain. Start with an alert on an absent `home_agent_application_running`
metric, pending for two minutes.

### Trips

Migration 029 adds the `trips` and `trip_bookings` tables, and migration 030 adds
a `travel_pause` column to jobs and watches; neither can be undone, so take a data
backup before the first deploy that carries them. No setting is needed. Each user
turns the Trips scan on in `/settings`; it is offered only while Google mail or
calendar is on for them, and it runs daily at 07:30 as the job "Trips" (also
visible in `/jobs`). Heads-up moved to 07:30 and 18:30; at start, a Heads-up job
still on the old schedule is moved once, and one the user changed is left alone.
Moving a Heads-up job from 19:00 to 18:30 skips that one evening run.

Heads-up now runs a built-in method and keeps the job's text as the user's
notes. At start, a job whose text is still exactly the old built-in
instruction gets the default notes; a job whose text the user edited keeps it
as its notes. A job that stores a `model` cannot be read by an older image,
so rolling back to an image from before this change fails every run of any job
that stored one until the model is removed.

Migration 033 adds `trip_scans`, which keeps when each user's last successful
scan started. A scan with no success in the last 7 days searches 180 days of
mail on a budget of 60 tool calls; otherwise it searches 8 days on 40. A failed
or unreadable scan, one whose cards could not be sent, or one that could not
read mail (`mail=false` on `trip.scan.done`) records nothing. Turning Trips on,
or pressing *Scan now* in `/settings`, queues a manual run at once and logs
`trips.scan.queued`, unless a scan is already queued or running.

`home_agent_schedule_travel_paused_total` counts occurrences skipped because a job was
paused for a trip, and `schedule.run.travel_paused` logs the same with IDs only.
If a scan fails or sends no card, look for `trip.scan.done` (the days searched and counts
of found, new, updated, and dropped trips), `trip.scan.unreadable` (the model did not return
valid JSON), and `trip.scan.card_failed` in the logs, and check that the user's
model subscription is connected and Google mail or calendar is on. A manual Run
now from `/jobs` answers "nothing to report" when nothing is new, and otherwise
says how many trips it found or updated. Start and end
notices log `trip.notice.sent` and, when a send fails and is retried on the next
minute, `trip.notice.send_failed`. Logs carry IDs and counts, never titles,
destinations, or references.

### Weekly review

Migration 044 adds `tool_outcome_counts`, one count per user, UTC day, tool, and
outcome, written in the background and pruned after 30 days, and a `heading`
column on `approvals`; neither can be undone, so take a data backup before the
first deploy that carries them. No setting is needed. Each user turns the review
on in the Memory page of `/settings`; it runs Sundays at 18:00 as the job
"Weekly review" (also visible in `/jobs`), and *Review now* queues a manual run
unless one is already queued or running. Its cards go to the Reports tab and
stay confirmable for 24 hours, or until a restart expires them like any pending
proposal.

`home_agent_memory_reviews_total` counts reviews by outcome: `sent`, `nothing`
(the model proposed nothing valid), `skipped` (memory off, or a week with nothing
to review, which asks no model), and `failed`. `home_agent_memory_review_changes_total`
counts the proposals sent, and `home_agent_memory_lessons_total` now carries a
`mechanism` label (`feedback`, `procedure`, `review`), so dashboard queries on it
should sum or filter by that label. In the logs, `memory.review.done` gives the
sent and dropped counts, `memory.review.dropped` the action and reason of each
dropped change, `memory.review.unreadable` a reply that was not the JSON asked for
(nothing is sent, and it counts as `failed`), and `memory.review.exchange_unreadable`
a session file that could not be read. `tools.outcome.dropped` counts tool outcomes
the background writer had no room for, `tools.outcome.unrecorded` a failed write.
Logs carry IDs and counts, never reply texts or memory entries.

### Inbound events

Migration 045 adds `event_sources`, `inbound_events`, and `event_token_messages`;
it cannot be undone, so take a data backup before the first deploy that carries
it. No setting is needed: inbound events are on with conversation. Each user adds
sources in the Events page of `/settings`, which shows each token once and
deletes that message after ten minutes, also across a restart.

`POST /api/events` is served on the application's listener beside `/healthz`
and `/api/backlog`, so the home edge proxy's site for the assistant must forward
the path `/api/events` to the application, as it forwards `/api/backlog`. Keep it
on the private network; the token is the boundary only if it is ever exposed
further, which is out of scope. From a system on the home network:

```sh
curl -sS https://assistant.example/api/events \
  -H "Authorization: Bearer $EVENT_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"title":"Nightly backup failed","text":"Exit status 2.","severity":"warning","dedupe_key":"backup-2026-11-01"}'
```

It answers `202` with the event's ID, `401` for a missing or wrong token or a
user no longer allowlisted, `403` while the source is paused, `400` for a body
over 16 KiB, invalid JSON, a field out of range, or a control character, and
`429` past 60 events in an hour; a body not sent within 10 seconds is cut off.
A source in assess mode spends one model turn of its owner's subscription per
event, at most 90 seconds before the event is forwarded instead, counted in the
usual turn metrics; forward mode spends none. `home_agent_events_inbound_total` counts finished events by mode and
outcome (`delivered`, `suppressed`, `failed`, `dropped`, `duplicate`). In the
logs, `events.api.accepted` and `events.api.rejected` carry the source ID and
reason, `events.event.finished` each outcome, `events.event.retry` a send to be
retried, `events.assess.failed` an assessment that fell back to forwarding, and
`events.token_message.deleted` a token message removed; none carries a token or
an event's text.

### Anchored jobs, conditions, and weather watches

Migration 047 rebuilds `scheduled_jobs` and `schedule_runs` to admit jobs anchored
to a repeating event (`anchor = 'repeating'`, with an `occurrence_key` and a `last_occurrence_key`), keeping
every job and run with their travel flag, destination, and delivery progress.
Migration 048 rebuilds the three watch tables so their kind check admits
`weather` and `place` watches, keeping `travel_pause` and every index. Both are
this release's riskiest steps and neither can be undone: take a data backup
before the first deploy that carries them and keep it until jobs and watches are
verified, and never roll back to an older image afterwards (section 7). No setting
is needed.

Calendar anchors are read only for users with anchored jobs, with their own
Google accounts: each enabled anchored job is read every 15 minutes, once more
within 30 minutes of its run, and right after it is saved or run, each call
limited to 10 seconds, in a loop of its own beside the scheduler. Weather watches
are offered with the web connector and read Open-Meteo's 15-minute forecast every
10 minutes under the owner's web access switch, one request per place per 10
minutes whoever asks. In the logs, `schedule.anchor.moved` marks a job that
followed its event, `schedule.anchor.notice` a cancelled event (`anchor_gone`) or
a calendar that could not be read three passes in a row (`anchor_failed`), and
`schedule.anchor.failed` each failed read; a run whose condition did not hold is
`schedule.run.skipped` with reason `condition_unmet`, and a failed condition check
is `schedule.condition.failed`, after which the run still goes ahead. Weather
checks count in `home_agent_watch_checks_total` with kind `weather`, and a
failed forecast in `home_agent_watch_upstream_errors_total` with service
`weather`. Logs carry IDs and kinds, never event titles or places.

### Saved places and arrivals

Saved places come with shared locations (`HOME_AGENT_LOCATION_ENABLED=true`);
there is no setting of their own. Migration 049 only adds the `places` and
`place_presence` tables; as with any migration, an older image refuses the
database afterwards (section 7). Place watches need watches on as well, and place anchors need nothing more.
Arrivals and leavings are noticed only while a user shares a live location, and
may be noticed late when the phone pauses its updates. In the logs,
`location.place.crossed` marks each arrival or leaving with its place ID and
edge, `location.place.stale` one older than five minutes, from edits Telegram
queued while the bot was down, which is not acted on, `schedule.anchor.moved` with kind `place` a job it made due, and
`watch.alert.queued` an alert it queued; `location.presence.failed` is a failed
presence step, which never fails the share. Place checks count in
`home_agent_watch_checks_total` with kind `place`. No log line, metric label, or
span attribute carries a place's point or a position.

### Projects

Migration 050 adds `projects`, `project_items`, and `project_links`; it cannot be
undone, so take a data backup before the first deploy that carries it. No setting
is needed: projects are on with conversation, and with tabs on each open project
gets its own tab like a trip's. `/projects` lists them. A project's nudges and
reminders are ordinary jobs whose `destination` is `project:<id>`, so they show
in `/jobs` and count toward the job limits. In the logs, `project.created` and
`project.changed` carry the project ID, the tool, and item and link counts, and
`telegram.project_tab.created`, `telegram.project_tab.failed`, and
`telegram.project_tab.pin_failed` follow its tab as the `telegram.trip_tab.*`
events follow a trip's; none carries a title, an item, or a link.

### Content intake

Migration 051 adds `intake_records` and `intake_scans`; it cannot be undone, so
take a data backup before the first deploy that carries it. No setting is
needed: the intake tools are on with conversation. Each user turns the inbox
check on as Inbox intake in the Everyday tools page of `/settings`; it is
offered only while Google mail is on for them, runs as the job "Inbox intake"
(also visible in `/jobs`) every three hours between 08:00 and 22:00 in the
household zone, and each run is one model turn on the user's own subscription
with a budget of 40 tool calls. With tabs on, its summaries and cards go to the
Inbox tab.

In the logs, `intake.recorded`, `intake.updated`, and `intake.deleted` mark
records, `intake.scan.done` sums up a check with counts of what it recorded,
dropped, offered, kept waiting, and its stalls, `intake.scan.unreadable` a reply
that was not the JSON asked for (the run fails and records nothing; a second one
in a row skips that stretch of mail and tells the user once),
`intake.scan.refused` a follow-up its builder refused, such as a reminder time
already past or one agent watch more than the cap, `intake.scan.lost` a
follow-up dropped because an earlier check failed while offering it, and
`intake.scan.offer_failed` or `intake.scan.undelivered` a card that could not be
stored or sent. Logs carry IDs and counts, never record text. Turning Inbox
intake off forgets where it read to, so turning it on again reads the last day.

### Reply traces and /why

Migration 052 adds `turn_traces`, `turn_trace_calls`, and `turn_messages`, and
a `trace_key` column on `watch_alerts`; it cannot be undone, so take a data
backup before the first deploy that carries it. No setting is needed. Each
keyed turn writes one trace row and a row per tool call when it ends, and each
delivered message one link row. The scheduler's hourly prune removes traces
after 30 days, with their calls and links, and the traces of watch checks and
event assessments that raised no message after 2 days. Messages sent before the
deploy have no trace, so `/why` on them answers that the record is gone.

A trace holds no arguments or results, but its source labels can name private
things, such as a mail's sender and subject or a file path, so they stay in the
database: logs carry only `turn.trace.failed` and
`turn.trace.message_failed` with the principal and error, and
`provenance.reply.sent`, `provenance.reply.failed`,
`provenance.explanation.failed`, and `provenance.history.failed` with the
principal, the trace's kind, and a call count. Each `/why`, or "how do you
know?" in reply, on a model-written message costs one turn without tools on the
asker's own subscription and the runtime's default model, counted in the usual
model metrics as a `chat` process; each user has at most one running, and a
bare "why?" goes to the conversation instead. A chat trace still being written
at shutdown is waited for by the "turn traces" step.

## 7. Logs and rollback

```sh
ssh -tt home-nas 'cd /volume1/docker/home-ai-assistant/deploy && sudo docker compose --env-file runtime.env -f compose.app.yaml logs --tail=200 home-agent'
```

Logs are JSON with trace and span IDs. They never contain message text,
usernames, Telegram IDs, or tokens, and rotate at 10 MB × 5 files.

To roll back, choose a line from `deployments.log`, which gives the tag and the
`<repository>@sha256:<digest>` it ran, and pin both:

```sh
ssh -tt home-nas
cd /volume1/docker/home-ai-assistant/deploy
sed -i -e 's|^HOME_AGENT_IMAGE_TAG=.*|HOME_AGENT_IMAGE_TAG=<tag>|' \
  -e 's|^HOME_AGENT_IMAGE_DIGEST=.*|HOME_AGENT_IMAGE_DIGEST=sha256:<digest>|' runtime.env
sudo docker compose --env-file runtime.env -f compose.app.yaml pull home-agent
sudo docker compose --env-file runtime.env -f compose.app.yaml up -d home-agent
```

Never start an older binary against a database with newer migrations. Keep a data
backup until the replacement is verified.

## 8. Backup and restore

A user's `/export` and the operator's backup do different jobs. `/export` gives
one user a file with what they set up and keep here, which `/restore` adds back
into the same user's account, adding only what is missing. It holds no secrets,
connections, conversations, or records of what ran, so it is no replacement
for the operator's backup, which alone recovers the whole installation.

The file is signed with the installation's export key, `export.key` in the data
directory: 32 bytes, mode 0600, created on the first export. The data backup
below keeps it, so exports made before a recovery still restore after it.
Losing the key makes every earlier export unrestorable, and restoring a user's
export into another installation needs that installation to hold the same key,
which only the operator copies, deliberately. A restored event source has no
token until its owner presses *New token* in `/settings`.

For a consistent backup, stop `home-agent`, snapshot or copy the whole data
directory, and start it again. Application state matters more than telemetry, and
Prometheus, Tempo, and Alloy state can be recreated. A full recovery needs the
repository, the Keychain secrets, `deploy/.env.nas`, the users, roots, and MCP
catalog files, and the recorded image digest.

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
- Event source token: the user presses *New token* for the source in
  `/settings`, which ends the old token at once, and updates the system that
  posts with it; *Pause* refuses its events meanwhile.
- Lost Mac or SSH key: revoke the public key on the NAS.
- Treat an unexpected host-key change, plaintext registry credentials, broad NAS
  secret permissions, or a token in logs as an incident, and stop deploying.
