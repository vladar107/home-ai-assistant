package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/parcel"
)

var _ parcel.Repository = (*Store)(nil)

const parcelColumns = `id, principal_id, carrier, number, order_ref, title, sender, source, source_key, status,
	window_from, window_to, delivered_at, pickup_point, link, card_message_id, card_kind, created_at, updated_at, stopped_at`

func scanParcel(row rowScanner) (parcel.Parcel, error) {
	var item parcel.Parcel
	var source, status string
	var cardKind sql.NullString
	var cardMessage sql.NullInt64
	var created, updated string
	var from, to, delivered, stopped sql.NullString
	if err := row.Scan(&item.ID, &item.PrincipalID, &item.Carrier, &item.Number, &item.OrderRef, &item.Title, &item.Sender, &source, &item.SourceKey,
		&status, &from, &to, &delivered, &item.PickupPoint, &item.Link, &cardMessage, &cardKind, &created, &updated, &stopped); err != nil {
		return parcel.Parcel{}, err
	}
	item.Source, item.Status, item.CardKind, item.CardMessage = parcel.Source(source), parcel.Status(status), parcel.CardKind(cardKind.String), cardMessage.Int64
	var err error
	if item.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return parcel.Parcel{}, fmt.Errorf("parse parcel creation time: %w", err)
	}
	if item.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
		return parcel.Parcel{}, fmt.Errorf("parse parcel update time: %w", err)
	}
	for target, value := range map[*time.Time]sql.NullString{&item.WindowFrom: from, &item.WindowTo: to, &item.DeliveredAt: delivered, &item.StoppedAt: stopped} {
		if *target, err = parseOptionalTime(value); err != nil {
			return parcel.Parcel{}, err
		}
	}
	return item, nil
}

func newParcelID() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate parcel identifier: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func (s *Store) UpsertParcel(ctx context.Context, item parcel.Parcel) (parcel.Parcel, bool, error) {
	if s == nil || s.db == nil {
		return parcel.Parcel{}, false, fmt.Errorf("save parcel: SQLite store is not open")
	}
	if item.PrincipalID == "" {
		return parcel.Parcel{}, false, fmt.Errorf("save parcel: the parcel has no owner")
	}
	if err := item.Validate(); err != nil {
		return parcel.Parcel{}, false, fmt.Errorf("save parcel: %w", err)
	}
	item.Carrier = parcel.DefaultCarriers().Settle(item.Carrier, item.Number)
	if item.UpdatedAt.IsZero() {
		item.UpdatedAt = item.CreatedAt
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = item.UpdatedAt
	}
	if item.UpdatedAt.IsZero() {
		return parcel.Parcel{}, false, fmt.Errorf("save parcel: the parcel has no time")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return parcel.Parcel{}, false, fmt.Errorf("begin save parcel: %w", err)
	}
	defer tx.Rollback()
	existing, found, err := findParcel(ctx, tx, item)
	if err != nil {
		return parcel.Parcel{}, false, err
	}
	if found && existing.Stopped() {
		return existing, false, nil
	}
	if !found {
		stampDelivery(&item)
		if item.ID, err = newParcelID(); err != nil {
			return parcel.Parcel{}, false, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO parcels (id, principal_id, carrier, number, order_ref, title, sender, source, source_key, status,
			     window_from, window_to, delivered_at, pickup_point, link, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			item.ID, item.PrincipalID, item.Carrier, item.Number, item.OrderRef, item.Title, item.Sender, string(item.Source), item.SourceKey, string(item.Status),
			nullableTime(item.WindowFrom), nullableTime(item.WindowTo), nullableTime(item.DeliveredAt), item.PickupPoint, item.Link,
			formatTime(item.CreatedAt), formatTime(item.UpdatedAt)); err != nil {
			return parcel.Parcel{}, false, fmt.Errorf("insert parcel: %w", err)
		}
		item.CardMessage, item.CardKind, item.StoppedAt = 0, "", time.Time{}
		if err := tx.Commit(); err != nil {
			return parcel.Parcel{}, false, fmt.Errorf("commit save parcel: %w", err)
		}
		return item, true, nil
	}
	merged, err := foldOrder(ctx, tx, mergeParcel(existing, item))
	if err != nil {
		return parcel.Parcel{}, false, err
	}
	var cardMessage, cardKind any
	if merged.CardMessage != 0 {
		cardMessage, cardKind = merged.CardMessage, string(merged.CardKind)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE parcels SET carrier = ?, number = ?, order_ref = ?, title = ?, sender = ?, source = ?, source_key = ?, status = ?,
		     window_from = ?, window_to = ?, delivered_at = ?, pickup_point = ?, link = ?, card_message_id = ?, card_kind = ?, updated_at = ?
		 WHERE id = ? AND principal_id = ?`,
		merged.Carrier, merged.Number, merged.OrderRef, merged.Title, merged.Sender, string(merged.Source), merged.SourceKey, string(merged.Status),
		nullableTime(merged.WindowFrom), nullableTime(merged.WindowTo), nullableTime(merged.DeliveredAt), merged.PickupPoint, merged.Link,
		cardMessage, cardKind, formatTime(merged.UpdatedAt), merged.ID, merged.PrincipalID); err != nil {
		return parcel.Parcel{}, false, fmt.Errorf("update parcel: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return parcel.Parcel{}, false, fmt.Errorf("commit save parcel: %w", err)
	}
	return merged, false, nil
}

// findParcel looks for the stored parcel the incoming one is: by carrier and number, under any
// carrier the number's pattern shares with its own, then, for a parcel whose number was not known
// before, by sender and order reference, then by the source's key.
func findParcel(ctx context.Context, tx *sql.Tx, item parcel.Parcel) (parcel.Parcel, bool, error) {
	// A parcel that gained a number stays the match of a reading that still has none, so only a
	// reading with a number is held to a number-less stored parcel.
	unnumbered := ""
	if item.Number != "" {
		unnumbered = `number = '' AND `
	}
	twins := parcel.DefaultCarriers().Twins(item.Carrier, item.Number)
	byNumber := []any{item.Number}
	for _, carrier := range twins {
		byNumber = append(byNumber, carrier)
	}
	lookups := []struct {
		applies bool
		where   string
		args    []any
	}{
		{item.Number != "", `number = ? AND carrier IN (?` + strings.Repeat(", ?", len(twins)-1) + `)`, byNumber},
		{item.OrderRef != "" && item.Sender != "", unnumbered + `sender = ? AND order_ref = ?`, []any{item.Sender, item.OrderRef}},
		{item.SourceKey != "", unnumbered + `source = ? AND source_key = ?`, []any{string(item.Source), item.SourceKey}},
	}
	for _, lookup := range lookups {
		if !lookup.applies {
			continue
		}
		existing, err := scanParcel(tx.QueryRowContext(ctx,
			`SELECT `+parcelColumns+` FROM parcels WHERE principal_id = ? AND `+lookup.where+` ORDER BY created_at LIMIT 1`,
			append([]any{item.PrincipalID}, lookup.args...)...))
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return parcel.Parcel{}, false, fmt.Errorf("find parcel: %w", err)
		}
		return existing, true, nil
	}
	return parcel.Parcel{}, false, nil
}

// mergeParcel lays what the incoming parcel knows over the stored one: a field the incoming one
// leaves empty, and a status it does not know, keep the stored value.
func mergeParcel(existing, incoming parcel.Parcel) parcel.Parcel {
	merged := existing
	// A reading without a number, such as an order mail after a hand-off, says nothing about the
	// carrier or the tracking link of a parcel that has one. Nor does mail's guess between carriers
	// that share the number's pattern; only Home Assistant, which knows its own carrier, corrects
	// one that mail guessed.
	numbered := existing.Number != "" && incoming.Number == ""
	twin := existing.Number != "" && existing.Carrier != incoming.Carrier &&
		!(incoming.Source == parcel.SourceHomeAssistant && existing.Source == parcel.SourceMail)
	keep := numbered || twin
	for _, field := range []struct {
		target *string
		value  string
		skip   bool
	}{
		{&merged.Carrier, incoming.Carrier, keep}, {&merged.Number, incoming.Number, false}, {&merged.OrderRef, incoming.OrderRef, false},
		{&merged.Title, incoming.Title, false}, {&merged.Sender, incoming.Sender, false}, {&merged.PickupPoint, incoming.PickupPoint, false},
		{&merged.Link, incoming.Link, keep},
	} {
		if field.value != "" && !field.skip {
			*field.target = field.value
		}
	}
	if incoming.SourceKey != "" {
		merged.Source, merged.SourceKey = incoming.Source, incoming.SourceKey
	}
	if incoming.Status.Supersedes(merged.Status) {
		merged.Status = incoming.Status
	}
	if !incoming.WindowFrom.IsZero() || !incoming.WindowTo.IsZero() {
		merged.WindowFrom, merged.WindowTo = incoming.WindowFrom, incoming.WindowTo
	}
	if !incoming.DeliveredAt.IsZero() {
		merged.DeliveredAt = incoming.DeliveredAt
	}
	merged.UpdatedAt = incoming.UpdatedAt
	stampDelivery(&merged)
	return merged
}

// stampDelivery gives a delivered parcel the time it was first seen delivered when its source
// stated none, so retention has a time to count from.
func stampDelivery(item *parcel.Parcel) {
	if item.Status == parcel.StatusDelivered && item.DeliveredAt.IsZero() {
		item.DeliveredAt = item.UpdatedAt
	}
}

// foldOrder takes the number-less parcel of the merged parcel's order into it and deletes that
// parcel, which a hand-off left behind: what the survivor lacks is filled from it, including its
// card, so the card the user sees stays the one that is edited. It leaves no tombstone, since the
// survivor carries the order's keys.
func foldOrder(ctx context.Context, tx *sql.Tx, merged parcel.Parcel) (parcel.Parcel, error) {
	if merged.Number == "" || merged.OrderRef == "" || merged.Sender == "" {
		return merged, nil
	}
	orphan, err := scanParcel(tx.QueryRowContext(ctx,
		`SELECT `+parcelColumns+` FROM parcels
		 WHERE principal_id = ? AND id != ? AND number = '' AND sender = ? AND order_ref = ? AND stopped_at IS NULL ORDER BY created_at LIMIT 1`,
		merged.PrincipalID, merged.ID, merged.Sender, merged.OrderRef))
	if errors.Is(err, sql.ErrNoRows) {
		return merged, nil
	}
	if err != nil {
		return parcel.Parcel{}, fmt.Errorf("find order parcel: %w", err)
	}
	if merged.Title == "" {
		merged.Title = orphan.Title
	}
	if merged.WindowFrom.IsZero() && merged.WindowTo.IsZero() {
		merged.WindowFrom, merged.WindowTo = orphan.WindowFrom, orphan.WindowTo
	}
	if merged.CardMessage == 0 && orphan.CardMessage != 0 {
		merged.CardMessage, merged.CardKind = orphan.CardMessage, orphan.CardKind
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM parcels WHERE id = ? AND principal_id = ?`, orphan.ID, orphan.PrincipalID); err != nil {
		return parcel.Parcel{}, fmt.Errorf("delete folded parcel: %w", err)
	}
	return merged, nil
}

func (s *Store) LoadParcel(ctx context.Context, principalID, id string) (parcel.Parcel, bool, error) {
	if s == nil || s.db == nil {
		return parcel.Parcel{}, false, fmt.Errorf("load parcel: SQLite store is not open")
	}
	item, err := scanParcel(s.db.QueryRowContext(ctx, `SELECT `+parcelColumns+` FROM parcels WHERE id = ? AND principal_id = ?`, id, principalID))
	if errors.Is(err, sql.ErrNoRows) {
		return parcel.Parcel{}, false, nil
	}
	if err != nil {
		return parcel.Parcel{}, false, fmt.Errorf("load parcel: %w", err)
	}
	return item, true, nil
}

func (s *Store) ListParcels(ctx context.Context, principalID string) ([]parcel.Parcel, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list parcels: SQLite store is not open")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+parcelColumns+` FROM parcels WHERE principal_id = ? ORDER BY created_at, id`, principalID)
	if err != nil {
		return nil, fmt.Errorf("list parcels: %w", err)
	}
	defer rows.Close()
	var items []parcel.Parcel
	for rows.Next() {
		item, err := scanParcel(rows)
		if err != nil {
			return nil, fmt.Errorf("read parcel: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list parcels: %w", err)
	}
	return items, nil
}

func (s *Store) SaveCard(ctx context.Context, principalID, id string, messageID int64, kind parcel.CardKind) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("save parcel card: SQLite store is not open")
	}
	var message, cardKind any
	if messageID != 0 {
		message, cardKind = messageID, string(kind)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE parcels SET card_message_id = ?, card_kind = ? WHERE id = ? AND principal_id = ?`, message, cardKind, id, principalID); err != nil {
		return fmt.Errorf("save parcel card: %w", err)
	}
	return nil
}

func (s *Store) StopParcel(ctx context.Context, principalID, id string, at time.Time) (parcel.Parcel, bool, error) {
	if s == nil || s.db == nil {
		return parcel.Parcel{}, false, fmt.Errorf("stop parcel: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return parcel.Parcel{}, false, fmt.Errorf("begin stop parcel: %w", err)
	}
	defer tx.Rollback()
	item, err := scanParcel(tx.QueryRowContext(ctx, `SELECT `+parcelColumns+` FROM parcels WHERE id = ? AND principal_id = ?`, id, principalID))
	if errors.Is(err, sql.ErrNoRows) {
		return parcel.Parcel{}, false, nil
	}
	if err != nil {
		return parcel.Parcel{}, false, fmt.Errorf("load parcel to stop: %w", err)
	}
	if !item.Stopped() {
		if _, err := tx.ExecContext(ctx, `UPDATE parcels SET stopped_at = ?, updated_at = ? WHERE id = ? AND principal_id = ?`, formatTime(at), formatTime(at), id, principalID); err != nil {
			return parcel.Parcel{}, false, fmt.Errorf("stop parcel: %w", err)
		}
		item.StoppedAt, item.UpdatedAt = at.UTC(), at.UTC()
	}
	if err := leaveTombstones(ctx, tx, item, at); err != nil {
		return parcel.Parcel{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return parcel.Parcel{}, false, fmt.Errorf("commit stop parcel: %w", err)
	}
	return item, true, nil
}

// leaveTombstones remembers the parcel under every key it is known by.
func leaveTombstones(ctx context.Context, tx *sql.Tx, item parcel.Parcel, at time.Time) error {
	for _, key := range item.Keys() {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO parcel_tombstones (principal_id, key, created_at, last_seen_at) VALUES (?, ?, ?, ?)
			 ON CONFLICT (principal_id, key) DO UPDATE SET last_seen_at = max(last_seen_at, excluded.last_seen_at)`,
			item.PrincipalID, key, formatTime(at), formatTime(at)); err != nil {
			return fmt.Errorf("leave parcel tombstone: %w", err)
		}
	}
	return nil
}

func (s *Store) DeleteExpired(ctx context.Context, before, now time.Time) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("delete expired parcels: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin delete expired parcels: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx,
		`SELECT `+parcelColumns+` FROM parcels
		 WHERE stopped_at < ? OR (stopped_at IS NULL AND status = 'delivered' AND coalesce(delivered_at, updated_at) < ?)`,
		formatTime(before), formatTime(before))
	if err != nil {
		return 0, fmt.Errorf("find expired parcels: %w", err)
	}
	var expired []parcel.Parcel
	for rows.Next() {
		item, err := scanParcel(rows)
		if err != nil {
			rows.Close()
			return 0, fmt.Errorf("read expired parcel: %w", err)
		}
		expired = append(expired, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("find expired parcels: %w", err)
	}
	rows.Close()
	for _, item := range expired {
		if err := leaveTombstones(ctx, tx, item, now); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM parcels WHERE id = ? AND principal_id = ?`, item.ID, item.PrincipalID); err != nil {
			return 0, fmt.Errorf("delete expired parcel: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit delete expired parcels: %w", err)
	}
	return len(expired), nil
}

func (s *Store) DeleteIdle(ctx context.Context, principalID string, before time.Time) ([]parcel.Parcel, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("delete idle parcels: SQLite store is not open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin delete idle parcels: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx,
		`SELECT `+parcelColumns+` FROM parcels
		 WHERE principal_id = ? AND stopped_at IS NULL AND status != 'delivered' AND updated_at < ? ORDER BY created_at, id`,
		principalID, formatTime(before))
	if err != nil {
		return nil, fmt.Errorf("find idle parcels: %w", err)
	}
	var idle []parcel.Parcel
	for rows.Next() {
		item, err := scanParcel(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("read idle parcel: %w", err)
		}
		idle = append(idle, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("find idle parcels: %w", err)
	}
	rows.Close()
	for _, item := range idle {
		if _, err := tx.ExecContext(ctx, `DELETE FROM parcels WHERE id = ? AND principal_id = ?`, item.ID, principalID); err != nil {
			return nil, fmt.Errorf("delete idle parcel: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit delete idle parcels: %w", err)
	}
	return idle, nil
}

func (s *Store) TombstoneSeenAny(ctx context.Context, principalID string, keys []string, now time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("check parcel tombstone: SQLite store is not open")
	}
	seen := false
	for _, key := range keys {
		result, err := s.db.ExecContext(ctx,
			`UPDATE parcel_tombstones SET last_seen_at = max(last_seen_at, ?) WHERE principal_id = ? AND key = ?`, formatTime(now), principalID, key)
		if err != nil {
			return false, fmt.Errorf("check parcel tombstone: %w", err)
		}
		found, err := result.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("check parcel tombstone: %w", err)
		}
		seen = seen || found == 1
	}
	return seen, nil
}

func (s *Store) PruneTombstones(ctx context.Context, before time.Time) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("prune parcel tombstones: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM parcel_tombstones WHERE last_seen_at < ?`, formatTime(before))
	if err != nil {
		return 0, fmt.Errorf("prune parcel tombstones: %w", err)
	}
	dropped, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune parcel tombstones: %w", err)
	}
	return int(dropped), nil
}

func (s *Store) SaveAddedNumber(ctx context.Context, number parcel.AddedNumber) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("save added parcel number: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO parcel_added_numbers (principal_id, carrier, number, domain, entry_id, added_at) VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (principal_id, carrier, number) DO UPDATE SET domain = excluded.domain, entry_id = excluded.entry_id, added_at = excluded.added_at`,
		number.PrincipalID, number.Carrier, number.Number, number.Domain, number.EntryID, formatTime(number.AddedAt)); err != nil {
		return fmt.Errorf("save added parcel number: %w", err)
	}
	return nil
}

func (s *Store) LoadAddedNumber(ctx context.Context, principalID, carrier, number string) (parcel.AddedNumber, bool, error) {
	if s == nil || s.db == nil {
		return parcel.AddedNumber{}, false, fmt.Errorf("load added parcel number: SQLite store is not open")
	}
	added := parcel.AddedNumber{PrincipalID: principalID, Carrier: carrier, Number: number}
	var at string
	err := s.db.QueryRowContext(ctx,
		`SELECT domain, entry_id, added_at FROM parcel_added_numbers WHERE principal_id = ? AND carrier = ? AND number = ?`, principalID, carrier, number,
	).Scan(&added.Domain, &added.EntryID, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return parcel.AddedNumber{}, false, nil
	}
	if err != nil {
		return parcel.AddedNumber{}, false, fmt.Errorf("load added parcel number: %w", err)
	}
	if added.AddedAt, err = time.Parse(time.RFC3339Nano, at); err != nil {
		return parcel.AddedNumber{}, false, fmt.Errorf("parse added parcel number time: %w", err)
	}
	return added, true, nil
}

func (s *Store) ListAddedNumbers(ctx context.Context, principalID string) ([]parcel.AddedNumber, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("list added parcel numbers: SQLite store is not open")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT carrier, number, domain, entry_id, added_at FROM parcel_added_numbers WHERE principal_id = ? ORDER BY added_at, carrier, number`, principalID)
	if err != nil {
		return nil, fmt.Errorf("list added parcel numbers: %w", err)
	}
	defer rows.Close()
	var numbers []parcel.AddedNumber
	for rows.Next() {
		added := parcel.AddedNumber{PrincipalID: principalID}
		var at string
		if err := rows.Scan(&added.Carrier, &added.Number, &added.Domain, &added.EntryID, &at); err != nil {
			return nil, fmt.Errorf("read added parcel number: %w", err)
		}
		if added.AddedAt, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return nil, fmt.Errorf("parse added parcel number time: %w", err)
		}
		numbers = append(numbers, added)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list added parcel numbers: %w", err)
	}
	return numbers, nil
}

func (s *Store) DeleteAddedNumber(ctx context.Context, principalID, carrier, number string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("delete added parcel number: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM parcel_added_numbers WHERE principal_id = ? AND carrier = ? AND number = ?`, principalID, carrier, number); err != nil {
		return fmt.Errorf("delete added parcel number: %w", err)
	}
	return nil
}

func (s *Store) AddedNumberOwners(ctx context.Context, carrier, number string) ([]string, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("find added parcel number owners: SQLite store is not open")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT principal_id FROM parcel_added_numbers WHERE carrier = ? AND number = ? ORDER BY added_at, principal_id`, carrier, number)
	if err != nil {
		return nil, fmt.Errorf("find added parcel number owners: %w", err)
	}
	defer rows.Close()
	var owners []string
	for rows.Next() {
		var owner string
		if err := rows.Scan(&owner); err != nil {
			return nil, fmt.Errorf("read added parcel number owner: %w", err)
		}
		owners = append(owners, owner)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find added parcel number owners: %w", err)
	}
	return owners, nil
}

func (s *Store) RecordProposal(ctx context.Context, proposal parcel.ProposedNumber) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("record parcel proposal: SQLite store is not open")
	}
	if proposal.Outcome == "" {
		proposal.Outcome = parcel.OutcomeProposed
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO parcel_proposed_numbers (principal_id, number, carrier, outcome, approval_id, offers, proposed_at, updated_at) VALUES (?, ?, ?, ?, ?, 1, ?, ?)
		 ON CONFLICT (principal_id, number) DO UPDATE SET carrier = excluded.carrier, approval_id = excluded.approval_id,
		     offers = parcel_proposed_numbers.offers + 1, proposed_at = excluded.proposed_at, updated_at = excluded.updated_at
		 WHERE parcel_proposed_numbers.outcome = 'proposed'`,
		proposal.PrincipalID, proposal.Number, proposal.Carrier, string(proposal.Outcome), proposal.ApprovalID, formatTime(proposal.ProposedAt), formatTime(proposal.ProposedAt)); err != nil {
		return fmt.Errorf("record parcel proposal: %w", err)
	}
	return nil
}

func (s *Store) SetProposalOutcome(ctx context.Context, principalID, number string, outcome parcel.Outcome, at time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("set parcel proposal outcome: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO parcel_proposed_numbers (principal_id, number, outcome, offers, proposed_at, updated_at) VALUES (?, ?, ?, 0, ?, ?)
		 ON CONFLICT (principal_id, number) DO UPDATE SET outcome = excluded.outcome,
		     approval_id = CASE WHEN excluded.outcome = 'proposed' THEN parcel_proposed_numbers.approval_id ELSE '' END,
		     updated_at = excluded.updated_at`,
		principalID, number, string(outcome), formatTime(at), formatTime(at)); err != nil {
		return fmt.Errorf("set parcel proposal outcome: %w", err)
	}
	return nil
}

func (s *Store) LoadProposal(ctx context.Context, principalID, number string) (parcel.ProposedNumber, bool, error) {
	if s == nil || s.db == nil {
		return parcel.ProposedNumber{}, false, fmt.Errorf("load parcel proposal: SQLite store is not open")
	}
	proposal := parcel.ProposedNumber{PrincipalID: principalID, Number: number}
	var outcome, proposed, updated string
	err := s.db.QueryRowContext(ctx,
		`SELECT carrier, outcome, approval_id, offers, proposed_at, updated_at FROM parcel_proposed_numbers WHERE principal_id = ? AND number = ?`, principalID, number,
	).Scan(&proposal.Carrier, &outcome, &proposal.ApprovalID, &proposal.Offers, &proposed, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return parcel.ProposedNumber{}, false, nil
	}
	if err != nil {
		return parcel.ProposedNumber{}, false, fmt.Errorf("load parcel proposal: %w", err)
	}
	proposal.Outcome = parcel.Outcome(outcome)
	if proposal.ProposedAt, err = time.Parse(time.RFC3339Nano, proposed); err != nil {
		return parcel.ProposedNumber{}, false, fmt.Errorf("parse parcel proposal time: %w", err)
	}
	if proposal.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated); err != nil {
		return parcel.ProposedNumber{}, false, fmt.Errorf("parse parcel proposal time: %w", err)
	}
	return proposal, true, nil
}

func (s *Store) RecordNotice(ctx context.Context, principalID, parcelID string, status parcel.Status, at time.Time) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("record parcel notice: SQLite store is not open")
	}
	result, err := s.db.ExecContext(ctx,
		`INSERT INTO parcel_notices_sent (parcel_id, status, principal_id, sent_at)
		 SELECT id, ?, principal_id, ? FROM parcels WHERE id = ? AND principal_id = ?
		 ON CONFLICT (parcel_id, status) DO NOTHING`,
		string(status), formatTime(at), parcelID, principalID)
	if err != nil {
		return false, fmt.Errorf("record parcel notice: %w", err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record parcel notice: %w", err)
	}
	return inserted == 1, nil
}

func (s *Store) LoadMailCursor(ctx context.Context, principalID string) (parcel.MailCursor, bool, error) {
	if s == nil || s.db == nil {
		return parcel.MailCursor{}, false, fmt.Errorf("load parcel mail cursor: SQLite store is not open")
	}
	var cursor parcel.MailCursor
	var processed string
	err := s.db.QueryRowContext(ctx, `SELECT after_epoch, processed FROM parcel_mail_cursors WHERE principal_id = ?`, principalID).Scan(&cursor.AfterEpoch, &processed)
	if errors.Is(err, sql.ErrNoRows) {
		return parcel.MailCursor{}, false, nil
	}
	if err != nil {
		return parcel.MailCursor{}, false, fmt.Errorf("load parcel mail cursor: %w", err)
	}
	if err := json.Unmarshal([]byte(processed), &cursor.Processed); err != nil {
		return parcel.MailCursor{}, false, fmt.Errorf("decode parcel mail cursor: %w", err)
	}
	return cursor, true, nil
}

func (s *Store) SaveMailCursor(ctx context.Context, principalID string, cursor parcel.MailCursor, at time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("save parcel mail cursor: SQLite store is not open")
	}
	if cursor.Processed == nil {
		cursor.Processed = []string{}
	}
	processed, err := json.Marshal(cursor.Processed)
	if err != nil {
		return fmt.Errorf("encode parcel mail cursor: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO parcel_mail_cursors (principal_id, after_epoch, processed, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (principal_id) DO UPDATE SET after_epoch = excluded.after_epoch, processed = excluded.processed, updated_at = excluded.updated_at`,
		principalID, cursor.AfterEpoch, string(processed), formatTime(at)); err != nil {
		return fmt.Errorf("save parcel mail cursor: %w", err)
	}
	return nil
}

func (s *Store) LoadSettings(ctx context.Context, principalID string) (parcel.Settings, error) {
	if s == nil || s.db == nil {
		return parcel.Settings{}, fmt.Errorf("load parcel settings: SQLite store is not open")
	}
	settings := parcel.Settings{PrincipalID: principalID}
	err := s.db.QueryRowContext(ctx, `SELECT postcode FROM parcel_settings WHERE principal_id = ?`, principalID).Scan(&settings.Postcode)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return parcel.Settings{}, fmt.Errorf("load parcel settings: %w", err)
	}
	return settings, nil
}

func (s *Store) SaveSettings(ctx context.Context, settings parcel.Settings) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("save parcel settings: SQLite store is not open")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO parcel_settings (principal_id, postcode) VALUES (?, ?)
		 ON CONFLICT (principal_id) DO UPDATE SET postcode = excluded.postcode`,
		settings.PrincipalID, settings.Postcode); err != nil {
		return fmt.Errorf("save parcel settings: %w", err)
	}
	return nil
}

func (s *Store) LoadTracking(ctx context.Context, principalID string) (parcel.Tracking, error) {
	if s == nil || s.db == nil {
		return parcel.Tracking{}, fmt.Errorf("load parcel tracking: SQLite store is not open")
	}
	tracking := parcel.Tracking{PrincipalID: principalID}
	var baselined sql.NullString
	var off, owed int
	err := s.db.QueryRowContext(ctx, `SELECT baselined_at, sources_fingerprint, cards_off, sources_owed FROM parcel_tracking WHERE principal_id = ?`, principalID).
		Scan(&baselined, &tracking.Fingerprint, &off, &owed)
	if errors.Is(err, sql.ErrNoRows) {
		return tracking, nil
	}
	if err != nil {
		return parcel.Tracking{}, fmt.Errorf("load parcel tracking: %w", err)
	}
	tracking.CardsOff, tracking.SourcesOwed = off == 1, owed == 1
	if baselined.Valid {
		if tracking.BaselinedAt, err = time.Parse(time.RFC3339Nano, baselined.String); err != nil {
			return parcel.Tracking{}, fmt.Errorf("parse parcel baseline time: %w", err)
		}
	}
	return tracking, nil
}

func (s *Store) SaveTracking(ctx context.Context, tracking parcel.Tracking) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("save parcel tracking: SQLite store is not open")
	}
	var baselined any
	if !tracking.BaselinedAt.IsZero() {
		baselined = formatTime(tracking.BaselinedAt)
	}
	off, owed := 0, 0
	if tracking.CardsOff {
		off = 1
	}
	if tracking.SourcesOwed {
		owed = 1
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO parcel_tracking (principal_id, baselined_at, sources_fingerprint, cards_off, sources_owed) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (principal_id) DO UPDATE SET baselined_at = excluded.baselined_at,
		   sources_fingerprint = excluded.sources_fingerprint, cards_off = excluded.cards_off, sources_owed = excluded.sources_owed`,
		tracking.PrincipalID, baselined, tracking.Fingerprint, off, owed); err != nil {
		return fmt.Errorf("save parcel tracking: %w", err)
	}
	return nil
}
