# 0124 — History over any time series, with charts

Status: `proposed`

## Context

The assistant can already read history:

- Home Assistant statistics through `ha_history`: change, mean, min, and max,
  or up to 60 points by day, week, or month.
- Prometheus range queries through `grafana_metrics`.

Both return text that is capped for the model (for Grafana, 20 series of at
most 12 sample points). Trends and comparisons ("this month against last",
"what is unusual this week") therefore depend on the model reading a few
numbers. Nothing draws them.

The day card (plan 0028) shows that the service can render a PNG in Go with
`golang.org/x/image` and the embedded fonts, and send it with the reply.

## Goal

- The user can ask for a trend, a comparison, or an outlier over any series
  the assistant can reach, for example:
  - "Show the living room temperature this week."
  - "Compare solar production this month with last month."
  - "Plot the price list you just found."

  They get a chart image with the reply.
- The numbers in the reply come from code: start, end, change, min and max
  and when they happened, mean, and outliers. The model does not estimate
  them from a small sample.
- Up to four series fit on one chart. A series can be shifted by a period to
  compare it with the same series earlier.

## Decisions

- **One `chart` tool that fetches its own data.** The model names the
  series. The tool fetches it at chart resolution, computes the statistics,
  draws the chart, and stages the PNG for the reply as the day card does.
  Fetching in the tool keeps hundreds of points out of the model's context
  and out of its arithmetic.

  Sources:
  - `ha`: a sensor, a period, and a resolution of `hour`, `day`, `week`, or
    `month`, through the same recorder statistics `ha_history` uses. Needs
    the Home switch.
  - `grafana`: a PromQL query, a range of up to 30 days, and an optional
    step, through the same client `grafana_metrics` uses, with at most 500
    points per series. It needs the Grafana switch, and is limited to the
    principals the operator allowed for Grafana, as `grafana_metrics` is.
  - `values`: up to 200 points passed by the model. This is for any data a
    tool returned in this turn, such as a web table or Notion rows.

  Each series has a label, an optional unit, and an optional
  `shift: "1w" | "1M" | "1y"` for comparisons.
- **Rendering in Go, no new dependency.** A new `internal/chart` package draws
  with `golang.org/x/image/vector` and the fonts the day card embeds:
  - chart types: line, step, and bar;
  - a shared time axis, up to two value axes when units differ, a legend,
    and min and max markers;
  - a colour palette checked for contrast;
  - a fixed 1200×700 size.

  Rendering time is recorded as the day card's is.
- **Statistics in code.** For each series, the tool computes:
  - first and last value, change, min and max with their times, and mean;
  - for a shifted comparison, the difference of means and totals;
  - outliers: points more than 3 median absolute deviations from the median.

  The tool result returns these as text, so the reply is grounded (plan
  0056).
- **Privacy and outside data.** `ha` and `grafana` sources are private reads.
  A `values` series carries whatever trust its origin had. The tool does not
  change exposure.
- **One image per reply.** The outbox keeps one PNG per turn, and a later
  stage replaces an earlier one. The outbox gains a stage-once rule that the
  day card and `chart` both use. A second image staged by the same turn run
  fails with "this reply already has an image; combine series instead",
  whichever tool staged first. The rule tracks what this run staged, not
  what is on disk, so a re-run of a turn is not blocked by its own earlier
  file. The day card's guidance, which today says a later card replaces it,
  changes to match.

  Several images per reply, through a media-group outbox, is a follow-up.
- **Read risk.** `chart` is `RiskRead`, so scheduled reports and background
  tasks can send a chart. Agent watch checks have no turn key, so `chart`
  fails there, as the day card does.
- **Limits.** At most four series and 2000 points in total after fetching,
  downsampled by bucket mean.

## Approach

1. The `internal/chart` renderer and statistics, with golden-image tests at a
   small tolerance.
2. The `chart` tool and its data fetching, reusing the Home Assistant and
   Grafana clients. Staged PNG output through `internal/outbox`.
3. Guidance: use `chart` for "show", "trend", and "compare" questions. Keep
   `ha_history` and `grafana_metrics` for single numbers.
4. README and `docs/architecture.md`.

## Files affected

- `internal/chart` (new)
- `internal/daycard/assets.go`: the embedded Inter fonts move to a shared
  `internal/fonts` package that both renderers use
- `internal/connectors/homeassistant` (statistics at resolution),
  `internal/connectors/grafana` (raw range points)
- `internal/outbox` (stage-once), `internal/connectors/web/card.go` (day
  card uses it), `internal/wiring`, `internal/prompt`
- README, `docs/architecture.md`

## Validation

- Tests:
  - golden images for each chart type, two axes, and a shifted comparison;
  - statistics against hand-computed series, including gaps and outliers;
  - fake Home Assistant websocket and Prometheus HTTP servers at their
    boundaries: the tool fetches, downsamples, and stages exactly one PNG;
  - a source whose switch is off fails with the switch named;
  - a principal outside the Grafana allowlist cannot chart a Grafana source;
  - a second chart, a chart after a day card, and a day card after a chart
    in one turn each fail without replacing the staged image;
  - another principal's switches are never used.
- A live check from the GoLand server: chart a real sensor for the last week.
- Formatting, module, test, vet, build, and plan checks.

## Risks and open questions

- Statistics about long periods depend on the recorder's long-term
  statistics being kept. A sensor without them returns a clear "no history"
  failure.
- Collections from plan 0125 could become a fourth source (a numeric field
  over a date field). That is left to that plan or a follow-up.
