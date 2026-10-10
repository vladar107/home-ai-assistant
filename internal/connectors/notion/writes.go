package notion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	"Every proposal you make in this turn is shown to the user together and confirmed or rejected as one batch, so propose every change the request needs in this turn, once each, and withdraw a wrong one with proposal_withdraw; a proposal in a later turn replaces a batch still waiting. " +
	"Property values are Notion API property value objects keyed by property name, for example {\"Status\": {\"status\": {\"name\": \"Done\"}}}. " +
	"Read the target's structure before proposing property values: notion_get_page shows a row's Structure line, and a data source lists every property with the options it accepts. " +
	"Spell an option exactly as the table spells it, because Notion cannot add one. " +
	"A change to several rows is one notion_update_page proposal per row, each naming its row and the value it writes, so the user can check the mapping; build that mapping from rows you read in a defined order, never from an order you assume. " +
	"Every proposal is checked against that structure before the user sees it, so a wrong property or option comes back to you as a tool failure naming the valid alternatives; correct it and propose again in the same turn."

var writeDefinitions = []tools.Definition{
	{
		Name:        toolCreatePage,
		Description: "Propose creating a Notion page under a shared page or in a data source, with a title, optional property values, and optional body paragraphs. The user must confirm before anything is created.",
		Risk:        tools.RiskWrite,
		Private:     true,
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
		Private:     true,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"page_id":{"type":"string","description":"The page ID from an earlier result."},` +
			`"properties":{"type":"object","description":"Property values to set, keyed by property name."}},` +
			`"required":["page_id","properties"],"additionalProperties":false}`),
	},
	{
		Name:        toolAppendBlocks,
		Description: "Propose appending paragraphs to the end of a shared Notion page. The user must confirm before anything is added.",
		Risk:        tools.RiskWrite,
		Private:     true,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"page_id":{"type":"string","description":"The page ID from an earlier result."},` +
			`"paragraphs":{"type":"array","items":{"type":"string"},"minItems":1,"description":"Paragraphs to append, in order."}},` +
			`"required":["page_id","paragraphs"],"additionalProperties":false}`),
	}}

// Proposer is the core approval service as the connector sees it.
type Proposer interface {
	Propose(context.Context, string, approval.Proposal) (approval.Record, error)
}

var (
	_ approval.Executor        = (*Service)(nil)
	_ approval.PrivateExecutor = (*Service)(nil)
)

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
// except the reads that name the target and check the write against its schema.
func (s *Service) propose(ctx context.Context, principalID, token string, call tools.Call) (string, error) {
	if s.proposer == nil {
		return "", fmt.Errorf("Notion writes are not available")
	}
	arguments, err := validateWrite(call)
	if err != nil {
		return "", err
	}
	if err := s.record(ctx, principalID, token, call.Name, arguments); err != nil {
		return "", err
	}
	return approval.ProposedText, nil
}

// record checks validated write arguments against their target and records the proposal.
func (s *Service) record(ctx context.Context, principalID, token, tool string, arguments json.RawMessage) error {
	if s.proposer == nil {
		return fmt.Errorf("Notion writes are not available")
	}
	summary, intent, err := s.prepareProposal(ctx, token, tool, arguments)
	if err != nil {
		return err
	}
	if _, err := s.proposer.Propose(ctx, principalID, approval.Proposal{Tool: tool, Arguments: arguments, Summary: summary, Intent: intent}); err != nil {
		if approval.Refused(err) {
			return err
		}
		s.logger.WarnContext(ctx, "notion.proposal.failed", "principal_id", principalID, "tool", tool, "error", err)
		return fmt.Errorf("the proposal could not be recorded")
	}
	return nil
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

// checkRowProperties validates proposed row values against the schema of the data source that
// holds them. A page outside a data source carries no data source ID and has no schema to check.
func (s *Service) checkRowProperties(ctx context.Context, token, dataSourceID string, properties map[string]json.RawMessage) error {
	if dataSourceID == "" || len(properties) == 0 {
		return nil
	}
	dataSource, found := s.dataSourceSchema(ctx, token, dataSourceID)
	if !found {
		return nil
	}
	return validateAgainstSchema(dataSource.Properties, properties)
}

// dataSourceSchema retrieves the row schema of a data source. Being able to write matters more
// than the check, so a schema that cannot be read is reported as absent and the write carries on
// unvalidated, exactly as it did before the check existed.
func (s *Service) dataSourceSchema(ctx context.Context, token, dataSourceID string) (Object, bool) {
	dataSource, err := s.client.DataSource(ctx, token, dataSourceID)
	if err != nil {
		var notionErr *Error
		if errorAs(err, &notionErr) {
			s.logger.InfoContext(ctx, "notion.proposal.schema_unavailable", "error_class", notionErr.Class)
		}
		return Object{}, false
	}
	return dataSource, true
}

// computedTypes are the property types Notion derives itself and rejects on every write.
var computedTypes = map[string]bool{
	"formula": true, "rollup": true, "created_time": true, "created_by": true,
	"last_edited_time": true, "last_edited_by": true, "unique_id": true,
	"button": true, "verification": true,
}

// validateAgainstSchema checks proposed property values against a data source's row schema and
// names the valid alternatives, so the model corrects a wrong property or option during the same
// turn instead of the write failing after the user has confirmed it. An option a select does not
// already hold is refused as well, even though Notion would create it, so a typo cannot add
// options to the user's own table.
func validateAgainstSchema(schema, properties map[string]json.RawMessage) error {
	for _, name := range sortedKeys(properties) {
		definition, known := schema[name]
		if !known {
			return fmt.Errorf("this table has no property %q; its settable properties are %s", name, settableNames(schema))
		}
		var property struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(definition, &property) != nil || property.Type == "" {
			continue
		}
		if computedTypes[property.Type] {
			return fmt.Errorf("property %q is %s, which Notion computes itself and never accepts on a write", name, property.Type)
		}
		var value map[string]json.RawMessage
		_ = json.Unmarshal(properties[name], &value)
		chosen, matching := value[property.Type]
		if !matching {
			return fmt.Errorf("property %q is %s; send it as {%q:{%q:%s}}", name, property.Type, name, property.Type, valueShape(property.Type))
		}
		if err := validateOptions(name, property.Type, definition, chosen); err != nil {
			return err
		}
	}
	return nil
}

// validateOptions refuses an option the property does not already hold. A value explicitly set to
// null clears the property and names no option.
func validateOptions(name, propertyType string, definition, value json.RawMessage) error {
	allowed := schemaOptionNames(propertyType, definition)
	for _, chosen := range chosenOptionNames(propertyType, value) {
		if !slices.Contains(allowed, chosen) {
			options := schemaOptions(propertyType, definition)
			if options == "" {
				options = "(none)"
			}
			return fmt.Errorf("property %q has no option %q; its options are %s", name, chosen, options)
		}
	}
	return nil
}

// chosenOptionNames lists the option names a select, status, or multi-select value names. An
// option chosen by ID alone is left to Notion.
func chosenOptionNames(propertyType string, value json.RawMessage) []string {
	if string(value) == "null" {
		return nil
	}
	var options []struct {
		Name string `json:"name"`
	}
	switch propertyType {
	case "select", "status":
		var option struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(value, &option) != nil {
			return nil
		}
		options = append(options, option)
	case "multi_select":
		if json.Unmarshal(value, &options) != nil {
			return nil
		}
	default:
		return nil
	}
	names := make([]string, 0, len(options))
	for _, option := range options {
		if option.Name != "" {
			names = append(names, option.Name)
		}
	}
	return names
}

// settableNames lists the properties of a schema a write may set, in name order.
func settableNames(schema map[string]json.RawMessage) string {
	names := make([]string, 0, len(schema))
	for _, name := range sortedKeys(schema) {
		var property struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(schema[name], &property) == nil && computedTypes[property.Type] {
			continue
		}
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}

// valueShape is the value a property type expects, so a mismatch tells the model the shape to send
// rather than only the type.
func valueShape(propertyType string) string {
	switch propertyType {
	case "select", "status":
		return `{"name":"..."}`
	case "multi_select":
		return `[{"name":"..."}]`
	case "title", "rich_text":
		return `[{"text":{"content":"..."}}]`
	case "date":
		return `{"start":"YYYY-MM-DD"}`
	case "checkbox":
		return "true"
	case "number":
		return "0"
	case "url", "email", "phone_number":
		return `"..."`
	default:
		return "..."
	}
}

// prepareProposal checks the write against the target's real schema and renders the text the user
// confirms: the page it changes and, for every property, the value it replaces. Both need the
// target itself, so it is read once for the two of them, and a page that is not shared fails
// before anything is proposed.
func (s *Service) prepareProposal(ctx context.Context, token, tool string, arguments json.RawMessage) (string, approval.Intent, error) {
	var builder strings.Builder
	var intent approval.Intent
	switch tool {
	case toolCreatePage:
		var create createArguments
		_ = json.Unmarshal(arguments, &create)
		parent := "page " + create.ParentID
		if create.ParentType == "data_source" {
			parent = "data source " + create.ParentID
			if dataSource, found := s.dataSourceSchema(ctx, token, create.ParentID); found {
				if err := validateAgainstSchema(dataSource.Properties, create.Properties); err != nil {
					return "", approval.Intent{}, err
				}
				if name := strings.TrimSpace(plainText(dataSource.Title)); name != "" {
					parent = boldName(name)
				}
			}
		}
		intent = approval.Intent{Verb: "create", TargetKind: "Notion page", TargetName: create.Title}
		fmt.Fprintf(&builder, "Notion · create page\n%s in %s", boldName(create.Title), parent)
		writeLines(&builder, renderInputProperties(create.Properties))
		writeParagraphs(&builder, create.Paragraphs)
	case toolUpdatePage:
		var update updateArguments
		_ = json.Unmarshal(arguments, &update)
		page, err := s.client.Page(ctx, token, update.PageID)
		if err != nil {
			return "", approval.Intent{}, err
		}
		if err := s.checkRowProperties(ctx, token, page.Parent.DataSourceID, update.Properties); err != nil {
			return "", approval.Intent{}, err
		}
		intent = approval.Intent{Verb: "update", TargetKind: "Notion page", TargetName: objectTitle(page)}
		fmt.Fprintf(&builder, "Notion · update page\n%s", pageLink(page))
		writeLines(&builder, renderPropertyChanges(page.Properties, update.Properties))
	case toolAppendBlocks:
		var appendTo appendArguments
		_ = json.Unmarshal(arguments, &appendTo)
		page, err := s.client.Page(ctx, token, appendTo.PageID)
		if err != nil {
			return "", approval.Intent{}, err
		}
		intent = approval.Intent{Verb: "append to", TargetKind: "Notion page", TargetName: objectTitle(page)}
		fmt.Fprintf(&builder, "Notion · append %d paragraph(s)\n%s", len(appendTo.Paragraphs), pageLink(page))
		writeParagraphs(&builder, appendTo.Paragraphs)
	}
	return builder.String(), intent, nil
}

// pageLink renders the page as a bold link the user can open, and as a bold title alone when the
// page has no URL or its title carries the markers a link is written with.
func pageLink(page Object) string {
	title := objectTitle(page)
	if page.URL == "" || strings.ContainsAny(title, "[]()*`") || strings.ContainsAny(page.URL, "()[]") {
		return boldName(title)
	}
	return "**[" + title + "](" + page.URL + ")**"
}

func boldName(name string) string {
	return "**" + name + "**"
}

func writeLines(builder *strings.Builder, lines []string) {
	for _, line := range lines {
		builder.WriteString("\n" + line)
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

// renderInputProperties renders the proposed property values of a page that does not exist yet,
// which therefore has nothing they replace.
func renderInputProperties(properties map[string]json.RawMessage) []string {
	names := sortedKeys(properties)
	lines := make([]string, 0, len(names))
	for _, name := range names {
		lines = append(lines, name+": "+renderInputProperty(properties[name]))
	}
	return lines
}

// renderPropertyChanges renders each proposed value beside the value it replaces, so the user
// confirms a change rather than a value.
func renderPropertyChanges(current, proposed map[string]json.RawMessage) []string {
	names := sortedKeys(proposed)
	lines := make([]string, 0, len(names))
	for _, name := range names {
		value := renderInputProperty(proposed[name])
		switch before := currentPropertyValue(current[name]); {
		case before == value:
			lines = append(lines, name+": "+value+" (no change)")
		case before == "":
			lines = append(lines, name+": (empty) → "+value)
		default:
			lines = append(lines, name+": "+before+" → "+value)
		}
	}
	return lines
}

// renderInputProperty renders a property value input, which may omit the "type" field, by the
// single value key it carries.
func renderInputProperty(raw json.RawMessage) string {
	var value map[string]json.RawMessage
	_ = json.Unmarshal(raw, &value)
	rendered := renderPropertyValue(inputPropertyType(value), raw)
	if rendered == "" {
		rendered = compactJSON(raw)
	}
	return truncateText(rendered, maximumPropertyText)
}

// currentPropertyValue renders the value a page already holds for one property, which carries its
// own type, and nothing for a property the page does not hold.
func currentPropertyValue(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var property struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &property) != nil {
		return ""
	}
	return truncateText(renderPropertyValue(property.Type, raw), maximumPropertyText)
}

// inputPropertyType is the type a property value input names: its "type" field when the model sent
// one, and otherwise the value key it carries.
func inputPropertyType(value map[string]json.RawMessage) string {
	if raw, present := value["type"]; present {
		var propertyType string
		if json.Unmarshal(raw, &propertyType) == nil && propertyType != "" {
			return propertyType
		}
	}
	for key := range value {
		if key != "id" && key != "type" {
			return key
		}
	}
	return ""
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
// PrivateOutcomes reports that a write's outcome may quote the page's table, such as the property
// names and options of a rejected write.
func (s *Service) PrivateOutcomes() bool { return true }

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
			if notionErr.Class == ErrorInvalid {
				if cause := s.invalidCause(ctx, token, record); cause != "" {
					return approval.Outcome{}, &approval.ActionError{Reason: "invalid", Text: "Notion rejected the write: " + cause + ". Nothing was written."}
				}
			}
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
		_, err := s.client.UpdatePage(ctx, token, arguments.PageID, arguments.Properties)
		var notionErr *Error
		if errors.As(err, &notionErr) && notionErr.Class == ErrorUnavailable {
			_, err = s.client.UpdatePage(ctx, token, arguments.PageID, arguments.Properties)
		}
		if err != nil {
			return approval.Outcome{}, err
		}
		return approval.Outcome{Text: "Updated."}, nil
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

// invalidCause re-checks a confirmed write Notion rejected as invalid against the target's schema,
// so the user learns which property or option no longer matches. Notion's own message stays out of
// the chat, so the cause comes from the schema alone and is empty when the schema still accepts
// the write.
func (s *Service) invalidCause(ctx context.Context, token string, record approval.Record) string {
	var err error
	switch record.Tool {
	case toolCreatePage:
		var arguments createArguments
		if json.Unmarshal(record.Arguments, &arguments) != nil || arguments.ParentType != "data_source" {
			return ""
		}
		err = s.checkRowProperties(ctx, token, arguments.ParentID, arguments.Properties)
	case toolUpdatePage:
		var arguments updateArguments
		if json.Unmarshal(record.Arguments, &arguments) != nil {
			return ""
		}
		page, pageErr := s.client.Page(ctx, token, arguments.PageID)
		if pageErr != nil {
			return ""
		}
		err = s.checkRowProperties(ctx, token, page.Parent.DataSourceID, arguments.Properties)
	}
	if err == nil {
		return ""
	}
	return err.Error()
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
