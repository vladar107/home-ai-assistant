package notion

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	// maximumContentBytes bounds flattened page content so a rendered page stays below the tool
	// broker's result limit together with its properties.
	maximumContentBytes = 12 << 10
	maximumPropertyText = 400
	// maximumOptionsText bounds the options listed for one property so a table with hundreds of
	// them cannot fill the tool result by itself.
	maximumOptionsText = 200
)

func errorAs(err error, target **Error) bool {
	return errors.As(err, target)
}

func plainText(parts []RichText) string {
	var builder strings.Builder
	for _, part := range parts {
		if part.PlainText == "" {
			builder.WriteString(part.Text.Content)
			continue
		}
		builder.WriteString(part.PlainText)
	}
	return builder.String()
}

// renderList renders search or query results as compact lines the model can quote IDs from.
func renderList(heading string, list List[Object]) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "%s: %d result(s)", heading, len(list.Results))
	if list.HasMore && list.NextCursor != "" {
		fmt.Fprintf(&builder, "; more available with cursor %s", list.NextCursor)
	}
	builder.WriteString("\n")
	if len(list.Results) == 0 {
		builder.WriteString("(none)\n")
	}
	for _, object := range list.Results {
		builder.WriteString(renderSummary(object))
	}
	return strings.TrimRight(builder.String(), "\n")
}

func renderSummary(object Object) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "- [%s] %s\n  id: %s", object.Object, objectTitle(object), object.ID)
	if object.URL != "" {
		fmt.Fprintf(&builder, "; url: %s", object.URL)
	}
	if object.LastEditedTime != "" {
		fmt.Fprintf(&builder, "; last edited: %s", object.LastEditedTime)
	}
	builder.WriteString("\n")
	switch object.Object {
	case "page":
		for _, line := range renderPropertyLines(object.Properties, true) {
			builder.WriteString("  " + line + "\n")
		}
	case "data_source":
		if schema := renderSchema(object.Properties); schema != "" {
			builder.WriteString("  properties: " + schema + "\n")
		}
	}
	return builder.String()
}

// renderPage renders one page with its properties, the schema of the data source it belongs to
// when that could be read, and, when present, its flattened content.
func renderPage(page Object, schema, content string, truncated bool) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Page: %s\nid: %s", objectTitle(page), page.ID)
	if page.URL != "" {
		fmt.Fprintf(&builder, "; url: %s", page.URL)
	}
	if page.LastEditedTime != "" {
		fmt.Fprintf(&builder, "; last edited: %s", page.LastEditedTime)
	}
	if parent := renderParent(page.Parent); parent != "" {
		fmt.Fprintf(&builder, "; parent: %s", parent)
	}
	if page.InTrash {
		builder.WriteString("; in trash")
	}
	builder.WriteString("\n")
	lines := renderPropertyLines(page.Properties, false)
	if len(lines) > 0 {
		builder.WriteString("Properties:\n")
		for _, line := range lines {
			builder.WriteString("- " + line + "\n")
		}
	}
	if schema != "" {
		builder.WriteString("Structure: " + schema + "\n")
	}
	if content != "" {
		builder.WriteString("Content:\n" + content + "\n")
		if truncated {
			builder.WriteString("[content truncated]\n")
		}
	}
	return strings.TrimRight(builder.String(), "\n")
}

// renderDataSources lists the data sources of a database so the model can name one, for the
// database that holds more than a single data source.
func renderDataSources(database Database) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Database %s holds %d data sources; query one of them by ID:", databaseTitle(database), len(database.DataSources))
	for _, source := range database.DataSources {
		name := strings.TrimSpace(source.Name)
		if name == "" {
			name = "(untitled)"
		}
		fmt.Fprintf(&builder, "\n- %s\n  id: %s", name, source.ID)
	}
	return builder.String()
}

func databaseTitle(database Database) string {
	if title := strings.TrimSpace(plainText(database.Title)); title != "" {
		return title
	}
	return "(untitled)"
}

func renderParent(parent Parent) string {
	switch parent.Type {
	case "data_source_id":
		return "data source " + parent.DataSourceID
	case "database_id":
		return "database " + parent.DatabaseID
	case "page_id":
		return "page " + parent.PageID
	case "block_id":
		return "block " + parent.BlockID
	case "workspace":
		return "workspace"
	default:
		return ""
	}
}

func objectTitle(object Object) string {
	if title := strings.TrimSpace(plainText(object.Title)); title != "" {
		return title
	}
	for _, raw := range object.Properties {
		var property struct {
			Type  string     `json:"type"`
			Title []RichText `json:"title"`
		}
		if json.Unmarshal(raw, &property) == nil && property.Type == "title" {
			if title := strings.TrimSpace(plainText(property.Title)); title != "" {
				return title
			}
			return "(untitled)"
		}
	}
	return "(untitled)"
}

// renderPropertyLines renders page property values as "Name: value" lines in name order, leaving
// the title out because it heads the rendering already. Empty values are omitted from summaries.
func renderPropertyLines(properties map[string]json.RawMessage, summary bool) []string {
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	var lines []string
	for _, name := range names {
		var property struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(properties[name], &property) != nil || property.Type == "title" {
			continue
		}
		value := renderPropertyValue(property.Type, properties[name])
		if value == "" && summary {
			continue
		}
		if value == "" {
			value = "(empty)"
		}
		lines = append(lines, name+": "+truncateText(value, maximumPropertyText))
	}
	return lines
}

func renderPropertyValue(propertyType string, raw json.RawMessage) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return ""
	}
	value, present := fields[propertyType]
	if !present || string(value) == "null" {
		return ""
	}
	switch propertyType {
	case "rich_text", "title":
		var parts []RichText
		_ = json.Unmarshal(value, &parts)
		return strings.TrimSpace(plainText(parts))
	case "number":
		return compactJSON(value)
	case "select", "status":
		return namedValue(value)
	case "multi_select":
		var options []struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(value, &options)
		names := make([]string, 0, len(options))
		for _, option := range options {
			names = append(names, option.Name)
		}
		return strings.Join(names, ", ")
	case "date":
		return renderDate(value)
	case "checkbox":
		return compactJSON(value)
	case "url", "email", "phone_number", "created_time", "last_edited_time":
		var text string
		if json.Unmarshal(value, &text) == nil {
			return text
		}
		return compactJSON(value)
	case "people":
		var people []struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(value, &people)
		names := make([]string, 0, len(people))
		for _, person := range people {
			if person.Name != "" {
				names = append(names, person.Name)
			}
		}
		if len(names) == 0 && len(people) > 0 {
			return fmt.Sprintf("%d person(s)", len(people))
		}
		return strings.Join(names, ", ")
	case "files":
		var files []struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(value, &files)
		names := make([]string, 0, len(files))
		for _, file := range files {
			names = append(names, file.Name)
		}
		return strings.Join(names, ", ")
	case "formula", "rollup":
		var nested struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(value, &nested) == nil && nested.Type != "" && nested.Type != "array" {
			return renderPropertyValue(nested.Type, value)
		}
		return compactJSON(value)
	case "relation":
		var relations []struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(value, &relations)
		ids := make([]string, 0, len(relations))
		for _, relation := range relations {
			ids = append(ids, relation.ID)
		}
		return strings.Join(ids, ", ")
	case "unique_id":
		var unique struct {
			Prefix string `json:"prefix"`
			Number int64  `json:"number"`
		}
		if json.Unmarshal(value, &unique) == nil {
			if unique.Prefix != "" {
				return fmt.Sprintf("%s-%d", unique.Prefix, unique.Number)
			}
			return fmt.Sprintf("%d", unique.Number)
		}
		return compactJSON(value)
	case "string", "boolean":
		return compactJSON(value)
	default:
		return compactJSON(value)
	}
}

func namedValue(value json.RawMessage) string {
	var option struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(value, &option) == nil {
		return option.Name
	}
	return compactJSON(value)
}

func renderDate(value json.RawMessage) string {
	var date struct {
		Start string `json:"start"`
		End   string `json:"end"`
	}
	if json.Unmarshal(value, &date) != nil {
		return compactJSON(value)
	}
	if date.End != "" {
		return date.Start + " to " + date.End
	}
	return date.Start
}

func compactJSON(value json.RawMessage) string {
	var text string
	if json.Unmarshal(value, &text) == nil {
		return text
	}
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, value); err != nil {
		return strings.TrimSpace(string(value))
	}
	return buffer.String()
}

// renderSchema lists a data source's property names and types so the model can build filters, and
// the options of the properties that only accept a fixed set of values.
func renderSchema(properties map[string]json.RawMessage) string {
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		var property struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(properties[name], &property) != nil || property.Type == "" {
			continue
		}
		part := name + " (" + property.Type
		if options := schemaOptions(property.Type, properties[name]); options != "" {
			part += ": " + options
		}
		parts = append(parts, part+")")
	}
	return strings.Join(parts, ", ")
}

// schemaOptions lists the option names a select, status, or multi-select property accepts,
// separated by a pipe because an option name may hold a comma.
func schemaOptions(propertyType string, raw json.RawMessage) string {
	return truncateText(strings.Join(schemaOptionNames(propertyType, raw), " | "), maximumOptionsText)
}

// schemaOptionNames returns the options of a select, status, or multi-select property, and nothing
// for a property type that holds no options.
func schemaOptionNames(propertyType string, raw json.RawMessage) []string {
	switch propertyType {
	case "select", "status", "multi_select":
	default:
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	var definition struct {
		Options []struct {
			Name string `json:"name"`
		} `json:"options"`
	}
	if json.Unmarshal(fields[propertyType], &definition) != nil {
		return nil
	}
	names := make([]string, 0, len(definition.Options))
	for _, option := range definition.Options {
		names = append(names, option.Name)
	}
	return names
}

// flattenBlocks renders top-level blocks as plain text lines up to the byte cap and reports
// whether anything was left out.
func flattenBlocks(blocks []Block, limit int) (string, bool) {
	var builder strings.Builder
	for index, block := range blocks {
		line := renderBlock(block, index)
		if block.HasChildren {
			line += " (has nested content; block id: " + block.ID + ")"
		}
		if builder.Len()+len(line)+1 > limit {
			return strings.TrimRight(builder.String(), "\n"), true
		}
		builder.WriteString(line + "\n")
	}
	return strings.TrimRight(builder.String(), "\n"), false
}

// renderBlockChildren presents one level of content nested inside a block, reached through the
// block ID printed beside a nested-content note in a page render.
func renderBlockChildren(blockID, content string, truncated bool) string {
	if content == "" {
		return "Block " + blockID + " holds no nested content."
	}
	text := "Nested content of block " + blockID + ":\n" + content
	if truncated {
		text += "\n[content truncated]"
	}
	return text
}

func renderBlock(block Block, index int) string {
	var content struct {
		RichText   []RichText   `json:"rich_text"`
		Checked    bool         `json:"checked"`
		Language   string       `json:"language"`
		Title      string       `json:"title"`
		URL        string       `json:"url"`
		Expression string       `json:"expression"`
		Cells      [][]RichText `json:"cells"`
		External   struct {
			URL string `json:"url"`
		} `json:"external"`
		File struct {
			URL string `json:"url"`
		} `json:"file"`
	}
	_ = json.Unmarshal(block.Content, &content)
	text := strings.TrimSpace(plainText(content.RichText))
	switch block.Type {
	case "paragraph":
		return text
	case "heading_1":
		return "# " + text
	case "heading_2":
		return "## " + text
	case "heading_3":
		return "### " + text
	case "bulleted_list_item":
		return "- " + text
	case "numbered_list_item":
		return fmt.Sprintf("%d. %s", index+1, text)
	case "to_do":
		if content.Checked {
			return "[x] " + text
		}
		return "[ ] " + text
	case "toggle", "quote", "callout":
		return "> " + text
	case "code":
		return "```" + content.Language + "\n" + text + "\n```"
	case "divider":
		return "---"
	case "child_page":
		return "[page] " + content.Title + " (page id: " + block.ID + ")"
	case "child_database":
		return "[database] " + content.Title + " (database id: " + block.ID + ")"
	case "table_row":
		cells := make([]string, 0, len(content.Cells))
		for _, cell := range content.Cells {
			cells = append(cells, strings.TrimSpace(plainText(cell)))
		}
		return "| " + strings.Join(cells, " | ") + " |"
	case "bookmark":
		return "[bookmark] " + content.URL
	case "image", "file", "video", "pdf":
		link := content.External.URL
		if link == "" {
			link = content.File.URL
		}
		return "[" + block.Type + "] " + link
	case "equation":
		return "[equation] " + content.Expression
	default:
		if text != "" {
			return text
		}
		return "[" + block.Type + "]"
	}
}

func truncateText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !isRuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}

func isRuneStart(b byte) bool {
	return b&0xC0 != 0x80
}
