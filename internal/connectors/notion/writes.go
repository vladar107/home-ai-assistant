package notion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/vladar107/home-ai-assistant/internal/approval"
	"github.com/vladar107/home-ai-assistant/internal/tools"
)

const (
	toolCreatePage   = "notion_create_page"
	toolUpdatePage   = "notion_update_page"
	toolAppendBlocks = "notion_append_blocks"

	maximumParagraphs     = 50
	maximumParagraphRunes = 2000
	maximumProperties     = 50
	maximumTitleRunes     = 500
	summaryTextRunes      = 200
)

const writeGuidance = "Notion writes are on, and every write needs the user's confirmation: notion_create_page, notion_update_page, and notion_append_blocks only propose a change. " +
	"After your reply the user sees the exact proposal with Confirm and Reject buttons, so describe what you proposed and never claim it was written. " +
	"Only one proposal can wait at a time, so propose a single change per turn; a second proposal replaces the first. " +
	"Property values are Notion API property value objects keyed by property name, for example {\"Status\": {\"status\": {\"name\": \"Done\"}}}."

var writeDefinitions = []tools.Definition{
	{
		Name:        toolCreatePage,
		Description: "Propose creating a Notion page under a shared page or in a data source, with a title, optional property values, and optional body paragraphs. The user must confirm before anything is created.",
		Risk:        tools.RiskWrite,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"parent_type":{"type":"string","enum":["page","data_source"],"description":"Whether parent_id names a page or a data source."},` +
			`"parent_id":{"type":"string","description":"The ID of the parent page or data source from an earlier result."},` +
			`"title":{"type":"string","description":"The page title."},` +
			`"properties":{"type":"object","description":"Property values keyed by property name, for a data source parent."},` +
			`"paragraphs":{"type":"array","items":{"type":"string"},"description":"Body paragraphs, in order."}},` +
			`"required":["parent_type","parent_id","title"],"additionalProperties":false}`),
	},
	{
		Name:        toolUpdatePage,
		Description: "Propose setting property values on a shared Notion page. The user must confirm before anything changes.",
		Risk:        tools.RiskWrite,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"page_id":{"type":"string","description":"The page ID from an earlier result."},` +
			`"properties":{"type":"object","description":"Property values to set, keyed by property name."}},` +
			`"required":["page_id","properties"],"additionalProperties":false}`),
	},
	{
		Name:        toolAppendBlocks,
		Description: "Propose appending paragraphs to the end of a shared Notion page. The user must confirm before anything is added.",
		Risk:        tools.RiskWrite,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"page_id":{"type":"string","description":"The page ID from an earlier result."},` +
			`"paragraphs":{"type":"array","items":{"type":"string"},"minItems":1,"description":"Paragraphs to append, in order."}},` +
			`"required":["page_id","paragraphs"],"additionalProperties":false}`),
	}}

// Proposer is the core approval service as the connector sees it.
type Proposer interface {
	Propose(context.Context, string, approval.Proposal) (approval.Record, error)
}

var _ approval.Executor = (*Service)(nil)

func (s *Service) ActionTools() []string {
	return []string{toolCreatePage, toolUpdatePage, toolAppendBlocks}
}

func isWriteTool(name string) bool {
	return name == toolCreatePage || name == toolUpdatePage || name == toolAppendBlocks
}

type createArguments struct {
	ParentType string                     `json:"parent_type"`
	ParentID   string                     `json:"parent_id"`
	Title      string                     `json:"title"`
	Properties map[string]json.RawMessage `json:"properties"`
	Paragraphs []string                   `json:"paragraphs"`
}

type updateArguments struct {
	PageID     string                     `json:"page_id"`
	Properties map[string]json.RawMessage `json:"properties"`
}

type appendArguments struct {
	PageID     string   `json:"page_id"`
	Paragraphs []string `json:"paragraphs"`
}

// propose validates a write call and records it for confirmation; nothing reaches Notion here
// except a read of the target page so the user sees its title.
func (s *Service) propose(ctx context.Context, principalID, token string, call tools.Call) (string, error) {
	if s.proposer == nil {
		return "", fmt.Errorf("Notion writes are not available")
	}
	arguments, err := validateWrite(call)
	if err != nil {
		return "", err
	}
	summary, err := s.renderProposal(ctx, token, call.Name, arguments)
	if err != nil {
		return "", err
	}
	if _, err := s.proposer.Propose(ctx, principalID, approval.Proposal{Tool: call.Name, Arguments: arguments, Summary: summary}); err != nil {
		s.logger.WarnContext(ctx, "notion.proposal.failed", "principal_id", principalID, "tool", call.Name, "error", err)
		return "", fmt.Errorf("the proposal could not be recorded")
	}
	return approval.ProposedText, nil
}

// validateWrite checks the model's arguments and returns them re-encoded without unknown fields.
func validateWrite(call tools.Call) (json.RawMessage, error) {
	switch call.Name {
	case toolCreatePage:
		var arguments createArguments
		if err := decodeArguments(call.Arguments, &arguments); err != nil {
			return nil, err
		}
		if arguments.ParentType != "page" && arguments.ParentType != "data_source" {
			return nil, fmt.Errorf("parent_type must be page or data_source")
		}
		if strings.TrimSpace(arguments.ParentID) == "" {
			return nil, fmt.Errorf("parent_id is required")
		}
		arguments.Title = strings.TrimSpace(arguments.Title)
		if arguments.Title == "" || utf8.RuneCountInString(arguments.Title) > maximumTitleRunes {
			return nil, fmt.Errorf("title must be between 1 and %d characters", maximumTitleRunes)
		}
		if arguments.ParentType == "page" && len(arguments.Properties) > 0 {
			return nil, fmt.Errorf("properties are only accepted for a data source parent")
		}
		if err := validateProperties(arguments.Properties); err != nil {
			return nil, err
		}
		if err := validateParagraphs(arguments.Paragraphs, false); err != nil {
			return nil, err
		}
		return json.Marshal(arguments)
	case toolUpdatePage:
		var arguments updateArguments
		if err := decodeArguments(call.Arguments, &arguments); err != nil {
			return nil, err
		}
		if strings.TrimSpace(arguments.PageID) == "" {
			return nil, fmt.Errorf("page_id is required")
		}
		if len(arguments.Properties) == 0 {
			return nil, fmt.Errorf("properties must set at least one property")
		}
		if err := validateProperties(arguments.Properties); err != nil {
			return nil, err
		}
		return json.Marshal(arguments)
	case toolAppendBlocks:
		var arguments appendArguments
		if err := decodeArguments(call.Arguments, &arguments); err != nil {
			return nil, err
		}
		if strings.TrimSpace(arguments.PageID) == "" {
			return nil, fmt.Errorf("page_id is required")
		}
		if err := validateParagraphs(arguments.Paragraphs, true); err != nil {
			return nil, err
		}
		return json.Marshal(arguments)
	default:
		return nil, fmt.Errorf("unknown Notion tool")
	}
}

func validateProperties(properties map[string]json.RawMessage) error {
	if len(properties) > maximumProperties {
		return fmt.Errorf("properties must not exceed %d entries", maximumProperties)
	}
	for name, raw := range properties {
		var value map[string]json.RawMessage
		if strings.TrimSpace(name) == "" || json.Unmarshal(raw, &value) != nil || len(value) == 0 {
			return fmt.Errorf("property %q must be a Notion property value object", name)
		}
	}
	return nil
}

func validateParagraphs(paragraphs []string, required bool) error {
	if required && len(paragraphs) == 0 {
		return fmt.Errorf("paragraphs must hold at least one paragraph")
	}
	if len(paragraphs) > maximumParagraphs {
		return fmt.Errorf("paragraphs must not exceed %d entries", maximumParagraphs)
	}
	for _, paragraph := range paragraphs {
		if strings.TrimSpace(paragraph) == "" || utf8.RuneCountInString(paragraph) > maximumParagraphRunes {
			return fmt.Errorf("each paragraph must be between 1 and %d characters", maximumParagraphRunes)
		}
	}
	return nil
}

// renderProposal produces the text the user confirms: exactly what will be sent, with property
// values shown as text where possible and as compact JSON otherwise.
func (s *Service) renderProposal(ctx context.Context, token, tool string, arguments json.RawMessage) (string, error) {
	var builder strings.Builder
	switch tool {
	case toolCreatePage:
		var create createArguments
		_ = json.Unmarshal(arguments, &create)
		parent := "page " + create.ParentID
		if create.ParentType == "data_source" {
			parent = "data source " + create.ParentID
		}
		fmt.Fprintf(&builder, "Notion proposal: create page %q in %s.", create.Title, parent)
		writeProperties(&builder, create.Properties)
		writeParagraphs(&builder, create.Paragraphs)
	case toolUpdatePage:
		var update updateArguments
		_ = json.Unmarshal(arguments, &update)
		title, err := s.pageTitle(ctx, token, update.PageID)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&builder, "Notion proposal: update page %s.", title)
		writeProperties(&builder, update.Properties)
	case toolAppendBlocks:
		var appendTo appendArguments
		_ = json.Unmarshal(arguments, &appendTo)
		title, err := s.pageTitle(ctx, token, appendTo.PageID)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&builder, "Notion proposal: append %d paragraph(s) to page %s.", len(appendTo.Paragraphs), title)
		writeParagraphs(&builder, appendTo.Paragraphs)
	}
	return builder.String(), nil
}

// pageTitle reads the target page so the proposal names it and a page that is not shared fails
// before anything is proposed.
func (s *Service) pageTitle(ctx context.Context, token, pageID string) (string, error) {
	page, err := s.client.Page(ctx, token, pageID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%q (%s)", objectTitle(page), page.ID), nil
}

func writeProperties(builder *strings.Builder, properties map[string]json.RawMessage) {
	if len(properties) == 0 {
		return
	}
	builder.WriteString("\nProperties:")
	for _, line := range renderInputProperties(properties) {
		builder.WriteString("\n- " + line)
	}
}

func writeParagraphs(builder *strings.Builder, paragraphs []string) {
	if len(paragraphs) == 0 {
		return
	}
	fmt.Fprintf(builder, "\nBody (%d paragraph(s)):", len(paragraphs))
	for index, paragraph := range paragraphs {
		fmt.Fprintf(builder, "\n%d. %s", index+1, truncateText(paragraph, summaryTextRunes))
	}
}

// renderInputProperties renders property value inputs, which may omit the "type" field, by the
// single value key they carry.
func renderInputProperties(properties map[string]json.RawMessage) []string {
	names := sortedKeys(properties)
	lines := make([]string, 0, len(names))
	for _, name := range names {
		var value map[string]json.RawMessage
		_ = json.Unmarshal(properties[name], &value)
		propertyType := ""
		if raw, present := value["type"]; present {
			_ = json.Unmarshal(raw, &propertyType)
		}
		if propertyType == "" {
			for key := range value {
				if key != "id" && key != "type" {
					propertyType = key
					break
				}
			}
		}
		rendered := renderPropertyValue(propertyType, properties[name])
		if rendered == "" {
			rendered = compactJSON(properties[name])
		}
		lines = append(lines, name+": "+truncateText(rendered, maximumPropertyText))
	}
	return lines
}

func sortedKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// ExecuteApproved performs a confirmed proposal with the principal's own token. Creates and
// appends are never retried after an ambiguous failure; an update is retried once.
func (s *Service) ExecuteApproved(ctx context.Context, record approval.Record) (approval.Outcome, error) {
	token, connected, err := s.token(ctx, record.PrincipalID)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !connected {
		return approval.Outcome{}, &approval.ActionError{Reason: "not_connected", Text: "Notion is no longer connected; nothing was written."}
	}
	row, _, err := s.repository.LoadIntegrationConnection(ctx, record.PrincipalID, Integration)
	if err != nil {
		return approval.Outcome{}, err
	}
	if !row.WritesEnabled {
		return approval.Outcome{}, &approval.ActionError{Reason: "writes_disabled", Text: "Notion writes are off; nothing was written."}
	}
	outcome, err := s.write(ctx, token, record)
	if err != nil {
		var notionErr *Error
		if errors.As(err, &notionErr) {
			s.logger.InfoContext(ctx, "notion.action.failed", "principal_id", record.PrincipalID, "tool", record.Tool, "error_class", notionErr.Class)
			return approval.Outcome{}, actionError(record.Tool, notionErr.Class)
		}
		return approval.Outcome{}, err
	}
	s.logger.InfoContext(ctx, "notion.action.executed", "principal_id", record.PrincipalID, "tool", record.Tool)
	return outcome, nil
}

func (s *Service) write(ctx context.Context, token string, record approval.Record) (approval.Outcome, error) {
	switch record.Tool {
	case toolCreatePage:
		var arguments createArguments
		if err := json.Unmarshal(record.Arguments, &arguments); err != nil {
			return approval.Outcome{}, fmt.Errorf("decode stored arguments: %w", err)
		}
		body := map[string]any{"properties": map[string]any{"title": titleValue(arguments.Title)}}
		if arguments.ParentType == "data_source" {
			body["parent"] = map[string]any{"type": "data_source_id", "data_source_id": arguments.ParentID}
			properties := map[string]any{}
			for name, value := range arguments.Properties {
				properties[name] = value
			}
			properties["title"] = titleValue(arguments.Title)
			body["properties"] = properties
		} else {
			body["parent"] = map[string]any{"type": "page_id", "page_id": arguments.ParentID}
		}
		if len(arguments.Paragraphs) > 0 {
			body["children"] = paragraphBlocks(arguments.Paragraphs)
		}
		page, err := s.client.CreatePage(ctx, token, body)
		if err != nil {
			return approval.Outcome{}, err
		}
		return approval.Outcome{Text: "Created page: " + pageReference(page)}, nil
	case toolUpdatePage:
		var arguments updateArguments
		if err := json.Unmarshal(record.Arguments, &arguments); err != nil {
			return approval.Outcome{}, fmt.Errorf("decode stored arguments: %w", err)
		}
		page, err := s.client.UpdatePage(ctx, token, arguments.PageID, arguments.Properties)
		var notionErr *Error
		if errors.As(err, &notionErr) && notionErr.Class == ErrorUnavailable {
			page, err = s.client.UpdatePage(ctx, token, arguments.PageID, arguments.Properties)
		}
		if err != nil {
			return approval.Outcome{}, err
		}
		return approval.Outcome{Text: "Updated page: " + pageReference(page)}, nil
	case toolAppendBlocks:
		var arguments appendArguments
		if err := json.Unmarshal(record.Arguments, &arguments); err != nil {
			return approval.Outcome{}, fmt.Errorf("decode stored arguments: %w", err)
		}
		if _, err := s.client.AppendBlockChildren(ctx, token, arguments.PageID, paragraphBlocks(arguments.Paragraphs)); err != nil {
			return approval.Outcome{}, err
		}
		return approval.Outcome{Text: fmt.Sprintf("Appended %d paragraph(s) to page %s.", len(arguments.Paragraphs), arguments.PageID)}, nil
	default:
		return approval.Outcome{}, fmt.Errorf("unknown Notion action")
	}
}

func titleValue(title string) map[string]any {
	return map[string]any{"title": []map[string]any{{"type": "text", "text": map[string]any{"content": title}}}}
}

func paragraphBlocks(paragraphs []string) []map[string]any {
	blocks := make([]map[string]any, 0, len(paragraphs))
	for _, paragraph := range paragraphs {
		blocks = append(blocks, map[string]any{
			"object": "block",
			"type":   "paragraph",
			"paragraph": map[string]any{
				"rich_text": []map[string]any{{"type": "text", "text": map[string]any{"content": paragraph}}},
			},
		})
	}
	return blocks
}

func pageReference(page Object) string {
	if page.URL != "" {
		return page.URL
	}
	return page.ID
}

// actionError maps a Notion failure of a confirmed action to what the user is told. A failure of
// unknown outcome on a create or append tells the user to check Notion before trying again.
func actionError(tool string, class ErrorClass) *approval.ActionError {
	switch class {
	case ErrorUnauthorized:
		return &approval.ActionError{Reason: "unauthorized", Text: "Notion rejected the connection token; nothing was written. Reconnect Notion in /settings."}
	case ErrorRestricted:
		return &approval.ActionError{Reason: "restricted", Text: "The Notion connection lacks the capability for this write; nothing was written."}
	case ErrorNotShared:
		return &approval.ActionError{Reason: "not_shared", Text: "Notion has no shared page or data source with that ID; nothing was written."}
	case ErrorInvalid:
		return &approval.ActionError{Reason: "invalid", Text: "Notion rejected the request; nothing was written."}
	case ErrorRateLimited:
		return &approval.ActionError{Reason: "rate_limited", Text: "Notion is rate limiting requests; nothing was written. Try again later."}
	default:
		if tool == toolUpdatePage {
			return &approval.ActionError{Reason: "unavailable", Text: "Notion did not confirm the update; check the page before trying again."}
		}
		return &approval.ActionError{Reason: "ambiguous", Text: "Notion did not confirm the write; check Notion for a partial result before trying again."}
	}
}
