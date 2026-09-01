# Go → Pi Subscription Check

This spike verifies that a Go process can drive Pi through JSONL RPC while Pi
uses its `openai-codex` ChatGPT subscription provider.

It intentionally disables tools, sessions, extensions, skills, templates,
themes, and context-file discovery. The program never reads or prints Pi’s
credential file.

## Prerequisites

- Go
- Pi `0.84.1` or a deliberately reviewed later version
- An authenticated Pi `openai-codex` profile

Check readiness without printing credentials:

```sh
pi auth check --provider openai-codex --json
```

If credentials are not configured, start Pi interactively and run `/login`, then
select ChatGPT Plus/Pro (Codex). Complete the supported browser flow.

## Run

```sh
go run .
```

Optional explicit model:

```sh
go run . -model '<model-id>'
```

Successful output has this shape:

```text
pi_go_check=passed
provider=openai-codex
prompt_acknowledged=true
agent_settled=true
tool_events=0
elapsed=...
response=PI_GO_OK
```

## Security properties checked

- `openai-codex` provider rather than `openai` API-key provider
- no Pi tool events
- no session persistence
- no project resource discovery
- bounded execution time
- deterministic subprocess cleanup
- no credentials or raw Pi stderr in output

The spike passed twice on 2026-08-30 with Pi `0.84.1`, Go `1.27.0`, and the
`openai-codex/gpt-5.5` model. Both runs completed in about 2.8 seconds and
reported zero tool events. It does not prove long-term token refresh, NAS
deployment, multi-user suitability, or permission to share one subscription
between principals.
