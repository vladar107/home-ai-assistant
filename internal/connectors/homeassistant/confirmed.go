package homeassistant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/locale"
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
	if err := tools.DecodeArguments(raw, &arguments); err != nil {
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
	summary := serviceCallSummary.Format(ctx, verb, entity.name, entity.entityID)
	if len(arguments.Data) > 0 {
		keys := make([]string, 0, len(arguments.Data))
		for key := range arguments.Data {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			var value any
			decoder := json.NewDecoder(bytes.NewReader(arguments.Data[key]))
			decoder.UseNumber()
			rendered := string(arguments.Data[key])
			if decoder.Decode(&value) == nil {
				if text, err := renderValue(value); err == nil {
					rendered = text
				}
			}
			summary += fmt.Sprintf("\n%s: %s", key, rendered)
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
		return approval.Outcome{}, actionFailed(ctx, "unknown_tool", actionUnsupported)
	}
	s.forget()
	snap, err := s.current(ctx)
	if err != nil {
		return approval.Outcome{}, actionError(ctx, err)
	}
	arguments, entity, err := s.validateServiceCall(ctx, snap, record.Arguments)
	if err != nil {
		return approval.Outcome{}, actionError(ctx, err)
	}
	body := map[string]any{"entity_id": arguments.EntityID}
	for key, value := range arguments.Data {
		body[key] = value
	}
	if err := s.client.postJSON(ctx, "/api/services/"+entity.domain+"/"+arguments.Service, body, nil); err != nil {
		return approval.Outcome{}, actionError(ctx, err)
	}
	s.logger.InfoContext(ctx, "homeassistant.confirmed_action", "principal_id", record.PrincipalID, "domain", entity.domain)
	s.forget()
	service := strings.ReplaceAll(arguments.Service, "_", " ")
	return approval.Outcome{Text: serviceDone.FormatIn(locale.English, service, entity.name), UserText: serviceDone.Format(ctx, service, entity.name)}, nil
}

var (
	actionUnsupported = locale.T("This action is not supported.", "Это действие не поддерживается.")
	serviceDone       = locale.T("Done: %s %s.", "Выполнено: %[1]s для %[2]s.")
	// The service is Home Assistant's own name for it, such as "turn on", in every language.
	serviceCallSummary = locale.T("Home Assistant: %s %s (%s)", "Home Assistant: %[1]s для %[2]s (%[3]s)")

	apiErrorTexts = map[string]locale.Text{
		errUnauthorized.class: locale.T("Home Assistant refused the household token; the operator needs to check it.", "Home Assistant отклонил токен дома; оператору нужно его проверить."),
		errNotFound.class:     locale.T("Home Assistant does not know that entity, service, or tool.", "Home Assistant не знает такого устройства, службы или инструмента."),
		errBadRequest.class:   locale.T("Home Assistant could not carry out that request.", "Home Assistant не смог выполнить этот запрос."),
		errUnavailable.class:  locale.T("Home Assistant is unavailable right now.", "Home Assistant сейчас недоступен."),
		errTimeout.class:      locale.T("Home Assistant did not respond in time.", "Home Assistant не ответил вовремя."),
		errMalformed.class:    locale.T("Home Assistant returned an unexpected response.", "Home Assistant вернул неожиданный ответ."),
	}
)

func actionError(ctx context.Context, err error) error {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		failed := &approval.ActionError{Reason: apiErr.class, Text: apiErr.text}
		if message, known := apiErrorTexts[apiErr.class]; known && message.FormatIn(locale.English) == apiErr.text {
			failed.UserText = message.Format(ctx)
		}
		return failed
	}
	return &approval.ActionError{Reason: "rejected", Text: err.Error()}
}
