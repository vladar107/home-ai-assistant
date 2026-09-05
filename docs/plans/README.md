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
| 0004 | [Per-user model subscriptions](0004-per-user-model-subscriptions.md) | proposed |
| 0005 | [Bot-registered commands and actionable provider failures](0005-bot-commands-and-provider-failures.md) | done |
| 0006 | [Quoted replies](0006-quoted-replies.md) | proposed |

Start new plans from [the template](TEMPLATE.md). Run `scripts/check-plans`
before every commit.
