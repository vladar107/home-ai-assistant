# Deployment and Network

## Hardware placement

### Primary host

NAS:

- x86_64 CPU
- Limited memory installed
- Wired network interfaces
- NAS address currently `10.0.0.10`
- Docker/container and virtual-machine capability

The CPU and network are sufficient. Memory is the limiting resource when
combined with Immich and monitoring.


### Edge device

The available single-board computer has Wi-Fi, BLE, and GPIO. It is not the primary host.

Possible later roles:

- independent DNS/network probe;
- temperature or humidity sensor gateway;
- BLE scanner;
- IR transmitter;
- physical approval button or status LED.


## Current network topology

The home network is segmented. The NAS sits on a trusted segment, other
segments cannot reach it, and remote administration uses a VPN.

## Deployment diagram

```mermaid
flowchart LR
    GH[Private GitHub repository]
    CI[GitHub Actions]
    MAC[Operator Mac]
    INTERNET[Internet]
    ROUTER[Router\n<router-address>]
    SWITCH[Switch]

    GH --> CI
    MAC -->|source and configuration over SSH| NAS
    INTERNET --> ROUTER
    ROUTER -->|trunk| SWITCH

    subgraph HOME[Home VLAN]
        NAS[NAS\n10.0.0.10]
        USERS[Personal devices]
    end

    subgraph NASWORKLOADS[NAS workloads]
        MON[Independent monitoring Compose project]
        AGENT[Home Agent Compose project]
        PI[Pi runtime inside Home Agent]
        IMMICH[Immich]
        PROM[Prometheus]
        TEMPO[Tempo monolithic]
        ALLOY[Grafana Alloy]
        GRAFANA[Grafana]
        EXPORTER[Later host exporters]
    end

    SWITCH -->|access port| NAS
    NAS --- NASWORKLOADS
    USERS --> NAS
    VPN[WireGuard clients] --> ROUTER
    ROUTER --> HOME

    AGENT -->|outbound 443| INTERNET
    PI -->|outbound model traffic| INTERNET
    AGENT -->|OTLP, home-observability network| ALLOY
    ALLOY --> PROM
    ALLOY --> TEMPO
    GRAFANA --> PROM
    GRAFANA --> TEMPO
```

## Internet exposure

Default policy:

```text
Internet → NAS: no inbound port forwards
NAS → Telegram/OpenAI/Google/Notion/GitHub: outbound TLS
Home/WireGuard → Grafana/admin endpoints: allowed as configured
Other VLANs → NAS: blocked
```

Telegram uses outbound long polling. Existing VPN access already
solves remote administration. CloudFront, Cloudflare Tunnel, and additional DDNS
are not required for the initial agent.

The first deployment binds Grafana to the configured NAS LAN address (for this
NAS, `10.0.0.10:3000`). Prometheus, Tempo, Alloy, application health, and OTLP
listeners stay on the private container network or NAS loopback. No service is
published to the internet; restrict the Grafana port to the trusted LAN in the
NAS firewall.

## Build and deployment flow

The source lives in a private GitHub repository. CI validates the source and
may publish an image artifact, but production deployment is an explicit
operator-triggered build on the NAS. The Mac copies versioned source and
configuration files over SSH; no application image is streamed or pulled from a
registry.

The monitoring project is deployed first:

1. Copy the monitoring Compose file, Alloy configuration, Prometheus
   configuration, Grafana provisioning, and non-secret environment file into a
   versioned directory under `/volume1/docker/monitoring`.
2. Pull the pinned public Alloy, Prometheus, Tempo, and Grafana images on the
   NAS.
3. Ensure the private external `home-observability` network exists and start
   the monitoring project on it.
4. Verify readiness, Grafana data sources, dashboards, and the SSH tunnel.

The application is deployed separately:

1. Copy the checked-out Go and Docker build context, application Compose file,
   multi-user users file, and non-secret environment file into a versioned
   directory under `/volume1/docker/home-ai-assistant`.
2. Build the `linux/amd64` image on the NAS.
3. Materialize only the Telegram token from Keychain into the protected
   application secret directory.
4. Start the application project on the existing `home-observability` network.
5. Check the application health endpoints and Telegram smoke flows, then point
   the application `current` symlink at the release.

Monitoring and application releases, persistent data, and rollback operations
remain independent. A monitoring restart must not stop the application, and an
application rollback must not rewrite monitoring state.

## SSH operator access

The NAS account holds only the public half of a dedicated Ed25519 key in its
`authorized_keys`. The encrypted private key never leaves the Mac, and its
passphrase is cached through macOS Keychain. `~/.ssh/config` defines the
`home-nas` alias with the verified address, operator account, port, identity
file, `IdentitiesOnly yes`, `AddKeysToAgent yes`, and `UseKeychain yes`.

Before changing Docker permissions or choosing storage paths, verify the NAS in
person or through its trusted administration UI, validate the SSH host-key
fingerprint, then inventory architecture, account UID/GID, Docker Engine,
Docker Compose, filesystem capacity, and Docker-socket access. Docker control
is effectively root-equivalent; do not grant it to a broad household account
or mount the socket into the application. The NAS vendor documents its
Terminal controls and
firewall configuration.

## Container topology

Initial services:

| Service | Network exposure | Persistent data | Suggested memory envelope |
| --- | --- | --- | ---: |
| `home-agent` | Telegram/API egress; private OTLP/health endpoints | SQLite and application state | 256–384 MB |
| `pi-runtime` | Model egress; private IPC with agent | Isolated auth/profile and optional session state | 256–768 MB |
| `alloy` | Private OTLP input and backend output only | Normally none | 128–256 MB |
| `prometheus` | Private only | Metrics TSDB | 512 MB–1 GB |
| `tempo` | Private only, monolithic mode | Trace blocks and WAL | 256–512 MB |
| `grafana` | Configured NAS LAN address | Internal database, dashboards, alert state | 256–512 MB |
| later host exporters | Prometheus only | None | under 128 MB total |

These are conservative planning envelopes, not measured requirements or a
license to reserve every maximum simultaneously. Compose starts with protective
limits for the initial services; adjust them only from observed startup and
steady-state evidence. A limit that causes restart loops is worse than a
measured alert threshold. Pi, Prometheus, and Tempo behaviour must be measured
on the target NAS before production use.

Grafana is the query, dashboard, and alert interface; it does not bundle
Prometheus or Tempo. Alloy is the OpenTelemetry collector and routing layer.
Loki is deliberately excluded at launch: structured stdout logs use bounded
container rotation, and local log aggregation is added only after a measured
need. Home Assistant and another application database also remain deferred.
See the official [Grafana data-source documentation](https://grafana.com/docs/grafana/latest/datasources/)
and [Grafana Alloy introduction](https://grafana.com/docs/alloy/latest/introduction/).

## Eight-gigabyte feasibility

The application and observability stack, excluding Pi, should be budgeted at
roughly 1.4–2.8 GB including container/runtime overhead. Adding Pi raises the
planning range to roughly 1.7–3.6 GB before Immich and filesystem cache. These
are deliberately cautious estimates; deployment on 8 GB is conditional on an
idle and load-test baseline that leaves meaningful memory available to NAS OS,
Immich, and the kernel page cache.

Upgrade criteria:

- sustained low `MemAvailable`, initially use 1–1.5 GB as an investigation
  threshold rather than a hard SLO;
- repeated swapping with user-visible latency;
- OOM kills or container restart loops;
- Immich machine-learning jobs starving control-plane services;
- adding Loki, additional Tempo components, or virtual machines.

If triggered, 16 GB is the practical minimum upgrade; 32 GB provides comfortable
growth. The hardware supports larger capacity.

## Volumes and permissions

Proposed logical volumes:

```text
monitoring/releases/       Versioned monitoring Compose and configuration
monitoring/prometheus/     Metrics data
monitoring/tempo/          Trace blocks and WAL
monitoring/grafana/        Dashboards and alert configuration
monitoring/alloy/          Alloy queue/state
home-ai-assistant/releases/  Versioned application source and Compose
home-ai-assistant/data/      SQLite and per-user Pi state
home-ai-assistant/config/    Multi-user and other non-secret configuration
home-ai-assistant/secrets/   Protected Telegram and runtime secret copies
```

These logical volumes live under the existing `/volume1/docker` shared folder,
alongside but separate from `immich-app`. The operator must still verify the
live filesystem, backup policy, and the UID/GID used by each container before
binding them into Compose.

NAS content roots are mounted read-only and separately from application state.
Pi does not receive NAS content mounts in the generic runtime configuration.
The application state directory is private (`0700`) and must live below
operator-controlled parent directories; the SQLite database and its WAL files
are private (`0600`). The service rejects an existing data directory with a
broader mode instead of changing permissions on an operator-supplied path.

macOS Keychain is the canonical store for runtime secrets. The NAS keeps only
the decrypted copies required by an always-running service, in a dedicated
directory with mode `0700`; each secret file has mode `0600`, appropriate
ownership, and a read-only mount visible only to its consumer. Secrets never
enter Git, Compose environment values, image layers, CI, shell command
arguments, or logs. File-backed secrets are preferred because environment
values are visible through container inspection. Docker documents the
[Compose secrets model](https://docs.docker.com/compose/how-tos/use-secrets/).

Compose implementations commonly bind-mount file-backed secrets and may not
honour requested UID/GID/mode overrides. Validate the actual in-container mode
and readability on this NAS before starting the application. Do not broaden a
file beyond `0600` to make a mismatched container UID work; align ownership or
the container identity instead.

## Container security

- Non-root user where supported
- Read-only root filesystem where practical
- Drop Linux capabilities
- No privileged mode
- No Docker socket
- No host PID namespace
- Explicit outbound destinations where enforceable
- Health checks and restart policy
- Memory/CPU limits based on measured behaviour
- Secrets supplied through protected files or a supported secret mechanism,
  never committed configuration

## Network hardening path

The Home VLAN provides more reach than the agent needs. Before enabling
infrastructure or account writes, choose one:

1. Assign the agent container a unique macvlan/ipvlan address and restrict it by
   source address in UniFi.
2. Add a Services VLAN and place the workload or NAS service interface there.
3. Use a constrained VM with a dedicated network identity.

The preferred rule shape is explicit allow to required APIs and LAN services,
then deny access to Work, Guest, and unrelated IoT segments.

## Backup and restore

Back up:

- SQLite database using a transaction-consistent mechanism;
- non-secret configuration and dashboard provisioning;
- encrypted credential records or documented re-authorization procedures;
- Pi session state only if it becomes part of the product contract.

Do not assume a backup is valid until a restore has been tested. Audit logs and
metrics have lower recovery priority than principals, schedules, grants, and
outbox state.
