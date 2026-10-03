# 0036 — Several Google accounts per user, with Mail and Calendar switches

Status: `done`

## Context

[0025](0025-gmail-read-connection.md) gives each principal one read-only Gmail connection, stored
as one `integration_connections` row and one `gmail.json` credential file keyed by principal.
Household members have more than one mailbox, and a calendar connector
([0037](0037-google-calendar.md)) needs the same Google sign-in. Asking for a second sign-in per
product, or a second OAuth client, would make every member paste a redirect twice per account.

0025 also assumed that `job-apply`'s Google Cloud project was published. It was in Testing, so the
operator created a separate, published project for this application on 2026-10-02.

Nothing beyond the operator's own test connection exists yet, so the stored shape can change.

## Goal

A principal connects any number of their own Google accounts from `/settings`. Each account has a
Mail switch and a Calendar switch. Gmail tools search every account with Mail on and say which
account each result came from, and the Calendar connector reads every account with Calendar on.
Disconnecting one account revokes only that account's grant. One account is never reachable from
another principal's turn.

## Decisions

- **One Google account is one grant.** A sign-in asks for `openid email gmail.readonly
  calendar.readonly` with `include_granted_scopes=true`, so Mail and Calendar share one paste.
  Google's granular consent lets the user untick either product. A product whose scope was not
  granted cannot be switched on; its switch says so and reconnecting is the way to grant it.
  A sign-in that grants neither product is a refusal, and the partial grant is revoked.
- Google keeps one grant per account and OAuth client, so revoking any of its tokens ends it for
  every holder. A sign-in for an account that another principal already holds is refused without
  revoking anything, unlike 0025, which would have disconnected the owner. A grant is revoked at
  sign-in only when nobody holds the account.
- **The account's identity is Google's `sub`**, read from the OpenID userinfo endpoint, with the
  email address as its display name. `users.getProfile` is no longer needed, so a Calendar-only
  account still has a name.
- **Switches are local.** Turning a product off withdraws its tools for that account without
  touching the grant; both start on for the granted scopes. Neither is a write permission.
- **Storage:** a new `google_accounts` table keyed by `(principal_id, account_id)` with a unique
  `account_id`, so one Google account belongs to one principal. It holds the address, the granted
  products, and the two switches. The refresh token stays in the existing
  `integration.Credentials` store as `google-<sub>.json` under the principal's directory.
- Reconnecting an account already connected for the same principal replaces its refresh token and
  granted products, keeps its switches, and does not revoke the old token, because revoking any
  token of a grant revokes the whole grant at Google.
- **A new `internal/connectors/google` package** owns the OAuth client, the sign-in intake, the
  accounts, and access tokens per account. It is the module registered under `google`. Gmail and
  Calendar become tool providers that ask it for `Accounts(principal, product)` and
  `AccessToken(principal, account, product, force)`; neither holds a credential.
- **Gmail tools:** `gmail_search` searches every Mail account, up to the limit in total, and
  labels every message with its account address; an optional `account` narrows it to one address.
  `gmail_read_message` takes the `account` from the search result as well as the id.
- **Configuration is renamed for what it is:** `HOME_AGENT_GOOGLE_ENABLED`,
  `HOME_AGENT_GOOGLE_CLIENT_ID`, `HOME_AGENT_GOOGLE_CLIENT_SECRET(_FILE)`,
  `HOME_AGENT_GOOGLE_REDIRECT_URL`, the Keychain item `home-agent.google-client-secret`, and
  `deploy/compose.app.google.yaml`. The old Gmail names were never deployed.
- **The old single connection is dropped.** The migration deletes `gmail` rows from
  `integration_connections`. At startup the Google module removes any leftover `gmail.json`
  credential file and revokes its token, so the operator's test grant does not linger at Google.
- `/settings` shows one line per account with its products, and Connect a Google account. In the
  keyboard each account has a header button with its address, which does nothing when pressed,
  above a row with its Mail, Calendar, and Disconnect buttons. Callback data carries the `sub`,
  which keeps it inside Telegram's 64 bytes; an unknown or another principal's `sub` only
  refreshes the view.

## Approach

1. Migration `010_google_accounts.sql` and `internal/store/sqlite/google.go`: save, list, load,
   delete, set switches, find the owner of an account. Complexity: moderate.
2. `internal/connectors/google`: the client (authorization link, exchange, refresh, revoke,
   userinfo), the service (intake, accounts, switches, access tokens, rejected grants, the
   leftover-file cleanup), and the account API that Gmail and Calendar consume. The sign-in and
   OAuth parts move from `internal/connectors/gmail`. Complexity: complex.
3. `internal/connectors/gmail`: keep the Gmail API calls, tools, and rendering; take accounts and
   tokens from the Google module; add the `account` argument and per-message account labels.
   Complexity: moderate.
4. `internal/testutil/googlesim` replaces `gmailsim`: token, revocation, userinfo, and Gmail
   endpoints for several accounts, with granular consent. Calendar endpoints are 0037's.
   Complexity: moderate.
5. `internal/telegram/google.go` replaces `gmail.go`: the accounts section, its per-account rows,
   and the paste interceptor. `settingsView` gains extra keyboard rows. Complexity: moderate.
6. `internal/config/google.go`, `internal/app/app.go`, scripts, compose override, and
   `deploy/nas.env.example` for the rename. Complexity: simple.
7. `docs/architecture.md`, `docs/operations.md`, and this plan with its index row.
   Complexity: simple.

## Files affected

- new `internal/store/sqlite/migrations/010_google_accounts.sql`, new
  `internal/store/sqlite/google.go`
- new `internal/connectors/google/`; `internal/connectors/gmail/`
- `internal/testutil/gmailsim/` → `internal/testutil/googlesim/`
- `internal/telegram/gmail.go` → `internal/telegram/google.go`, `internal/telegram/settings.go`,
  `internal/telegram/service.go`, `internal/telegram/gmail_integration_test.go` →
  `google_integration_test.go`, `internal/telegram/conversation_integration_test.go`
- `internal/config/gmail.go` → `internal/config/google.go`, `internal/config/config.go`,
  `internal/app/app.go`
- `scripts/deploy-app-nas`, `scripts/nas-sync-secrets`, `scripts/keychain-set-secret`,
  `deploy/compose.app.gmail.yaml` → `deploy/compose.app.google.yaml`, `deploy/nas.env.example`
- `docs/architecture.md`, `docs/operations.md`, `docs/plans/README.md`

## Validation

- `gofmt -l`, `go mod tidy -diff`, `go test -race ./...`, `go vet ./...`,
  `go build -buildvcs=false ./...`, `scripts/check-plans`.
- Against the simulator, through `/settings` and the fake Telegram server: two accounts connect for
  one principal and both are listed; a search returns messages from both, labelled; `account`
  narrows it; reading needs the right account; switching Mail off for one account removes its
  results and, with no Mail account left, the tools; a Calendar-only consent connects with Mail
  unavailable; a consent granting neither connects nothing and is revoked; an account connected to
  another principal is refused and revoked; reconnecting keeps the switches and revokes nothing;
  disconnecting one account revokes only its grant; a grant revoked at Google shows Reconnect for
  that account only.
- A leftover `gmail.json` is removed and revoked at startup.
- Live, from the operator's phone: connect two accounts, search across both, switch one off,
  disconnect one; the grant disappears from that Google account's third-party access page.

## Risks and open questions

- Asking for Calendar at every sign-in widens the consent screen for a member who only wants
  mail; they can untick it, and the switch shows it as not granted.
- A search across several accounts makes one Gmail request per message per account; the overall
  limit bounds it.
- The rejected-grant state is still held in memory, as in 0025.
