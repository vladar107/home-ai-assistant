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
	"github.com/vladar107/home-ai-assistant/internal/locale"
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

var (
	changedSinceRefusal   = locale.T("it was changed since; change it with a new request instead.", "это уже изменили; измени это новым запросом.")
	unreadableRefusal     = locale.T("what to undo could not be read.", "не удалось прочитать, что отменять.")
	recordChangedRefusal  = locale.T("a record it added was changed since; remove it with a new request instead.", "добавленную запись уже изменили; удали её новым запросом.")
	notUndoableRefusal    = locale.T("this action cannot be undone.", "это действие нельзя отменить.")
	collectionGoneRefusal = locale.T("the collection no longer exists.", "коллекции больше нет.")
	fieldsInvalidRefusal  = locale.T("the fields as they were are no longer valid (%s).", "поля в прежнем виде больше не подходят (%s).")
	fieldInUseRefusal     = locale.T("records already have values in the field %s; remove it with a new request instead.", "в записях уже есть значения в поле %s; удали его новым запросом.")
	recordGoneRefusal     = locale.T("the record no longer exists.", "записи больше нет.")
	recordUnfitRefusal    = locale.T("the record as it was no longer fits the collection (%s).", "запись в прежнем виде больше не подходит к коллекции (%s).")
	recordForeignRefusal  = locale.T("the record belongs to someone else.", "запись принадлежит другому человеку.")
	recordNoFitRefusal    = locale.T("the record no longer fits the collection (%s).", "запись больше не подходит к коллекции (%s).")
	tooManyRecordsRefusal = locale.T("The collection %s would hold more than %d records, which is the most allowed; remove some first.", "В коллекции %s было бы больше %d записей, а это предел; сначала удали несколько.")
	recordBackRefusal     = locale.T("the record is already back.", "запись уже восстановлена.")

	recordsGoneOutcome     = locale.T("The records it added were already removed.", "Добавленных записей уже нет.")
	recordsRemovedOutcome  = locale.N("Removed %d record.", "Removed %d records.", "Удалена %d запись.", "Удалены %d записи.", "Удалено %d записей.")
	fieldsRestoredOutcome  = locale.T("Restored the fields of %s as they were.", "Поля коллекции %s возвращены как были.")
	recordRestoredOutcome  = locale.T("Restored the record as it was.", "Запись возвращена как была.")
	recordRecreatedOutcome = locale.T("Restored the record.", "Запись восстановлена.")
)

// undoRefusal keeps the English reason for the record and the user's language for the user.
func undoRefusal(ctx context.Context, message locale.Text, arguments ...any) *approval.UndoRefusal {
	return &approval.UndoRefusal{Reason: message.FormatIn(locale.English, arguments...), UserReason: message.Format(ctx, arguments...)}
}

func undoOutcome(ctx context.Context, message locale.Text, arguments ...any) approval.Outcome {
	return approval.Outcome{Text: message.FormatIn(locale.English, arguments...), UserText: message.Format(ctx, arguments...)}
}

// Undo reverses a direct collection action. Anything changed since, or gone, is refused.
func (s *Service) Undo(ctx context.Context, record approval.Record, raw json.RawMessage) (approval.Outcome, error) {
	unreadable := undoRefusal(ctx, unreadableRefusal)
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
			return approval.Outcome{}, undoRefusal(ctx, recordChangedRefusal)
		}
		s.logger.InfoContext(ctx, "collection.records.deleted", "principal_id", record.PrincipalID, "records", deleted)
		if deleted == 0 {
			return undoOutcome(ctx, recordsGoneOutcome), nil
		}
		return approval.Outcome{Text: recordsRemovedOutcome.FormatIn(locale.English, deleted), UserText: recordsRemovedOutcome.Format(ctx, deleted)}, nil
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
		return approval.Outcome{}, undoRefusal(ctx, notUndoableRefusal)
	}
}

func (s *Service) undoAlter(ctx context.Context, principalID string, undo alterUndo) (approval.Outcome, error) {
	current, found, err := s.repository.LoadCollection(ctx, principalID, undo.CollectionID)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !found {
		return approval.Outcome{}, undoRefusal(ctx, collectionGoneRefusal)
	}
	if !current.UpdatedAt.Equal(undo.UpdatedAt) {
		return approval.Outcome{}, undoRefusal(ctx, changedSinceRefusal)
	}
	fields, _, err := normalizeFields(undo.Before, current.NextKey)
	if err != nil {
		return approval.Outcome{}, undoRefusal(ctx, fieldsInvalidRefusal, err.Error())
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
		return approval.Outcome{}, undoRefusal(ctx, fieldInUseRefusal, strings.Join(names, ", "))
	}
	if err != nil {
		return approval.Outcome{}, err
	}
	if !updated {
		return approval.Outcome{}, undoRefusal(ctx, changedSinceRefusal)
	}
	s.logger.InfoContext(ctx, "collection.altered", "principal_id", principalID, "collection_id", current.ID, "fields", len(fields))
	return undoOutcome(ctx, fieldsRestoredOutcome, current.Name), nil
}

func (s *Service) undoUpdate(ctx context.Context, principalID string, undo updateUndo) (approval.Outcome, error) {
	current, found, err := s.repository.LoadRecord(ctx, principalID, undo.RecordID)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !found {
		return approval.Outcome{}, undoRefusal(ctx, recordGoneRefusal)
	}
	if !current.UpdatedAt.Equal(undo.UpdatedAt) {
		return approval.Outcome{}, undoRefusal(ctx, changedSinceRefusal)
	}
	collection, found, err := s.repository.LoadCollection(ctx, principalID, current.CollectionID)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !found {
		return approval.Outcome{}, undoRefusal(ctx, collectionGoneRefusal)
	}
	if err := checkValues(collection, undo.Before, s.location); err != nil {
		return approval.Outcome{}, undoRefusal(ctx, recordUnfitRefusal, err.Error())
	}
	next := current
	next.Values, next.UpdatedAt = undo.Before, s.stamp()
	updated, err := s.repository.UpdateRecord(ctx, next, current.UpdatedAt)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !updated {
		return approval.Outcome{}, undoRefusal(ctx, changedSinceRefusal)
	}
	s.logger.InfoContext(ctx, "collection.record.updated", "principal_id", principalID, "collection_id", collection.ID, "record_id", current.ID)
	return undoOutcome(ctx, recordRestoredOutcome), nil
}

func (s *Service) undoRemove(ctx context.Context, principalID string, removed Record) (approval.Outcome, error) {
	if removed.PrincipalID != principalID {
		return approval.Outcome{}, undoRefusal(ctx, recordForeignRefusal)
	}
	collection, found, err := s.repository.LoadCollection(ctx, principalID, removed.CollectionID)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !found {
		return approval.Outcome{}, undoRefusal(ctx, collectionGoneRefusal)
	}
	if err := checkValues(collection, removed.Values, s.location); err != nil {
		return approval.Outcome{}, undoRefusal(ctx, recordNoFitRefusal, err.Error())
	}
	removed.UpdatedAt = s.stamp()
	created, err := s.repository.CreateRecords(ctx, []Record{removed}, MaximumRecords)
	switch {
	case errors.Is(err, ErrTooManyRecords):
		return approval.Outcome{}, undoRefusal(ctx, tooManyRecordsRefusal, collection.Name, MaximumRecords)
	case errors.Is(err, ErrCollectionGone):
		return approval.Outcome{}, undoRefusal(ctx, collectionGoneRefusal)
	case err != nil:
		return approval.Outcome{}, err
	case created == 0:
		return approval.Outcome{}, undoRefusal(ctx, recordBackRefusal)
	}
	s.logger.InfoContext(ctx, "collection.record.restored", "principal_id", principalID, "collection_id", collection.ID, "record_id", removed.ID)
	return undoOutcome(ctx, recordRecreatedOutcome), nil
}
