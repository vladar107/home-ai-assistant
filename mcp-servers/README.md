# Home MCP Servers

Model Context Protocol servers for the household NAS that have no hosted
endpoint. Each runs as untrusted third-party code: isolated on its own internal
network, allowed out only to the hosts it needs, and reached by clients through
a relay. The services run as one Docker Compose project, `home-mcp-servers`.
Clients such as [Home Agent](https://github.com/vladar107/home-ai-assistant)
join the private `home-mcp` network and call `http://<server>:8000/mcp`.

## Servers

| Name | Upstream | Tools | Outbound hosts |
| --- | --- | --- | --- |
| `marktplaats` | [jasp-nerd/marktplaats-mcp](https://github.com/jasp-nerd/marktplaats-mcp), pinned by commit in `marktplaats/Dockerfile` | Read-only search of Marktplaats and 2dehands: listings, details, sellers, categories, new listings, price analysis | `marktplaats/egress.filter` |

Over HTTP the Marktplaats server registers no account tools and refuses to
start with account cookies configured, so it is read-only by construction. It
is an unofficial client of the site's own endpoints and leaves from the home
address; `MARKTPLAATS_MIN_INTERVAL_MS` spaces its requests.

## Layout

```text
compose.yaml              The single Compose file; three services per server
proxy/Dockerfile          tinyproxy and socat, shared by every egress and relay
proxy/egress.conf         Forward proxy settings shared by every egress
marktplaats/Dockerfile    The server, built from the pinned upstream source
marktplaats/egress.filter The hosts that server may reach
smoke/                    Checks that scripts/smoke runs on the NAS
scripts/deploy            Build and start the committed configuration on the NAS
scripts/smoke             Check the client path and the isolation
scripts/nas-preflight     Read-only NAS inventory
nas.env.example           Template for the ignored .env.nas
```

NAS, below `MCP_ROOT`:

```text
deploy/           compose.yaml, .env with the revision, and the build inputs; replaced on every deploy
deployments.log   One "<utc-timestamp> <commit>" line per deploy, mode 0600
```

## Deploy

Prerequisites: an SSH alias `home-nas` with key authentication, and Docker
Engine 28 or later with Compose v2 and buildx on the NAS. Docker there requires `sudo`,
which prompts for a password, so every Docker step runs in one interactive
`ssh -tt` session.

```sh
cp nas.env.example .env.nas   # once; set MCP_ROOT
scripts/nas-preflight         # once; prints the Docker versions
scripts/deploy
scripts/smoke
```

`scripts/deploy` requires committed inputs, creates the internal `home-mcp`
network if it is missing, builds the images on the NAS tagged with the commit,
and starts the services. The build needs GitHub, PyPI, Docker Hub, and GHCR.
Roll back by checking out the previous commit and deploying it; its images are
still on the NAS, so nothing is rebuilt.

`scripts/smoke` passes when a client on `home-mcp` can list the tools and run a
search, the server cannot connect to the internet, to a port on the NAS itself,
or resolve outside names, and the proxy refuses a host that is not in the
filter.

## Update a server

1. Read the upstream diff from the pinned commit to the new one.
2. Set `UPSTREAM_SHA` and `UPSTREAM_SHA256` (the SHA-256 of
   `https://github.com/<owner>/<repo>/archive/<sha>.tar.gz`) in its Dockerfile.
3. If it reaches new hosts, add them to its `egress.filter`.
4. Run the stack locally, run the smoke checks, commit, and deploy.

## Add a server

Copy the three `marktplaats*` services in `compose.yaml` and the `marktplaats/`
directory under the new name, with its own internal, isolated network and its
own filter.
Then add the server to each client's catalog.
