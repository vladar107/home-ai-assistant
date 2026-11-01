package collection

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
)

// alterUndo puts back the fields a direct alter replaced, while the collection is as it left it.
type alterUndo struct {
	CollectionID string    `json:"collection_id"`
	UpdatedAt    time.Time `json:"updated_at"`
	Before       []Field   `json:"before"`
}

// addUndo deletes the records a direct add stored, each while it is as it was stored.
type addUndo struct {
	Records map[string]time.Time `json:"records"`
}

// updateUndo puts back the values a direct update replaced, while the record is as it left it.
type updateUndo struct {
	RecordID  string                     `json:"record_id"`
	UpdatedAt time.Time                  `json:"updated_at"`
	Before    map[string]json.RawMessage `json:"before"`
}

// removeUndo recreates the record a direct removal deleted, under its own ID.
type removeUndo struct {
	Removed *Record `json:"removed"`
}

var changedSince = &approval.UndoRefusal{Reason: "it was changed since; change it with a new request instead."}

// Undo reverses a direct collection action. Anything changed since, or gone, is refused.
func (s *Service) Undo(ctx context.Context, record approval.Record, raw json.RawMessage) (approval.Outcome, error) {
	unreadable := &approval.UndoRefusal{Reason: "what to undo could not be read."}
	switch record.Tool {
	case toolAlter:
		var undo alterUndo
		if err := json.Unmarshal(raw, &undo); err != nil || undo.CollectionID == "" || len(undo.Before) == 0 {
			return approval.Outcome{}, unreadable
		}
		return s.undoAlter(ctx, record.PrincipalID, undo)
	case toolAdd:
		var undo addUndo
		if err := json.Unmarshal(raw, &undo); err != nil || len(undo.Records) == 0 {
			return approval.Outcome{}, unreadable
		}
		// A record already deleted counts as undone; only one changed since refuses the Undo.
		deleted, unchanged, err := s.repository.DeleteUnchangedRecords(ctx, record.PrincipalID, undo.Records)
		if err != nil {
			return approval.Outcome{}, err
		}
		if !unchanged {
			return approval.Outcome{}, &approval.UndoRefusal{Reason: "a record it added was changed since; remove it with a new request instead."}
		}
		s.logger.InfoContext(ctx, "collection.records.deleted", "principal_id", record.PrincipalID, "records", deleted)
		if deleted == 0 {
			return approval.Outcome{Text: "The records it added were already removed."}, nil
		}
		return approval.Outcome{Text: "Removed " + countText(deleted) + "."}, nil
	case toolUpdate:
		var undo updateUndo
		if err := json.Unmarshal(raw, &undo); err != nil || undo.RecordID == "" || len(undo.Before) == 0 {
			return approval.Outcome{}, unreadable
		}
		return s.undoUpdate(ctx, record.PrincipalID, undo)
	case toolRemove:
		var undo removeUndo
		if err := json.Unmarshal(raw, &undo); err != nil || undo.Removed == nil || undo.Removed.ID == "" {
			return approval.Outcome{}, unreadable
		}
		return s.undoRemove(ctx, record.PrincipalID, *undo.Removed)
	default:
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "this action cannot be undone."}
	}
}

func (s *Service) undoAlter(ctx context.Context, principalID string, undo alterUndo) (approval.Outcome, error) {
	current, found, err := s.repository.LoadCollection(ctx, principalID, undo.CollectionID)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !found {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "the collection no longer exists."}
	}
	if !current.UpdatedAt.Equal(undo.UpdatedAt) {
		return approval.Outcome{}, changedSince
	}
	fields, _, err := normalizeFields(undo.Before, current.NextKey)
	if err != nil {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "the fields as they were are no longer valid (" + err.Error() + ")."}
	}
	next := current
	next.Fields, next.UpdatedAt = fields, s.stamp()
	removed := removedKeys(current.Fields, fields)
	// Undoing an addition would drop what was written in the new field since, so it is refused
	// then rather than taking the values with it.
	updated, err := s.repository.UpdateCollectionFields(ctx, next, current.UpdatedAt, removed, true)
	if errors.Is(err, ErrFieldInUse) {
		names := make([]string, 0, len(removed))
		for _, field := range current.Fields {
			if slices.Contains(removed, field.Key) {
				names = append(names, strconv.Quote(field.Name))
			}
		}
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "records already have values in the field " + strings.Join(names, ", ") + "; remove it with a new request instead."}
	}
	if err != nil {
		return approval.Outcome{}, err
	}
	if !updated {
		return approval.Outcome{}, changedSince
	}
	s.logger.InfoContext(ctx, "collection.altered", "principal_id", principalID, "collection_id", current.ID, "fields", len(fields))
	return approval.Outcome{Text: "Restored the fields of " + current.Name + " as they were."}, nil
}

func (s *Service) undoUpdate(ctx context.Context, principalID string, undo updateUndo) (approval.Outcome, error) {
	current, found, err := s.repository.LoadRecord(ctx, principalID, undo.RecordID)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !found {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "the record no longer exists."}
	}
	if !current.UpdatedAt.Equal(undo.UpdatedAt) {
		return approval.Outcome{}, changedSince
	}
	collection, found, err := s.repository.LoadCollection(ctx, principalID, current.CollectionID)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !found {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "the collection no longer exists."}
	}
	if err := checkValues(collection, undo.Before, s.location); err != nil {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "the record as it was no longer fits the collection (" + err.Error() + ")."}
	}
	next := current
	next.Values, next.UpdatedAt = undo.Before, s.stamp()
	updated, err := s.repository.UpdateRecord(ctx, next, current.UpdatedAt)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !updated {
		return approval.Outcome{}, changedSince
	}
	s.logger.InfoContext(ctx, "collection.record.updated", "principal_id", principalID, "collection_id", collection.ID, "record_id", current.ID)
	return approval.Outcome{Text: "Restored the record as it was."}, nil
}

func (s *Service) undoRemove(ctx context.Context, principalID string, removed Record) (approval.Outcome, error) {
	if removed.PrincipalID != principalID {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "the record belongs to someone else."}
	}
	collection, found, err := s.repository.LoadCollection(ctx, principalID, removed.CollectionID)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !found {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "the collection no longer exists."}
	}
	if err := checkValues(collection, removed.Values, s.location); err != nil {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "the record no longer fits the collection (" + err.Error() + ")."}
	}
	removed.UpdatedAt = s.stamp()
	created, err := s.repository.CreateRecords(ctx, []Record{removed}, MaximumRecords)
	switch {
	case errors.Is(err, ErrTooManyRecords):
		return approval.Outcome{}, &approval.UndoRefusal{Reason: tooManyRecordsText(collection)}
	case errors.Is(err, ErrCollectionGone):
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "the collection no longer exists."}
	case err != nil:
		return approval.Outcome{}, err
	case created == 0:
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "the record is already back."}
	}
	s.logger.InfoContext(ctx, "collection.record.restored", "principal_id", principalID, "collection_id", collection.ID, "record_id", removed.ID)
	return approval.Outcome{Text: "Restored the record."}, nil
}
