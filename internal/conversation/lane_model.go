package conversation

import (
	"context"
	"errors"
	"fmt"

	"github.com/vladar107/home-ai-assistant/internal/modelcatalog"
	agentruntime "github.com/vladar107/home-ai-assistant/internal/runtime"
)

// ErrUnsupportedChoice means the principal's catalog does not offer the model, or the model does
// not run on the effort.
var ErrUnsupportedChoice = errors.New("the model or effort is not available")

// LaneTurn is what /status knows of a lane's recent chat turns.
type LaneTurn struct {
	// PassedOver is the lane's choice that its latest turn did not use because the catalog no
	// longer offered it, so the turn ran on the chat default; empty when the choice was used.
	PassedOver agentruntime.ModelEffort
	// Ran reports that a turn has finished in the lane since the service started; Model and Tokens
	// describe the latest one. Model is empty when the runtime did not report it.
	Ran    bool
	Model  string
	Tokens agentruntime.TokenCounts
}

// Stale reports that the latest turn ran on the chat default instead of the lane's choice.
func (t LaneTurn) Stale() bool { return t.PassedOver.Model != "" }

// Choice returns the lane's own model and effort, and false when it has none and runs on the
// chat default. It is returned as stored, even if the catalog has since dropped it.
func (s *Service) Choice(ctx context.Context, lane Lane) (agentruntime.ModelEffort, bool, error) {
	return s.repository.LaneModel(ctx, lane)
}

// SetChoice makes the lane run its chat turns on the pair until /new or the tab is closed. It
// reads the principal's catalog; a caller that already has the models uses SetChoiceFrom.
func (s *Service) SetChoice(ctx context.Context, lane Lane, choice agentruntime.ModelEffort) error {
	models, err := s.catalog(ctx, lane.PrincipalID)
	if err != nil {
		return err
	}
	return s.SetChoiceFrom(ctx, lane, choice, models)
}

// SetChoiceFrom is SetChoice against models the caller read from the catalog. A nil list means
// the catalog could not be read, and only the pairs the configured defaults name are accepted.
// A pair the list does not offer, or an invalid model id or effort, is refused with
// ErrUnsupportedChoice; an alias level is stored as the level the model runs it as.
func (s *Service) SetChoiceFrom(ctx context.Context, lane Lane, choice agentruntime.ModelEffort, models []modelcatalog.Model) error {
	if !modelcatalog.ValidChoice(choice) {
		return fmt.Errorf("%w: %s", ErrUnsupportedChoice, choice.Effort)
	}
	if models == nil {
		models = modelcatalog.Fallback(s.config.ChatModel, s.config.TaskModel)
	}
	canonical, ok := modelcatalog.Offered(models, choice)
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnsupportedChoice, choice)
	}
	choice.Effort = canonical
	if err := s.repository.SaveLaneModel(ctx, lane, choice); err != nil {
		return err
	}
	s.resetLane(lane)
	return nil
}

// ClearChoice returns the lane to the chat default.
func (s *Service) ClearChoice(ctx context.Context, lane Lane) error {
	if err := s.repository.ClearLaneModel(ctx, lane); err != nil {
		return err
	}
	s.resetLane(lane)
	return nil
}

// LaneTurn reports the lane's latest chat turn from memory.
func (s *Service) LaneTurn(lane Lane) LaneTurn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.laneTurns[lane]
}

// EffectiveChoice is the pair the lane's next chat turn would run on: its own choice while the
// catalog still offers it, otherwise the chat default. stale reports a choice that was passed
// over. A catalog that cannot be read is trusted to be unchanged.
func (s *Service) EffectiveChoice(ctx context.Context, lane Lane) (choice agentruntime.ModelEffort, stale bool, err error) {
	choice, passedOver, _, err := s.resolveChoice(ctx, lane)
	return choice, passedOver.Model != "", err
}

// EffectiveChoiceFrom is EffectiveChoice judged against the models the caller already read, nil
// meaning the catalog could not be read, so a caller that holds them does not take Pi's catalog
// lock again.
func (s *Service) EffectiveChoiceFrom(ctx context.Context, lane Lane, models []modelcatalog.Model) (choice agentruntime.ModelEffort, stale bool, err error) {
	choice, passedOver, _, err := s.resolveChoiceWith(ctx, lane, func() ([]modelcatalog.Model, error) { return models, nil })
	return choice, passedOver.Model != "", err
}

// resolveChoice also returns the choice it passed over and the lane's epoch from before it read
// anything, which recordLaneTurn needs to ignore a turn that a later change has overtaken.
func (s *Service) resolveChoice(ctx context.Context, lane Lane) (choice, passedOver agentruntime.ModelEffort, epoch int, err error) {
	return s.resolveChoiceWith(ctx, lane, func() ([]modelcatalog.Model, error) { return s.catalog(ctx, lane.PrincipalID) })
}

func (s *Service) resolveChoiceWith(ctx context.Context, lane Lane, load func() ([]modelcatalog.Model, error)) (choice, passedOver agentruntime.ModelEffort, epoch int, err error) {
	s.mu.Lock()
	epoch = s.laneEpochs[lane]
	s.mu.Unlock()
	stored, found, err := s.repository.LaneModel(ctx, lane)
	if err != nil {
		return agentruntime.ModelEffort{}, agentruntime.ModelEffort{}, epoch, err
	}
	if !found {
		return s.config.ChatModel, agentruntime.ModelEffort{}, epoch, nil
	}
	models, err := load()
	if err != nil {
		return agentruntime.ModelEffort{}, agentruntime.ModelEffort{}, epoch, err
	}
	if models == nil {
		return stored, agentruntime.ModelEffort{}, epoch, nil
	}
	canonical, ok := modelcatalog.Offered(models, stored)
	if !ok {
		return s.config.ChatModel, stored, epoch, nil
	}
	stored.Effort = canonical
	return stored, agentruntime.ModelEffort{}, epoch, nil
}

// catalog returns the principal's models, or nil when the catalog cannot be read.
func (s *Service) catalog(ctx context.Context, principalID string) ([]modelcatalog.Model, error) {
	if s.config.Models == nil {
		return nil, nil
	}
	models, err := s.config.Models.Models(ctx, principalID)
	if errors.Is(err, modelcatalog.ErrUnavailable) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read model catalog: %w", err)
	}
	if models == nil {
		models = []modelcatalog.Model{}
	}
	return models, nil
}

// resetLane forgets the lane's latest turn and invalidates the turns still running under the old
// choice.
func (s *Service) resetLane(lane Lane) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.laneTurns, lane)
	s.laneEpochs[lane]++
}

func (s *Service) recordLaneTurn(lane Lane, epoch int, passedOver agentruntime.ModelEffort, result agentruntime.RunResult, succeeded bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.laneEpochs[lane] != epoch {
		return
	}
	turn := s.laneTurns[lane]
	turn.PassedOver = passedOver
	if succeeded {
		turn.Ran, turn.Model, turn.Tokens = true, result.Model, result.Tokens
	}
	s.laneTurns[lane] = turn
}
