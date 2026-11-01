package collection

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
)

// Repository stores collections and their records, always scoped to their principal.
type Repository interface {
	// CreateCollection stores a new collection unless one with its ID exists, refusing with
	// ErrTooManyCollections at limit and ErrNameTaken for a name the principal already uses.
	CreateCollection(ctx context.Context, collection Collection, limit int) (bool, error)
	// ListCollections returns the principal's collections, oldest first, with their record counts.
	ListCollections(ctx context.Context, principalID string) ([]Collection, error)
	LoadCollection(ctx context.Context, principalID, collectionID string) (Collection, bool, error)
	// UpdateCollectionFields replaces the collection's fields only while it was last changed at
	// expectedUpdatedAt, raises its next key to the collection's NextKey, and drops the values its
	// records hold under removedKeys, all or none. With refuseUsed it refuses with ErrFieldInUse
	// instead when a record holds a value under one of them.
	UpdateCollectionFields(ctx context.Context, collection Collection, expectedUpdatedAt time.Time, removedKeys []string, refuseUsed bool) (bool, error)
	// DeleteCollection deletes the collection with its records.
	DeleteCollection(ctx context.Context, principalID, collectionID string) (bool, error)

	// ListRecords returns the collection's records, oldest first.
	ListRecords(ctx context.Context, principalID, collectionID string) ([]Record, error)
	LoadRecord(ctx context.Context, principalID, recordID string) (Record, bool, error)
	// CreateRecords stores the records of one collection that do not exist yet, all or none,
	// refusing with ErrTooManyRecords past limit and ErrCollectionGone without the collection. It
	// returns how many it stored.
	CreateRecords(ctx context.Context, records []Record, limit int) (int, error)
	// UpdateRecord replaces the record's values only while it was last changed at
	// expectedUpdatedAt.
	UpdateRecord(ctx context.Context, record Record, expectedUpdatedAt time.Time) (bool, error)
	DeleteRecord(ctx context.Context, principalID, recordID string) (bool, error)
	// DeleteUnchangedRecords deletes the records that still exist, only while each was last
	// changed at its given time, all or none; false means one was changed since. It returns how
	// many it deleted.
	DeleteUnchangedRecords(ctx context.Context, principalID string, records map[string]time.Time) (int, bool, error)
}

// Proposer is the core approval service as collections see it.
type Proposer interface {
	Propose(context.Context, string, approval.Proposal) (approval.Record, error)
	Act(context.Context, string, approval.Proposal) (string, error)
}

type Config struct {
	// Location is the household's time zone, which local dates and times are read in.
	Location *time.Location
	Now      func() time.Time
}

type Service struct {
	repository Repository
	proposer   Proposer
	location   *time.Location
	now        func() time.Time
	logger     *slog.Logger
}

// NewService builds the collection service. A nil proposer leaves out the tools that change
// collections; /collections still deletes them.
func NewService(cfg Config, repository Repository, proposer Proposer, logger *slog.Logger) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("collection repository is required")
	}
	if logger == nil {
		return nil, fmt.Errorf("collection logger is required")
	}
	if cfg.Location == nil {
		return nil, fmt.Errorf("collection location is required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{repository: repository, proposer: proposer, location: cfg.Location, now: now, logger: logger}, nil
}

// stamp is now as the store keeps it, so a time read back compares equal to the one written.
func (s *Service) stamp() time.Time { return s.now().UTC().Truncate(time.Millisecond) }

// Collections lists the principal's collections, oldest first, with their record counts.
func (s *Service) Collections(ctx context.Context, principalID string) ([]Collection, error) {
	return s.repository.ListCollections(ctx, principalID)
}

// Collection returns one of the principal's collections with its records, oldest first.
func (s *Service) Collection(ctx context.Context, principalID, collectionID string) (Collection, []Record, bool, error) {
	collection, found, err := s.repository.LoadCollection(ctx, principalID, collectionID)
	if err != nil || !found {
		return Collection{}, nil, false, err
	}
	records, err := s.repository.ListRecords(ctx, principalID, collectionID)
	if err != nil {
		return Collection{}, nil, false, err
	}
	collection.Records = len(records)
	return collection, records, true, nil
}

// RemoveRecord deletes one of the principal's records and returns the collection it was in;
// false means it no longer exists.
func (s *Service) RemoveRecord(ctx context.Context, principalID, recordID string) (string, bool, error) {
	record, found, err := s.repository.LoadRecord(ctx, principalID, recordID)
	if err != nil || !found {
		return "", false, err
	}
	deleted, err := s.repository.DeleteRecord(ctx, principalID, recordID)
	if err == nil && deleted {
		s.logger.InfoContext(ctx, "collection.record.deleted", "principal_id", principalID, "collection_id", record.CollectionID, "record_id", recordID)
	}
	return record.CollectionID, deleted, err
}

// DeleteCollection deletes one of the principal's collections with its records; false means it no
// longer exists.
func (s *Service) DeleteCollection(ctx context.Context, principalID, collectionID string) (bool, error) {
	deleted, err := s.repository.DeleteCollection(ctx, principalID, collectionID)
	if err == nil && deleted {
		s.logger.InfoContext(ctx, "collection.deleted", "principal_id", principalID, "collection_id", collectionID)
	}
	return deleted, err
}

// RecordLine is a record's values in one line, as /collections lists it.
func (s *Service) RecordLine(collection Collection, record Record) string {
	return describeValues(collection, record.Values, s.location)
}

// TurnLine names the principal's collections with their record counts for the turn's message, or
// is "" without any. Fields and records stay with the tools.
func (s *Service) TurnLine(ctx context.Context, principalID string) (string, error) {
	collections, err := s.repository.ListCollections(ctx, principalID)
	if err != nil || len(collections) == 0 {
		return "", err
	}
	parts := make([]string, 0, len(collections))
	for _, collection := range collections {
		parts = append(parts, strconv.Quote(collection.Name)+" ("+countText(collection.Records)+")")
	}
	return "The user's collections, read with collection_query: " + strings.Join(parts, ", ") + ".", nil
}

func countText(records int) string {
	if records == 1 {
		return "1 record"
	}
	return strconv.Itoa(records) + " records"
}

// byName finds the collection called name, ignoring case.
func byName(collections []Collection, name string) (Collection, bool) {
	wanted := cleanLine(name)
	for _, collection := range collections {
		if strings.EqualFold(collection.Name, wanted) {
			return collection, true
		}
	}
	return Collection{}, false
}

func listNames(collections []Collection) string {
	names := make([]string, 0, len(collections))
	for _, collection := range collections {
		names = append(names, strconv.Quote(collection.Name))
	}
	return strings.Join(names, ", ")
}
