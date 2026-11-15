package eval

import (
	"cmp"

	"github.com/vladar107/home-ai-assistant/internal/testutil/notionsim"
)

// seedNotionTables shares each table of the case's notion section with the world's token: the data
// source with its typed columns, and a page for every row.
func seedNotionTables(sim *notionsim.Simulator, tables []notionDataSource, edited string) {
	for _, table := range tables {
		title := cmp.Or(table.TitleProperty, defaultNotionTitleProperty)
		schema := map[string]any{}
		types := map[string]notionProperty{}
		for _, property := range table.Properties {
			schema[property.Name] = notionSchema(property)
			types[property.Name] = property
		}
		sim.ShareDataSource(notionToken, notionsim.DataSource{
			ID: table.ID, Title: table.Title, URL: "https://notion.example.test/" + table.ID, TitleName: title, Properties: schema,
		})
		for _, row := range table.Rows {
			values := map[string]any{}
			for _, property := range table.Properties {
				values[property.Name] = notionValue(property, row.Values[property.Name])
			}
			sim.SharePage(notionToken, notionsim.Page{
				ID: row.ID, Title: row.Title, TitleName: title, URL: "https://notion.example.test/" + row.ID,
				LastEdited: edited, DataSourceID: table.ID, Properties: values,
			})
		}
	}
}

func notionSchema(property notionProperty) map[string]any {
	switch property.Type {
	case "select", "status":
		schema := notionsim.SchemaPropertyWithOptions(property.Type, property.Options...)
		schema["name"] = property.Name
		return schema
	case "number":
		return map[string]any{"id": "prop-" + property.Name, "name": property.Name, "type": "number", "number": map[string]any{"format": "number"}}
	}
	schema := notionsim.SchemaProperty(property.Type)
	schema["id"], schema["name"] = "prop-"+property.Name, property.Name
	return schema
}

// notionValue is a row's cell in the shape Notion returns, empty when the case gave no value.
func notionValue(property notionProperty, value any) map[string]any {
	id := "prop-" + property.Name
	cell := map[string]any{"id": id, "type": property.Type}
	switch property.Type {
	case "date":
		cell["date"] = nil
		if start, ok := value.(string); ok {
			cell["date"] = map[string]any{"start": start, "end": nil, "time_zone": nil}
		}
	case "number":
		cell["number"] = value
	case "select", "status":
		cell[property.Type] = nil
		if name, ok := value.(string); ok {
			cell[property.Type] = map[string]any{"id": "opt-" + name, "name": name, "color": "default"}
		}
	case "rich_text":
		text, _ := value.(string)
		cell["rich_text"] = notionsim.RichTextProperty(text)["rich_text"]
	case "checkbox":
		checked, _ := value.(bool)
		cell["checkbox"] = checked
	}
	return cell
}
