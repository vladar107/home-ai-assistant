package homeassistant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const maximumIntentRunes = 80

type actionArguments struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

// proposeAction records an immediate action for confirmation because the conversation has read
// outside content; nothing reaches the device.
func (s *Service) proposeAction(ctx context.Context, principalID, name string, tool mappedTool, arguments map[string]any) tools.Result {
	if s.proposer == nil {
		return tools.Result{Text: "This conversation has read outside content, so the action needs the user's confirmation, which is unavailable. Do not retry; tell the user the action was not taken.", IsError: true}
	}
	encoded, err := json.Marshal(actionArguments{Tool: name, Arguments: arguments})
	if err != nil {
		return failure(errMalformed)
	}
	rendered, err := renderArguments(arguments)
	if err != nil || len([]rune(rendered)) > maximumActionArgumentRunes {
		return tools.Result{Text: fmt.Sprintf("The arguments are too long to show for confirmation; send them in at most %d characters.", maximumActionArgumentRunes), IsError: true}
	}
	description := strings.Join(strings.Fields(tool.definition.Description), " ")
	summary := fmt.Sprintf("Home Assistant action %s: %s\nArguments: %s\nI read outside content in this conversation, so I need your confirmation. /new starts a fresh conversation where home actions run at once.", name, description, rendered)
	target := description
	if device, ok := arguments["name"].(string); ok && strings.TrimSpace(device) != "" {
		target = description + " " + device
	}
	intent := approval.Intent{Verb: "run", TargetKind: "home action", TargetName: truncateRunes(target, maximumIntentRunes)}
	if _, err := s.proposer.Propose(ctx, principalID, approval.Proposal{Tool: toolAction, Arguments: encoded, Summary: summary, Intent: intent}); err != nil {
		if errors.Is(err, approval.ErrBundleFull) {
			return failure(err)
		}
		s.logger.WarnContext(ctx, "homeassistant.proposal.failed", "principal_id", principalID, "error", err)
		return failure(fmt.Errorf("the proposal could not be recorded"))
	}
	return tools.Result{Text: approval.ProposedText}
}

// executeApprovedAction re-checks the switch, a fresh snapshot, that actions are still allowed, and
// that the tool is still a write tool before running it with the stored arguments.
func (s *Service) executeApprovedAction(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	status, err := s.Status(ctx, record.PrincipalID)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !status.Enabled {
		return approval.Outcome{}, &approval.ActionError{Reason: "disabled", Text: "The home is off for you; turn it on in /settings and ask again."}
	}
	var stored actionArguments
	if err := json.Unmarshal(record.Arguments, &stored); err != nil || stored.Tool == "" {
		return approval.Outcome{}, &approval.ActionError{Reason: "malformed", Text: "The stored action could not be read."}
	}
	s.forget()
	snap, err := s.current(ctx)
	if err != nil {
		return approval.Outcome{}, actionError(err)
	}
	if !snap.actionsAllowed() {
		return approval.Outcome{}, &approval.ActionError{Reason: "withheld", Text: "Home actions are withheld until the household fixes which devices are exposed; check /settings."}
	}
	tool, found := snap.byName[stored.Tool]
	if !found || tool.definition.Risk != tools.RiskWrite {
		return approval.Outcome{}, &approval.ActionError{Reason: "unavailable", Text: "That home action is no longer available."}
	}
	if stored.Arguments == nil {
		stored.Arguments = map[string]any{}
	}
	text, isError, err := s.callTool(ctx, tool.remote, stored.Arguments)
	s.logger.InfoContext(ctx, "homeassistant.action", "principal_id", record.PrincipalID, "tool", stored.Tool, "is_error", isError || err != nil)
	s.forget()
	if err != nil {
		return approval.Outcome{}, actionError(err)
	}
	if isError {
		return approval.Outcome{}, &approval.ActionError{Reason: "rejected", Text: "Home Assistant could not do that: " + text}
	}
	if text == "" {
		text = "Done."
	}
	return approval.Outcome{Text: text}, nil
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

// renderArguments shows the arguments on one line as the user will confirm them; the reply renderer
// escapes markup, so the JSON is not HTML-escaped as well.
func renderArguments(arguments map[string]any) (string, error) {
	var rendered bytes.Buffer
	encoder := json.NewEncoder(&rendered)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(arguments); err != nil {
		return "", err
	}
	return strings.TrimSpace(rendered.String()), nil
}
