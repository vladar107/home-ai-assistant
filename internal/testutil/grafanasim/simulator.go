// Package grafanasim is a loopback fake of the Grafana HTTP API the grafana connector reads:
// dashboard search and fetch, Prometheus and Loki queries through the data source proxy, alert
// rules and the alert history. It requires the connector's bearer token, refuses a query
// parameter the endpoint does not take, and answers a query nobody seeded as the data source does
// for one it cannot parse, with a 400 and an error message, so a case can test recovery from a
// bad query.
//
// A metric or log query is matched by its exact text ("query") or by a fragment of it
// ("contains"); the first seeded entry that matches answers.
//
// Example seed (fictional services):
//
//	{
//	  "dashboards": [{"uid": "svc-health", "title": "Service health", "folder": "Operations", "tags": ["ops"],
//	    "document": {"title": "Service health", "panels": [{"id": 1, "title": "Up", "type": "stat",
//	      "targets": [{"expr": "up{job=\"web\"}"}]}]}}],
//	  "metrics": [{"query": "up{job=\"web\"}", "series": [{"metric": {"job": "web"}, "value": 1,
//	    "values": [{"time": "2026-03-10T07:00:00Z", "value": 1}, {"time": "2026-03-10T07:30:00Z", "value": 0}]}]}],
//	  "logs": [{"contains": "web", "streams": [{"labels": {"container": "web"},
//	    "lines": [{"time": "2026-03-10T07:31:00Z", "line": "connection refused"}]}]}],
//	  "rules": [{"name": "WebDown", "health": "ok", "instances": [{"labels": {"severity": "critical"},
//	    "annotations": {"summary": "web is down"}, "state": "Alerting", "active_at": "2026-03-10T07:30:00Z"}]}],
//	  "annotations": [{"alert_name": "WebDown", "prev_state": "Normal", "new_state": "Alerting",
//	    "time": "2026-03-10T07:30:00Z"}]
//	}
package grafanasim

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/testutil/simkit"
)

// Token is the bearer token the fake accepts when the seed names none.
const Token = "sim-token"

const (
	prometheusBase = "/api/datasources/proxy/uid/prometheus/api/v1/"
	lokiRange      = "/api/datasources/proxy/uid/loki/loki/api/v1/query_range"
)

type Seed struct {
	// Token overrides Token.
	Token       string       `json:"token"`
	Dashboards  []Dashboard  `json:"dashboards"`
	Metrics     []Metric     `json:"metrics"`
	Logs        []Log        `json:"logs"`
	Rules       []Rule       `json:"rules"`
	Annotations []Annotation `json:"annotations"`
}

// Dashboard is listed by search and, with its UID, fetched as Document: the dashboard JSON with
// its title, description, panels and templating.
type Dashboard struct {
	UID      string          `json:"uid"`
	Title    string          `json:"title"`
	Folder   string          `json:"folder"`
	Tags     []string        `json:"tags"`
	Document json.RawMessage `json:"document"`
}

type Metric struct {
	Query    string   `json:"query"`
	Contains string   `json:"contains"`
	Series   []Series `json:"series"`
}

// Series is one Prometheus series: Value answers an instant query, Values a range query.
type Series struct {
	Metric map[string]string `json:"metric"`
	Value  float64           `json:"value"`
	Values []Point           `json:"values"`
}

type Point struct {
	Time  string  `json:"time"`
	Value float64 `json:"value"`
}

type Log struct {
	Query    string      `json:"query"`
	Contains string      `json:"contains"`
	Streams  []LogStream `json:"streams"`
	// Series answers a metric LogQL query such as count_over_time; streams answer a log query.
	Series []Series `json:"series"`
}

type LogStream struct {
	Labels map[string]string `json:"labels"`
	Lines  []LogLine         `json:"lines"`
}

type LogLine struct {
	Time string `json:"time"`
	Line string `json:"line"`
}

type Rule struct {
	Name      string          `json:"name"`
	Health    string          `json:"health"`
	LastError string          `json:"last_error"`
	Instances []AlertInstance `json:"instances"`
}

type AlertInstance struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	State       string            `json:"state"`
	ActiveAt    string            `json:"active_at"`
}

type Annotation struct {
	AlertName string   `json:"alert_name"`
	PrevState string   `json:"prev_state"`
	NewState  string   `json:"new_state"`
	Time      string   `json:"time"`
	Tags      []string `json:"tags"`
}

// Validate checks that seed has the shape the fake accepts.
func Validate(seed json.RawMessage) error {
	_, err := parse(seed)
	return err
}

func parse(raw json.RawMessage) (Seed, error) {
	var seed Seed
	if err := simkit.Decode(raw, &seed); err != nil {
		return seed, err
	}
	for index, dashboard := range seed.Dashboards {
		if dashboard.UID == "" || dashboard.Title == "" {
			return seed, fmt.Errorf("dashboards[%d] needs a uid and a title", index)
		}
		if len(dashboard.Document) > 0 {
			if err := simkit.Object(fmt.Sprintf("dashboards[%d].document", index), dashboard.Document); err != nil {
				return seed, err
			}
		}
	}
	checkTimes := func(name string, moments ...string) error {
		for _, moment := range moments {
			if _, err := time.Parse(time.RFC3339, moment); err != nil {
				return fmt.Errorf("%s times must be RFC 3339", name)
			}
		}
		return nil
	}
	for index, metric := range seed.Metrics {
		if (metric.Query == "") == (metric.Contains == "") {
			return seed, fmt.Errorf("metrics[%d] needs exactly one of query and contains", index)
		}
		for _, series := range metric.Series {
			for _, point := range series.Values {
				if err := checkTimes(fmt.Sprintf("metrics[%d]", index), point.Time); err != nil {
					return seed, err
				}
			}
		}
	}
	for index, entry := range seed.Logs {
		if (entry.Query == "") == (entry.Contains == "") {
			return seed, fmt.Errorf("logs[%d] needs exactly one of query and contains", index)
		}
		for _, stream := range entry.Streams {
			for _, line := range stream.Lines {
				if err := checkTimes(fmt.Sprintf("logs[%d]", index), line.Time); err != nil {
					return seed, err
				}
			}
		}
		for _, series := range entry.Series {
			for _, point := range series.Values {
				if err := checkTimes(fmt.Sprintf("logs[%d]", index), point.Time); err != nil {
					return seed, err
				}
			}
		}
	}
	for index, rule := range seed.Rules {
		for _, instance := range rule.Instances {
			if instance.ActiveAt != "" {
				if err := checkTimes(fmt.Sprintf("rules[%d]", index), instance.ActiveAt); err != nil {
					return seed, err
				}
			}
		}
	}
	for index, annotation := range seed.Annotations {
		if err := checkTimes(fmt.Sprintf("annotations[%d]", index), annotation.Time); err != nil {
			return seed, err
		}
	}
	return seed, nil
}

// Server is the running fake.
type Server struct {
	*simkit.Server
	seed  Seed
	token string
}

// Start runs the fake on a loopback port; the caller closes it.
func Start(raw json.RawMessage) (*Server, error) {
	seed, err := parse(raw)
	if err != nil {
		return nil, err
	}
	s := &Server{seed: seed, token: cmp.Or(seed.Token, Token)}
	inner, err := simkit.Serve(http.HandlerFunc(s.serve))
	if err != nil {
		return nil, err
	}
	s.Server = inner
	return s, nil
}

// New runs the fake for the length of a test.
func New(t testing.TB, raw json.RawMessage) *Server {
	t.Helper()
	s, err := Start(raw)
	if err != nil {
		t.Fatal(err)
	}
	s.Cleanup(t)
	return s
}

// Token is the bearer token the fake accepts.
func (s *Server) Token() string { return s.token }

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+s.token {
		simkit.WriteJSON(w, http.StatusUnauthorized, map[string]string{"message": "invalid API key"})
		return
	}
	query := r.URL.Query()
	var allowed []string
	var handler func(http.ResponseWriter, *http.Request)
	switch path := r.URL.Path; {
	case path == "/api/search":
		allowed, handler = []string{"type", "limit"}, s.search
	case strings.HasPrefix(path, "/api/dashboards/uid/"):
		handler = s.dashboard
	case path == "/api/prometheus/grafana/api/v1/rules":
		handler = s.rules
	case path == "/api/annotations":
		allowed, handler = []string{"type", "from", "to", "limit"}, s.annotations
	case path == prometheusBase+"query":
		allowed, handler = []string{"query", "time"}, s.instant
	case path == prometheusBase+"query_range":
		allowed, handler = []string{"query", "start", "end", "step"}, s.rangeQuery
	case path == lokiRange:
		allowed, handler = []string{"query", "start", "end", "step", "limit", "direction"}, s.logs
	default:
		http.NotFound(w, r)
		return
	}
	if unknown := simkit.UnknownParameters(query, allowed...); len(unknown) > 0 {
		http.Error(w, "unknown query parameter: "+strings.Join(unknown, ", "), http.StatusBadRequest)
		return
	}
	handler(w, r)
}

func (s *Server) search(w http.ResponseWriter, _ *http.Request) {
	hits := []map[string]any{}
	for _, dashboard := range s.seed.Dashboards {
		hits = append(hits, map[string]any{
			"uid": dashboard.UID, "title": dashboard.Title, "folderTitle": dashboard.Folder, "tags": dashboard.Tags, "type": "dash-db",
		})
	}
	simkit.WriteJSON(w, http.StatusOK, hits)
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimPrefix(r.URL.Path, "/api/dashboards/uid/")
	for _, dashboard := range s.seed.Dashboards {
		if dashboard.UID != uid {
			continue
		}
		document := dashboard.Document
		if len(document) == 0 {
			document, _ = json.Marshal(map[string]any{"title": dashboard.Title})
		}
		simkit.WriteJSON(w, http.StatusOK, map[string]any{
			"dashboard": document, "meta": map[string]any{"folderTitle": dashboard.Folder},
		})
		return
	}
	simkit.WriteJSON(w, http.StatusNotFound, map[string]string{"message": "Dashboard not found"})
}

func moment(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339, value)
	return parsed
}

func unixSeconds(at time.Time) float64 { return float64(at.UnixMilli()) / 1000 }

func formatNumber(value float64) string { return strconv.FormatFloat(value, 'f', -1, 64) }

func (s *Server) instant(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	series, found := s.metric(query.Get("query"))
	if !found {
		refuseQuery(w, query.Get("query"))
		return
	}
	at, err := strconv.ParseFloat(query.Get("time"), 64)
	if err != nil {
		at = unixSeconds(time.Now())
	}
	result := []map[string]any{}
	for _, one := range series {
		result = append(result, map[string]any{"metric": one.Metric, "value": []any{at, formatNumber(one.Value)}})
	}
	simkit.WriteJSON(w, http.StatusOK, map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
}

func (s *Server) rangeQuery(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	series, found := s.metric(query.Get("query"))
	if !found {
		refuseQuery(w, query.Get("query"))
		return
	}
	simkit.WriteJSON(w, http.StatusOK, map[string]any{"status": "success", "data": map[string]any{
		"resultType": "matrix", "result": matrix(series, seconds(query.Get("start")), seconds(query.Get("end"))),
	}})
}

func seconds(value string) time.Time {
	number, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return time.Time{}
	}
	return time.UnixMilli(int64(number * 1000))
}

func matrix(series []Series, start, end time.Time) []map[string]any {
	result := []map[string]any{}
	for _, one := range series {
		values := [][]any{}
		for _, point := range one.Values {
			at := moment(point.Time)
			if (!start.IsZero() && at.Before(start)) || (!end.IsZero() && at.After(end)) {
				continue
			}
			values = append(values, []any{unixSeconds(at), formatNumber(point.Value)})
		}
		result = append(result, map[string]any{"metric": one.Metric, "values": values})
	}
	return result
}

func (s *Server) metric(query string) ([]Series, bool) {
	for _, entry := range s.seed.Metrics {
		if entry.Query == query || (entry.Contains != "" && strings.Contains(query, entry.Contains)) {
			return entry.Series, true
		}
	}
	return nil, false
}

func refuseQuery(w http.ResponseWriter, query string) {
	simkit.WriteJSON(w, http.StatusBadRequest, map[string]string{
		"status": "error", "errorType": "bad_data",
		"error": fmt.Sprintf("invalid parameter \"query\": 1:1: parse error: no data is seeded for %q", query),
	})
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	var entry *Log
	for index := range s.seed.Logs {
		candidate := &s.seed.Logs[index]
		if candidate.Query == query.Get("query") || (candidate.Contains != "" && strings.Contains(query.Get("query"), candidate.Contains)) {
			entry = candidate
			break
		}
	}
	if entry == nil {
		http.Error(w, "parse error at line 1, col 1: no data is seeded for this query", http.StatusBadRequest)
		return
	}
	start, end := nanoseconds(query.Get("start")), nanoseconds(query.Get("end"))
	if len(entry.Series) > 0 {
		simkit.WriteJSON(w, http.StatusOK, map[string]any{"status": "success", "data": map[string]any{
			"resultType": "matrix", "result": matrix(entry.Series, start, end),
		}})
		return
	}
	limit, err := strconv.Atoi(query.Get("limit"))
	if err != nil || limit < 1 {
		limit = 100
	}
	type line struct {
		at   time.Time
		text string
	}
	streams := []map[string]any{}
	budget := limit
	for _, stream := range entry.Streams {
		var lines []line
		for _, item := range stream.Lines {
			at := moment(item.Time)
			if (!start.IsZero() && at.Before(start)) || (!end.IsZero() && at.After(end)) {
				continue
			}
			lines = append(lines, line{at, item.Line})
		}
		slices.SortStableFunc(lines, func(a, b line) int {
			if query.Get("direction") == "forward" {
				return a.at.Compare(b.at)
			}
			return b.at.Compare(a.at)
		})
		lines = lines[:min(len(lines), budget)]
		budget -= len(lines)
		values := [][2]string{}
		for _, item := range lines {
			values = append(values, [2]string{strconv.FormatInt(item.at.UnixNano(), 10), item.text})
		}
		if len(values) > 0 {
			streams = append(streams, map[string]any{"stream": stream.Labels, "values": values})
		}
	}
	simkit.WriteJSON(w, http.StatusOK, map[string]any{"status": "success", "data": map[string]any{"resultType": "streams", "result": streams}})
}

func nanoseconds(value string) time.Time {
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(0, number)
}

func (s *Server) rules(w http.ResponseWriter, _ *http.Request) {
	rules := []map[string]any{}
	for _, rule := range s.seed.Rules {
		instances := []map[string]any{}
		for _, instance := range rule.Instances {
			instances = append(instances, map[string]any{
				"labels": instance.Labels, "annotations": instance.Annotations, "state": instance.State, "activeAt": instance.ActiveAt,
			})
		}
		rules = append(rules, map[string]any{
			"name": rule.Name, "health": cmp.Or(rule.Health, "ok"), "lastError": rule.LastError, "alerts": instances,
		})
	}
	simkit.WriteJSON(w, http.StatusOK, map[string]any{"status": "success", "data": map[string]any{
		"groups": []map[string]any{{"name": "rules", "rules": rules}},
	}})
}

func (s *Server) annotations(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	from, _ := strconv.ParseInt(query.Get("from"), 10, 64)
	to, _ := strconv.ParseInt(query.Get("to"), 10, 64)
	limit, err := strconv.Atoi(query.Get("limit"))
	if err != nil || limit < 1 {
		limit = 100
	}
	found := []map[string]any{}
	for _, item := range s.seed.Annotations {
		at := moment(item.Time).UnixMilli()
		if (from != 0 && at < from) || (to != 0 && at > to) {
			continue
		}
		found = append(found, map[string]any{
			"alertName": item.AlertName, "prevState": item.PrevState, "newState": item.NewState, "time": at, "tags": item.Tags,
		})
	}
	simkit.WriteJSON(w, http.StatusOK, found[:min(len(found), limit)])
}
