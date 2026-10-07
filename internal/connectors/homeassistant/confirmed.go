package homeassistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const maximumDataBytes = 4 << 10

var (
	entityIDPattern = regexp.MustCompile(`^[a-z0-9_]+\.[a-z0-9_]+$`)
	servicePattern  = regexp.MustCompile(`^[a-z0-9_]+$`)
)

var _ approval.Executor = (*Service)(nil)

func (s *Service) ActionTools() []string {
	return []string{toolServiceCall, toolAction}
}

type serviceCallArguments struct {
	EntityID string                     `json:"entity_id"`
	Service  string                     `json:"service"`
	Data     map[string]json.RawMessage `json:"data,omitempty"`
}

// domainServices reads the services of one domain with their field names, flattening the sections
// Home Assistant groups advanced fields into.
func (s *Service) domainServices(ctx context.Context, domain string) (map[string][]string, error) {
	var listed []struct {
		Domain   string `json:"domain"`
		Services map[string]struct {
			Fields map[string]json.RawMessage `json:"fields"`
		} `json:"services"`
	}
	if err := s.client.getJSON(ctx, "/api/services", &listed); err != nil {
		return nil, err
	}
	services := make(map[string][]string)
	for _, entry := range listed {
		if entry.Domain != domain {
			continue
		}
		for name, service := range entry.Services {
			var fields []string
			for field, raw := range service.Fields {
				var section struct {
					Fields map[string]json.RawMessage `json:"fields"`
				}
				if json.Unmarshal(raw, &section) == nil && section.Fields != nil {
					for nested := range section.Fields {
						fields = append(fields, nested)
					}
					continue
				}
				fields = append(fields, field)
			}
			sort.Strings(fields)
			services[name] = fields
		}
	}
	return services, nil
}

func (s *Service) confirmedDevices(ctx context.Context, snap *snapshot) tools.Result {
	if !snap.guarded || len(snap.confirmed) == 0 {
		return tools.Result{Text: "No device is labelled as needing confirmation."}
	}
	ids := make([]string, 0, len(snap.confirmed))
	for id := range snap.confirmed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	byDomain := make(map[string]map[string][]string)
	var builder strings.Builder
	builder.WriteString("Devices that need the user's confirmation:\n")
	for _, id := range ids {
		entity := snap.confirmed[id]
		services, cached := byDomain[entity.domain]
		if !cached {
			var err error
			if services, err = s.domainServices(ctx, entity.domain); err != nil {
				return failure(err)
			}
			byDomain[entity.domain] = services
		}
		fmt.Fprintf(&builder, "- %s (%s): %s\n", entity.name, entity.entityID, entity.state)
		names := make([]string, 0, len(services))
		for name := range services {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if fields := services[name]; len(fields) > 0 {
				fmt.Fprintf(&builder, "  %s (fields: %s)\n", name, strings.Join(fields, ", "))
			} else {
				fmt.Fprintf(&builder, "  %s\n", name)
			}
		}
	}
	return tools.Result{Text: builder.String()}
}

// validateServiceCall checks a call against the labelled entities and the domain's services and
// returns it re-encoded, with the entity it targets.
func (s *Service) validateServiceCall(ctx context.Context, snap *snapshot, raw json.RawMessage) (serviceCallArguments, confirmedEntity, error) {
	var arguments serviceCallArguments
	if err := decodeArguments(raw, &arguments); err != nil {
		return arguments, confirmedEntity{}, err
	}
	if !entityIDPattern.MatchString(arguments.EntityID) || !servicePattern.MatchString(arguments.Service) {
		return arguments, confirmedEntity{}, fmt.Errorf("entity_id and service must be Home Assistant identifiers, such as lock.front_door and unlock")
	}
	entity, labelled := snap.confirmed[arguments.EntityID]
	if !snap.guarded || !labelled {
		return arguments, confirmedEntity{}, fmt.Errorf("that entity is not one of the devices %s lists", toolConfirmedDevices)
	}
	encoded, err := json.Marshal(arguments.Data)
	if err != nil || len(encoded) > maximumDataBytes {
		return arguments, confirmedEntity{}, fmt.Errorf("data must be a JSON object of at most %d bytes", maximumDataBytes)
	}
	services, err := s.domainServices(ctx, entity.domain)
	if err != nil {
		return arguments, confirmedEntity{}, err
	}
	fields, known := services[arguments.Service]
	if !known {
		return arguments, confirmedEntity{}, fmt.Errorf("%s does not accept the service %s; %s lists the ones it does", entity.name, arguments.Service, toolConfirmedDevices)
	}
	for key := range arguments.Data {
		if key == "entity_id" || !slices.Contains(fields, key) {
			return arguments, confirmedEntity{}, fmt.Errorf("%s does not take the field %s", arguments.Service, key)
		}
	}
	return arguments, entity, nil
}

// propose records a labelled entity's service call for confirmation; nothing reaches the device.
func (s *Service) propose(ctx context.Context, principalID string, snap *snapshot, raw json.RawMessage) (string, error) {
	if s.proposer == nil {
		return "", fmt.Errorf("confirmed home actions are not available")
	}
	arguments, entity, err := s.validateServiceCall(ctx, snap, raw)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return "", errMalformed
	}
	verb := strings.ReplaceAll(arguments.Service, "_", " ")
	summary := fmt.Sprintf("Home Assistant: %s %s (%s)", verb, entity.name, entity.entityID)
	if len(arguments.Data) > 0 {
		keys := make([]string, 0, len(arguments.Data))
		for key := range arguments.Data {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			summary += fmt.Sprintf("\n%s: %s", key, arguments.Data[key])
		}
	}
	intent := approval.Intent{Verb: verb, TargetKind: entity.domain, TargetName: entity.name}
	if _, err := s.proposer.Propose(ctx, principalID, approval.Proposal{Tool: toolServiceCall, Arguments: encoded, Summary: summary, Intent: intent}); err != nil {
		if approval.Refused(err) {
			return "", err
		}
		s.logger.WarnContext(ctx, "homeassistant.proposal.failed", "principal_id", principalID, "error", err)
		return "", fmt.Errorf("the proposal could not be recorded")
	}
	return approval.ProposedText, nil
}

// ExecuteApproved re-checks that the entity still carries the label and the call still fits its
// domain before running it once.
func (s *Service) ExecuteApproved(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	switch record.Tool {
	case toolServiceCall:
	case toolAction:
		return s.executeApprovedAction(ctx, record)
	default:
		return approval.Outcome{}, &approval.ActionError{Reason: "unknown_tool", Text: "This action is not supported."}
	}
	status, err := s.Status(ctx, record.PrincipalID)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !status.Enabled {
		return approval.Outcome{}, &approval.ActionError{Reason: "disabled", Text: "The home is off for you; turn it on in /settings and ask again."}
	}
	s.forget()
	snap, err := s.current(ctx)
	if err != nil {
		return approval.Outcome{}, actionError(err)
	}
	arguments, entity, err := s.validateServiceCall(ctx, snap, record.Arguments)
	if err != nil {
		return approval.Outcome{}, actionError(err)
	}
	body := map[string]any{"entity_id": arguments.EntityID}
	for key, value := range arguments.Data {
		body[key] = value
	}
	if err := s.client.postJSON(ctx, "/api/services/"+entity.domain+"/"+arguments.Service, body, nil); err != nil {
		return approval.Outcome{}, actionError(err)
	}
	s.logger.InfoContext(ctx, "homeassistant.confirmed_action", "principal_id", record.PrincipalID, "domain", entity.domain)
	s.forget()
	return approval.Outcome{Text: fmt.Sprintf("Done: %s %s.", strings.ReplaceAll(arguments.Service, "_", " "), entity.name)}, nil
}

func actionError(err error) error {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		return &approval.ActionError{Reason: apiErr.class, Text: apiErr.text}
	}
	return &approval.ActionError{Reason: "rejected", Text: err.Error()}
}
