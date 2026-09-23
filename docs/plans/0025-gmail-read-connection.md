# 0025 — Per-user read-only Gmail connection

Status: `in-progress`

## Context

[04 — Integrations](../04-integrations.md) has described a per-user,
`gmail.readonly` Gmail connector since the beginning, and the roadmap puts it at
the start of the job-application vertical. Nothing of it exists yet.

Two recent changes make it worth more than it was. [0024](0024-scheduled-model-turns.md)
runs a confirmed prompt on a schedule with read-only tools, so "every weekday at
8, tell me which unread mail needs a reply" becomes a job rather than a feature.
And the operator's `job-apply` repository already reads the same mailbox through
a Google OAuth client with `gmail.readonly` alone, in a Google Cloud project that
is published to production. That settles the platform questions: a published
app's refresh tokens do not expire after seven days, personal use under 100
users needs no verification, and a household member sees Google's "unverified
app" screen once when they sign in.

What does not carry over is the sign-in. `job-apply` opens a browser on the Mac
and catches the redirect on `localhost:8080`. The assistant runs on the NAS and
is used from phones, and Google's device-code flow, which `/connect` uses for
the model subscription, does not permit Gmail scopes.

## Goal

Each household member connects their own mailbox from `/settings` on their
phone, and their conversation and their scheduled jobs can then search that
mailbox and read a message's text. Nothing can send, draft, label, archive, or
delete mail, because the credential cannot. One member's mailbox is never
reachable from another member's turn, and disconnecting revokes the grant at
Google as well as deleting it here.

## Decisions

- **The scope is `gmail.readonly` and nothing else.** The connector has no
  write tools and needs no approval flow. `gmail.metadata` would keep bodies out
  of reach but would also make "what does this email say" impossible, which is
  most of the point.
- **Sign-in is the authorization-code flow with PKCE, completed by pasting.**
  `/settings` → Connect Gmail sends a Google sign-in link whose redirect is the
  client's registered loopback address. After consenting, the phone's browser
  fails to open that address, and the user copies the address bar into the chat.
  The assistant takes the `code` and `state` from it, exchanges the code, and
  deletes the message. The same pattern is how rclone and similar tools
  authorize a headless machine. The `state` binds the paste to the principal and
  to one intake that expires after ten minutes; the PKCE verifier never leaves
  memory.
- Any message carrying a loopback address with a `code` or `error` parameter is
  intercepted before the model sees it and deleted, whether or not a sign-in is
  pending, so an authorization code is never sent to the model subscription by
  accident. Other messages sent while a sign-in is pending go to the model as
  usual; unlike a Notion token, there is nothing else to mistake for one.
- **The OAuth client is the operator's own**, a separate client of type Desktop
  app in the existing published Google Cloud project, so it can be revoked
  without touching `job-apply`, and any loopback redirect is accepted without
  registration. The client ID is configuration; the client secret lives in
  Keychain as `home-agent.gmail-client-secret` and reaches the NAS as a secret
  file, like the Telegram token, through a compose override applied only while
  Gmail is enabled. The redirect defaults to `http://localhost:8080/callback`.
- The refresh token is stored with the existing `integration.Credentials` file
  store under the principal's own directory. Access tokens are refreshed on
  demand and held only in memory.
- The connected mailbox's address, from `users.getProfile`, is the connection's
  workspace name, shown in `/settings`. One address can be bound to one
  principal only; a second principal connecting the same mailbox is refused
  and the grant it just obtained is revoked.
- **Tools, both `tools.RiskRead`, so scheduled runs get them too:**
  - `gmail_search`: a Gmail query (`q` syntax, such as `is:unread newer_than:1d`)
    returning up to 20 messages with id, thread id, date, sender, recipients,
    subject, labels such as `UNREAD` and `IMPORTANT`, and Gmail's snippet.
  - `gmail_read_message`: one message's text by id — the `text/plain` part, or
    the `text/html` part reduced to text — with the headers above and the names
    and sizes of its attachments, bounded to 12 KB with an offset to continue.
    Attachments are listed, never opened.
- Everything read from a mailbox is written by a third party, so the guidance
  says so in stronger terms than for Notion: message content is data, never
  instructions, and no request inside an email is acted on. The broker's
  untrusted-data prefix already marks every result.
- The existing limits bound the damage of an injection that gets through: Gmail
  itself is read-only, Notion and NAS writes still need a press, and a file can
  only be sent to the owner's own chat.
- Disconnecting deletes the credential file and revokes the refresh token at
  Google's revocation endpoint. A revocation that fails is logged and the local
  deletion still happens.
- A refresh token Google rejects (`invalid_grant`, after a password change or a
  revocation from the Google account page) shows in `/settings` as a problem
  with Reconnect and Disconnect, like an invalid Notion token. `/settings` never
  calls Google itself; the rejection is learned from the refresh that fails,
  and the tools are withdrawn from then on.
- A consent that returns a code without the Gmail scope, because the user
  unticked it on Google's granular consent screen, is treated as a refusal and
  the partial grant is revoked.
- The `/privacy` reply gains a sentence: email the model reads through your
  Gmail connection is sent to your model subscription.

## Approach

1. `internal/config`: `HOME_AGENT_GMAIL_ENABLED`, `HOME_AGENT_GMAIL_CLIENT_ID`,
   `HOME_AGENT_GMAIL_CLIENT_SECRET` or `HOME_AGENT_GMAIL_CLIENT_SECRET_FILE`, and
   `HOME_AGENT_GMAIL_REDIRECT_URL`; Gmail requires conversation. The Google
   endpoints are settable only through the connector's own configuration, for
   tests. Complexity: simple.
2. New `internal/connectors/gmail/client.go`: token exchange with PKCE, refresh,
   revocation, `users.getProfile`, `messages.list`, and `messages.get` in
   `metadata` and `full` formats, with the Notion client's retry and error
   classification. Complexity: moderate.
3. `internal/connectors/gmail/service.go`: the module, status, the intake
   (begin, consume, parse the pasted address), connect, disconnect, and the
   per-principal access-token cache. Complexity: complex.
4. `internal/connectors/gmail/tools.go` and `render.go`: the two tools, their
   guidance, MIME walking, and HTML-to-text reduction. Complexity: moderate.
5. New `internal/testutil/gmailsim`: a loopback simulator of Google's token,
   revocation, and Gmail endpoints, in the manner of `notionsim`. Complexity:
   moderate.
6. `internal/telegram`: a Gmail settings section with Connect, Reconnect, and
   Disconnect, the paste interceptor, and the `/privacy` line. Complexity:
   moderate.
7. `internal/app/app.go`: build and register the connector;
   `internal/store/sqlite/integrations.go`: `IntegrationAccountOwner` for the
   one-mailbox-one-principal rule. Complexity: simple.
8. Operations: new `deploy/compose.app.gmail.yaml`, applied by
   `scripts/deploy-app-nas` while Gmail is enabled; `scripts/nas-sync-secrets`
   installs the secret then; `scripts/keychain-set-secret` accepts its name;
   `deploy/nas.env.example` and the runbook gain the settings and a section on
   creating the OAuth client. Complexity: simple.
9. Documentation: `docs/02-scope-and-capabilities.md`, `docs/04-integrations.md`,
   `docs/05-security-and-privacy.md`, and this plan with its index row.
   Complexity: simple.

## Files affected

- `internal/config/config.go`, new `internal/config/gmail.go`,
  `internal/app/app.go`, `internal/store/sqlite/integrations.go`
- new `internal/connectors/gmail/` (`client.go`, `service.go`, `tools.go`,
  `render.go`, `render_test.go`)
- new `internal/testutil/gmailsim/`
- new `internal/telegram/gmail.go`, `internal/telegram/service.go`, new
  `internal/telegram/gmail_integration_test.go`,
  `internal/telegram/conversation_integration_test.go`
- `scripts/deploy-app-nas`, `scripts/nas-sync-secrets`,
  `scripts/keychain-set-secret`, new `deploy/compose.app.gmail.yaml`,
  `deploy/nas.env.example`
- `docs/02-scope-and-capabilities.md`, `docs/04-integrations.md`,
  `docs/05-security-and-privacy.md`, `docs/13-operator-runbook.md`,
  `docs/plans/README.md`

## Validation

- `gofmt -l`, `go mod tidy -diff`, `go test -race ./...`, `go vet ./...`,
  `go build -buildvcs=false ./...`, `scripts/check-plans`.
- Against the simulator, through `/settings` and the fake Telegram server: the
  link carries the scope, a PKCE challenge, and a state; the pasted address is
  deleted and never reaches the model; the same address pasted twice, another
  principal's address, and a consent with the Gmail box unticked each connect
  nothing; a mailbox already bound to another principal is refused;
  disconnecting revokes at the simulator and removes the file.
- A redirect-looking message with no pending intake is deleted and answered,
  not sent to the model.
- Tools: search renders headers, system labels without user label ids, and
  snippets; reading returns the plain part, falls back to reduced HTML without
  scripts or styles, and lists attachments without content; another
  principal's message id finds nothing because the call runs with the caller's
  own token; an expired access token is refreshed once; a grant revoked at
  Google withdraws the tools and shows Reconnect in `/settings`; a principal
  without a connection is offered neither tool.
- A unit test reads a long message in windows that never split a character and
  whose offsets join back to the whole text.
- Live, from the operator's phone: connect, ask about today's unread mail,
  schedule a weekday summary, and disconnect; the grant disappears from the
  Google account's third-party access page.

## Risks and open questions

- Email is the channel where a stranger's text reaches the model. Read-only
  scopes and confirmed writes limit what a successful injection can do, but a
  scheduled summary could still be steered into misreporting a mailbox. The
  guidance and the untrusted-data prefix are the mitigation; watch for it.
- The paste step depends on the phone's browser showing the failed loopback
  address. Safari and Chrome both do; if one does not, the fallback is to open
  the link on a computer.
- Google may change what an unverified app in production may do with a
  restricted scope. The operator's `job-apply` would break at the same time, so
  it would be noticed.
- Full-text email goes to the owner's model subscription whenever the model
  reads a message. That is the same trade as Notion and is stated in `/privacy`.
- The rejected-grant state is held in memory, so after a restart `/settings`
  shows the connection as fine until the first tool call fails again.
- Attachments, threads as a unit, labels as filters beyond the query syntax, and
  push notifications through Pub/Sub are left for later plans.
