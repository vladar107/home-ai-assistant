# 0099 — Close the gaps where outside text passes as the user's own

Status: `in-progress`

## Context

Plan 0051 made actions wait for Confirm once a run or conversation has read
text from outside the household. The broker marks the run external when a
`ThirdParty` tool without `Household` starts, and marks the conversation on
success. The gates read those marks: `approval.Act` (with `Strict` for
memory instructions), the seen-address rule of `fetch_page`, the first rule
of the search check, and immediate Home Assistant actions.

A review found five places where outside text still gets the authority of
the user's own request:

1. Notion and NAS reads are `Private` but not `ThirdParty`. Plan 0079 kept
   them that way. But a Notion page can carry text from others: shared pages,
   guest edits, comments, forms, and web clips. A file the user received in
   Telegram, which `read_attachment` treats as outside content, can be saved
   with `nas_save_attachment` and read back through `nas_read_file`, which
   treats it as clean.
2. A forwarded Telegram message, or a forwarded voice note's transcript, is
   used as the user's own words. `tools.NewRun(request.Message)` takes the
   addresses in it as the user's, and nothing tells the model the text is
   someone else's.
3. `memory_remove` acts at once with no gate and no Undo. Injected text can
   erase a standing instruction in one step.
4. The error result of a third-party tool reaches the model without the
   untrusted prefix and without the conversation's exposure mark. Only the
   success branch marks.
5. A Home Assistant tool whose intent starts with `get_` counts as a read by
   its name alone. A script named `get_…` that changes something then skips
   confirmation and is offered to read-only runs. This defeats the
   `assistant-read` opt-in for scripts that plan 0093 added.

## Goal

- Text from outside the household reaches no gate as clean: not through
  Notion, NAS files, forwarded messages, or error results.
- Removing a memory instruction is gated like writing one, and its Undo
  restores the entry exactly.
- A Home Assistant script is a read only when it carries the `assistant-read`
  label.

## Decisions

- **Notion reads are outside content.** `notion_search`, `notion_get_page`,
  `notion_query_data_source`, `notion_get_block_children`, and `movies_next`
  become `ThirdParty`, and stay `Private` and unscreened, so nothing they
  return reaches TypeSafe.
  - **Authorship is not trusted.** `last_edited_by` names only the last
    editor. Earlier edits, comments, synced blocks, and form answers stay in
    the page. A web clip is written by the user's own account but holds a
    stranger's text. So no page counts as the user's own, whoever edited it
    last.
  - **Not `Household`.** That flag means text from the household's own
    devices. A Notion workspace holds text from anyone it is shared with.
  - **Trade-off.** This reverses the plan 0079 decision that a turn which read
    a Notion page may still update a Notion property at once. A Notion write
    after a Notion read in the same conversation now proposes, and so do
    home actions, reminders, and tasks. That includes movie scores and dates
    that start from a Notion search. Undo-able direct writes stay for
    conversations that reach Notion without reading it first. This is
    accepted: the alternative lets a page edited by anyone with access steer
    the user's tools.
- **NAS file contents are outside content, in every folder.**
  `nas_read_file` becomes `ThirdParty`, stays `Private`, and is unscreened.
  The application cannot tell where a file came from. The inbox holds files
  forwarded from anyone, and other folders hold downloads, scans, and mail
  attachments. No folder is trusted, so there is no inbox exception.
  - **Listings stay clean.** `nas_list_files` and `nas_search_files` return
    names, not contents. A saved attachment keeps the name its sender gave
    it, so a name can carry a short outside phrase. This is accepted as
    residual risk: marking listings would make browsing the NAS gate every
    later home action.
- **Forwarded messages are outside content.**
  - Telegram's `forward_origin` and `is_automatic_forward` mark a message as
    forwarded. The text, the caption, or the transcript of a forwarded voice
    note is then queued with a `forwarded` flag. Migration `038` adds the
    flag to `conversation_requests`, so a restart between enqueue and turn
    keeps it.
  - When the turn starts, the run is marked external with the message text,
    and the conversation is marked externally exposed until `/new`, as for a
    turn that carries an attachment. The addresses in the text count as
    seen in outside text, so `fetch_page` can still open a forwarded link.
  - The model's message starts with a `[system]` line saying the text was
    forwarded, is someone else's, and is not the user's instruction.
  - A forwarded message is not recorded as one of the user's recent
    messages, which the intent check and the search check judge actions
    against.
  - **External replies and quotes are not marked.** A reply to a message in
    another chat (`external_reply`) and a quote (`quote`) carry text the
    application never reads, so none of it reaches the model. The user's own
    text beside them stays their instruction.
- **Removing a memory entry follows writing one.**
  - An instruction goes through `approval.Act` with `Strict`. It is removed at
    once in a clean turn that answers the user's own message, and proposed
    after any third-party content, household devices included.
  - A fact is removed at once while the conversation has read no
    third-party content, as a fact is written, and is proposed after.
  - A proposal stores the entry's ID and text. On Confirm the entry must
    still hold that text, or nothing is removed.
  - A direct removal of an instruction keeps the whole entry as its Undo:
    ID, kind, text, source, and both timestamps. Undo puts it back exactly,
    unless an entry with that ID exists again, or memory has no room or
    already holds the text.
  - A direct fact removal has no Undo, as a direct fact write has none. It
    shows a "Forgot" line in the reply.
  - `memory_remove` joins `ActionTools`.
- **Third-party errors are marked like results.** A `tool_error` result of a
  `ThirdParty` tool gets the untrusted prefix. It sets the same principal,
  run, and conversation marks as a success. The run remembers the
  addresses in the error text too. The broker's own failure texts (timeout,
  failure, oversize) carry no provider text and stay unmarked. A failed
  outside call therefore marks its conversation until `/new`, not only its
  run.
- **Home Assistant reads are an allowlist.** A tool counts as a read by name
  only when its remote name is one of the known reads:
  `homeassistant__GetLiveContext`, `todo__get_items`, or
  `calendar__get_events`. Every other tool, including any script and any tool
  Home Assistant adds later, is a write. A script becomes a read only through
  the `assistant-read` label.

## Approach

1. Notion and NAS read definitions; guidance and docs.
2. Broker: marks and prefix for third-party error results.
3. Telegram forward detection, the `forwarded` request flag and migration
   `038`, the turn's external mark, and the model's notice.
4. `memory_remove` gating, its executor and Undo, and `ActionTools`.
5. Home Assistant read allowlist.
6. `docs/architecture.md` and `docs/operations.md` where they
   describe these rules.

## Files affected

- `internal/connectors/notion/tools.go`, `internal/connectors/notion/movies.go`
- `internal/connectors/nas/tools.go`
- `internal/tools/broker.go`
- `internal/telegram/client.go`, `internal/telegram/service.go`
- `internal/conversation/types.go`, `internal/conversation/service.go`
- `internal/store/sqlite/conversation.go`, migration `038`
- `internal/memory/tools.go`
- `internal/connectors/homeassistant/snapshot.go`
- `docs/architecture.md`, `docs/operations.md`
- Tests beside each

## Validation

- Formatting, module, vet, build, test, and plan checks.
- Integration tests over real SQLite:
  - a Notion read and a NAS file read make a later direct write propose;
  - a third-party error result carries the prefix and marks the
    conversation;
  - a forwarded message marks its turn and conversation external, tells the
    model, and survives the queue;
  - instruction removal acts and undoes exactly in a clean turn, and
    proposes after third-party content; fact removal proposes after it;
  - a `get_`-named script is a write without the label.

## Risks and open questions

- Notion writes now confirm in most conversations, which brings back cards
  plan 0079 removed. A later plan could trust pages that only the user can
  edit, if Notion's API can show that reliably.
- NAS file names stay unmarked, as above.
- A forwarded message that starts with a command, or holds a token the
  settings sections intercept, is still handled as if the user typed it.
  Neither reaches the model; closing that is left to the work on token
  interception.
- A Home Assistant read whose remote name is not in the allowlist now needs
  Confirm and is not offered to read-only runs, until it is added.
