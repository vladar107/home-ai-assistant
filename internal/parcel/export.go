package parcel

import (
	"context"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/export"
)

const (
	parcelsTable          = "parcels"
	addedNumbersTable     = "parcel_added_numbers"
	proposedNumbersTable  = "parcel_proposed_numbers"
	tombstonesTable       = "parcel_tombstones"
	settingsTable         = "parcel_settings"
	maximumPostcodeLength = 16
)

// Exporter is the parcels section of a user's export: the parcels with their facts, the numbers
// added and proposed, the tombstones, and the postcode. The cards' messages are left out, since
// they mean something only in one chat, and so are stopped parcels, which their tombstones stand
// for. The notices sent and how far the mail job has read are not in the section.
func Exporter() export.Exporter { return exporter{} }

type exporter struct{}

func (exporter) Section() export.Section {
	return export.Section{Name: "parcels", Label: "Parcels you are waiting for", Version: 1, Tables: []export.Table{
		{Name: parcelsTable, Omit: []string{"card_message_id", "card_kind"}},
		{Name: addedNumbersTable},
		{Name: proposedNumbersTable, Omit: []string{"approval_id"}},
		{Name: tombstonesTable},
		{Name: settingsTable},
	}}
}

func (exporter) Keep(table string, row export.Row) bool {
	return table != parcelsTable || row.Null("stopped_at")
}

// Plan adds each row the principal does not hold yet, found by its natural key, through the same
// validation the store's limits express.
func (exporter) Plan(ctx context.Context, plan *export.Planning, _ int, tables export.Tables) error {
	if err := planParcels(ctx, plan, tables[parcelsTable], tables[tombstonesTable]); err != nil {
		return err
	}
	if err := planNumbers(ctx, plan, addedNumbersTable, tables[addedNumbersTable], func(row export.Row) (string, string) {
		return row.Text("carrier") + "|" + row.Text("number"), row.Text("number")
	}, func(row export.Row) bool {
		_, timeOK := row.Time("added_at")
		return timeOK && carrierPattern.MatchString(row.Text("carrier")) && row.Text("domain") != "" && row.Text("entry_id") != "" &&
			utf8.RuneCountInString(row.Text("domain")) <= 64 && utf8.RuneCountInString(row.Text("entry_id")) <= 128
	}); err != nil {
		return err
	}
	if err := planNumbers(ctx, plan, proposedNumbersTable, tables[proposedNumbersTable], func(row export.Row) (string, string) {
		return row.Text("number"), row.Text("number")
	}, func(row export.Row) bool {
		_, proposedOK := row.Time("proposed_at")
		_, updatedOK := row.Time("updated_at")
		outcome := Outcome(row.Text("outcome"))
		offers, offersOK := row.Int("offers")
		return proposedOK && updatedOK && offersOK && offers >= 0 && offers <= 10000 && (outcome == OutcomeProposed || outcome == OutcomeAdded || outcome == OutcomeDeclined) &&
			(row.Text("carrier") == "" || carrierPattern.MatchString(row.Text("carrier")))
	}); err != nil {
		return err
	}
	if err := planTombstones(ctx, plan, tables[tombstonesTable]); err != nil {
		return err
	}
	return planSettings(ctx, plan, tables[settingsTable])
}

func parcelFromRow(row export.Row) Parcel {
	return Parcel{
		Carrier: row.Text("carrier"), Number: row.Text("number"), OrderRef: row.Text("order_ref"), Title: row.Text("title"),
		Sender: row.Text("sender"), Source: Source(row.Text("source")), SourceKey: row.Text("source_key"),
		Status: Status(row.Text("status")), PickupPoint: row.Text("pickup_point"), Link: row.Text("link"),
	}
}

func planParcels(ctx context.Context, plan *export.Planning, rows, filed []export.Row) error {
	current, err := plan.Current(ctx, parcelsTable)
	if err != nil {
		return err
	}
	held := map[string]bool{}
	for _, row := range current {
		for _, key := range parcelFromRow(row).Keys() {
			held[key] = true
		}
	}
	stopped, err := tombstonedKeys(ctx, plan, filed)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.Text("id"))
	}
	assigned, err := plan.Assign(ctx, parcelsTable, ids)
	if err != nil {
		return err
	}
	for _, row := range rows {
		parcel := parcelFromRow(row)
		title := export.Quote(parcel.Title)
		if parcel.Title == "" {
			title = export.Quote(parcel.Carrier + " parcel")
		}
		if assigned[row.Text("id")].Present {
			plan.Present()
			continue
		}
		_, createdOK := row.Time("created_at")
		_, updatedOK := row.Time("updated_at")
		if reason := invalidParcel(row, parcel); reason != "" || !createdOK || !updatedOK || row.Text("id") == "" {
			plan.Skip(title, "it could not be read")
			continue
		}
		if anyKey(stopped, parcel.Keys()) {
			plan.Skip(title, "stopped")
			continue
		}
		if anyKey(held, parcel.Keys()) {
			plan.Present()
			continue
		}
		write := row.Clone()
		write["id"] = assigned[row.Text("id")].ID
		plan.Add(parcelsTable, write)
		plan.Adding(title)
		for _, key := range parcel.Keys() {
			held[key] = true
		}
	}
	return nil
}

// tombstonedKeys is every key the principal's tombstones hold now, with those in the file.
func tombstonedKeys(ctx context.Context, plan *export.Planning, filed []export.Row) (map[string]bool, error) {
	current, err := plan.Current(ctx, tombstonesTable)
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for _, row := range slices.Concat(current, filed) {
		keys[row.Text("key")] = true
	}
	return keys, nil
}

func anyKey(held map[string]bool, keys []string) bool {
	return slices.ContainsFunc(keys, func(key string) bool { return held[key] })
}

func invalidParcel(row export.Row, parcel Parcel) string {
	if err := parcel.Validate(); err != nil {
		return err.Error()
	}
	for _, column := range []string{"window_from", "window_to", "delivered_at", "stopped_at"} {
		if row.Null(column) {
			continue
		}
		if _, ok := row.Time(column); !ok {
			return "a time could not be read"
		}
	}
	return ""
}

// planNumbers adds the rows of a table keyed by a natural key the principal does not hold. key
// returns the row's key and the title shown; valid checks the row's other columns.
func planNumbers(ctx context.Context, plan *export.Planning, table string, rows []export.Row, key func(export.Row) (string, string), valid func(export.Row) bool) error {
	current, err := plan.Current(ctx, table)
	if err != nil {
		return err
	}
	held := map[string]bool{}
	for _, row := range current {
		identity, _ := key(row)
		held[identity] = true
	}
	for _, row := range rows {
		identity, title := key(row)
		number, normalised := NormalizeNumber(row.Text("number"))
		switch {
		case !normalised || number != row.Text("number") || !valid(row):
			plan.Skip(export.Quote(title), "it could not be read")
		case held[identity]:
			plan.Present()
		default:
			plan.Add(table, row.Clone())
			plan.Adding(export.Quote(title))
			held[identity] = true
		}
	}
	return nil
}

func planTombstones(ctx context.Context, plan *export.Planning, rows []export.Row) error {
	current, err := plan.Current(ctx, tombstonesTable)
	if err != nil {
		return err
	}
	held := map[string]bool{}
	for _, row := range current {
		held[row.Text("key")] = true
	}
	for _, row := range rows {
		key := row.Text("key")
		_, createdOK := row.Time("created_at")
		_, seenOK := row.Time("last_seen_at")
		switch {
		case key == "" || utf8.RuneCountInString(key) > 300 || !createdOK || !seenOK:
			plan.Skip("a parcel you stopped tracking", "it could not be read")
		case held[key]:
			plan.Present()
		default:
			plan.Add(tombstonesTable, row.Clone())
			plan.Adding("a parcel you stopped tracking")
			held[key] = true
		}
	}
	return nil
}

func planSettings(ctx context.Context, plan *export.Planning, rows []export.Row) error {
	current, err := plan.Current(ctx, settingsTable)
	if err != nil {
		return err
	}
	held := len(current) > 0
	for _, row := range rows {
		postcode := row.Text("postcode")
		switch {
		case utf8.RuneCountInString(postcode) > maximumPostcodeLength || strings.TrimSpace(postcode) != postcode:
			plan.Skip("your postcode", "it could not be read")
		case held || postcode == "":
			plan.Present()
		default:
			plan.Add(settingsTable, row.Clone())
			plan.Adding("your postcode")
			held = true
		}
	}
	return nil
}
