package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/vladar107/home-ai-assistant/internal/clip"
	"github.com/vladar107/home-ai-assistant/internal/prompt"
)

type decision struct {
	notify  bool
	message string
	// failed is true when no decision could be had, and the event is forwarded as it is.
	failed bool
}

// decide returns the event's assessment: the one kept from an earlier attempt, or a new one, run
// once under the assessment's own time limit and kept before anything is sent.
func (s *Service) decide(ctx context.Context, event Event) decision {
	switch event.Assessment {
	case AssessmentNotify:
		return decision{notify: true, message: event.AssessedMessage}
	case AssessmentQuiet:
		return decision{}
	case AssessmentFailed:
		return decision{failed: true}
	}
	s.mu.Lock()
	kept, found := s.decisions[event.ID]
	s.mu.Unlock()
	if found {
		return kept
	}
	assessCtx, cancel := context.WithTimeout(ctx, s.assessTimeout)
	decided, err := s.assess(assessCtx, event)
	cancel()
	if ctx.Err() != nil {
		return decision{}
	}
	if err != nil {
		s.logger.WarnContext(ctx, "events.assess.failed", "principal_id", event.PrincipalID, "source_id", event.SourceID, "event_id", event.ID, "error", err)
		decided = decision{failed: true}
	}
	s.mu.Lock()
	s.decisions[event.ID] = decided
	s.mu.Unlock()
	assessment := AssessmentQuiet
	switch {
	case decided.failed:
		assessment = AssessmentFailed
	case decided.notify:
		assessment = AssessmentNotify
	}
	storeCtx, storeCancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
	defer storeCancel()
	if _, err := s.repository.SaveInboundAssessment(storeCtx, event.PrincipalID, event.ID, assessment, decided.message); err != nil {
		s.logRetry(ctx, event, "store_assessment", err)
	}
	return decided
}

type assessment struct {
	Notify  *bool  `json:"notify"`
	Message string `json:"message"`
}

// assess runs the event's assessment turn on its owner's subscription. A failed turn or a reply
// without a valid decision is an error, and the caller forwards the event instead.
func (s *Service) assess(ctx context.Context, event Event) (decision, error) {
	message := prompt.InboundEvent(event.SourceName, event.ReceivedAt, s.location, event.Severity, event.Title, event.Text)
	reply, err := s.assessor.RunAssess(ctx, event.PrincipalID, message)
	if err != nil {
		return decision{}, err
	}
	if reply.ErrorKind != "" {
		return decision{}, fmt.Errorf("the assessment turn failed: %s", reply.ErrorKind)
	}
	return parseAssessment(reply.Text)
}

// parseAssessment reads the JSON object an assessment's reply ends with, allowing a code fence
// around it.
func parseAssessment(text string) (decision, error) {
	text = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), "```"))
	end := strings.LastIndex(text, "}")
	if end < 0 {
		return decision{}, errors.New("the assessment has no decision object")
	}
	body := text[:end+1]
	for start := strings.LastIndex(body, "{"); start >= 0; start = strings.LastIndex(body[:start], "{") {
		var found assessment
		if err := json.Unmarshal([]byte(body[start:]), &found); err != nil {
			continue
		}
		if found.Notify == nil {
			return decision{}, errors.New("the assessment's decision has no notify")
		}
		return decision{notify: *found.Notify, message: clip.Runes(strings.TrimSpace(found.Message), maximumMessage)}, nil
	}
	return decision{}, errors.New("the assessment has no valid decision object")
}
