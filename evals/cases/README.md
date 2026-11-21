# Eval cases

One JSON file per case, loaded by `internal/eval` (`LoadCases`). Decoding is strict: an unknown
field fails the load, and every error names the file. Cases are synthetic and public: use
neutral names, places and data, never a real message, trace, memory or device.

```json
{
  "id": "sample-reminder",
  "title": "A one-off reminder lands at the right local time",
  "tags": ["regression", "time"],
  "kind": "wrong-local-time",
  "clock": {"local": "2026-03-10T08:30", "zone": "Europe/Berlin"},
  "user": {"name": "Sam", "switches": {"schedule": true}, "memory": ["Prefers short replies."]},
  "world": {},
  "steps": [
    {
      "say": "Remind me tomorrow at 9 to call the dentist.",
      "expect": {
        "calls": [{"tool": "schedule_create", "args": [{"path": "when", "within": {"from": "2026-03-11T08:55", "to": "2026-03-11T09:05"}}]}],
        "not_called": ["schedule_delete"],
        "reply": {"language": "en"}
      }
    }
  ],
  "rubric": ["Does the reply state the local time the reminder was set for?"],
  "trials": 3
}
```

- `id`: unique across files, lowercase kebab case. `title` is required.
- `kind`: the failure kind the case covers, lowercase kebab case, or `happy` for a flow that works.
- `clock`: `local` is `YYYY-MM-DDTHH:MM` wall time in the IANA `zone`.
- `user`: all optional: `name`, `switches` (name to bool), `memory` lines, `model`, `effort`, `language` (`en` or `ru`, the interface language).
  A switch name is the integration's name, such as `typesafe` (safety checks and the tool hint,
  on by default in a run that has a TypeSafe key) or `widgets`.
- `world`: state the tools see, keyed by provider. Each key must be a section registered by a
  world seeder; an unknown key fails the load. Times are `YYYY-MM-DDTHH:MM` in the case's zone.
  Sections, each decoded strictly:
  - `gmail`: `{"account": "sam@example.test", "messages": [{"id", "from", "to", "subject",
    "received", "body", "html", "snippet", "labels", "thread_id", "attachments": {name: bytes}}]}`
  - `calendar`: `{"account", "events": [{"id", "summary", "start", "end", "all_day", "location",
    "description", "organizer", "attendees": [{"email", "name", "response"}]}]}`
  - `notion`: `{"pages": [{"id", "title", "url", "lines": ["text", "- bullet"]}], "data_sources": [{"id", "title",
    "title_property" (default "Title"), "properties": [{"name", "type": "date" | "number" | "select" | "status" |
    "rich_text" | "checkbox", "options"}], "rows": [{"id", "title", "values": {column: value}}]}], "writes": true}`;
    a data source is a table whose rows are pages, `writes` grants the Notion write tools
  - `jobs`: `[{"id", "name", "prompt" | "reminder" | "heads_up": true, "cron" | "at": "YYYY-MM-DD HH:MM",
    "zone", "model", "disabled"}]`; `id` is the name `run_job` uses. `heads_up` is the Heads-up job
    (its prompt is the notes; cron defaults to the Heads-up schedule).
  - `memory`: `[{"kind": "fact" | "instruction", "text"}]`, beside `user.memory` lines (facts)
  - `backlog`: `[{"title", "details"}]`
  - `routes`: `[{"name", "days": ["mon"], "arrive_by", "legs": [{"mode", "minutes", "from", "to",
    "via", "stop", "line", "towards", "departures"}]}]`
  - `places`: `[{"name", "lat", "lon", "radius_m"}]` (saved places)
  - `parcels`: `[{"carrier", "number", "title", "sender", "status", "window_from", "window_to", "pickup_point"}]`
    (parcels the user follows, found in mail; `status` is a parcel status such as `in_transit` or
    `out_for_delivery`, and times are local `YYYY-MM-DDTHH:MM`); the section turns Parcels on
  - `assistant_mail`: `{"address": "assistant@example.test", "tag": "sam1", "messages": [{"from", "subject", "body",
    "to", "thread_id", "received", "dmarc": "pass" | "fail", "sent"}]}`, the assistant's own mailbox with the user's tag
    (both defaults shown); a `triage_mail` step delivers the messages to the tagged address, `received`
    defaults to the case's clock and must not be earlier, and `dmarc` defaults to `pass`. A message
    with `"sent": true` is one the assistant sent for the user to `to` (no `from` or `dmarc`), such as
    the one that starts a thread; give a reply in that thread a later `received`. The user switch
    `assistant-mail-replies` turns on routine replies, which a notice reports under "Replied for you"
  - Connector fakes: each section is the seed of a loopback fake, decoded strictly by the fake
    (the full shape and an example are in the fake's package doc under `internal/testutil/`). A
    connector whose section the case lacks stays unconfigured, so its tools are not offered, and
    a case may name only tools its world offers (CI checks every case). Names and data are made up:
    - `trains` (`nssim`): `{"stations": [{"code": "EBK", "name": "Eastbrook Centraal"}], "trips", "departures", "disruptions", "ovfiets"}`;
      trips, departures and disruptions are the objects the NS API returns
    - `buses` (`bussim`): `{"stops": [{"id", "code", "name", "lat", "lon"}], "routes", "timing_points", "alerts", "places", "plans"}`
    - `weather` (`weathersim`): `{"places": [{"name": "Eastbrook", "latitude", "longitude", "timezone"}], "forecasts": [{"latitude", "longitude", "current", "days", "hours"}]}`
    - `pages` (`pagesim`): `{"pages": [{"host": "news.example", "path": "/market", "title", "body", "status", "redirect_to", "timeout"}]}`;
      `weather` and `pages` both configure the web connector, and only the seeded hosts resolve
    - `places_search` (`placessim`): `{"elements": [{"lat", "lon", "tags": {"amenity": "pharmacy", "name"}}], "reverse": [{"lat", "lon", "street", "areas"}]}`;
      it serves nearby-place search and the reverse geocoding that names a shared location. The
      name `places` stays the saved places above, and either section turns shared locations on
    - `flights` (`flightssim`): `{"flights": [{"number": "EX123", "date": "2026-03-10", "status", "departure", "arrival"}]}`
    - `monitoring` (`grafanasim`): `{"dashboards", "metrics", "logs", "rules", "annotations"}`
    - `nas` (`nassim`): `{"roots": [{"name": "home", "files": {"Documents/notes.md": "text"}, "folders", "writable"}], "default_write"}`
    - `home` (`homesim`): `{"entities": [{"entity_id", "name", "state", "area", "exposed", "statistics"}], "tools": [{"name", "answer", "effects"}], "services"}`;
      the fake runs on the case's clock, and the home tools are offered only when this section exists
    - `mcp`: an object from a catalogue server name (lowercase letters and digits) to its seed
      (`mcpsim`): `{"kitchen": {"tools": [{"name": "lookup_recipe", "input_schema", "text", "answers": [{"arguments": {"dish": "soup"}, "text"}]}]}}`;
      the tools are offered as `mcp_<server>_<tool>`, and each server is switched on for the user
      unless `user.switches` sets `mcp-<server>` to false
- `steps`: at least one, played in one session. A step has exactly one action: `say` (text),
  `confirm: true`, `reject: true`, `thumbs_down: true` (the last three act on the previous turn
  and cannot start a case), `run_job` (id of a scheduled job in the world), or `triage_mail: true`
  (delivers the `assistant_mail` section's messages and lets the mailbox triage them; the step's
  reply is the notice, and `reply.quiet` expects none).
- `expect` (optional per step):
  - `calls`: `{tool | any_of, min (default 1), max, args}`; `any_of` lists two or more tools,
    any of which satisfies the call (give `tool` or `any_of`, not both); each arg has a dot `path` into the tool's JSON
    arguments and exactly one of `equals`, `contains`, `matches` (regexp) or `within`
    (`{from, to}`, both `YYYY-MM-DDTHH:MM` or both `HH:MM`, local time);
  - `not_called`, `max_calls`, `ordered` (calls occur in the listed order);
  - `proposals`: `{min, max, tools}`; `no_writes`: nothing was written;
  - `reply`: `contains`, `not_contains` (case-insensitive substrings), `matches`, `not_matches`
    (regexps, validated on load; the reply must match, or must not match), `language` (BCP-47, such as `en`),
    `quiet` (a scheduled run that sends nothing);
  - `store`: checks on the final store, each an object with a `check` name.
- `rubric`: yes/no questions for the judge, each about one dimension.
- `trials`: runs per case, 1 to 10, default 3.
- `expected_probes`: `[{"probe", "reason"}]`, the trivial-answer probes the case passes on purpose,
  such as `refusal` and `no-tools` on an injection case whose right answer is to call nothing; the
  reason is required. `home-agent-eval probe` runs each case's checks, with no model, against
  synthetic turns: `empty` (an empty reply), `refusal`, `echo` (the user's last message, skipped
  for a case with no `say` step), `acknowledgement` and `no-tools` (a confident reply claiming the
  work was done); none calls a tool or offers a change, so a `confirm` or `reject` step has nothing
  to press. It lists every case a probe passes that does not expect it, the cases with no
  deterministic check as not probeable, and, for information only, the expected probes a case now
  fails. Tighten what it lists.
