// Package collection keeps each user's typed lists, such as things lent out or gift ideas: a
// collection has a schema of typed fields, and its records hold values checked against it. The
// model changes them through tools, filtering, sorting, and totals are done in code, and the user
// sees and deletes them with /collections. Only collection names and record counts reach the turn.
package collection

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	TypeText     = "text"
	TypeNumber   = "number"
	TypeMoney    = "money"
	TypeDate     = "date"
	TypeDateTime = "datetime"
	TypeBool     = "bool"
	TypeChoice   = "choice"
	TypePerson   = "person"

	MaximumCollectionsPerPrincipal = 20
	MaximumFields                  = 20
	MaximumRecords                 = 2000
	MaximumOptions                 = 20
	// MaximumQueryRecords is how many records one query lists; its totals cover every match.
	MaximumQueryRecords = 50

	maximumNameRunes        = 60
	maximumDescriptionRunes = 300
	maximumFieldNameRunes   = 40
	maximumOptionRunes      = 60
	maximumTextRunes        = 500
	// maximumMagnitude bounds numbers and amounts, so sums in cents stay exact in an int64.
	maximumMagnitude = 1e12

	dateLayout     = "2006-01-02"
	dateTimeLayout = "2006-01-02 15:04"
)

var types = []string{TypeText, TypeNumber, TypeMoney, TypeDate, TypeDateTime, TypeBool, TypeChoice, TypePerson}

var (
	// ErrTooManyCollections reports that the principal already holds MaximumCollectionsPerPrincipal.
	ErrTooManyCollections = errors.New("the principal already has the maximum number of collections")
	// ErrNameTaken reports that another of the principal's collections has the name.
	ErrNameTaken = errors.New("the principal already has a collection with that name")
	// ErrTooManyRecords reports that a collection would hold more than MaximumRecords.
	ErrTooManyRecords = errors.New("the collection already has the maximum number of records")
	// ErrCollectionGone reports that the collection a record belongs to no longer exists.
	ErrCollectionGone = errors.New("the collection no longer exists")
	// ErrFieldInUse reports that a record holds a value in a field a change would drop.
	ErrFieldInUse = errors.New("a record holds a value in a field the change would drop")
)

// Field is one typed field of a collection. Key is what record values are stored under, so a
// rename leaves the records alone; it is never shown to the model.
type Field struct {
	Key      string   `json:"key"`
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Required bool     `json:"required,omitempty"`
	Options  []string `json:"options,omitempty"`
}

// Collection is one principal's stored collection. Records is how many records it holds, as
// listed.
type Collection struct {
	ID          string
	PrincipalID string
	Name        string
	Description string
	Fields      []Field
	// NextKey is the number the key of the next field added takes. It only grows, so a field
	// added after a removal never takes over a key whose values were dropped.
	NextKey   int
	Records   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Record is one record of a collection; Values maps field keys to their canonical JSON values.
type Record struct {
	ID           string
	PrincipalID  string
	CollectionID string
	Values       map[string]json.RawMessage
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// field returns the collection's field called name, ignoring case.
func (c Collection) field(name string) (Field, bool) {
	wanted := cleanLine(name)
	for _, field := range c.Fields {
		if strings.EqualFold(field.Name, wanted) {
			return field, true
		}
	}
	return Field{}, false
}

func (c Collection) fieldNames() string {
	names := make([]string, 0, len(c.Fields))
	for _, field := range c.Fields {
		names = append(names, strconv.Quote(field.Name))
	}
	return strings.Join(names, ", ")
}

func (c Collection) unknownField(name string) error {
	return fmt.Errorf("the collection %s has no field called %q; its fields are %s", c.Name, cleanLine(name), c.fieldNames())
}

// Describe is the field as the model and the cards read it, such as "lent on: date, required".
func (f Field) Describe() string {
	text := f.Name + ": " + f.Type
	if f.Type == TypeChoice {
		text += " (" + strings.Join(f.Options, ", ") + ")"
	}
	if f.Required {
		text += ", required"
	}
	return text
}

func cleanLine(value string) string { return strings.Join(strings.Fields(value), " ") }

func plainText(value string, limit int, what string) (string, error) {
	text := cleanLine(value)
	if utf8.RuneCountInString(text) > limit {
		return "", fmt.Errorf("%s has at most %d characters", what, limit)
	}
	if strings.IndexFunc(text, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("%s must be plain text", what)
	}
	return text, nil
}

func normalizeName(value string) (string, error) {
	name, err := plainText(value, maximumNameRunes, "a collection name")
	if err != nil {
		return "", err
	}
	if name == "" {
		return "", fmt.Errorf("a collection needs a name, such as lent items")
	}
	return name, nil
}

func normalizeDescription(value string) (string, error) {
	return plainText(value, maximumDescriptionRunes, "a description")
}

// normalizeField checks a new or changed field; its key is the caller's.
func normalizeField(field Field) (Field, error) {
	name, err := plainText(field.Name, maximumFieldNameRunes, "a field name")
	if err != nil {
		return Field{}, err
	}
	if name == "" {
		return Field{}, fmt.Errorf("every field needs a name")
	}
	kind := strings.ToLower(strings.TrimSpace(field.Type))
	if !slices.Contains(types, kind) {
		return Field{}, fmt.Errorf("field %q: type must be one of %s", name, strings.Join(types, ", "))
	}
	result := Field{Key: field.Key, Name: name, Type: kind, Required: field.Required}
	if kind != TypeChoice {
		if len(field.Options) > 0 {
			return Field{}, fmt.Errorf("field %q: only a choice field has options", name)
		}
		return result, nil
	}
	if len(field.Options) == 0 || len(field.Options) > MaximumOptions {
		return Field{}, fmt.Errorf("field %q: a choice field has 1 to %d options", name, MaximumOptions)
	}
	for _, value := range field.Options {
		option, err := plainText(value, maximumOptionRunes, fmt.Sprintf("field %q: an option", name))
		if err != nil {
			return Field{}, err
		}
		if option == "" {
			return Field{}, fmt.Errorf("field %q: an option is empty", name)
		}
		if slices.ContainsFunc(result.Options, func(existing string) bool { return strings.EqualFold(existing, option) }) {
			return Field{}, fmt.Errorf("field %q: the option %q is listed twice", name, option)
		}
		result.Options = append(result.Options, option)
	}
	return result, nil
}

// keyNumber is the number of a field key such as f3, or 0.
func keyNumber(key string) int {
	number, err := strconv.Atoi(strings.TrimPrefix(key, "f"))
	if err != nil || !strings.HasPrefix(key, "f") || number < 1 {
		return 0
	}
	return number
}

func fieldKey(number int) string { return "f" + strconv.Itoa(number) }

// normalizeFields checks a whole schema: 1 to MaximumFields fields with distinct names and keys,
// giving each field without a key the next one from next. It returns the schema and the next key
// number after it, which is never below next.
func normalizeFields(fields []Field, next int) ([]Field, int, error) {
	if len(fields) == 0 || len(fields) > MaximumFields {
		return nil, 0, fmt.Errorf("a collection has 1 to %d fields", MaximumFields)
	}
	next = max(next, 1)
	for _, field := range fields {
		next = max(next, keyNumber(field.Key)+1)
	}
	result := make([]Field, 0, len(fields))
	for _, field := range fields {
		normalized, err := normalizeField(field)
		if err != nil {
			return nil, 0, err
		}
		if normalized.Key == "" {
			normalized.Key = fieldKey(next)
			next++
		} else if keyNumber(normalized.Key) == 0 {
			return nil, 0, fmt.Errorf("a field has a malformed key")
		}
		for _, existing := range result {
			if strings.EqualFold(existing.Name, normalized.Name) {
				return nil, 0, fmt.Errorf("two fields are called %q", normalized.Name)
			}
			if existing.Key == normalized.Key {
				return nil, 0, fmt.Errorf("two fields share a key")
			}
		}
		result = append(result, normalized)
	}
	return result, next, nil
}

// Value is a decoded record value; which part is set depends on the field's type.
type Value struct {
	// Text is a text, person, choice, or date value.
	Text string
	// Number is a number, or a money amount.
	Number   float64
	Currency string
	Bool     bool
	// Time is a datetime value.
	Time time.Time
}

// normalizeValue checks a value the model gave against its field and returns its canonical JSON,
// or nil for null or an empty text, which leaves the field empty.
func normalizeValue(field Field, raw json.RawMessage, location *time.Location) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	refuse := func(want string) error { return fmt.Errorf("field %q needs %s", field.Name, want) }
	switch field.Type {
	case TypeText, TypePerson, TypeChoice, TypeDate, TypeDateTime:
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return nil, refuse(stringWant(field))
		}
		text = cleanLine(text)
		if text == "" {
			return nil, nil
		}
		switch field.Type {
		case TypeChoice:
			index := slices.IndexFunc(field.Options, func(option string) bool { return strings.EqualFold(option, text) })
			if index < 0 {
				return nil, refuse(stringWant(field))
			}
			text = field.Options[index]
		case TypeDate:
			parsed, err := time.Parse(dateLayout, text)
			if err != nil {
				return nil, refuse(stringWant(field))
			}
			text = parsed.Format(dateLayout)
		case TypeDateTime:
			parsed, ok := parseDateTime(text, location)
			if !ok {
				return nil, refuse(stringWant(field))
			}
			text = parsed.In(location).Format(time.RFC3339)
		default:
			if utf8.RuneCountInString(text) > maximumTextRunes {
				return nil, fmt.Errorf("field %q has at most %d characters", field.Name, maximumTextRunes)
			}
		}
		return json.Marshal(text)
	case TypeNumber:
		var number float64
		if err := json.Unmarshal(trimmed, &number); err != nil || math.IsNaN(number) || math.Abs(number) > maximumMagnitude {
			return nil, refuse("a number between -1000000000000 and 1000000000000")
		}
		return json.Marshal(number)
	case TypeMoney:
		money, ok := parseMoney(trimmed, true)
		if !ok {
			return nil, refuse(`an amount of at most 1000000000000 and a currency, such as {"amount":12.5,"currency":"EUR"}`)
		}
		return json.Marshal(money)
	case TypeBool:
		var flag bool
		if err := json.Unmarshal(trimmed, &flag); err != nil {
			return nil, refuse("true or false")
		}
		return json.Marshal(flag)
	default:
		return nil, fmt.Errorf("field %q has an unknown type", field.Name)
	}
}

func stringWant(field Field) string {
	switch field.Type {
	case TypeChoice:
		return "one of " + strings.Join(field.Options, ", ")
	case TypeDate:
		return "a date, YYYY-MM-DD"
	case TypeDateTime:
		return "a local date and time, YYYY-MM-DDTHH:MM"
	default:
		return "text"
	}
}

// money is a money value as stored; Amount is rounded to cents.
type money struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

func (m money) cents() int64 { return int64(math.Round(m.Amount * 100)) }

// parseMoney reads {"amount","currency"}; without needCurrency a bare number is an amount alone.
func parseMoney(raw json.RawMessage, needCurrency bool) (money, bool) {
	var number float64
	if !needCurrency && json.Unmarshal(raw, &number) == nil {
		return money{Amount: math.Round(number*100) / 100}, !math.IsNaN(number) && math.Abs(number) <= maximumMagnitude
	}
	var value struct {
		Amount   *float64 `json:"amount"`
		Currency string   `json:"currency"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil || value.Amount == nil || math.IsNaN(*value.Amount) || math.Abs(*value.Amount) > maximumMagnitude {
		return money{}, false
	}
	currency := strings.ToUpper(strings.TrimSpace(value.Currency))
	if len(currency) != 3 || strings.IndexFunc(currency, func(r rune) bool { return r < 'A' || r > 'Z' }) >= 0 {
		return money{}, false
	}
	return money{Amount: math.Round(*value.Amount*100) / 100, Currency: currency}, true
}

func parseDateTime(text string, location *time.Location) (time.Time, bool) {
	if parsed, err := time.Parse(time.RFC3339, text); err == nil {
		return parsed, true
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02T15:04:05"} {
		if parsed, err := time.ParseInLocation(layout, text, location); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

// decodeValue reads a stored value; false means the record holds none for the field.
func decodeValue(field Field, raw json.RawMessage) (Value, bool) {
	if len(raw) == 0 {
		return Value{}, false
	}
	var value Value
	var err error
	switch field.Type {
	case TypeNumber:
		err = json.Unmarshal(raw, &value.Number)
	case TypeMoney:
		var stored money
		err = json.Unmarshal(raw, &stored)
		value.Number, value.Currency = stored.Amount, stored.Currency
	case TypeBool:
		err = json.Unmarshal(raw, &value.Bool)
	case TypeDateTime:
		var text string
		if err = json.Unmarshal(raw, &text); err == nil {
			value.Time, err = time.Parse(time.RFC3339, text)
		}
	default:
		err = json.Unmarshal(raw, &value.Text)
	}
	return value, err == nil
}

// display is a value as lists show it.
func display(field Field, value Value, location *time.Location) string {
	switch field.Type {
	case TypeNumber:
		return formatNumber(value.Number)
	case TypeMoney:
		return formatMoney(value.Number, value.Currency)
	case TypeBool:
		if value.Bool {
			return "yes"
		}
		return "no"
	case TypeDateTime:
		return value.Time.In(location).Format(dateTimeLayout)
	default:
		return value.Text
	}
}

func formatNumber(number float64) string { return strconv.FormatFloat(number, 'f', -1, 64) }

func formatMoney(amount float64, currency string) string {
	return strings.TrimSpace(strconv.FormatFloat(amount, 'f', 2, 64) + " " + currency)
}

// describeValues is a record's values in field order, such as "item: drill; lent to: Sam".
func describeValues(c Collection, values map[string]json.RawMessage, location *time.Location) string {
	parts := make([]string, 0, len(c.Fields))
	for _, field := range c.Fields {
		if value, ok := decodeValue(field, values[field.Key]); ok {
			parts = append(parts, field.Name+": "+display(field, value, location))
		}
	}
	if len(parts) == 0 {
		return "(no values)"
	}
	return strings.Join(parts, "; ")
}

// checkValues checks stored values against the collection's current fields: every key must be a
// field's, every value its type's, and every required field set.
func checkValues(c Collection, values map[string]json.RawMessage, location *time.Location) error {
	for key, raw := range values {
		index := slices.IndexFunc(c.Fields, func(field Field) bool { return field.Key == key })
		if index < 0 {
			return fmt.Errorf("a value belongs to a field the collection no longer has")
		}
		normalized, err := normalizeValue(c.Fields[index], raw, location)
		if err != nil {
			return err
		}
		if normalized == nil {
			return fmt.Errorf("field %q is empty", c.Fields[index].Name)
		}
	}
	for _, field := range c.Fields {
		if _, set := values[field.Key]; field.Required && !set {
			return fmt.Errorf("field %q is required", field.Name)
		}
	}
	return nil
}

// applyValues sets the given values, by field name, over current, and returns the result checked.
// A null or empty value clears its field.
func applyValues(c Collection, current map[string]json.RawMessage, given map[string]json.RawMessage, location *time.Location) (map[string]json.RawMessage, error) {
	result := make(map[string]json.RawMessage, len(current)+len(given))
	for key, value := range current {
		result[key] = value
	}
	names := make([]string, 0, len(given))
	for name := range given {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		field, found := c.field(name)
		if !found {
			return nil, c.unknownField(name)
		}
		normalized, err := normalizeValue(field, given[name], location)
		if err != nil {
			return nil, err
		}
		if normalized == nil {
			delete(result, field.Key)
			continue
		}
		result[field.Key] = normalized
	}
	if err := checkValues(c, result, location); err != nil {
		return nil, err
	}
	return result, nil
}

func sameValues(a, b map[string]json.RawMessage) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if other, found := b[key]; !found || !bytes.Equal(value, other) {
			return false
		}
	}
	return true
}
