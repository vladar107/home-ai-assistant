package notion

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/vladar107/home-ai-assistant/internal/approval"
)

// truncatedRichText is the number of rich text items from which Notion may cut a property value in a
// page read, so a value that long cannot be written back whole.
const truncatedRichText = 25

var _ approval.Undoer = (*Service)(nil)

// updateUndo is what reverses a direct property update: the values the page held just before it,
// in the shape a write sends.
type updateUndo struct {
	PageID   string                     `json:"page_id"`
	Previous map[string]json.RawMessage `json:"previous"`
}

// captureUndo reads the page just before a direct update and keeps the values the update replaces.
// It returns nothing when the page cannot be read, or when a written or replaced value is of a kind
// that cannot be written back or recognised later, so such an update has no Undo.
func (s *Service) captureUndo(ctx context.Context, token string, arguments updateArguments) json.RawMessage {
	page, err := s.client.Page(ctx, token, arguments.PageID)
	if err != nil {
		return nil
	}
	previous := make(map[string]json.RawMessage, len(arguments.Properties))
	for name, written := range arguments.Properties {
		if _, ok := comparableInput(written); !ok {
			return nil
		}
		value, ok := requestValue(page.Properties[name])
		if !ok {
			return nil
		}
		previous[name] = value
	}
	payload, err := json.Marshal(updateUndo{PageID: arguments.PageID, Previous: previous})
	if err != nil {
		return nil
	}
	return payload
}

// Undo writes back the values a direct update replaced, while the page still holds the values the
// update wrote.
func (s *Service) Undo(ctx context.Context, record approval.Record, payload json.RawMessage) (approval.Outcome, error) {
	var undo updateUndo
	var arguments updateArguments
	if record.Tool != toolUpdatePage || json.Unmarshal(payload, &undo) != nil || json.Unmarshal(record.Arguments, &arguments) != nil || undo.PageID == "" {
		return approval.Outcome{}, &approval.UndoRefusal{Reason: "the change could not be read back."}
	}
	token, connected, err := s.token(ctx, record.PrincipalID)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !connected {
		return approval.Outcome{}, errors.New("notion is not connected")
	}
	page, err := s.client.Page(ctx, token, undo.PageID)
	if err != nil {
		var notionErr *Error
		if errors.As(err, &notionErr) && notionErr.Class == ErrorNotShared {
			return approval.Outcome{}, &approval.UndoRefusal{Reason: "the page is gone from Notion or no longer shared."}
		}
		return approval.Outcome{}, err
	}
	for name, written := range arguments.Properties {
		want, ok := comparableInput(written)
		current, found := comparableCurrent(page.Properties[name])
		if !ok || !found || want != current {
			return approval.Outcome{}, &approval.UndoRefusal{Reason: "changed in Notion since."}
		}
	}
	_, err = s.client.UpdatePage(ctx, token, undo.PageID, undo.Previous)
	var notionErr *Error
	if errors.As(err, &notionErr) && notionErr.Class == ErrorUnavailable {
		_, err = s.client.UpdatePage(ctx, token, undo.PageID, undo.Previous)
	}
	if err != nil {
		if errors.As(err, &notionErr) {
			s.logger.InfoContext(ctx, "notion.undo.failed", "principal_id", record.PrincipalID, "error_class", notionErr.Class)
		}
		return approval.Outcome{}, err
	}
	s.logger.InfoContext(ctx, "notion.undo.executed", "principal_id", record.PrincipalID, "tool", record.Tool)
	return approval.Outcome{Text: "Restored the previous values."}, nil
}

// propertyValue is a property value in either shape: a page read names its type, and a write may
// name it or carry only the value key.
type propertyValue struct {
	kind  string
	value json.RawMessage
}

func parseCurrent(raw json.RawMessage) (propertyValue, bool) {
	var property map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &property) != nil {
		return propertyValue{}, false
	}
	var kind string
	if json.Unmarshal(property["type"], &kind) != nil || kind == "" {
		return propertyValue{}, false
	}
	value, present := property[kind]
	if !present {
		return propertyValue{}, false
	}
	return propertyValue{kind: kind, value: value}, true
}

func parseInput(raw json.RawMessage) (propertyValue, bool) {
	var property map[string]json.RawMessage
	if json.Unmarshal(raw, &property) != nil {
		return propertyValue{}, false
	}
	kind := inputPropertyType(property)
	value, present := property[kind]
	if kind == "" || !present {
		return propertyValue{}, false
	}
	return propertyValue{kind: kind, value: value}, true
}

func comparableInput(raw json.RawMessage) (string, bool) {
	property, ok := parseInput(raw)
	if !ok {
		return "", false
	}
	return comparable(property)
}

func comparableCurrent(raw json.RawMessage) (string, bool) {
	property, ok := parseCurrent(raw)
	if !ok {
		return "", false
	}
	return comparable(property)
}

// comparable renders a property value in a form that is the same for what a write sent and what a
// later read returns, so Undo can tell whether the property still holds what was written.
func comparable(property propertyValue) (string, bool) {
	null := string(property.value) == "null"
	var rendered string
	switch property.kind {
	case "title", "rich_text":
		var parts []struct {
			Type      string `json:"type"`
			PlainText string `json:"plain_text"`
			Text      *struct {
				Content string `json:"content"`
			} `json:"text"`
		}
		if !null && json.Unmarshal(property.value, &parts) != nil {
			return "", false
		}
		var builder strings.Builder
		for _, part := range parts {
			switch {
			case part.Text != nil:
				builder.WriteString(part.Text.Content)
			case part.PlainText != "":
				builder.WriteString(part.PlainText)
			default:
				return "", false
			}
		}
		rendered = builder.String()
	case "number":
		if !null {
			var number float64
			if json.Unmarshal(property.value, &number) != nil {
				return "", false
			}
			rendered = strconv.FormatFloat(number, 'g', -1, 64)
		}
	case "checkbox":
		var checked bool
		if json.Unmarshal(property.value, &checked) != nil {
			return "", false
		}
		rendered = strconv.FormatBool(checked)
	case "select", "status":
		if !null {
			var option struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(property.value, &option) != nil || option.Name == "" {
				return "", false
			}
			rendered = option.Name
		}
	case "multi_select":
		var options []struct {
			Name string `json:"name"`
		}
		if !null && json.Unmarshal(property.value, &options) != nil {
			return "", false
		}
		names := make([]string, 0, len(options))
		for _, option := range options {
			if option.Name == "" {
				return "", false
			}
			names = append(names, option.Name)
		}
		encoded, _ := json.Marshal(names)
		rendered = string(encoded)
	case "date":
		if !null {
			var date struct {
				Start    string  `json:"start"`
				End      *string `json:"end"`
				TimeZone *string `json:"time_zone"`
			}
			// A time written in a named zone has no offset, and a read returns it with one, so the two
			// cannot be compared and such a date gets no Undo.
			if json.Unmarshal(property.value, &date) != nil || date.Start == "" || date.TimeZone != nil {
				return "", false
			}
			rendered = comparableInstant(date.Start)
			if date.End != nil {
				rendered += "/" + comparableInstant(*date.End)
			}
		}
	case "url", "email", "phone_number":
		if !null {
			var text string
			if json.Unmarshal(property.value, &text) != nil {
				return "", false
			}
			rendered = text
		}
	default:
		return "", false
	}
	return property.kind + ":" + rendered, true
}

// comparableInstant puts a date with a time into one zone, since Notion may return it with an
// offset it was not written with; a plain date is kept as written.
func comparableInstant(value string) string {
	if instant, err := time.Parse(time.RFC3339, value); err == nil {
		return instant.UTC().Format(time.RFC3339)
	}
	return value
}

// requestValue converts a property value from a page read into the value a write sends, for the
// kinds a write accepts and a read returns whole. Anything else cannot be written back.
func requestValue(raw json.RawMessage) (json.RawMessage, bool) {
	property, ok := parseCurrent(raw)
	if !ok {
		return nil, false
	}
	null := string(property.value) == "null"
	var value any
	switch property.kind {
	case "title", "rich_text":
		var parts []struct {
			Type string `json:"type"`
			Text *struct {
				Content string          `json:"content"`
				Link    json.RawMessage `json:"link"`
			} `json:"text"`
			Annotations json.RawMessage `json:"annotations"`
		}
		if null || json.Unmarshal(property.value, &parts) != nil || len(parts) >= truncatedRichText {
			return nil, false
		}
		items := make([]map[string]any, 0, len(parts))
		for _, part := range parts {
			if part.Type != "text" || part.Text == nil {
				return nil, false
			}
			text := map[string]any{"content": part.Text.Content}
			if len(part.Text.Link) > 0 && string(part.Text.Link) != "null" {
				text["link"] = part.Text.Link
			}
			item := map[string]any{"type": "text", "text": text}
			if len(part.Annotations) > 0 && string(part.Annotations) != "null" {
				item["annotations"] = part.Annotations
			}
			items = append(items, item)
		}
		value = items
	case "number", "checkbox", "url", "email", "phone_number":
		value = property.value
	case "select", "status":
		// Notion clears a select but refuses an empty status, so an empty status cannot come back.
		if null && property.kind == "status" {
			return nil, false
		}
		if null {
			value = nil
			break
		}
		var option struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(property.value, &option) != nil || option.Name == "" {
			return nil, false
		}
		value = map[string]string{"name": option.Name}
	case "multi_select":
		var options []struct {
			Name string `json:"name"`
		}
		if null || json.Unmarshal(property.value, &options) != nil {
			return nil, false
		}
		names := make([]map[string]string, 0, len(options))
		for _, option := range options {
			if option.Name == "" {
				return nil, false
			}
			names = append(names, map[string]string{"name": option.Name})
		}
		value = names
	case "date":
		if null {
			value = nil
			break
		}
		var date map[string]json.RawMessage
		if json.Unmarshal(property.value, &date) != nil {
			return nil, false
		}
		kept := map[string]json.RawMessage{}
		for _, key := range []string{"start", "end", "time_zone"} {
			if field, present := date[key]; present && string(field) != "null" {
				kept[key] = field
			}
		}
		if _, present := kept["start"]; !present {
			return nil, false
		}
		value = kept
	default:
		return nil, false
	}
	encoded, err := json.Marshal(map[string]any{property.kind: value})
	if err != nil {
		return nil, false
	}
	return encoded, true
}
