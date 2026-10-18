package homeassistant

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/vladar107/home-ai-assistant/internal/httpretry"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	snapshotLifetime = 5 * time.Minute
	// staleSnapshotLimit is how long a snapshot keeps serving after refreshes start failing, so a
	// Home Assistant restart does not take the tools away mid-conversation.
	staleSnapshotLimit = 30 * time.Minute
	refreshTimeout     = 20 * time.Second
	// missRefreshInterval bounds how often a name miss may refresh the snapshot.
	missRefreshInterval = 30 * time.Second

	liveContextTool = "homeassistant__GetLiveContext"
	// ConfirmLabel marks an entity whose actions always need the user's confirmation; it must not
	// be exposed to Assist, where actions run at once.
	ConfirmLabel = "assistant-confirm"
	// ReadLabel marks a script that only reads, so the assistant treats it as a read: no
	// confirmation, and available to scheduled runs. A script that also carries ConfirmLabel stays
	// a write. Unlike ConfirmLabel it matches the label's name only, because it grants rather than
	// restricts, and a renamed label keeps its id.
	ReadLabel = "assistant-read"
)

// droppedTools are Home Assistant tools the application answers better itself: every turn already
// carries the local time.
var droppedTools = map[string]bool{"llm__GetDateTime": true}

// sensitiveDomains count as needing confirmation even without the label, so exposing one of them
// to Assist withholds every action instead of letting it run unconfirmed.
var sensitiveDomains = map[string]bool{
	"alarm_control_panel": true,
	"camera":              true,
	"lock":                true,
	"siren":               true,
	"update":              true,
	"valve":               true,
}

var sensitiveCoverClasses = map[string]bool{"door": true, "garage": true, "gate": true}

const writeSuffix = " Runs at once on the household's devices."

var toolNameUnsafe = regexp.MustCompile(`[^a-z0-9_]+`)

// mappedTool is a Home Assistant MCP tool as the broker offers it.
type mappedTool struct {
	definition tools.Definition
	remote     string
}

// exposedEntity is one entry of the live context: what Assist, and so the MCP tools, can reach.
type exposedEntity struct {
	names       []string
	domain      string
	deviceClass string
}

// confirmedEntity is an entity labelled for confirmation, reachable only through proposals.
type confirmedEntity struct {
	entityID string
	name     string
	domain   string
	state    string
}

type snapshot struct {
	fetched time.Time
	tools   []mappedTool
	byName  map[string]mappedTool
	// guarded reports that the exposure check ran; without it no action is offered.
	guarded bool
	// conflicts names exposed entities that need confirmation; while there are any, the MCP
	// action tools are withheld.
	conflicts []string
	exposed   []exposedEntity
	// location is Home Assistant's own time zone; UTC when it did not report one.
	location  *time.Location
	confirmed map[string]confirmedEntity
	// entityIDs maps a lowercased friendly name to the entities that carry it.
	entityIDs map[string][]string

	visibleOnce sync.Once
	visibleIDs  map[string]string
}

func (s *snapshot) actionsAllowed() bool {
	return s.guarded && len(s.conflicts) == 0
}

// current returns a snapshot no older than snapshotLifetime, refreshing it under the lock; a
// failed refresh keeps serving the previous one until it is staleSnapshotLimit old.
func (s *Service) current(ctx context.Context) (*snapshot, error) {
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	now := s.now()
	if s.snapshot != nil && now.Sub(s.snapshot.fetched) < snapshotLifetime {
		return s.snapshot, nil
	}
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()
	fresh, err := s.refresh(refreshCtx, now)
	if err != nil {
		s.logger.WarnContext(ctx, "homeassistant.refresh.failed", "error", err)
		if s.snapshot != nil && now.Sub(s.snapshot.fetched) < staleSnapshotLimit {
			return s.snapshot, nil
		}
		return nil, err
	}
	if len(fresh.conflicts) > 0 && (s.snapshot == nil || !slices.Equal(s.snapshot.conflicts, fresh.conflicts)) {
		s.logger.WarnContext(ctx, "homeassistant.exposure.conflict", "entities", len(fresh.conflicts))
	}
	s.snapshot = fresh
	return fresh, nil
}

// refreshOnMiss refreshes the snapshot for a lookup that found nothing, unless it was fetched under
// missRefreshInterval ago, so a newly exposed entity works at once and concurrent misses refresh
// once. A failed refresh returns the snapshot the caller already had.
func (s *Service) refreshOnMiss(ctx context.Context, seen *snapshot) *snapshot {
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	now := s.now()
	if s.snapshot != nil && now.Sub(s.snapshot.fetched) < missRefreshInterval {
		return s.snapshot
	}
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	defer cancel()
	fresh, err := s.refresh(refreshCtx, now)
	if err != nil {
		s.logger.WarnContext(ctx, "homeassistant.refresh.failed", "error", err)
		return seen
	}
	s.snapshot = fresh
	return fresh
}

// forget drops the snapshot so the next call refreshes it, after an action may have changed what
// the live context reports.
func (s *Service) forget() {
	s.snapshotMu.Lock()
	defer s.snapshotMu.Unlock()
	s.snapshot = nil
}

func (s *Service) refresh(ctx context.Context, now time.Time) (*snapshot, error) {
	var listed struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := s.client.mcp(ctx, "tools/list", nil, &listed); err != nil {
		return nil, err
	}
	fresh := &snapshot{fetched: now, byName: make(map[string]mappedTool)}
	sort.Slice(listed.Tools, func(i, j int) bool { return listed.Tools[i].Name < listed.Tools[j].Name })
	for _, remote := range listed.Tools {
		if droppedTools[remote.Name] {
			continue
		}
		mapped, ok := mapTool(remote.Name, remote.Description, remote.InputSchema)
		if !ok {
			s.logger.WarnContext(ctx, "homeassistant.tool.skipped", "reason", "unusable_definition")
			continue
		}
		if _, taken := fresh.byName[mapped.definition.Name]; taken {
			mapped.definition.Name = toolName(domainOf(remote.Name) + "_" + intentOf(remote.Name))
			if _, taken := fresh.byName[mapped.definition.Name]; taken {
				s.logger.WarnContext(ctx, "homeassistant.tool.skipped", "reason", "duplicate_name")
				continue
			}
		}
		fresh.tools = append(fresh.tools, mapped)
		fresh.byName[mapped.definition.Name] = mapped
	}

	var states []stateObject
	if err := s.client.getJSON(ctx, "/api/states", &states); err != nil {
		return nil, err
	}
	fresh.entityIDs = make(map[string][]string)
	byID := make(map[string]stateObject, len(states))
	for _, state := range states {
		byID[state.EntityID] = state
		if name := strings.ToLower(state.friendlyName()); name != "" {
			fresh.entityIDs[name] = append(fresh.entityIDs[name], state.EntityID)
		}
	}

	// A failed config read keeps the zone already known, so periods do not shift to UTC for one
	// refresh.
	fresh.location = time.UTC
	if s.snapshot != nil && s.snapshot.location != nil {
		fresh.location = s.snapshot.location
	}
	var config struct {
		TimeZone string `json:"time_zone"`
	}
	if err := s.client.getJSON(ctx, "/api/config", &config); err == nil && config.TimeZone != "" {
		if location, err := time.LoadLocation(config.TimeZone); err == nil {
			fresh.location = location
		}
	}

	exposed, guardErr := s.liveContext(ctx)
	confirmed, readOnly, labelErr := s.labelledEntities(ctx, byID)
	fresh.exposed = exposed
	fresh.confirmed = confirmed
	if guardErr == nil && labelErr == nil {
		fresh.markReadScripts(readOnly)
	}
	if guardErr != nil || labelErr != nil {
		s.logger.WarnContext(ctx, "homeassistant.guard.failed", "live_context", guardErr != nil, "labels", labelErr != nil)
		return fresh, nil
	}
	fresh.guarded = true
	fresh.conflicts = conflicts(exposed, confirmed)
	return fresh, nil
}

type stateObject struct {
	EntityID    string         `json:"entity_id"`
	State       string         `json:"state"`
	LastChanged string         `json:"last_changed"`
	Attributes  map[string]any `json:"attributes"`
}

func (s stateObject) friendlyName() string {
	name, _ := s.Attributes["friendly_name"].(string)
	return strings.TrimSpace(name)
}

func (s stateObject) unit() string {
	unit, _ := s.Attributes["unit_of_measurement"].(string)
	return unit
}

// options are the states an enum sensor declares it can report, empty when it declares none.
func (s stateObject) options() []string {
	listed, _ := s.Attributes["options"].([]any)
	options := make([]string, 0, len(listed))
	for _, option := range listed {
		if text, ok := option.(string); ok && text != "" {
			options = append(options, text)
		}
	}
	return options
}

// liveContext asks the MCP server for every exposed entity. A non-admin token cannot read the
// exposure settings, but this tool lists exactly what Assist exposes.
func (s *Service) liveContext(ctx context.Context) ([]exposedEntity, error) {
	text, isError, err := s.callTool(httpretry.Idempotent(ctx), liveContextTool, map[string]any{})
	if err != nil {
		return nil, err
	}
	if isError {
		return nil, errBadRequest
	}
	return parseLiveContext(text), nil
}

// parseLiveContext reads the YAML-like overview: an entry starts with "- names:" and carries
// "domain:" and, under attributes, "device_class:".
func parseLiveContext(text string) []exposedEntity {
	var entities []exposedEntity
	var current *exposedEntity
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "- names:"):
			entities = append(entities, exposedEntity{})
			current = &entities[len(entities)-1]
			for _, name := range strings.Split(yamlValue(strings.TrimPrefix(trimmed, "- names:")), ",") {
				if name = strings.TrimSpace(name); name != "" {
					current.names = append(current.names, name)
				}
			}
		case current == nil:
		case strings.HasPrefix(trimmed, "domain:"):
			current.domain = yamlValue(strings.TrimPrefix(trimmed, "domain:"))
		case strings.HasPrefix(trimmed, "device_class:"):
			current.deviceClass = yamlValue(strings.TrimPrefix(trimmed, "device_class:"))
		}
	}
	return entities
}

func yamlValue(raw string) string {
	value := strings.TrimSpace(raw)
	if len(value) >= 2 && (value[0] == '\'' || value[0] == '"') && value[len(value)-1] == value[0] {
		value = value[1 : len(value)-1]
	}
	return value
}

type registryLabel struct {
	LabelID string `json:"label_id"`
	Name    string `json:"name"`
}

// markReadScripts turns the scripts labelled ReadLabel, and not ConfirmLabel, into reads.
func (s *snapshot) markReadScripts(readOnly map[string]bool) {
	for index, tool := range s.tools {
		if domainOf(tool.remote) != "script" || !readOnly["script."+intentOf(tool.remote)] {
			continue
		}
		tool.definition.Risk = tools.RiskRead
		tool.definition.Description = strings.TrimSuffix(tool.definition.Description, writeSuffix)
		s.tools[index] = tool
		s.byName[tool.definition.Name] = tool
	}
}

// labelledEntities reads the label and entity registries, which a non-admin token may read, and
// returns the entities carrying ConfirmLabel and the ids of those carrying ReadLabel without it.
func (s *Service) labelledEntities(ctx context.Context, states map[string]stateObject) (map[string]confirmedEntity, map[string]bool, error) {
	results, err := s.client.websocketCommands(ctx,
		map[string]any{"type": "config/label_registry/list"},
		map[string]any{"type": "config/entity_registry/list_for_display"},
	)
	if err != nil {
		return nil, nil, err
	}
	var labels []registryLabel
	var registry struct {
		Entities []struct {
			EntityID string   `json:"ei"`
			Labels   []string `json:"lb"`
		} `json:"entities"`
	}
	if json.Unmarshal(results[0], &labels) != nil || json.Unmarshal(results[1], &registry) != nil {
		return nil, nil, errMalformed
	}
	confirmLabels := matchingLabels(labels, ConfirmLabel)
	readLabels := make(map[string]bool)
	for _, label := range labels {
		if strings.EqualFold(label.Name, ReadLabel) {
			readLabels[label.LabelID] = true
		}
	}
	confirmed := make(map[string]confirmedEntity)
	readOnly := make(map[string]bool)
	for _, entity := range registry.Entities {
		// Both labels are judged from the registry alone, so an entity missing from the states
		// cannot slip past the confirmation label.
		confirm := slices.ContainsFunc(entity.Labels, func(id string) bool { return confirmLabels[id] })
		if !confirm && slices.ContainsFunc(entity.Labels, func(id string) bool { return readLabels[id] }) {
			readOnly[entity.EntityID] = true
		}
		if !confirm {
			continue
		}
		state, found := states[entity.EntityID]
		if !found {
			continue
		}
		name := state.friendlyName()
		if name == "" {
			name = entity.EntityID
		}
		confirmed[entity.EntityID] = confirmedEntity{entityID: entity.EntityID, name: name, domain: domainOfEntity(entity.EntityID), state: state.State}
	}
	return confirmed, readOnly, nil
}

// matchingLabels returns the ids of the labels named like want, by name or id, with - or _.
func matchingLabels(labels []registryLabel, want string) map[string]bool {
	matched := make(map[string]bool)
	for _, label := range labels {
		if strings.EqualFold(label.Name, want) || strings.EqualFold(label.LabelID, want) || label.LabelID == strings.ReplaceAll(want, "-", "_") {
			matched[label.LabelID] = true
		}
	}
	return matched
}

// conflicts names the exposed entities that need confirmation: a sensitive domain or device class,
// or a name shared with a labelled entity. A shared name over-matches, which withholds actions.
func conflicts(exposed []exposedEntity, confirmed map[string]confirmedEntity) []string {
	labelledNames := make(map[string]bool, len(confirmed))
	for _, entity := range confirmed {
		labelledNames[strings.ToLower(entity.name)] = true
	}
	var found []string
	for _, entity := range exposed {
		sensitive := sensitiveDomains[entity.domain] || (entity.domain == "cover" && sensitiveCoverClasses[entity.deviceClass])
		for _, name := range entity.names {
			if labelledNames[strings.ToLower(name)] {
				sensitive = true
			}
		}
		if sensitive {
			found = append(found, strings.Join(entity.names, ", "))
		}
	}
	sort.Strings(found)
	return found
}

// readTools are the Home Assistant tools known to only read. A name alone proves nothing, because a
// script's tool is named after the script, so a script is a read only through ReadLabel.
var readTools = map[string]bool{
	liveContextTool:        true,
	"todo__get_items":      true,
	"calendar__get_events": true,
}

// mapTool turns an MCP tool into a broker definition. A tool is a read when it is one of readTools;
// every other one, including scripts and tools Home Assistant adds later, acts on the home.
func mapTool(remote, description string, schema json.RawMessage) (mappedTool, bool) {
	var object map[string]json.RawMessage
	if json.Unmarshal(schema, &object) != nil || object == nil {
		schema = json.RawMessage(`{"type":"object","properties":{}}`)
	} else {
		changed := false
		if _, typed := object["type"]; !typed {
			object["type"] = json.RawMessage(`"object"`)
			changed = true
		}
		if remote == liveContextTool && withoutFilters(object, liveContextFilters) {
			changed = true
		}
		if changed {
			if encoded, err := json.Marshal(object); err == nil {
				schema = encoded
			}
		}
	}
	intent := intentOf(remote)
	name := toolName(strings.TrimPrefix(intent, "Hass"))
	description = strings.TrimSpace(description)
	if name == "ha_" || description == "" {
		return mappedTool{}, false
	}
	risk := tools.RiskWrite
	if readTools[remote] {
		risk = tools.RiskRead
	}
	if risk == tools.RiskWrite {
		description += writeSuffix
	}
	return mappedTool{
		remote: remote,
		definition: tools.Definition{
			Name:        name,
			Description: "Home Assistant: " + description,
			InputSchema: schema,
			Risk:        risk,
			Activity:    tools.ActivityHome,
			Timeout:     toolTimeout,
			ThirdParty:  true,
			Household:   true,
		},
	}, true
}

// liveContextFilters are the live-context filters that drop a device's sensors: a domain keeps only
// the device's own entity, and a name matches only an exact entity name. An area keeps them, so it
// stays.
var liveContextFilters = []string{"name", "domain"}

// withoutFilters removes the filters from a tool schema's properties and required list, reporting
// whether anything was removed.
func withoutFilters(object map[string]json.RawMessage, filters []string) bool {
	removed := false
	var properties map[string]json.RawMessage
	if json.Unmarshal(object["properties"], &properties) == nil && properties != nil {
		for _, filter := range filters {
			if _, found := properties[filter]; found {
				delete(properties, filter)
				removed = true
			}
		}
		if encoded, err := json.Marshal(properties); err == nil {
			object["properties"] = encoded
		}
	}
	var required []string
	if json.Unmarshal(object["required"], &required) == nil && len(required) > 0 {
		kept := slices.DeleteFunc(slices.Clone(required), func(name string) bool { return slices.Contains(filters, name) })
		if len(kept) < len(required) {
			if len(kept) == 0 {
				delete(object, "required")
			} else if encoded, err := json.Marshal(kept); err == nil {
				object["required"] = encoded
			}
			removed = true
		}
	}
	return removed
}

func domainOf(remote string) string {
	domain, _, found := strings.Cut(remote, "__")
	if !found {
		return ""
	}
	return domain
}

func intentOf(remote string) string {
	if _, intent, found := strings.Cut(remote, "__"); found {
		return intent
	}
	return remote
}

func domainOfEntity(entityID string) string {
	domain, _, _ := strings.Cut(entityID, ".")
	return domain
}

// toolName converts a CamelCase or snake_case name into ha_snake_case within the broker's limits.
func toolName(raw string) string {
	var builder strings.Builder
	for index, r := range raw {
		if unicode.IsUpper(r) {
			if index > 0 {
				builder.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		builder.WriteRune(r)
	}
	name := "ha_" + strings.Trim(toolNameUnsafe.ReplaceAllString(builder.String(), "_"), "_")
	name = strings.ReplaceAll(name, "__", "_")
	if len(name) > 64 {
		name = strings.TrimRight(name[:64], "_")
	}
	return name
}
