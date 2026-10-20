package memory

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/vladar107/home-ai-assistant/internal/approval"
)

// proposedMerge is what a memory_merge record stores: the instructions it replaces with the texts
// the user saw, and the one text that replaces them.
type proposedMerge struct {
	IDs   []string `json:"ids"`
	Texts []string `json:"texts"`
	Text  string   `json:"text"`
}

// executeMerge writes a confirmed merge while every instruction it replaces still holds the text the
// user saw: the first takes the new text, as learned, and the others are forgotten.
func (s *Service) executeMerge(ctx context.Context, record approval.Record, entries []Entry) (approval.Outcome, error) {
	var confirmed proposedMerge
	if err := json.Unmarshal(record.Arguments, &confirmed); err != nil || len(confirmed.IDs) < 2 || len(confirmed.Texts) != len(confirmed.IDs) || validText(confirmed.Text) != "" {
		return approval.Outcome{}, &approval.ActionError{Reason: "malformed", Text: "The confirmed merge could not be read; nothing was changed."}
	}
	for index, id := range confirmed.IDs {
		entry, found := find(entries, id)
		if !found {
			return approval.Outcome{}, &approval.ActionError{Reason: "not_found", Text: "One of those instructions no longer exists; nothing was changed."}
		}
		if entry.Text != confirmed.Texts[index] {
			return approval.Outcome{}, &approval.ActionError{Reason: "changed", Text: "One of those instructions changed since; nothing was changed."}
		}
	}
	kept := slices.DeleteFunc(slices.Clone(entries), func(entry Entry) bool { return slices.Contains(confirmed.IDs, entry.ID) })
	if rejected := check(kept, KindInstruction, confirmed.Text, ""); rejected.outcome != "" {
		s.record(ctx, record.PrincipalID, KindInstruction, rejected.outcome)
		return approval.Outcome{}, &approval.ActionError{Reason: rejected.outcome, Text: userFacing(rejected)}
	}
	merged, err := s.repository.MergeMemory(ctx, record.PrincipalID, confirmed.IDs[0], confirmed.Text, SourceLearned, confirmed.IDs[1:], s.now())
	if err != nil {
		return approval.Outcome{}, err
	}
	if !merged {
		return approval.Outcome{}, &approval.ActionError{Reason: "not_found", Text: "One of those instructions no longer exists; nothing was changed."}
	}
	s.record(ctx, record.PrincipalID, KindInstruction, outcomeWritten)
	for range confirmed.IDs[1:] {
		s.record(ctx, record.PrincipalID, KindInstruction, outcomeRemoved)
	}
	s.recordLesson(ctx, record.PrincipalID, MechanismReview, outcomeWritten)
	return approval.Outcome{Text: "Merged."}, nil
}
