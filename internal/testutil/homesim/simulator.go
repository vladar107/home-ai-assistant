// Package homesim is a loopback fake of the Home Assistant server the home connector talks to: its
// MCP endpoint (a tool list and tool calls), the REST states, config and services endpoints, and
// the WebSocket commands for the label and entity registries and long-term statistics. A case
// invents the devices: entities with states, areas, labels and statistics, plus the MCP tools and
// the answer each returns. Nothing here is a real household's device.
//
// Exposed entities appear in the live-context tool the fake always offers, as Assist's exposure
// list does; the connector's own exposure and confirmation-label guard then runs unchanged, so an
// exposed lock withholds home actions exactly as in production. A tool call can change entity
// states through "effects", and every tool and service call is recorded for a case's checks.
//
// Example seed (invented devices):
//
//	{
//	  "time_zone": "Europe/Amsterdam",
//	  "entities": [
//	    {"entity_id": "light.study_lamp", "name": "Study lamp", "state": "off", "area": "Study", "exposed": true},
//	    {"entity_id": "sensor.hall_temperature", "name": "Hall temperature", "state": "21.4", "unit": "°C",
//	      "device_class": "temperature", "area": "Hall", "exposed": true,
//	      "statistics": [{"start": "2026-03-09T00:00:00Z", "mean": 20.5, "min": 19, "max": 22},
//	                     {"start": "2026-03-10T00:00:00Z", "mean": 21.2, "min": 20, "max": 23}]},
//	    {"entity_id": "vacuum.hall_robot", "name": "Hall robot", "state": "docked", "area": "Hall", "exposed": true,
//	      "attributes": {"schedule": "Mon 09:00; Thu 09:00"}},
//	    {"entity_id": "lock.garden_gate", "name": "Garden gate", "state": "locked", "labels": ["assistant-confirm"]}
//	  ],
//	  "tools": [
//	    {"name": "intent__HassTurnOn", "description": "Turns on a device or entity.",
//	      "input_schema": {"type": "object", "properties": {"name": {"type": "string"}}},
//	      "answer": "Turned on.", "effects": [{"entity_id": "light.study_lamp", "state": "on"}]}
//	  ],
//	  "services": [{"domain": "lock", "services": {"unlock": []}}]
//	}
package homesim

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"github.com/vladar107/home-ai-assistant/internal/testutil/simkit"
)

const (
	// Token is the bearer token the fake accepts when the seed names none.
	Token = "sim-home-token"

	liveContextTool = "homeassistant__GetLiveContext"
)

type Seed struct {
	// Token overrides Token.
	Token string `json:"token"`
	// TimeZone is Home Assistant's own zone; default UTC.
	TimeZone string    `json:"time_zone"`
	Entities []Entity  `json:"entities"`
	Tools    []Tool    `json:"tools"`
	Services []Service `json:"services"`
}

type Entity struct {
	EntityID    string         `json:"entity_id"`
	Name        string         `json:"name"`
	State       string         `json:"state"`
	Unit        string         `json:"unit"`
	DeviceClass string         `json:"device_class"`
	Area        string         `json:"area"`
	Attributes  map[string]any `json:"attributes"`
	// Exposed lists the entity in the live context, as exposing it to Assist does.
	Exposed bool `json:"exposed"`
	// Labels are label names, such as assistant-confirm; each becomes a registry label.
	Labels []string `json:"labels"`
	// Statistics are the entity's long-term statistics, one point per hour or day as the case likes;
	// queries aggregate them into buckets.
	Statistics []Statistic `json:"statistics"`
}

type Statistic struct {
	Start  string   `json:"start"`
	Mean   *float64 `json:"mean"`
	Min    *float64 `json:"min"`
	Max    *float64 `json:"max"`
	Change *float64 `json:"change"`
	State  *float64 `json:"state"`
}

// Tool is an MCP tool the server lists. Its answer is the text a call returns; effects set entity
// states when it is called.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Answer      string          `json:"answer"`
	IsError     bool            `json:"is_error"`
	Effects     []Effect        `json:"effects"`
}

type Effect struct {
	EntityID string `json:"entity_id"`
	State    string `json:"state"`
}

// Service is a domain's services with the field names each accepts.
type Service struct {
	Domain   string              `json:"domain"`
	Services map[string][]string `json:"services"`
}

// Call is one recorded tool or service call.
type Call struct {
	// Tool is an MCP tool name, or "service:<domain>.<service>" for a REST service call.
	Tool      string
	Arguments json.RawMessage
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
	if seed.TimeZone != "" {
		if _, err := time.LoadLocation(seed.TimeZone); err != nil {
			return seed, fmt.Errorf("time_zone %q is not a time zone", seed.TimeZone)
		}
	}
	ids := map[string]bool{}
	for index, entity := range seed.Entities {
		if !strings.Contains(entity.EntityID, ".") || entity.Name == "" {
			return seed, fmt.Errorf("entities[%d] needs an entity_id such as light.lamp and a name", index)
		}
		if ids[entity.EntityID] {
			return seed, fmt.Errorf("entities[%d] repeats %s", index, entity.EntityID)
		}
		ids[entity.EntityID] = true
		for _, point := range entity.Statistics {
			if _, err := time.Parse(time.RFC3339, point.Start); err != nil {
				return seed, fmt.Errorf("entities[%d] statistics start must be RFC 3339", index)
			}
		}
	}
	names := map[string]bool{}
	for index, tool := range seed.Tools {
		if tool.Name == "" || names[tool.Name] {
			return seed, fmt.Errorf("tools[%d] needs a unique name", index)
		}
		names[tool.Name] = true
		if len(tool.InputSchema) > 0 {
			if err := simkit.Object(fmt.Sprintf("tools[%d].input_schema", index), tool.InputSchema); err != nil {
				return seed, err
			}
		}
		for _, effect := range tool.Effects {
			if !ids[effect.EntityID] {
				return seed, fmt.Errorf("tools[%d] has an effect on unknown entity %s", index, effect.EntityID)
			}
		}
	}
	return seed, nil
}

// Server is the running fake.
type Server struct {
	*simkit.Server
	token    string
	location *time.Location
	now      func() time.Time

	mu     sync.Mutex
	seed   Seed
	states map[string]string
	calls  []Call
	tools  []Tool
}

// Start runs the fake on a loopback port, with calendar periods counted from the wall clock; the
// caller closes it.
func Start(raw json.RawMessage) (*Server, error) { return StartAt(raw, time.Now) }

// StartAt is Start with the clock that "today", "this week" and rolling periods count from.
func StartAt(raw json.RawMessage, now func() time.Time) (*Server, error) {
	seed, err := parse(raw)
	if err != nil {
		return nil, err
	}
	location := time.UTC
	if seed.TimeZone != "" {
		location, _ = time.LoadLocation(seed.TimeZone)
	}
	s := &Server{seed: seed, token: cmp.Or(seed.Token, Token), location: location, now: now, states: map[string]string{}}
	for _, entity := range seed.Entities {
		s.states[entity.EntityID] = entity.State
	}
	s.tools = slices.Clone(seed.Tools)
	if !slices.ContainsFunc(s.tools, func(tool Tool) bool { return tool.Name == liveContextTool }) {
		s.tools = append(s.tools, Tool{
			Name:        liveContextTool,
			Description: "Provides real-time information about the exposed entities.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"area":{"type":"string"}}}`),
		})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/mcp", s.authorized(s.mcp))
	mux.HandleFunc("/api/states", s.authorized(s.listStates))
	mux.HandleFunc("/api/states/", s.authorized(s.oneState))
	mux.HandleFunc("/api/config", s.authorized(s.config))
	mux.HandleFunc("/api/services", s.authorized(s.listServices))
	mux.HandleFunc("/api/services/", s.authorized(s.callService))
	mux.Handle("/api/websocket", websocket.Handler(s.socket))
	inner, err := simkit.Serve(mux)
	if err != nil {
		return nil, err
	}
	s.Server = inner
	return s, nil
}

// New runs the fake for the length of a test, with now as its clock.
func New(t testing.TB, raw json.RawMessage, now func() time.Time) *Server {
	t.Helper()
	s, err := StartAt(raw, now)
	if err != nil {
		t.Fatal(err)
	}
	s.Cleanup(t)
	return s
}

// Token is the bearer token the fake accepts.
func (s *Server) Token() string { return s.token }

// Calls are the tool and service calls received so far, oldest first.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// State is an entity's current state, and whether the entity exists.
func (s *Server) State(entityID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, found := s.states[entityID]
	return state, found
}

func (s *Server) authorized(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+s.token {
			http.Error(w, "401: Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (e Entity) friendly() map[string]any {
	attributes := map[string]any{"friendly_name": e.Name}
	if e.Unit != "" {
		attributes["unit_of_measurement"] = e.Unit
	}
	if e.DeviceClass != "" {
		attributes["device_class"] = e.DeviceClass
	}
	for key, value := range e.Attributes {
		attributes[key] = value
	}
	return attributes
}

func (s *Server) stateObject(entity Entity) map[string]any {
	return map[string]any{
		"entity_id": entity.EntityID, "state": s.states[entity.EntityID], "attributes": entity.friendly(),
		"last_changed": "2026-01-01T00:00:00+00:00",
	}
}

func (s *Server) listStates(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	states := make([]map[string]any, 0, len(s.seed.Entities))
	for _, entity := range s.seed.Entities {
		states = append(states, s.stateObject(entity))
	}
	simkit.WriteJSON(w, http.StatusOK, states)
}

func (s *Server) oneState(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/states/")
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entity := range s.seed.Entities {
		if entity.EntityID == id {
			simkit.WriteJSON(w, http.StatusOK, s.stateObject(entity))
			return
		}
	}
	simkit.WriteJSON(w, http.StatusNotFound, map[string]string{"message": "Entity not found."})
}

func (s *Server) config(w http.ResponseWriter, _ *http.Request) {
	simkit.WriteJSON(w, http.StatusOK, map[string]string{"time_zone": s.location.String()})
}

func (s *Server) listServices(w http.ResponseWriter, _ *http.Request) {
	listed := []map[string]any{}
	for _, domain := range s.seed.Services {
		services := map[string]any{}
		for name, fields := range domain.Services {
			described := map[string]any{}
			for _, field := range fields {
				described[field] = map[string]any{}
			}
			services[name] = map[string]any{"fields": described}
		}
		listed = append(listed, map[string]any{"domain": domain.Domain, "services": services})
	}
	simkit.WriteJSON(w, http.StatusOK, listed)
}

func (s *Server) callService(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/services/")
	domain, service, found := strings.Cut(rest, "/")
	known := false
	for _, entry := range s.seed.Services {
		if _, has := entry.Services[service]; entry.Domain == domain && has {
			known = true
		}
	}
	if !found || !known || r.Method != http.MethodPost {
		simkit.WriteJSON(w, http.StatusNotFound, map[string]string{"message": "Service not found."})
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	s.mu.Lock()
	s.calls = append(s.calls, Call{Tool: "service:" + domain + "." + service, Arguments: body})
	s.mu.Unlock()
	simkit.WriteJSON(w, http.StatusOK, []any{})
}

func (s *Server) mcp(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if r.Method != http.MethodPost || json.Unmarshal(body, &request) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	reply := func(result any, code int, message string) {
		answer := map[string]any{"jsonrpc": "2.0", "id": request.ID}
		if message != "" {
			answer["error"] = map[string]any{"code": code, "message": message}
		} else {
			answer["result"] = result
		}
		simkit.WriteJSON(w, http.StatusOK, answer)
	}
	switch request.Method {
	case "tools/list":
		listed := []map[string]any{}
		for _, tool := range s.tools {
			schema := tool.InputSchema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			listed = append(listed, map[string]any{"name": tool.Name, "description": tool.Description, "inputSchema": schema})
		}
		reply(map[string]any{"tools": listed}, 0, "")
	case "tools/call":
		index := slices.IndexFunc(s.tools, func(tool Tool) bool { return tool.Name == request.Params.Name })
		if index < 0 {
			reply(nil, -32602, "Unknown tool")
			return
		}
		tool := s.tools[index]
		s.mu.Lock()
		s.calls = append(s.calls, Call{Tool: tool.Name, Arguments: request.Params.Arguments})
		for _, effect := range tool.Effects {
			s.states[effect.EntityID] = effect.State
		}
		answer := tool.Answer
		if tool.Name == liveContextTool && answer == "" {
			answer = s.liveContext()
		}
		s.mu.Unlock()
		reply(map[string]any{"content": []map[string]string{{"type": "text", "text": answer}}, "isError": tool.IsError}, 0, "")
	default:
		reply(nil, -32601, "Method not found")
	}
}

// liveContext writes the exposed entities in the overview format Home Assistant's own tool uses.
func (s *Server) liveContext() string {
	var builder strings.Builder
	builder.WriteString("Live Context: An overview of the areas and the devices in this smart home:\n")
	for _, entity := range s.seed.Entities {
		if !entity.Exposed {
			continue
		}
		domain, _, _ := strings.Cut(entity.EntityID, ".")
		fmt.Fprintf(&builder, "- names: %s\n  domain: %s\n  state: '%s'\n", entity.Name, domain, s.states[entity.EntityID])
		if entity.Area != "" {
			fmt.Fprintf(&builder, "  areas: %s\n", entity.Area)
		}
		attributes := map[string]any{}
		if entity.Unit != "" {
			attributes["unit_of_measurement"] = entity.Unit
		}
		if entity.DeviceClass != "" {
			attributes["device_class"] = entity.DeviceClass
		}
		for key, value := range entity.Attributes {
			attributes[key] = value
		}
		if len(attributes) > 0 {
			builder.WriteString("  attributes:\n")
			keys := make([]string, 0, len(attributes))
			for key := range attributes {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				fmt.Fprintf(&builder, "    %s: '%v'\n", key, attributes[key])
			}
		}
	}
	return builder.String()
}

type socketCommand struct {
	ID          int      `json:"id"`
	Type        string   `json:"type"`
	AccessToken string   `json:"access_token"`
	StatisticID string   `json:"statistic_id"`
	IDs         []string `json:"statistic_ids"`
	Period      string   `json:"period"`
	StartTime   string   `json:"start_time"`
	EndTime     string   `json:"end_time"`
	Calendar    *struct {
		Period string `json:"period"`
		Offset int    `json:"offset"`
	} `json:"calendar"`
	Rolling *struct {
		Duration struct {
			Days int `json:"days"`
		} `json:"duration"`
	} `json:"rolling_window"`
}

func (s *Server) socket(conn *websocket.Conn) {
	if websocket.JSON.Send(conn, map[string]string{"type": "auth_required"}) != nil {
		return
	}
	var auth socketCommand
	if websocket.JSON.Receive(conn, &auth) != nil {
		return
	}
	if auth.AccessToken != s.token {
		_ = websocket.JSON.Send(conn, map[string]string{"type": "auth_invalid"})
		return
	}
	if websocket.JSON.Send(conn, map[string]string{"type": "auth_ok"}) != nil {
		return
	}
	for {
		var command socketCommand
		if websocket.JSON.Receive(conn, &command) != nil {
			return
		}
		result, ok := s.command(command)
		answer := map[string]any{"id": command.ID, "type": "result", "success": ok}
		if ok {
			answer["result"] = result
		} else {
			answer["error"] = map[string]string{"code": "unknown_command", "message": "Unknown command."}
		}
		if websocket.JSON.Send(conn, answer) != nil {
			return
		}
	}
}

func labelID(name string) string { return strings.ReplaceAll(name, "-", "_") }

func (s *Server) command(command socketCommand) (any, bool) {
	switch command.Type {
	case "config/label_registry/list":
		seen := map[string]bool{}
		labels := []map[string]string{}
		for _, entity := range s.seed.Entities {
			for _, name := range entity.Labels {
				if !seen[name] {
					seen[name] = true
					labels = append(labels, map[string]string{"label_id": labelID(name), "name": name})
				}
			}
		}
		return labels, true
	case "config/entity_registry/list_for_display":
		entities := []map[string]any{}
		for _, entity := range s.seed.Entities {
			ids := []string{}
			for _, name := range entity.Labels {
				ids = append(ids, labelID(name))
			}
			entities = append(entities, map[string]any{"ei": entity.EntityID, "lb": ids})
		}
		return map[string]any{"entities": entities}, true
	case "recorder/statistic_during_period":
		return s.periodStatistic(command)
	case "recorder/statistics_during_period":
		return s.bucketStatistics(command)
	}
	return nil, false
}

func (s *Server) statisticsOf(id string) []Statistic {
	for _, entity := range s.seed.Entities {
		if entity.EntityID == id {
			return entity.Statistics
		}
	}
	return nil
}

func floor(at time.Time, unit string) time.Time {
	day := time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location())
	switch unit {
	case "week":
		return day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
	case "month":
		return time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, at.Location())
	case "year":
		return time.Date(at.Year(), 1, 1, 0, 0, 0, 0, at.Location())
	}
	return day
}

func advance(at time.Time, unit string, count int) time.Time {
	switch unit {
	case "week":
		return at.AddDate(0, 0, 7*count)
	case "month":
		return at.AddDate(0, count, 0)
	case "year":
		return at.AddDate(count, 0, 0)
	}
	return at.AddDate(0, 0, count)
}

// periodStatistic aggregates a statistic over a calendar or rolling period counted from the
// fake's clock; without a period it covers every point.
func (s *Server) periodStatistic(command socketCommand) (any, bool) {
	start, end := time.Time{}, time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	now := s.now().In(s.location)
	switch {
	case command.Calendar != nil:
		start = advance(floor(now, command.Calendar.Period), command.Calendar.Period, command.Calendar.Offset)
		end = advance(start, command.Calendar.Period, 1)
	case command.Rolling != nil:
		start, end = now.AddDate(0, 0, -command.Rolling.Duration.Days), now
	}
	result := map[string]*float64{"change": nil, "mean": nil, "min": nil, "max": nil}
	var means []float64
	var change float64
	var changed bool
	for _, point := range s.statisticsOf(command.StatisticID) {
		at, _ := time.Parse(time.RFC3339, point.Start)
		if at.Before(start) || !at.Before(end) {
			continue
		}
		if point.Mean != nil {
			means = append(means, *point.Mean)
		}
		if point.Change != nil {
			change += *point.Change
			changed = true
		}
		if point.Min != nil && (result["min"] == nil || *point.Min < *result["min"]) {
			value := *point.Min
			result["min"] = &value
		}
		if point.Max != nil && (result["max"] == nil || *point.Max > *result["max"]) {
			value := *point.Max
			result["max"] = &value
		}
	}
	if len(means) > 0 {
		sum := 0.0
		for _, mean := range means {
			sum += mean
		}
		value := math.Round(sum/float64(len(means))*1000) / 1000
		result["mean"] = &value
	}
	if changed {
		result["change"] = &change
	}
	return result, true
}

// bucketStatistics groups a statistic's points into day, week or month buckets over a time range.
func (s *Server) bucketStatistics(command socketCommand) (any, bool) {
	start, startErr := time.Parse(time.RFC3339, command.StartTime)
	end, endErr := time.Parse(time.RFC3339, command.EndTime)
	if startErr != nil || endErr != nil || !slices.Contains([]string{"day", "week", "month"}, command.Period) {
		return nil, false
	}
	result := map[string][]map[string]any{}
	for _, id := range command.IDs {
		type bucket struct {
			means  []float64
			change *float64
			state  *float64
		}
		buckets := map[time.Time]*bucket{}
		for _, point := range s.statisticsOf(id) {
			at, _ := time.Parse(time.RFC3339, point.Start)
			if at.Before(start) || !at.Before(end) {
				continue
			}
			key := floor(at.In(s.location), command.Period)
			current := buckets[key]
			if current == nil {
				current = &bucket{}
				buckets[key] = current
			}
			if point.Mean != nil {
				current.means = append(current.means, *point.Mean)
			}
			if point.Change != nil {
				total := *point.Change
				if current.change != nil {
					total += *current.change
				}
				current.change = &total
			}
			if point.State != nil {
				value := *point.State
				current.state = &value
			}
		}
		starts := make([]time.Time, 0, len(buckets))
		for key := range buckets {
			starts = append(starts, key)
		}
		slices.SortFunc(starts, func(a, b time.Time) int { return a.Compare(b) })
		points := []map[string]any{}
		for _, key := range starts {
			current := buckets[key]
			entry := map[string]any{"start": float64(key.UnixMilli()), "mean": nil, "change": current.change, "state": current.state}
			if len(current.means) > 0 {
				sum := 0.0
				for _, mean := range current.means {
					sum += mean
				}
				entry["mean"] = sum / float64(len(current.means))
			}
			points = append(points, entry)
		}
		result[id] = points
	}
	return result, true
}
