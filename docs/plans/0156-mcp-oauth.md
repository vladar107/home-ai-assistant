# 0156 — OAuth for MCP servers

Status: `proposed`

## Context

Plan 0136 added MCP servers from an operator catalog with two auth kinds: `none`, a shared
server with no credential, and `key`, where each user pastes their own key. That key is a
static secret the service issued out of band, such as an API key or a personal access token.
The client sends it unchanged in the header the entry names (`Authorization: Bearer ` by
default). It lives in `data/integrations/<principal>/mcp-<server>.json` through
`integration.Credentials`. Its non-secret record is a personal row in
`integration_connections` whose workspace ID is the origin of the server's URL, and the key is
not sent once the catalog's origin changes. OAuth was left to a later plan.

Many hosted MCP servers accept no key at all, only OAuth. Hosted issue trackers,
note-taking services and error trackers are examples. Their metadata, checked while writing
this plan, publishes Protected Resource Metadata, supports Client ID Metadata Documents and
Dynamic Client Registration, requires PKCE with `S256`, and sends the `iss` authorization
response parameter. Home Assistant's own MCP server, which plan 0152 considers, also signs in
through OAuth. It uses IndieAuth-style client IDs: the client ID is a URL, there is no
registration, and the redirect must share the client ID's scheme and host.

The MCP authorization specification is current at revision
[2026-07-28](https://modelcontextprotocol.io/specification/2026-07-28/basic/authorization).
The client in `internal/connectors/mcp` speaks the transport of 2025-11-25 and earlier.
Authorization is a transport-level layer common to both, and this plan follows the current
revision. It requires the following:

- Protected Resource Metadata (RFC 9728). It is found from `resource_metadata` in a 401's
  `WWW-Authenticate`, or else at the path-inserted and then the root well-known URI.
- Authorization server metadata, tried as RFC 8414 and then as OIDC discovery in a fixed
  order. The client rejects a document whose `issuer` differs from the one it asked for, and
  refuses to proceed when `code_challenge_methods_supported` is absent.
- PKCE with `S256`.
- The `resource` parameter (RFC 8707) in both the authorization and the token request,
  carrying the server's canonical URI.
- Issuer validation of the authorization response (RFC 9207). This is new in this revision.
- Client registration in this order: pre-registered, then Client ID Metadata Documents
  (CIMD) when the authorization server advertises `client_id_metadata_document_supported`,
  then Dynamic Client Registration (DCR), which this revision deprecates.
- Scopes taken from the challenge's `scope`, which is authoritative, or else from
  `scopes_supported`, with step-up on `403 insufficient_scope`.
- No token sent to a server other than one its own authorization server issued.
- Refresh tokens kept confidential in transit and storage.
- Redirect URIs that are loopback or HTTPS.

The [security best practices](https://modelcontextprotocol.io/docs/2026-07-28/tutorials/security/security_best_practices)
add SSRF guards for every URL learned during discovery. They also require the client to
validate authorization URLs and to accept tokens issued only for the intended resource.

What exists to build on:

- **Google sign-in** (`internal/connectors/google`) is an authorization-code flow with PKCE
  and `state`. `/settings` sends the consent link. The redirect URI is a plain HTTP loopback
  address that no server answers, so the user copies the failed address from the browser and
  pastes it into the chat.
  - A paste completes only the pending sign-in of the principal who pasted it, and only when
    its `state` matches.
  - The Google section's interceptor takes any loopback address that carries a code or an
    error. It uses up that principal's pending Google sign-in before checking it.
  - Pending sign-ins live in memory for ten minutes, one per principal.
  - The refresh token is stored through `integration.Credentials`, and the access token stays
    in memory.
  - A refresh token the token endpoint rejects is remembered until the user reconnects.
  - Disconnect revokes the grant as best effort.
- **The credential store holds plaintext.** Files are mode 0600 inside 0700 directories on
  the NAS volume, and nothing encrypts them at rest. Data backups and filesystem snapshots
  copy them as they are. The model subscription in `internal/connection` uses Pi's
  `auth.json`, kept the same way.
- **The web search guard** (`internal/connectors/web/guard.go`) resolves every host itself
  and dials only public addresses, redirect hops included. That defeats DNS rebinding and
  encoding tricks without parsing addresses by hand.
- **The application's HTTP server** is reachable as `assistant.home` through the home edge,
  only from the home network or its VPN, behind the household's private CA (plan 0076). A
  browser elsewhere cannot reach it, and an authorization server on the internet cannot reach
  it either.
  - The edge, in its own repository, forwards `/api/*` to the application.
  - The edge's access log records each request's full URI, query included.
- **The official Go SDK** (`modelcontextprotocol/go-sdk`, v1.8) has a client-side
  `auth.AuthorizationCodeHandler`. It supports CIMD, pre-registration and DCR, validates
  `iss`, and leaves the redirect to the caller. It drives the SDK's own transport, depends on
  `golang.org/x/oauth2`, and fetches through `http.DefaultClient` unless given another
  client. `go.mod` has neither the SDK nor `x/oauth2`, and 0136 chose a hand-written client.

## Goal

The operator can declare a catalog server as `oauth`. Each user connects it from `/settings`
by approving access in their browser, and the assistant then calls that server's allowlisted
read tools with that user's own token. The token is bound to the user, the server, the
server's resource URI, and its issuer. It is refreshed without the user's help, never sent
anywhere else, never logged, and removed and revoked when the user disconnects.

Success means the following:

- Two users connect the same OAuth server, and each sees only their own data.
- Connecting works from the home network, through the callback and a confirming tap in the
  chat, and from anywhere by pasting the address.
- A link forwarded to someone else cannot bind their account to the user who asked.
- A refresh survives a restart.
- A server that points discovery at a private address, at another issuer, or at an
  authorization server without PKCE is refused before any user is sent anywhere.

## Decisions

- **Three auth kinds, told apart by who mints the credential.**
  - `none` is unchanged.
  - `key` stays for a static secret the user obtained from the service themselves, such as
    an API key or a personal access token. It is sent in the header the entry names, is never
    refreshed, and works until the user replaces it.
  - The new `oauth` kind is for a token the user grants through the service's consent screen.
    It is short-lived, refreshable, bound to the server's resource URI and issuer, scoped, and
    always sent as `Authorization: Bearer`.
  - An `oauth` entry has no `key_header`, `key_prefix`, or `key_pattern`.
  - A service that accepts both kinds of credential is declared as one of them. The operator
    picks, and OAuth is preferred when the service offers it.
- **The operator declares an `oauth` entry fully.** Its `oauth` object holds the following
  fields:
  - `issuer` (required), the authorization server the operator expects.
  - `scopes` (required), the maximum scopes that may be requested. It may be empty for a
    server that defines none.
  - `client`, which is `metadata_document` (the default), `registered`, or `dynamic`.
  - For `registered` only, `client_id` and an optional `client_secret_file` read like
    Google's client secret file.
  - `redirect`, which is `callback` (the default) or `loopback`. It is the only per-entry
    redirect choice, and it is explicit.

  An `oauth` server must use HTTPS. Loopback is allowed for tests only, as for `key`.
- **Discovery follows the specification, and the operator's pin decides.** On Connect, the
  client sends an unauthenticated `initialize` and reads the 401's `WWW-Authenticate`, taking
  `resource_metadata` and `scope` from it. Without that header it tries the well-known URIs
  in the specification's order.
  - The PRM document's `resource` must equal the catalog URL in canonical form.
  - Its `authorization_servers` must contain the pinned `issuer`. Otherwise Connect fails as
    "this service's sign-in changed", and the operator sees why in a log line that carries no
    URL.
  - When the server publishes no PRM at all, the pinned issuer is used, which the
    specification permits as pre-configured values.
  - Authorization server metadata is fetched in the specification's RFC 8414-then-OIDC order.
    The client requires an identical `issuer`, HTTPS `authorization_endpoint` and
    `token_endpoint`, and `S256` in `code_challenge_methods_supported`.
  - Issuers, the PRM's `authorization_servers`, and the response's `iss` are compared as
    exact strings, with no case folding, default-port elision, trailing-slash or
    percent-encoding normalization.
  - The result is cached in memory per server for an hour and fetched again on every Connect.
- **Every discovery fetch goes through an SSRF guard.** The web search guard moves to a
  shared package with no change to its behaviour.
  - OAuth fetches use it with no redirects followed, HTTPS only, a ten-second deadline, and a
    64 KiB body cap.
  - Private, loopback and link-local destinations are refused, except the origin of the
    catalog URL and the origin of the pinned issuer. The operator chose those two, which keeps
    a LAN server such as Home Assistant possible.
  - A URL the server or its metadata names is accepted only on those terms. This covers
    `resource_metadata`, the PRM's servers, and every endpoint.
  - The authorization URL the user is sent to must be `https`, or loopback `http` in tests.
- **Client registration: CIMD by default, with pre-registration and DCR as explicit choices.**
  - A CIMD client ID is an HTTPS URL that the authorization server fetches. A LAN-only
    application cannot serve it, so the operator publishes one static JSON document at a
    public HTTPS address they control, named by `HOME_AGENT_MCP_OAUTH_CLIENT_ID`.
  - The document holds the public client: `token_endpoint_auth_method: none`,
    `grant_types: [authorization_code, refresh_token]`, the client name, and both redirect
    URIs, the callback and the loopback one. It holds no secret, and the repository ships an
    example of it. Because it is public, it publishes the edge's host name.
  - The document is not served by the application. It is fetched as a self-check on the
    first Connect after start: its `client_id` must equal its URL and its `redirect_uris`
    must contain both configured redirects. A mismatch fails Connect with an operator log
    line.
  - A `metadata_document` entry is used only when the authorization server's metadata has
    `client_id_metadata_document_supported: true`. Otherwise Connect fails with the
    `cimd_unsupported` class, and the operator moves the entry to `registered` or `dynamic`.
  - `registered` uses an OAuth client the operator created at the service, bound to the
    pinned issuer.
  - `dynamic` registers once per issuer and keeps the household-wide registration in the
    credential store. It is offered only for an authorization server without CIMD, because
    this revision deprecates DCR.
  - The client never chooses a registration path on its own beyond what the entry says.
- **Two global redirects, chosen per entry.**
  - `HOME_AGENT_MCP_OAUTH_REDIRECT_URL` is the callback,
    `https://<the app's edge name>/api/oauth/callback`. The authorization server never
    contacts a redirect URI; only the user's browser follows it, so a private name works.
  - On the home network or VPN, the browser reaches the callback through the edge's existing
    `/api/*` route. Elsewhere it fails to load the address, and the user copies it from the
    address bar into the chat, as with Google today.
  - `HOME_AGENT_MCP_OAUTH_LOOPBACK_REDIRECT_URL` is optional. It is a loopback address with a
    fixed port and path that no server answers, so it is paste-only. An entry uses it only
    with `redirect: loopback`, for an authorization server that refuses a private host name.
- **The callback leaves no code behind.**
  - The callback answers `303 See Other` to a clean confirmation address without a query.
    The code then does not stay in the address bar, in the browser's history, or in
    synchronized history.
  - The page at that address says to return to Telegram. It has
    `Cache-Control: no-store`, `Referrer-Policy: no-referrer`, a CSP of `default-src 'none'`,
    and nothing from the request.
  - A prerequisite in the separate edge repository: the access log must skip, or log without
    its query, the callback path. Otherwise every code, `state` and `iss` lands in the edge's
    log.
- **An authorization belongs to the principal who started it.** Connect on an `oauth` row in
  `/settings` sends the consent link as a URL button and says the link expires in ten
  minutes. The model has no tool to start, finish, or see an authorization.
  - Only one authorization is pending per principal, in memory. It holds the server, the PKCE
    verifier, `state`, the recorded issuer and its `authorization_response_iss_parameter_supported`,
    the requested scopes, and the redirect used. A restart drops it.
  - `state` is 32 random bytes and is compared in constant time.
  - The client validates `iss` by the specification's RFC 9207 table before the code is sent
    anywhere. A mismatched error response is neither acted on nor shown.
  - Taking a pending authorization is atomic, and it is used up before the code exchange
    starts, so a replayed callback or paste exchanges nothing.
  - **A paste** completes only the pasting principal's own pending authorization, and only
    when its `state` matches, as Google's does. A paste comes from that principal's chat, so
    it needs no further confirmation.
  - **A browser callback** is matched by `state` and exchanges the code at once, because codes
    are short-lived. The tokens are held in memory, unstored and unused. The starting
    principal's chat gets a message that names the server and the granted scopes, with
    Connect and Cancel buttons. Only Connect stores the grant and turns the server on.
    Cancel, or no tap within ten minutes, revokes the tokens and discards them. Whoever opened
    a forwarded link therefore cannot bind their account without the starting user's tap.
  - A denied consent replies "Access to <server title> was not granted. Use /settings to try
    again." to the starting principal.
  - Messages that carry a redirect are routed by an exact prefix match on the configured
    redirect, ahead of the Google section's interceptor. A loopback paste for an MCP server
    then never uses up a pending Google sign-in.
  - A pasted address is removed from the chat and never becomes a turn, also when its update
    is delivered again. A message shaped like either redirect is kept from the model even with
    no authorization pending.
- **Grants are stored per principal, server, resource and issuer.**
  - The refresh token, the access token and its expiry go in a credential record through
    `integration.Credentials`, in the same file conventions (0600 files inside 0700
    directories), under `mcp-<server>`.
  - A new table `mcp_oauth_grants` holds the non-secret part: the resource URI, the issuer,
    the client ID, the granted scopes, and the connected and refreshed times. Its migration
    number is assigned at implementation, because parallel plans hold the next ones.
  - The table is listed in `ExcludedFromExport`, because a grant is not portable: an export
    restores the switch, and the user connects again.
  - A grant whose resource or issuer differs from the catalog's is stale. Its token is not
    sent, and the row asks for a reconnect, as a key with a changed origin does today.
  - With no refresh token issued, the access token is used until it expires, and the row then
    asks for a reconnect.
  - The application does not encrypt the store at rest. Backups and filesystem snapshots
    keep a token after the user disconnects, so revocation at the authorization server is
    the only real removal. Encryption is a separate decision for every integration (see
    Risks).
- **Refresh rotates safely.** One refresh at a time runs for each grant, under a lock held
  in the process.
  - A refresh starts within a minute of expiry, or once after a 401.
  - The rotated refresh token is written before the new access token is used. A write that
    fails loses the grant, which is logged, and the row asks for a reconnect.
  - `invalid_grant` is remembered until the user reconnects.
  - An authorization server that refuses the connection leaves the grant intact and fails the
    call as unavailable.
  - A refresh that times out has an unknown outcome: the server may already have rotated the
    token. It is retried once with the same refresh token. A second failure asks for a
    reconnect instead of retrying more, so the server's reuse detection does not revoke the
    whole grant.
  - The MCP session is not ended by a refresh: the client takes the current access token on
    every request.
- **The operator's maximum caps the scopes.**
  - A challenge's `scope` is authoritative. When it is not a subset of the entry's `scopes`,
    Connect is refused with "this service asks for more access than the household allows".
  - Without a challenge scope, the request is the PRM's `scopes_supported` intersected with
    the entry's `scopes`.
  - `offline_access` is added when the authorization server lists it.
  - A token response that grants more than the maximum is revoked and refused.
  - Tools remain reads that the catalog allowlists. Scopes are a second fence, not the first.
- **Step-up is reported, not run in the middle of a turn.**
  - A `403 insufficient_scope` whose scopes are within the maximum marks the grant "needs
    more access". The call fails with a result the model can explain, and the row's Reconnect
    asks for the union of the old and new scopes.
  - A challenge beyond the maximum is a refusal.
  - A grant asks for more access at most twice in 24 hours. A third challenge in that window
    marks the grant refused until the user reconnects, so a server cannot loop the user.
- **Disconnect revokes as best effort.** It forgets the session and the list, removes the
  credential record and the grant row, and turns the switch off. It then revokes the refresh
  token, or else the access token, at `revocation_endpoint` (RFC 7009) when the metadata names
  one. A failed revocation is logged, and the grant is gone here either way.
- **No token reaches anything but its own resource.**
  - An OAuth token goes only to the catalog URL it was issued for. It goes to no sidecar,
    relay, or other server, and never into a tool result, a log, a trace, an export, or the
    model's context.
  - The client follows no redirects, so a token is not carried elsewhere.
  - A local sidecar that needs an upstream OAuth API is its own OAuth client. The application
    never passes it a user's token.
- **Failures are classified and observable without secrets.**
  - Logs carry the principal, the server, the step, and a class such as `discovery`,
    `issuer_mismatch`, `pkce_unsupported`, `cimd_unsupported`, `ssrf_refused`,
    `state_unmatched`, `iss_mismatch`, `invalid_grant`, `refresh_unknown`, `scope_exceeded`,
    `unconfirmed`, or `unavailable`.
  - Logs never carry a token, code, verifier, `state`, or a URL with its query.
  - Counters for connects, refreshes and step-ups by server and outcome follow the
    connector's existing ones.
- **A hand-written client, no new dependency.** The SDK's handler assumes the SDK's transport
  and a default HTTP client, and adopting it would reverse 0136's decision. The flow is a few
  hundred lines on top of what the Google client already does. The SDK's handler is the
  reference implementation for conformance.

## Approach

Ten chunks run in six waves, with the full test suite passing after each. Chunks in the same
wave are independent:

| Wave | Chunks |
| --- | --- |
| 1 | 1, 2, 3 |
| 2 | 4 |
| 3 | 5, 6 |
| 4 | 7 |
| 5 | 8, 9 |
| 6 | 10 |

1. **Shared outbound guard** (Complex; no dependencies). Move the address checks and the
   guarded dialer from `internal/connectors/web/guard.go` into a shared package. Add an option
   that allows named operator origins, and a mode with no redirects. Web search behaves as
   before.
   - Files: new `internal/netguard/`, `internal/connectors/web/guard.go`, tests.
   - Acceptance: the web guard's existing tests pass unchanged. New tests refuse private,
     loopback, link-local, IPv4-mapped and NAT64-embedded addresses, accept an allowed
     origin, and refuse a redirect.
2. **Catalog and settings** (Simple; no dependencies). Add `auth: "oauth"`, the `oauth`
   object and its validation, plus `HOME_AGENT_MCP_OAUTH_CLIENT_ID`,
   `HOME_AGENT_MCP_OAUTH_REDIRECT_URL` and `HOME_AGENT_MCP_OAUTH_LOOPBACK_REDIRECT_URL`.
   - The client ID and the callback are required when any entry is `oauth`.
   - The loopback redirect is required when any entry says `redirect: loopback`.
   - Neither redirect carries a query or a fragment.
   - Files: `internal/config/mcp.go`, `internal/config/mcp_test.go`,
     `deploy/mcp-servers.example.json`, `.env.example`, `deploy/nas.env.example`, a CIMD
     document example under `deploy/`.
   - Acceptance: an invalid entry stops startup with a message that echoes no URL. A
     `registered` entry without `client_id` is refused, and so are key fields on `oauth`.
3. **Grant storage** (Simple; no dependencies). The `mcp_oauth_grants` migration, numbered
   when implemented, and its repository methods. The table is listed in
   `ExcludedFromExport`. Add a typed credential record to `integration.Credentials` that
   keeps the existing file conventions and leaves the plain token record readable as before.
   - Files: `internal/store/sqlite/migrations/`, `internal/store/sqlite/` (repository and
     tests), `internal/wiring/export.go`, `internal/integration/credentials.go`, tests.
   - Acceptance:
     - A grant round-trips, and a principal's grant is invisible to another.
     - An operator-placed key file still reads.
     - A written grant file has mode 0600 and its directories 0700.
     - `TestEveryPrincipalTableIsExportedOrExcluded` passes.
4. **OAuth client and simulators** (Complex; needs 1). The client, in a new package, has
   these pieces, each with a typed error class:
   - the `WWW-Authenticate` parser;
   - PRM discovery and validation;
   - authorization server metadata discovery in order, with the exact issuer, PKCE and CIMD
     support checks;
   - the authorization URL with PKCE `S256`, `state`, `resource` and scope;
   - response validation of `state` and of `iss` by RFC 9207;
   - code exchange with `resource`;
   - revocation;
   - the CIMD self-check and the `registered` client.

   Two simulators are built with it:
   - A loopback authorization server: metadata for each discovery variant, an authorize
     endpoint that redirects with `code`, `state` and `iss`, and a token endpoint that checks
     the PKCE verifier and `resource`. It rotates refresh tokens, can issue none, can answer
     `invalid_grant`, and can hang. It also serves revocation, registration and the CIMD
     fetch.
   - An OAuth mode for `internal/testutil/mcpsim`: 401 with `resource_metadata` and `scope`,
     a PRM document, audience checks, `403 insufficient_scope`, and hostile variants whose
     metadata points at another loopback port, another issuer, or a server without PKCE.

   Files: new `internal/connectors/mcp/oauth/`, new `internal/testutil/oauthsim/`,
   `internal/testutil/mcpsim/`, tests. Acceptance: table tests for every discovery order and
   validation rule in Context, against the simulators only, with no network.
5. **DCR** (Simple; needs 4). The `dynamic` client: register once per issuer with
   `application_type` derived from the redirect. Persist the household registration bound to
   the issuer, and register again when the issuer changes.
   - Files: `internal/connectors/mcp/oauth/`, tests.
   - Acceptance: a second user reuses the registration, and a changed issuer registers anew.
   - Skipped if the operator declines DCR (see open questions).
6. **Token source and refresh** (Complex; needs 3, 4). Built in the `oauth` package:
   - a token source per grant over the credential record;
   - the refresh lock;
   - rotation written before use;
   - `invalid_grant` remembered;
   - the timeout rule of one retry, then a reconnect;
   - the scope cap on token responses.
   - Files: `internal/connectors/mcp/oauth/`, tests.
   - Acceptance: against the simulators:
     - Concurrent callers cause one refresh.
     - A rotation survives a restart.
     - A hanging token endpoint leads to one retry, then a reconnect.
     - An over-broad grant is revoked.
7. **Connector integration** (Complex; needs 2, 5, 6). Wire the following into the connector:
   - `BeginAuthorization`, `CompletePaste`, `CompleteCallback` and `ConfirmCallback`, over
     the atomic pending store, and the held, unconfirmed tokens with their expiry;
   - an `Authorize` hook in the client's `Options` that is read on every request;
   - 401 handling: refresh once, retry once, then ask for a reconnect;
   - 403 step-up as "needs more access", with its bound;
   - stale grants;
   - Disconnect with revocation;
   - status states for `/settings`;
   - list and session keys by grant rather than by token;
   - a notifier interface through which the callback's confirmation reaches the chat.
   - Files: `internal/connectors/mcp/service.go`, `client.go`, `transport.go`, `errors.go`,
     tests.
   - Acceptance: against the simulators:
     - Two principals' tokens never cross.
     - Hostile discovery is refused before any link is built.
     - A paste with another principal's `state` completes nothing.
     - No captured log line holds a secret.
8. **Callback endpoint** (Complex; needs 7). `GET /api/oauth/callback` and the clean
   confirmation address are registered only when the callback redirect is configured.
   - The callback answers 303 to the clean address in every case, and the page there is the
     same whatever happened.
   - The endpoint calls only the connector. The chat is reached through chunk 7's notifier.
   - Files: `internal/app/http.go`, a handler in `internal/connectors/mcp/`, tests.
   - Acceptance:
     - A replayed callback completes nothing.
     - The response carries no request data, has the headers above, and is a 303 to an
       address without a query.
9. **Telegram** (Complex; needs 7). All of the work in `internal/telegram`:
   - On `oauth` rows in the Services section: Connect with the link button, Reconnect for
     stale and needs-more-access grants, and Disconnect.
   - The callback's confirmation message, with its Connect and Cancel buttons, implementing
     the notifier.
   - Routing of redirect messages by exact prefix, ahead of the Google section's interceptor,
     with removal of the message and a guard against the model.
   - Replies, including the denied-consent text; `/privacy` lines; the `services` help topic.
   - Files: `internal/telegram/services.go`, the section ordering in `internal/telegram/`,
     tests.
   - Acceptance:
     - A pasted address completes and is deleted.
     - A loopback MCP paste leaves a pending Google sign-in usable.
     - A non-matching address reaches no server and no model.
     - Another principal's tap on the confirmation does nothing.
10. **Wiring, documentation and end to end** (Complex; needs 8, 9). Do the following:
    - Build the guard and the OAuth client in wiring.
    - End-to-end tests through the Telegram simulator:
      - connect by callback and a confirming tap;
      - connect by paste;
      - a forwarded link left unconfirmed, then revoked;
      - a tool call with the token;
      - refresh across a restart;
      - step-up;
      - disconnect with revocation.
    - Update `docs/architecture.md` and `docs/operations.md`: how to publish the CIMD
      document, the edge's access-log prerequisite, how to pin an issuer, how to choose
      scopes, and how to remove grants of a removed server.
    - Mirror the new settings into the local env files.
    - Files: `internal/wiring/`, `internal/telegram/` integration tests, `docs/`, the env
      files.
    - Acceptance: the Validation list below.

**Before the first deploy, in the edge's repository:** the access log skips the callback path
or strips its query. The operator then publishes the CIMD document.

**Live check, by the operator after deployment.** Connect one hosted OAuth server with reads
only, and expect these outcomes:

- From the home network, the browser ends on the clean confirmation page with no query in
  the address bar. The chat asks to confirm, naming the server and its scopes. Connect turns
  the row on.
- From outside the home network, by paste, the pasted message disappears and the row turns
  on.
- A read tool answers in a chat turn.
- The edge's access log shows the callback path without its query.
- Disconnect turns the row off, and the service's own list of connected applications no
  longer shows the assistant.

## Files affected

- `internal/netguard/` (new), `internal/connectors/web/guard.go`
- `internal/config/mcp.go`, tests, `deploy/mcp-servers.example.json`, `.env.example`,
  `deploy/nas.env.example`, a CIMD document example under `deploy/`
- A new migration in `internal/store/sqlite/migrations/`, `internal/store/sqlite/`
- `internal/integration/credentials.go`, `internal/wiring/export.go`
- `internal/connectors/mcp/` (service, client, transport, errors, callback handler) and new
  `internal/connectors/mcp/oauth/`
- `internal/testutil/oauthsim/` (new), `internal/testutil/mcpsim/`
- `internal/app/http.go`, `internal/telegram/`, `internal/wiring/`
- `docs/architecture.md`, `docs/operations.md`, `docs/plans/README.md`
- Outside this repository: the edge's access-log rule for the callback path

## Validation

- Formatting, module tidiness, vet, build, the full suite with and without the race detector,
  and `scripts/check-plans`.
- Discovery:
  - `resource_metadata` from the header, path-inserted then root well-known URIs, and no PRM
    with a pinned issuer.
  - PRM `resource` mismatch and a missing pinned issuer are refused.
  - The metadata order holds both with and without an issuer path.
  - An `issuer` that differs only by a trailing slash or case is refused.
  - A missing `code_challenge_methods_supported` is refused.
  - A `metadata_document` entry against a server without CIMD support fails as
    `cimd_unsupported`.
  - A `javascript:` or plain-HTTP authorization endpoint is refused.
- SSRF: a `resource_metadata` URL, an authorization server, or a token endpoint at another
  loopback port or at a private address is refused before any request reaches it. A
  redirecting metadata endpoint is refused.
- Flow:
  - The token request carries the PKCE verifier and `resource`.
  - These each complete nothing: an `iss` mismatch, an absent `iss` where it is advertised,
    an unknown or reused `state`, and another principal's `state` in a paste.
  - A callback's tokens are stored only after the starting principal's tap. They are revoked
    after Cancel or after ten minutes without a tap.
  - A denied consent sends the defined reply.
  - The callback answers 303 to an address without a query.
- Tokens:
  - Every MCP request carries `Authorization: Bearer`, and two users' tokens never cross.
  - A rotated refresh token is persisted before use. `invalid_grant` asks for a reconnect.
  - A token request that times out is retried once, then asks for a reconnect.
  - A 401 refreshes once. A grant made stale by a changed URL or issuer is not sent.
  - The grant file is 0600 inside 0700 directories.
  - No token, code, verifier or `state` appears in any captured log line or trace attribute.
- Scopes:
  - A challenge not within the maximum is refused.
  - Without a challenge, the request is capped by the maximum.
  - An over-broad grant is revoked.
  - `insufficient_scope` within the maximum shows "needs more access", and Reconnect asks for
    the union.
  - A third step-up in 24 hours marks the grant refused.
- Disconnect removes the record and the row, turns the switch off, and calls revocation.
- Telegram:
  - Connect by callback with a tap, and by paste.
  - The pasted message is deleted and never becomes a turn.
  - A loopback MCP paste does not disturb a pending Google sign-in.
  - A full catalog with `oauth` rows renders, and the export restores the switch but no grant.
- Live, by the operator: the outcomes listed under the live check in Approach.

## Risks and open questions

- **The CIMD document must be public.** It names the assistant and both redirect URIs, which
  publishes the edge's host name, and it holds no secret. Publishing it is the operator's act
  and the operator's hosting choice. Question for the operator: is publishing the edge's host
  name acceptable? Recommendation: yes, on a static page the operator controls, outside this
  repository; the name resolves only inside the home network. Without the document,
  `registered` or `dynamic` are the only paths.
- **DCR is deprecated.** Recommendation: build it (chunk 5) as an explicit per-entry choice
  for authorization servers without CIMD, or drop the chunk if every server the operator
  plans uses CIMD or a registered client.
- **A private redirect host.** Some authorization servers may refuse a redirect to a name
  that does not resolve publicly. The per-entry `redirect: loopback`, which is paste-only, is
  the fallback. Recommendation: start with the callback and change only an entry that fails.
- **Callback path.** `/api/oauth/callback` needs no new edge route, because the edge already
  forwards `/api/*`. It still needs the access-log rule above. Recommendation: keep it under
  `/api`.
- **Binding a browser to a principal.** A paste is bound to the principal who pasted it. A
  callback is bound by the confirming tap in the starting principal's chat, which shows the
  server and the granted scopes. That costs one tap on the home network and none by paste.
  MCP has no standard identity to show, unlike Google, so the tap shows scopes rather than an
  account, and one remote account may be connected by two principals.
- **No encryption at rest.** Refresh tokens are long-lived and sit beside the other
  integration tokens as plaintext files, which backups and snapshots keep after a
  disconnect. The specification requires refresh tokens to be kept confidential, and this
  plan meets that only with file modes, private deployment, and revocation. Question for the
  operator: should a follow-up plan encrypt the whole credential store with a key from a
  Docker secret? Recommendation: yes, as its own plan covering every integration, before
  more OAuth servers are added.
- **Specification drift.** The 2026-07-28 revision added `iss` validation and deprecated DCR,
  and the CIMD draft is not yet an RFC. The simulators pin the behaviour this plan relies on.
- **Home Assistant (plan 0152).** Home Assistant's IndieAuth client IDs are URLs whose
  redirect must share their host. A `registered` entry with the app's own HTTPS address as
  `client_id` and Home Assistant's issuer pinned fits this design, but whether that or a
  long-lived token is better is for 0152 to decide.
- **Writes.** This plan keeps reads only. Writes over OAuth, which need scopes such as
  `write` and confirmed tools, are a later plan.
