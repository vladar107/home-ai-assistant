# Implementation Plans

Plans are sequential implementation records. Create the next plan before a
non-trivial change and commit its completed form with the corresponding change.

A plan may evolve while its status is `proposed` or `in-progress`. Once its
status becomes `done`, `superseded`, or `abandoned`, both the plan file and its
existing index row are immutable. Later discoveries belong in a new numbered
plan that links back to the earlier record.

| Number | Plan | Status |
| --- | --- | --- |
| 0001 | [Telegram, observability, and NAS foundation](0001-telegram-observability-nas-foundation.md) | done |
| 0002 | [Private repository and plan governance](0002-private-repository-and-plan-governance.md) | done |
| 0003 | [Local model conversation](0003-local-model-conversation.md) | done |
| 0004 | [Per-user model subscriptions](0004-per-user-model-subscriptions.md) | done |
| 0005 | [Bot-registered commands and actionable provider failures](0005-bot-commands-and-provider-failures.md) | done |
| 0006 | [Quoted replies](0006-quoted-replies.md) | done |
| 0007 | [Settings command with inline controls](0007-settings-command.md) | done |
| 0008 | [NAS deployment of monitoring and the application](0008-nas-deployment.md) | done |
| 0009 | [Model tool calls through a core broker and Pi bridge](0009-model-tool-calls-through-pi.md) | done |
| 0010 | [Per-user Notion workspace connection](0010-notion-workspace-connection.md) | proposed |
| 0011 | [Confirmed Notion writes through core approvals](0011-confirmed-notion-writes.md) | proposed |
| 0012 | [Pull the application image from GHCR by digest](0012-ghcr-image-deployment.md) | done |
| 0013 | [Runtime metrics and Grafana dashboards](0013-runtime-metrics-and-dashboards.md) | done |
| 0014 | [Generic Go runtime dashboard and duration histogram buckets](0014-go-runtime-dashboard.md) | done |
| 0015 | [Resumable Pi sessions across turns](0015-resumable-pi-sessions.md) | done |

Start new plans from [the template](TEMPLATE.md). Run `scripts/check-plans`
before every commit.
