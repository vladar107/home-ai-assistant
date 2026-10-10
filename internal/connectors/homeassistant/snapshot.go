package homeassistant

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"sort"
	"strings"
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

	liveContextTool = "homeassistant__GetLiveContext"
	// ConfirmLabel marks an entity whose actions always need the user's confirmation; it must not
	// be exposed to Assist, where actions run at once.
	ConfirmLabel = "assistant-confirm"
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
	confirmed map[string]confirmedEntity
	// entityIDs maps a lowercased friendly name to the entities that carry it.
	entityIDs map[string][]string
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

	exposed, guardErr := s.liveContext(ctx)
	confirmed, labelErr := s.labelledEntities(ctx, byID)
	fresh.exposed = exposed
	fresh.confirmed = confirmed
	if guardErr != nil || labelErr != nil {
		s.logger.WarnContext(ctx, "homeassistant.guard.failed", "live_context", guardErr != nil, "labels", labelErr != nil)
		return fresh, nil
	}
	fresh.guarded = true
	fresh.conflicts = conflicts(exposed, confirmed)
	return fresh, nil
}

type stateObject struct {
	EntityID   string         `json:"entity_id"`
	State      string         `json:"state"`
	Attributes map[string]any `json:"attributes"`
}

func (s stateObject) friendlyName() string {
	name, _ := s.Attributes["friendly_name"].(string)
	return strings.TrimSpace(name)
}

func (s stateObject) unit() string {
	unit, _ := s.Attributes["unit_of_measurement"].(string)
	return unit
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

// labelledEntities reads the label and entity registries, which a non-admin token may read, and
// returns the entities carrying ConfirmLabel.
func (s *Service) labelledEntities(ctx context.Context, states map[string]stateObject) (map[string]confirmedEntity, error) {
	results, err := s.client.websocketCommands(ctx,
		map[string]any{"type": "config/label_registry/list"},
		map[string]any{"type": "config/entity_registry/list_for_display"},
	)
	if err != nil {
		return nil, err
	}
	var labels []struct {
		LabelID string `json:"label_id"`
		Name    string `json:"name"`
	}
	var registry struct {
		Entities []struct {
			EntityID string   `json:"ei"`
			Labels   []string `json:"lb"`
		} `json:"entities"`
	}
	if json.Unmarshal(results[0], &labels) != nil || json.Unmarshal(results[1], &registry) != nil {
		return nil, errMalformed
	}
	confirmLabels := make(map[string]bool)
	for _, label := range labels {
		if strings.EqualFold(label.Name, ConfirmLabel) || strings.EqualFold(label.LabelID, ConfirmLabel) || label.LabelID == strings.ReplaceAll(ConfirmLabel, "-", "_") {
			confirmLabels[label.LabelID] = true
		}
	}
	confirmed := make(map[string]confirmedEntity)
	for _, entity := range registry.Entities {
		if !slices.ContainsFunc(entity.Labels, func(id string) bool { return confirmLabels[id] }) {
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
	return confirmed, nil
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

// mapTool turns an MCP tool into a broker definition. A tool is a read when its intent starts with
// Get; every other one, including tools Home Assistant adds later, acts on the home.
func mapTool(remote, description string, schema json.RawMessage) (mappedTool, bool) {
	var object map[string]json.RawMessage
	if json.Unmarshal(schema, &object) != nil || object == nil {
		schema = json.RawMessage(`{"type":"object","properties":{}}`)
	} else if _, typed := object["type"]; !typed {
		object["type"] = json.RawMessage(`"object"`)
		if encoded, err := json.Marshal(object); err == nil {
			schema = encoded
		}
	}
	intent := intentOf(remote)
	name := toolName(strings.TrimPrefix(intent, "Hass"))
	description = strings.TrimSpace(description)
	if name == "ha_" || description == "" {
		return mappedTool{}, false
	}
	risk := tools.RiskWrite
	if strings.HasPrefix(intent, "Get") || strings.HasPrefix(intent, "get_") {
		risk = tools.RiskRead
	}
	if risk == tools.RiskWrite {
		description += " Runs at once on the household's devices."
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
