# Home MCP Servers Project Instructions

## Purpose

- This repository runs Model Context Protocol servers that have no hosted
  endpoint, as the `home-mcp-servers` Compose project on the NAS. Clients such
  as Home Agent join the private `home-mcp` network and call
  `http://<server>:8000/mcp`.
- Only servers that every household member can share belong here: no account,
  no key, read-only tools. A server that needs one person's login does not.
- The servers are third-party code. Treat every one as untrusted: it gets no
  secrets, no volumes, no published ports, and no network but its own.

## A Server Is Three Containers

- `<name>-server` runs the third-party code on an internal, isolated network
  named after it, with no route to the LAN, the internet, or the NAS itself.
- `<name>-egress` is its only way out: a forward proxy that allows CONNECT to
  port 443 of the hosts in `<name>/egress.filter` and nothing else.
- `<name>` relays port 8000 from `home-mcp`, so the server never shares a
  network with a client.
- Add a server by copying these three services and its directory. Never put two
  servers on one internal network or behind one egress filter.

## Development Workflow

- Work directly on `main`; this private project does not use pull requests.
- Keep history linear with signed conventional commits, one coherent change
  per commit. Amend the relevant commit when correcting committed work instead
  of adding a follow-up repair commit.
- Pin every base image by digest and every upstream source by commit and
  archive checksum. Read the upstream diff before a bump, and bump in its own
  commit.
- Install only hash-locked wheels. No build backend or other unpinned code may
  run during an image build.
- After changes run `bash -n scripts/*`, `shellcheck scripts/*`, and
  `REVISION=dev docker compose config --quiet`.
- Before deploying a change to a server or the proxy, bring the stack up
  locally and run `smoke/client.py` and `smoke/probes.py` as `scripts/smoke`
  does.

## Security and Operations

- Put configuration into images, never into bind mounts: the NAS admits only
  the deploying account to the deployment tree, whatever its mode bits say, so
  a container running as another user cannot read a mounted file.
- Never commit `.env.nas`. The NAS path is configuration, not a secret, but it
  stays in the ignored file.
- Deployment is manual from the operator's Mac with `scripts/deploy`. It builds
  the images on the NAS, tagged with the commit, and keeps the previous ones;
  roll back by deploying the previous commit.
- Run NAS Docker commands through `ssh -tt home-nas` with `sudo`; the NAS
  account has no Docker socket access and `sudo` always prompts for a
  password, so `sudo -n` and non-interactive sessions fail.
- A plain internal network still reaches services the NAS itself listens on,
  through the network's gateway address. Every server network therefore also
  sets `com.docker.network.bridge.gateway_mode_ipv4: isolated`, which gives the
  NAS no address on it; `scripts/smoke` fails if a host port answers.
- Container logs are shipped to Loki by the monitoring stack. Requests are
  logged without their content, but a server's error traceback can carry query
  text.
- Clients' deploy scripts require the `home-mcp` network to exist as an
  internal network; never remove, rename, or recreate it as a plain bridge.
