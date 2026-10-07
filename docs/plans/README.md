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
| 0024 | [Durable scheduler and scheduled model turns](0024-scheduled-model-turns.md) | done |
| 0025 | [Per-user read-only Gmail connection](0025-gmail-read-connection.md) | done |
| 0026 | [Web search, page reading, weather, and PDF text](0026-web-weather-and-pdf-tools.md) | done |
| 0027 | [NS train information: trips, departures, and disruptions](0027-ns-train-information.md) | done |
| 0028 | [A visual day card with the reply as its caption](0028-day-card.md) | done |
| 0029 | [TypeSafe judgments for approvals, outside content, and tool hints](0029-typesafe-guards.md) | in-progress |
| 0030 | [Reply latency, model, and business metrics](0030-reply-and-business-metrics.md) | done |
| 0031 | [Consolidate documentation into a README, C4 diagrams, and an operations guide](0031-documentation-consolidation.md) | done |
| 0032 | [Tell the model the current local time on every chat turn](0032-current-time-in-turns.md) | done |
| 0033 | [Bus information: trips, stop status, and disruptions](0033-live-bus-information.md) | done |
| 0034 | [Make unfinished model calls and rare failures visible](0034-unfinished-model-calls.md) | done |
| 0035 | [Upgrade Pi to 1.0.0](0035-pi-1-0-upgrade.md) | done |
| 0036 | [Several Google accounts per user, with Mail and Calendar switches](0036-google-accounts.md) | done |
| 0037 | [Read-only Google Calendar](0037-google-calendar.md) | done |
| 0038 | [Tell the model that NS tools take railway stations only](0038-railway-stations-only.md) | done |
| 0039 | [Home Assistant: everyday control through its MCP server, sensitive actions confirmed](0039-home-assistant.md) | done |
| 0040 | [One-time jobs and reminders](0040-one-time-jobs-and-reminders.md) | done |
| 0041 | [Grouped /settings with an overview and pages](0041-grouped-settings.md) | done |
| 0042 | [Batched approvals](0042-batched-approvals.md) | done |
| 0043 | [Shared location in conversation and location-aware tools](0043-shared-location.md) | done |
| 0044 | [Movie night tools for the Notion movie table](0044-movie-night-tools.md) | done |
| 0045 | [Grafana metrics and logs in conversation](0045-grafana-metrics-and-logs.md) | done |
| 0046 | [Per-user memory the user can see and edit](0046-per-user-memory.md) | done |
| 0047 | [Return to earlier conversations](0047-earlier-conversations.md) | done |
| 0048 | [Background sub-agents that do not block the chat](0048-background-tasks.md) | in-progress |
| 0049 | [Tell the model who the user is](0049-name-the-user.md) | done |
| 0050 | [Scrub private data before making the repository public](0050-public-release-scrub.md) | in-progress |
| 0051 | [Confirm actions after outside content](0051-confirm-actions-after-outside-content.md) | done |
| 0052 | [Supply-chain pins and public-repository hygiene](0052-supply-chain-pins-and-public-hygiene.md) | done |
| 0053 | [Edit scheduled jobs](0053-edit-scheduled-jobs.md) | done |
| 0054 | [Continue after Confirm, and runtime hygiene](0054-continue-after-confirm-and-runtime-hygiene.md) | done |
| 0055 | [Tool reliability: fewer wasted calls, retries, and clean scheduled replies](0055-tool-reliability.md) | done |
| 0056 | [Answers grounded in tool data](0056-grounded-answers.md) | done |
| 0057 | [Saved routes and easier schedule editing](0057-routes-and-schedule-editing.md) | proposed |
| 0058 | [Watches: tell me when my train or route goes wrong](0058-watches.md) | proposed |

Start new plans from [the template](TEMPLATE.md). Run `scripts/check-plans`
before every commit.
