package grafana

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// maximumTextBytes keeps a result clear of the broker's 16 KiB limit, so the model gets a
	// shortened answer with a note rather than a refusal.
	maximumTextBytes  = 12 << 10
	maximumSeries     = 20
	sequencePoints    = 12
	maximumLineRunes  = 500
	maximumDescRunes  = 200
	fullTimeLayout    = "2006-01-02 15:04 MST"
	shortTimeLayout   = "15:04"
	longTimeLayout    = "Jan 2 15:04"
	shortClockLayout  = "15:04:05"
	longClockLayout   = "Jan 2 15:04:05"
	truncatedNoteText = "The answer was cut short to fit; narrow the query to see the rest."
)

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// text collects lines up to maximumTextBytes; once a line does not fit, it drops the rest and
// ends with a note saying so.
type text struct {
	lines []string
	size  int
	full  bool
}

func (t *text) add(format string, args ...any) {
	if t.full {
		return
	}
	line := fmt.Sprintf(format, args...)
	if t.size+len(line)+1 > maximumTextBytes-len(truncatedNoteText)-1 {
		t.full = true
		return
	}
	t.lines = append(t.lines, line)
	t.size += len(line) + 1
}

func (t *text) String() string {
	if t.full {
		return strings.Join(append(t.lines, truncatedNoteText), "\n")
	}
	return strings.Join(t.lines, "\n")
}

type dashboardHit struct {
	UID         string   `json:"uid"`
	Title       string   `json:"title"`
	FolderTitle string   `json:"folderTitle"`
	Tags        []string `json:"tags"`
}

type dashboardDocument struct {
	Dashboard struct {
		Title       string  `json:"title"`
		Description string  `json:"description"`
		Panels      []panel `json:"panels"`
		Templating  struct {
			List []variable `json:"list"`
		} `json:"templating"`
	} `json:"dashboard"`
	Meta struct {
		FolderTitle string `json:"folderTitle"`
	} `json:"meta"`
}

type panel struct {
	ID          int             `json:"id"`
	Title       string          `json:"title"`
	Type        string          `json:"type"`
	Description string          `json:"description"`
	Datasource  json.RawMessage `json:"datasource"`
	FieldConfig struct {
		Defaults struct {
			Unit string `json:"unit"`
		} `json:"defaults"`
	} `json:"fieldConfig"`
	Targets []target `json:"targets"`
	// Panels are the children of a collapsed row.
	Panels []panel `json:"panels"`
}

type target struct {
	Expr       string          `json:"expr"`
	Hide       bool            `json:"hide"`
	Datasource json.RawMessage `json:"datasource"`
}

type variable struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Current struct {
		Value json.RawMessage `json:"value"`
	} `json:"current"`
}

func renderDashboardList(found []dashboardHit) string {
	if len(found) == 0 {
		return "Grafana has no dashboards the assistant can see."
	}
	var out text
	out.add("Dashboards (uid — title, folder):")
	for _, hit := range found {
		line := fmt.Sprintf("%s — %s", hit.UID, oneLine(hit.Title))
		if hit.FolderTitle != "" {
			line += ", " + oneLine(hit.FolderTitle)
		}
		out.add("%s", line)
	}
	return out.String()
}

func renderDashboard(uid string, found dashboardDocument) string {
	var out text
	title := oneLine(found.Dashboard.Title)
	if found.Meta.FolderTitle != "" {
		title += " (folder " + oneLine(found.Meta.FolderTitle) + ")"
	}
	out.add("Dashboard %s: %s", uid, title)
	if description := clip(found.Dashboard.Description, maximumDescRunes); description != "" {
		out.add("%s", description)
	}
	var variables []string
	for _, v := range found.Dashboard.Templating.List {
		if v.Name == "" || v.Type == "datasource" {
			continue
		}
		variables = append(variables, "$"+v.Name+" = "+variableValue(v.Current.Value))
	}
	if len(variables) > 0 {
		out.add("Variables: %s", strings.Join(variables, "; "))
	}
	out.add("Panels:")
	renderPanels(&out, found.Dashboard.Panels, "")
	return out.String()
}

func renderPanels(out *text, panels []panel, indent string) {
	for _, p := range panels {
		if p.Type == "row" {
			out.add("%sRow: %s", indent, oneLine(p.Title))
			renderPanels(out, p.Panels, indent+"  ")
			continue
		}
		header := fmt.Sprintf("%s- [%d] %s (%s", indent, p.ID, oneLine(p.Title), p.Type)
		if p.FieldConfig.Defaults.Unit != "" {
			header += ", unit " + p.FieldConfig.Defaults.Unit
		}
		out.add("%s)", header)
		if description := clip(p.Description, maximumDescRunes); description != "" {
			out.add("%s  %s", indent, description)
		}
		for _, t := range p.Targets {
			if t.Hide || strings.TrimSpace(t.Expr) == "" {
				continue
			}
			language := queryLanguage(t.Datasource)
			if language == "" {
				language = queryLanguage(p.Datasource)
			}
			if language == "" {
				language = "Query"
			}
			out.add("%s  %s: %s", indent, language, oneLine(t.Expr))
		}
	}
}

// queryLanguage names a target's language from its data source reference, which is either an
// object with a type or a bare name or variable.
func queryLanguage(raw json.RawMessage) string {
	var reference struct {
		Type string `json:"type"`
		UID  string `json:"uid"`
	}
	if json.Unmarshal(raw, &reference) != nil {
		var name string
		if json.Unmarshal(raw, &name) != nil {
			return ""
		}
		reference.UID = strings.ToLower(name)
	}
	switch {
	case reference.Type == "loki" || reference.UID == lokiUID:
		return "LogQL"
	case reference.Type == "prometheus" || reference.UID == prometheusUID:
		return "PromQL"
	default:
		return ""
	}
}

func variableValue(raw json.RawMessage) string {
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return oneLine(single)
	}
	var several []string
	if json.Unmarshal(raw, &several) == nil {
		return oneLine(strings.Join(several, ", "))
	}
	return "(unset)"
}

type queryResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

type sample [2]json.RawMessage

func (s sample) decode() (time.Time, float64, bool) {
	var seconds float64
	var value string
	if json.Unmarshal(s[0], &seconds) != nil || json.Unmarshal(s[1], &value) != nil {
		return time.Time{}, 0, false
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return time.Time{}, 0, false
	}
	return time.UnixMilli(int64(math.Round(seconds * 1000))), parsed, true
}

type series struct {
	Metric map[string]string `json:"metric"`
	Value  sample            `json:"value"`
	Values []sample          `json:"values"`
}

// renderResult shows a Prometheus or Loki metric answer: one line per series for an instant query,
// and a summary with a short sequence per series for a range.
func (s *Service) renderResult(response queryResponse, w window, source string) (string, error) {
	var out text
	switch response.Data.ResultType {
	case "scalar", "string":
		var value sample
		if json.Unmarshal(response.Data.Result, &value) != nil {
			return "", errMalformed
		}
		at, number, ok := value.decode()
		if !ok {
			return "", errMalformed
		}
		out.add("%s at %s: %s", source, at.In(s.location).Format(fullTimeLayout), formatValue(number))
		return out.String(), nil
	case "vector", "matrix":
	default:
		return "", errMalformed
	}
	var found []series
	if json.Unmarshal(response.Data.Result, &found) != nil {
		return "", errMalformed
	}
	if w.isRange() {
		header := fmt.Sprintf("%s over %s to %s, step %s: %s.", source, formatDuration(w.end.Sub(w.start)), w.end.In(s.location).Format(fullTimeLayout), formatDuration(w.step), countOf(len(found), "series", "series"))
		if w.widened {
			header += fmt.Sprintf(" The step was widened to %s to stay under %d points per series.", formatDuration(w.step), maximumPoints)
		}
		out.add("%s", header)
	} else {
		out.add("%s at %s: %s.", source, w.end.In(s.location).Format(fullTimeLayout), countOf(len(found), "series", "series"))
	}
	if len(found) == 0 {
		out.add("Nothing matched; check the metric and label names against a dashboard panel's query.")
		return out.String(), nil
	}
	if len(found) > maximumSeries {
		out.add("Showing the first %d of %d series; aggregate or filter to see the rest.", maximumSeries, len(found))
		found = found[:maximumSeries]
	}
	for _, one := range found {
		if response.Data.ResultType == "vector" {
			_, number, ok := one.Value.decode()
			if !ok {
				return "", errMalformed
			}
			out.add("%s: %s", formatLabels(one.Metric), formatValue(number))
			continue
		}
		s.renderSeries(&out, one, w)
	}
	return out.String(), nil
}

func (s *Service) renderSeries(out *text, one series, w window) {
	type point struct {
		at    time.Time
		value float64
	}
	var points []point
	for _, raw := range one.Values {
		at, value, ok := raw.decode()
		if ok {
			points = append(points, point{at, value})
		}
	}
	if len(points) == 0 {
		out.add("%s: no samples", formatLabels(one.Metric))
		return
	}
	minimum, maximum, sum, counted := math.Inf(1), math.Inf(-1), 0.0, 0
	for _, p := range points {
		if math.IsNaN(p.value) {
			continue
		}
		minimum, maximum = math.Min(minimum, p.value), math.Max(maximum, p.value)
		sum += p.value
		counted++
	}
	last := points[len(points)-1]
	layout := shortTimeLayout
	if w.end.Sub(w.start) > 24*time.Hour {
		layout = longTimeLayout
	}
	summary := fmt.Sprintf("%s: last %s at %s", formatLabels(one.Metric), formatValue(last.value), last.at.In(s.location).Format(layout))
	if counted > 0 {
		summary += fmt.Sprintf(", min %s, max %s, mean %s", formatValue(minimum), formatValue(maximum), formatValue(sum/float64(counted)))
	}
	out.add("%s (%d points)", summary, len(points))
	shown := make([]string, 0, sequencePoints)
	for i := range min(sequencePoints, len(points)) {
		index := i * (len(points) - 1) / max(1, min(sequencePoints, len(points))-1)
		p := points[index]
		shown = append(shown, p.at.In(s.location).Format(layout)+" "+formatValue(p.value))
	}
	out.add("  %s", strings.Join(shown, " · "))
}

type stream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

// renderStreams shows log lines in the requested order, each with the time in the household's zone
// and the container it came from.
func (s *Service) renderStreams(response queryResponse, w window, limit int, direction string) (string, error) {
	var found []stream
	if json.Unmarshal(response.Data.Result, &found) != nil {
		return "", errMalformed
	}
	type entry struct {
		at     int64
		source string
		line   string
	}
	var entries []entry
	for _, one := range found {
		source := streamSource(one.Stream)
		for _, value := range one.Values {
			at, err := strconv.ParseInt(value[0], 10, 64)
			if err != nil {
				return "", errMalformed
			}
			entries = append(entries, entry{at, source, value[1]})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if direction == "forward" {
			return entries[i].at < entries[j].at
		}
		return entries[i].at > entries[j].at
	})
	if len(entries) > limit {
		entries = entries[:limit]
	}
	order := "newest first"
	if direction == "forward" {
		order = "oldest first"
	}
	var out text
	out.add("Loki over %s to %s, %s: %s.", formatDuration(w.end.Sub(w.start)), w.end.In(s.location).Format(fullTimeLayout), order, countOf(len(entries), "line", "lines"))
	if len(entries) == 0 {
		out.add("Nothing matched; check the stream selector's labels and the filters.")
		return out.String(), nil
	}
	if len(entries) == limit {
		out.add("The limit of %d lines was reached; narrow the query or raise limit to see more.", limit)
	}
	layout := shortClockLayout
	if w.end.Sub(w.start) > 24*time.Hour {
		layout = longClockLayout
	}
	for _, e := range entries {
		out.add("%s %s: %s", time.Unix(0, e.at).In(s.location).Format(layout), e.source, clip(ansiEscape.ReplaceAllString(e.line, ""), maximumLineRunes))
	}
	return out.String(), nil
}

func streamSource(labels map[string]string) string {
	for _, name := range []string{"container", "service_name", "compose_project", "filename"} {
		if value := labels[name]; value != "" {
			return oneLine(value)
		}
	}
	return "unknown"
}

func formatLabels(labels map[string]string) string {
	name := labels["__name__"]
	keys := make([]string, 0, len(labels))
	for key := range labels {
		if key != "__name__" {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, key+"="+strconv.Quote(oneLine(labels[key])))
	}
	if name == "" && len(pairs) == 0 {
		return "{}"
	}
	if len(pairs) == 0 {
		return name
	}
	return name + "{" + strings.Join(pairs, ", ") + "}"
}

func formatValue(value float64) string {
	switch {
	case math.IsNaN(value):
		return "NaN"
	case math.IsInf(value, 0):
		return strconv.FormatFloat(value, 'f', -1, 64)
	case value == math.Trunc(value) && math.Abs(value) < 1e15:
		return strconv.FormatFloat(value, 'f', 0, 64)
	default:
		return strconv.FormatFloat(value, 'g', 6, 64)
	}
}

// formatDuration writes a duration in the largest whole unit, as the model wrote it.
func formatDuration(d time.Duration) string {
	for _, unit := range []struct {
		size time.Duration
		name string
	}{{7 * 24 * time.Hour, "w"}, {24 * time.Hour, "d"}, {time.Hour, "h"}, {time.Minute, "m"}} {
		if d >= unit.size && d%unit.size == 0 {
			return strconv.FormatInt(int64(d/unit.size), 10) + unit.name
		}
	}
	return strconv.FormatInt(int64(d/time.Second), 10) + "s"
}

func countOf(n int, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return strconv.Itoa(n) + " " + plural
}

// oneLine keeps text from services on a single line without control characters.
func oneLine(value string) string {
	return strings.Join(strings.FieldsFunc(value, func(r rune) bool {
		return r < 0x20 || r == 0x7f
	}), " ")
}

func clip(value string, limit int) string {
	value = strings.TrimSpace(oneLine(value))
	if runes := []rune(value); len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return value
}
