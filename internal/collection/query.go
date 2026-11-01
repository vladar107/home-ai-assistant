package collection

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	OpEqual    = "="
	OpNotEqual = "!="
	OpLess     = "<"
	OpGreater  = ">"
	OpContains = "contains"
	OpEmpty    = "empty"
	OpNotEmpty = "not_empty"

	emptyGroup = "(empty)"
	// maximumGroups is how many groups a query lists; the rest are counted.
	maximumGroups = 50
)

// Filter keeps the records whose field compares to Value under Op.
type Filter struct {
	Field string          `json:"field"`
	Op    string          `json:"op"`
	Value json.RawMessage `json:"value,omitempty"`
}

// Sort orders the matches by a field; records without a value come last either way.
type Sort struct {
	Field      string `json:"field"`
	Descending bool   `json:"descending,omitempty"`
}

// Query is what collection_query asks of one collection. A nil Limit lists MaximumQueryRecords.
type Query struct {
	Filters []Filter `json:"filters,omitempty"`
	Sort    *Sort    `json:"sort,omitempty"`
	Limit   *int     `json:"limit,omitempty"`
	GroupBy string   `json:"group_by,omitempty"`
}

// Total sums one numeric field over the records that have a value for it; money is totalled per
// currency.
type Total struct {
	Field    Field
	Currency string
	Count    int
	Sum      float64
	Min      float64
	Max      float64
}

func (t Total) Mean() float64 { return t.Sum / float64(t.Count) }

// Group is the matches that share one value of the group_by field.
type Group struct {
	Label  string
	Count  int
	Totals []Total
}

// Result is a query's answer: how many records match, the listed ones in order, the totals over
// every match, and the groups.
type Result struct {
	Collection Collection
	Matches    int
	Records    []Record
	Totals     []Total
	Grouped    *Field
	Groups     []Group
	// MoreGroups counts the groups past maximumGroups.
	MoreGroups int
}

type compiledFilter struct {
	field Field
	op    string
	value Value
	// dateOnly compares a datetime field by its local day.
	dateOnly bool
}

// Run answers the query over the collection's records, which the caller loaded oldest first. An
// error is worded for the model and names the field at fault.
func Run(c Collection, records []Record, query Query, location *time.Location) (Result, error) {
	filters := make([]compiledFilter, 0, len(query.Filters))
	for _, filter := range query.Filters {
		compiled, err := compileFilter(c, filter, location)
		if err != nil {
			return Result{}, err
		}
		filters = append(filters, compiled)
	}
	limit := MaximumQueryRecords
	if query.Limit != nil {
		if *query.Limit < 0 || *query.Limit > MaximumQueryRecords {
			return Result{}, fmt.Errorf("limit is 0 to %d", MaximumQueryRecords)
		}
		limit = *query.Limit
	}
	var sortField *Field
	if query.Sort != nil {
		field, found := c.field(query.Sort.Field)
		if !found {
			return Result{}, c.unknownField(query.Sort.Field)
		}
		sortField = &field
	}
	result := Result{Collection: c}
	if strings.TrimSpace(query.GroupBy) != "" {
		field, found := c.field(query.GroupBy)
		if !found {
			return Result{}, c.unknownField(query.GroupBy)
		}
		result.Grouped = &field
	}

	var matches []Record
	for _, record := range records {
		if slices.IndexFunc(filters, func(filter compiledFilter) bool { return !filter.keeps(record, location) }) < 0 {
			matches = append(matches, record)
		}
	}
	if sortField != nil {
		slices.SortStableFunc(matches, func(a, b Record) int {
			left, leftSet := decodeValue(*sortField, a.Values[sortField.Key])
			right, rightSet := decodeValue(*sortField, b.Values[sortField.Key])
			switch {
			case !leftSet && !rightSet:
				return 0
			case !leftSet:
				return 1
			case !rightSet:
				return -1
			}
			order := compare(*sortField, left, right)
			if query.Sort.Descending {
				order = -order
			}
			return order
		})
	}
	result.Matches = len(matches)
	result.Records = matches[:min(limit, len(matches))]
	result.Totals = totals(c, matches)
	if result.Grouped != nil {
		result.Groups, result.MoreGroups = groups(c, *result.Grouped, matches, location)
	}
	return result, nil
}

func compileFilter(c Collection, filter Filter, location *time.Location) (compiledFilter, error) {
	field, found := c.field(filter.Field)
	if !found {
		return compiledFilter{}, c.unknownField(filter.Field)
	}
	compiled := compiledFilter{field: field, op: strings.ToLower(strings.TrimSpace(filter.Op))}
	refuse := func(format string, arguments ...any) error {
		return fmt.Errorf("filter on %q: "+format, append([]any{field.Name}, arguments...)...)
	}
	switch compiled.op {
	case OpEmpty, OpNotEmpty:
		return compiled, nil
	case OpEqual, OpNotEqual:
	case OpLess, OpGreater:
		if field.Type != TypeNumber && field.Type != TypeMoney && field.Type != TypeDate && field.Type != TypeDateTime {
			return compiledFilter{}, refuse("< and > work on number, money, date, and datetime fields")
		}
	case OpContains:
		if field.Type != TypeText && field.Type != TypePerson && field.Type != TypeChoice {
			return compiledFilter{}, refuse("contains works on text, person, and choice fields")
		}
	default:
		return compiledFilter{}, refuse("op must be =, !=, <, >, contains, empty, or not_empty")
	}
	raw := bytes.TrimSpace(filter.Value)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return compiledFilter{}, refuse("give a value, or use the op empty")
	}
	switch field.Type {
	case TypeMoney:
		amount, ok := parseMoney(raw, false)
		if !ok {
			return compiledFilter{}, refuse(`the value is an amount, or {"amount":12.5,"currency":"EUR"}`)
		}
		compiled.value = Value{Number: amount.Amount, Currency: amount.Currency}
	case TypeDateTime:
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return compiledFilter{}, refuse("the value is a date, YYYY-MM-DD, or a local date and time, YYYY-MM-DDTHH:MM")
		}
		text = strings.TrimSpace(text)
		if day, err := time.Parse(dateLayout, text); err == nil {
			compiled.value, compiled.dateOnly = Value{Text: day.Format(dateLayout)}, true
			break
		}
		parsed, ok := parseDateTime(text, location)
		if !ok {
			return compiledFilter{}, refuse("the value is a date, YYYY-MM-DD, or a local date and time, YYYY-MM-DDTHH:MM")
		}
		compiled.value = Value{Time: parsed}
	case TypeText, TypePerson, TypeChoice:
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return compiledFilter{}, refuse("the value is text")
		}
		compiled.value = Value{Text: cleanLine(text)}
	default:
		normalized, err := normalizeValue(field, raw, location)
		if err != nil {
			return compiledFilter{}, refuse("%s", err.Error())
		}
		if normalized == nil {
			return compiledFilter{}, refuse("give a value, or use the op empty")
		}
		compiled.value, _ = decodeValue(field, normalized)
	}
	return compiled, nil
}

func (f compiledFilter) keeps(record Record, location *time.Location) bool {
	value, set := decodeValue(f.field, record.Values[f.field.Key])
	switch f.op {
	case OpEmpty:
		return !set
	case OpNotEmpty:
		return set
	case OpNotEqual:
		return !set || !f.equal(value, location)
	}
	if !set {
		return false
	}
	switch f.op {
	case OpEqual:
		return f.equal(value, location)
	case OpContains:
		return strings.Contains(strings.ToLower(value.Text), strings.ToLower(f.value.Text))
	}
	if f.field.Type == TypeMoney && f.value.Currency != "" && f.value.Currency != value.Currency {
		return false
	}
	order := f.order(value, location)
	if f.op == OpLess {
		return order < 0
	}
	return order > 0
}

func (f compiledFilter) equal(value Value, location *time.Location) bool {
	switch f.field.Type {
	case TypeText, TypePerson, TypeChoice:
		return strings.EqualFold(value.Text, f.value.Text)
	case TypeMoney:
		return (f.value.Currency == "" || f.value.Currency == value.Currency) && f.order(value, location) == 0
	default:
		return f.order(value, location) == 0
	}
}

// order compares a record's value with the filter's.
func (f compiledFilter) order(value Value, location *time.Location) int {
	if f.dateOnly {
		return strings.Compare(value.Time.In(location).Format(dateLayout), f.value.Text)
	}
	return compare(f.field, value, f.value)
}

func compare(field Field, a, b Value) int {
	switch field.Type {
	case TypeNumber:
		return cmp.Compare(a.Number, b.Number)
	case TypeMoney:
		return cmp.Compare(money{Amount: a.Number}.cents(), money{Amount: b.Number}.cents())
	case TypeDateTime:
		return a.Time.Compare(b.Time)
	case TypeBool:
		switch {
		case a.Bool == b.Bool:
			return 0
		case a.Bool:
			return 1
		default:
			return -1
		}
	case TypeDate:
		return strings.Compare(a.Text, b.Text)
	default:
		return strings.Compare(strings.ToLower(a.Text), strings.ToLower(b.Text))
	}
}

// totals sums every number and money field over the records, money per currency in cents so the
// sums do not drift.
func totals(c Collection, records []Record) []Total {
	var result []Total
	for _, field := range c.Fields {
		if field.Type != TypeNumber && field.Type != TypeMoney {
			continue
		}
		byCurrency := map[string]*Total{}
		var currencies []string
		cents := map[string]int64{}
		for _, record := range records {
			value, set := decodeValue(field, record.Values[field.Key])
			if !set {
				continue
			}
			total, found := byCurrency[value.Currency]
			if !found {
				total = &Total{Field: field, Currency: value.Currency, Min: value.Number, Max: value.Number}
				byCurrency[value.Currency] = total
				currencies = append(currencies, value.Currency)
			}
			total.Count++
			total.Min, total.Max = min(total.Min, value.Number), max(total.Max, value.Number)
			if field.Type == TypeMoney {
				cents[value.Currency] += money{Amount: value.Number}.cents()
			} else {
				total.Sum += value.Number
			}
		}
		slices.Sort(currencies)
		for _, currency := range currencies {
			total := byCurrency[currency]
			if field.Type == TypeMoney {
				total.Sum = float64(cents[currency]) / 100
			}
			result = append(result, *total)
		}
	}
	return result
}

// groups splits the matches by the field's value, text compared without case and a datetime by
// its local day, largest group first.
func groups(c Collection, field Field, records []Record, location *time.Location) ([]Group, int) {
	type bucket struct {
		label   string
		records []Record
	}
	var order []string
	buckets := map[string]*bucket{}
	for _, record := range records {
		label := emptyGroup
		if value, set := decodeValue(field, record.Values[field.Key]); set {
			label = display(field, value, location)
			if field.Type == TypeDateTime {
				label = value.Time.In(location).Format(dateLayout)
			}
		}
		key := strings.ToLower(label)
		found, exists := buckets[key]
		if !exists {
			found = &bucket{label: label}
			buckets[key] = found
			order = append(order, key)
		}
		found.records = append(found.records, record)
	}
	result := make([]Group, 0, len(order))
	for _, key := range order {
		found := buckets[key]
		result = append(result, Group{Label: found.label, Count: len(found.records), Totals: totals(c, found.records)})
	}
	slices.SortStableFunc(result, func(a, b Group) int { return cmp.Compare(b.Count, a.Count) })
	if len(result) > maximumGroups {
		return result[:maximumGroups], len(result) - maximumGroups
	}
	return result, 0
}
