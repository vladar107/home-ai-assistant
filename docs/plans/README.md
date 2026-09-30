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
| 0010 | [Per-user Notion workspace connection](0010-notion-workspace-connection.md) | done |
| 0011 | [Confirmed Notion writes through core approvals](0011-confirmed-notion-writes.md) | done |
| 0012 | [Pull the application image from GHCR by digest](0012-ghcr-image-deployment.md) | done |
| 0013 | [Runtime metrics and Grafana dashboards](0013-runtime-metrics-and-dashboards.md) | done |
| 0014 | [Generic Go runtime dashboard and duration histogram buckets](0014-go-runtime-dashboard.md) | done |
| 0015 | [Resumable Pi sessions across turns](0015-resumable-pi-sessions.md) | done |
| 0016 | [Reach data sources nested inside Notion pages](0016-notion-nested-databases.md) | done |
| 0017 | [Schema-checked Notion writes and readable confirmations](0017-schema-checked-notion-writes.md) | done |
| 0018 | [Tag-pinned deployment without release directories](0018-tag-pinned-deployment.md) | done |
| 0019 | [Read nested block content through a block-children tool](0019-nested-block-children.md) | done |
| 0020 | [Photos and files in Telegram conversations](0020-telegram-attachments.md) | done |
| 0021 | [NAS files through mounted per-user roots](0021-nas-file-access.md) | done |
| 0022 | [Monitoring stack in its own repository with a single deployment directory](0022-monitoring-repository-split.md) | done |
| 0023 | [Send a NAS file to the user in Telegram](0023-outbound-telegram-files.md) | done |
| 0024 | [Durable scheduler and scheduled model turns](0024-scheduled-model-turns.md) | in-progress |
| 0025 | [Per-user read-only Gmail connection](0025-gmail-read-connection.md) | in-progress |
| 0026 | [Web search, page reading, weather, and PDF text](0026-web-weather-and-pdf-tools.md) | done |
| 0027 | [NS train information: trips, departures, and disruptions](0027-ns-train-information.md) | in-progress |
| 0028 | [A visual day card with the reply as its caption](0028-day-card.md) | in-progress |
| 0029 | [TypeSafe judgments for approvals, outside content, and tool hints](0029-typesafe-guards.md) | in-progress |

Start new plans from [the template](TEMPLATE.md). Run `scripts/check-plans`
before every commit.
