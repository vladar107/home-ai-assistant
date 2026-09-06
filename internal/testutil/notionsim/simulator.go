// Package notionsim is a loopback simulator of the Notion API endpoints the connector uses. It
// models tokens with the pages and data sources shared with each of them, records every request
// without its bearer token in logs, and can fail the next request on demand.
package notionsim

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const Version = "2026-03-11"

type Token struct {
	Kind          string
	BotID         string
	BotName       string
	WorkspaceID   string
	WorkspaceName string
}

type Page struct {
	ID           string
	Title        string
	URL          string
	LastEdited   string
	DataSourceID string
	Properties   map[string]any
	Blocks       []map[string]any
}

type DataSource struct {
	ID         string
	Title      string
	URL        string
	Properties map[string]any
}

// Database is a container of data sources, as Notion has modelled databases since API version
// 2025-09-03. Its ID is what a child_database block in page content carries.
type Database struct {
	ID          string
	Title       string
	IsInline    bool
	DataSources []DataSourceRef
}

type DataSourceRef struct {
	ID   string
	Name string
}

// Request is one recorded call with the bearer token that made it and the decoded JSON body.
type Request struct {
	Method  string
	Path    string
	Token   string
	Version string
	Body    map[string]any
}

type failure struct {
	status     int
	retryAfter string
}

type Simulator struct {
	server      *httptest.Server
	mu          sync.Mutex
	tokens      map[string]Token
	pages       []*Page
	pageShares  map[string][]string
	dataSources []*DataSource
	dsShares    map[string][]string
	databases   []*Database
	dbShares    map[string][]string
	requests    []Request
	failures    []failure
	created     int
}

func New(t *testing.T) *Simulator {
	t.Helper()
	s := &Simulator{tokens: map[string]Token{}, pageShares: map[string][]string{}, dsShares: map[string][]string{}, dbShares: map[string][]string{}}
	s.server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.server.Close)
	return s
}

func (s *Simulator) URL() string          { return s.server.URL }
func (s *Simulator) Client() *http.Client { return s.server.Client() }

func (s *Simulator) AddToken(token string, info Token) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if info.BotID == "" {
		info.BotID = "bot-" + info.BotName
	}
	s.tokens[token] = info
}

func (s *Simulator) SharePage(token string, page Page) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := page
	s.pages = append(s.pages, &copied)
	s.pageShares[page.ID] = append(s.pageShares[page.ID], token)
}

func (s *Simulator) ShareDataSource(token string, dataSource DataSource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := dataSource
	s.dataSources = append(s.dataSources, &copied)
	s.dsShares[dataSource.ID] = append(s.dsShares[dataSource.ID], token)
}

// FailNext makes the next request answer with the status (and Retry-After when given) before
// normal handling resumes.
// ShareDatabase makes a database container reachable by the token. Its data sources must be
// shared separately, the way Notion shares the rows rather than the container.
func (s *Simulator) ShareDatabase(token string, database Database) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copied := database
	s.databases = append(s.databases, &copied)
	s.dbShares[database.ID] = append(s.dbShares[database.ID], token)
}

func (s *Simulator) FailNext(status int, retryAfter string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = append(s.failures, failure{status: status, retryAfter: retryAfter})
}

func (s *Simulator) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

func (s *Simulator) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	var body map[string]any
	if r.Body != nil {
		data, _ := io.ReadAll(r.Body)
		if len(data) > 0 {
			_ = json.Unmarshal(data, &body)
		}
	}
	s.mu.Lock()
	s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Token: token, Version: r.Header.Get("Notion-Version"), Body: body})
	var pending *failure
	if len(s.failures) > 0 {
		pending = &s.failures[0]
		s.failures = s.failures[1:]
	}
	info, known := s.tokens[token]
	s.mu.Unlock()

	if pending != nil {
		if pending.retryAfter != "" {
			w.Header().Set("Retry-After", pending.retryAfter)
		}
		writeError(w, pending.status, "simulated_failure", "synthetic-notion-error-text")
		return
	}
	if r.Header.Get("Notion-Version") != Version {
		writeError(w, http.StatusBadRequest, "missing_version", "Notion-Version header failed validation")
		return
	}
	if !known {
		writeError(w, http.StatusUnauthorized, "unauthorized", "API token is invalid. synthetic-notion-error-text")
		return
	}

	segments := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/users/me":
		writeJSON(w, botObject(info))
	case r.Method == http.MethodPost && r.URL.Path == "/v1/search":
		s.search(w, token, body)
	case r.Method == http.MethodGet && len(segments) == 3 && segments[1] == "pages":
		page := s.sharedPage(token, segments[2])
		if page == nil {
			writeError(w, http.StatusNotFound, "object_not_found", "Could not find page. synthetic-notion-error-text")
			return
		}
		writeJSON(w, pageObject(page))
	case r.Method == http.MethodGet && len(segments) == 4 && segments[1] == "blocks" && segments[3] == "children":
		page := s.sharedPage(token, segments[2])
		if page == nil {
			writeError(w, http.StatusNotFound, "object_not_found", "Could not find block. synthetic-notion-error-text")
			return
		}
		s.blockChildren(w, page, r.URL.Query())
	case r.Method == http.MethodPost && r.URL.Path == "/v1/pages":
		s.createPage(w, token, body)
	case r.Method == http.MethodPatch && len(segments) == 3 && segments[1] == "pages":
		page := s.sharedPage(token, segments[2])
		if page == nil {
			writeError(w, http.StatusNotFound, "object_not_found", "Could not find page. synthetic-notion-error-text")
			return
		}
		s.updatePage(w, page, body)
	case r.Method == http.MethodPatch && len(segments) == 4 && segments[1] == "blocks" && segments[3] == "children":
		page := s.sharedPage(token, segments[2])
		if page == nil {
			writeError(w, http.StatusNotFound, "object_not_found", "Could not find block. synthetic-notion-error-text")
			return
		}
		s.appendBlocks(w, page, body)
	case r.Method == http.MethodGet && len(segments) == 3 && segments[1] == "databases":
		database := s.sharedDatabase(token, segments[2])
		if database == nil {
			writeError(w, http.StatusNotFound, "object_not_found", "Could not find database. synthetic-notion-error-text")
			return
		}
		writeJSON(w, databaseObject(database))
	case r.Method == http.MethodPost && len(segments) == 4 && segments[1] == "data_sources" && segments[3] == "query":
		dataSource := s.sharedDataSource(token, segments[2])
		if dataSource == nil {
			writeError(w, http.StatusNotFound, "object_not_found", "Could not find data source. synthetic-notion-error-text")
			return
		}
		s.query(w, token, dataSource, body)
	default:
		writeError(w, http.StatusNotFound, "invalid_request_url", "unknown path")
	}
}

func (s *Simulator) sharedPage(token, id string) *Page {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, page := range s.pages {
		if page.ID == id && contains(s.pageShares[id], token) {
			return page
		}
	}
	return nil
}

func (s *Simulator) sharedDataSource(token, id string) *DataSource {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, dataSource := range s.dataSources {
		if dataSource.ID == id && contains(s.dsShares[id], token) {
			return dataSource
		}
	}
	return nil
}

func (s *Simulator) sharedDatabase(token, id string) *Database {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, database := range s.databases {
		if database.ID == id && contains(s.dbShares[id], token) {
			return database
		}
	}
	return nil
}

func (s *Simulator) search(w http.ResponseWriter, token string, body map[string]any) {
	query, _ := body["query"].(string)
	objectFilter := ""
	if filter, ok := body["filter"].(map[string]any); ok {
		objectFilter, _ = filter["value"].(string)
	}
	var results []any
	s.mu.Lock()
	for _, page := range s.pages {
		if contains(s.pageShares[page.ID], token) && (objectFilter == "" || objectFilter == "page") && matches(page.Title, query) {
			results = append(results, pageObject(page))
		}
	}
	for _, dataSource := range s.dataSources {
		if contains(s.dsShares[dataSource.ID], token) && (objectFilter == "" || objectFilter == "data_source") && matches(dataSource.Title, query) {
			results = append(results, dataSourceObject(dataSource))
		}
	}
	s.mu.Unlock()
	writeList(w, results, body)
}

func (s *Simulator) query(w http.ResponseWriter, token string, dataSource *DataSource, body map[string]any) {
	var pages []*Page
	s.mu.Lock()
	for _, page := range s.pages {
		if page.DataSourceID == dataSource.ID && contains(s.pageShares[page.ID], token) {
			pages = append(pages, page)
		}
	}
	s.mu.Unlock()
	if rawFilter, present := body["filter"]; present {
		filter, ok := rawFilter.(map[string]any)
		property, _ := filter["property"].(string)
		if !ok || property == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "body.filter should be a valid filter. synthetic-notion-error-text")
			return
		}
		var kept []*Page
		for _, page := range pages {
			if filterMatches(page, filter) {
				kept = append(kept, page)
			}
		}
		pages = kept
	}
	if rawSorts, present := body["sorts"]; present {
		sorts, ok := rawSorts.([]any)
		if !ok {
			writeError(w, http.StatusBadRequest, "validation_error", "body.sorts should be an array. synthetic-notion-error-text")
			return
		}
		if len(sorts) > 0 {
			sort.SliceStable(pages, func(i, j int) bool {
				first, _ := sorts[0].(map[string]any)
				direction, _ := first["direction"].(string)
				less := pages[i].Title < pages[j].Title
				if timestamp, _ := first["timestamp"].(string); timestamp == "last_edited_time" {
					less = pages[i].LastEdited < pages[j].LastEdited
				}
				if direction == "descending" {
					return !less
				}
				return less
			})
		}
	}
	results := make([]any, 0, len(pages))
	for _, page := range pages {
		results = append(results, pageObject(page))
	}
	writeList(w, results, body)
}

// Pages returns a snapshot of every page the simulator holds, in creation order.
func (s *Simulator) Pages() []Page {
	s.mu.Lock()
	defer s.mu.Unlock()
	pages := make([]Page, 0, len(s.pages))
	for _, page := range s.pages {
		pages = append(pages, *page)
	}
	return pages
}

func (s *Simulator) createPage(w http.ResponseWriter, token string, body map[string]any) {
	parent, _ := body["parent"].(map[string]any)
	properties, _ := body["properties"].(map[string]any)
	if parent == nil || properties == nil {
		writeError(w, http.StatusBadRequest, "validation_error", "body.parent and body.properties are required. synthetic-notion-error-text")
		return
	}
	page := &Page{Properties: map[string]any{}, LastEdited: "2026-09-06T12:00:00.000Z"}
	if dataSourceID, _ := parent["data_source_id"].(string); dataSourceID != "" {
		if s.sharedDataSource(token, dataSourceID) == nil {
			writeError(w, http.StatusNotFound, "object_not_found", "Could not find data source. synthetic-notion-error-text")
			return
		}
		page.DataSourceID = dataSourceID
	} else if pageID, _ := parent["page_id"].(string); pageID != "" {
		if s.sharedPage(token, pageID) == nil {
			writeError(w, http.StatusNotFound, "object_not_found", "Could not find page. synthetic-notion-error-text")
			return
		}
	} else {
		writeError(w, http.StatusBadRequest, "validation_error", "body.parent must name a page or data source. synthetic-notion-error-text")
		return
	}
	for name, value := range properties {
		property, ok := value.(map[string]any)
		if !ok {
			writeError(w, http.StatusBadRequest, "validation_error", "body.properties."+name+" must be an object. synthetic-notion-error-text")
			return
		}
		if parts, isTitle := property["title"].([]any); isTitle {
			page.Title = inputTextPlain(parts)
			continue
		}
		page.Properties[name] = normalizeInput(name, property)
	}
	if children, _ := body["children"].([]any); len(children) > 0 {
		for _, child := range children {
			if block, ok := child.(map[string]any); ok {
				page.Blocks = append(page.Blocks, block)
			}
		}
	}
	s.mu.Lock()
	s.created++
	page.ID = fmt.Sprintf("page-created-%d", s.created)
	page.URL = "https://notion.so/" + page.ID
	s.pages = append(s.pages, page)
	s.pageShares[page.ID] = append(s.pageShares[page.ID], token)
	s.mu.Unlock()
	writeJSON(w, pageObject(page))
}

func (s *Simulator) updatePage(w http.ResponseWriter, page *Page, body map[string]any) {
	properties, _ := body["properties"].(map[string]any)
	if properties == nil {
		writeError(w, http.StatusBadRequest, "validation_error", "body.properties is required. synthetic-notion-error-text")
		return
	}
	s.mu.Lock()
	for name, value := range properties {
		property, ok := value.(map[string]any)
		if !ok {
			s.mu.Unlock()
			writeError(w, http.StatusBadRequest, "validation_error", "body.properties."+name+" must be an object. synthetic-notion-error-text")
			return
		}
		if parts, isTitle := property["title"].([]any); isTitle {
			page.Title = inputTextPlain(parts)
			continue
		}
		if page.Properties == nil {
			page.Properties = map[string]any{}
		}
		page.Properties[name] = normalizeInput(name, property)
	}
	page.LastEdited = "2026-09-06T12:30:00.000Z"
	s.mu.Unlock()
	writeJSON(w, pageObject(page))
}

func (s *Simulator) appendBlocks(w http.ResponseWriter, page *Page, body map[string]any) {
	children, _ := body["children"].([]any)
	if len(children) == 0 || len(children) > 100 {
		writeError(w, http.StatusBadRequest, "validation_error", "body.children must hold 1 to 100 blocks. synthetic-notion-error-text")
		return
	}
	var results []any
	s.mu.Lock()
	for _, child := range children {
		block, ok := child.(map[string]any)
		if !ok {
			s.mu.Unlock()
			writeError(w, http.StatusBadRequest, "validation_error", "body.children items must be blocks. synthetic-notion-error-text")
			return
		}
		page.Blocks = append(page.Blocks, block)
		object := map[string]any{"object": "block", "id": fmt.Sprintf("%s-block-%d", page.ID, len(page.Blocks)-1), "has_children": false}
		for key, value := range block {
			object[key] = value
		}
		results = append(results, object)
	}
	s.mu.Unlock()
	writeList(w, results, nil)
}

// normalizeInput turns a property value input (which may omit "type") into the response shape.
func normalizeInput(name string, property map[string]any) map[string]any {
	normalized := map[string]any{"id": "prop-" + name}
	for key, value := range property {
		if key == "type" || key == "id" {
			continue
		}
		normalized["type"] = key
		if parts, ok := value.([]any); ok && (key == "rich_text" || key == "title") {
			normalized[key] = inputRichText(parts)
			continue
		}
		normalized[key] = value
	}
	return normalized
}

func inputRichText(parts []any) []any {
	converted := make([]any, 0, len(parts))
	for _, part := range parts {
		item, _ := part.(map[string]any)
		text, _ := item["text"].(map[string]any)
		content, _ := text["content"].(string)
		if content == "" {
			content, _ = item["plain_text"].(string)
		}
		converted = append(converted, richText(content))
	}
	return converted
}

func inputTextPlain(parts []any) string {
	var builder strings.Builder
	for _, part := range inputRichText(parts) {
		item, _ := part.(map[string]any)
		text, _ := item["plain_text"].(string)
		builder.WriteString(text)
	}
	return builder.String()
}

func (s *Simulator) blockChildren(w http.ResponseWriter, page *Page, query url.Values) {
	results := make([]any, 0, len(page.Blocks))
	for index, block := range page.Blocks {
		object := map[string]any{"object": "block", "id": fmt.Sprintf("%s-block-%d", page.ID, index), "has_children": false}
		for key, value := range block {
			object[key] = value
		}
		results = append(results, object)
	}
	body := map[string]any{}
	if cursor := query.Get("start_cursor"); cursor != "" {
		body["start_cursor"] = cursor
	}
	if size := query.Get("page_size"); size != "" {
		if parsed, err := strconv.Atoi(size); err == nil {
			body["page_size"] = float64(parsed)
		}
	}
	writeList(w, results, body)
}

// filterMatches supports equality filters on status, select, checkbox, and rich_text/title
// values, which is enough to prove pass-through; anything else matches nothing.
func filterMatches(page *Page, filter map[string]any) bool {
	property, _ := filter["property"].(string)
	value, ok := page.Properties[property].(map[string]any)
	if !ok {
		return false
	}
	for _, kind := range []string{"status", "select", "checkbox", "rich_text", "title"} {
		condition, present := filter[kind].(map[string]any)
		if !present {
			continue
		}
		expected := condition["equals"]
		switch kind {
		case "status", "select":
			option, _ := value[kind].(map[string]any)
			return option["name"] == expected
		case "checkbox":
			return value["checkbox"] == expected
		default:
			parts, _ := value[kind].([]any)
			return richTextPlain(parts) == expected
		}
	}
	return false
}

func richTextPlain(parts []any) string {
	var builder strings.Builder
	for _, part := range parts {
		if item, ok := part.(map[string]any); ok {
			text, _ := item["plain_text"].(string)
			builder.WriteString(text)
		}
	}
	return builder.String()
}

func writeList(w http.ResponseWriter, results []any, body map[string]any) {
	offset := 0
	if cursor, _ := body["start_cursor"].(string); cursor != "" {
		parsed, err := strconv.Atoi(strings.TrimPrefix(cursor, "cursor-"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "validation_error", "start_cursor is invalid")
			return
		}
		offset = parsed
	}
	size := 100
	if requested, ok := body["page_size"].(float64); ok {
		if requested < 1 || requested > 100 {
			writeError(w, http.StatusBadRequest, "validation_error", "page_size must be between 1 and 100")
			return
		}
		size = int(requested)
	}
	if offset > len(results) {
		offset = len(results)
	}
	end := offset + size
	if end > len(results) {
		end = len(results)
	}
	response := map[string]any{
		"object":      "list",
		"results":     results[offset:end],
		"has_more":    end < len(results),
		"next_cursor": nil,
	}
	if end < len(results) {
		response["next_cursor"] = fmt.Sprintf("cursor-%d", end)
	}
	writeJSON(w, response)
}

func botObject(info Token) map[string]any {
	owner := map[string]any{"type": "workspace", "workspace": true}
	workspaceName := any(info.WorkspaceName)
	if info.Kind == "personal" {
		owner = map[string]any{"type": "user", "user": map[string]any{"object": "user", "id": "user-owner", "type": "person", "person": map[string]any{"email": "owner@example.com"}}}
		workspaceName = nil
	}
	return map[string]any{
		"object": "user", "id": info.BotID, "type": "bot", "name": info.BotName,
		"bot": map[string]any{"owner": owner, "workspace_id": info.WorkspaceID, "workspace_name": workspaceName, "workspace_limits": map[string]any{"max_file_upload_size_in_bytes": 5242880}},
	}
}

func pageObject(page *Page) map[string]any {
	properties := map[string]any{"Name": TitleProperty(page.Title)}
	for name, value := range page.Properties {
		properties[name] = value
	}
	parent := map[string]any{"type": "workspace", "workspace": true}
	if page.DataSourceID != "" {
		parent = map[string]any{"type": "data_source_id", "data_source_id": page.DataSourceID}
	}
	return map[string]any{
		"object": "page", "id": page.ID, "url": page.URL, "created_time": page.LastEdited, "last_edited_time": page.LastEdited,
		"in_trash": false, "is_archived": false, "parent": parent, "properties": properties,
	}
}

func dataSourceObject(dataSource *DataSource) map[string]any {
	properties := map[string]any{"Name": map[string]any{"id": "title", "name": "Name", "type": "title", "title": map[string]any{}}}
	for name, value := range dataSource.Properties {
		properties[name] = value
	}
	return map[string]any{
		"object": "data_source", "id": dataSource.ID, "url": dataSource.URL, "last_edited_time": "2026-09-01T00:00:00.000Z",
		"title": []any{richText(dataSource.Title)}, "parent": map[string]any{"type": "database_id", "database_id": "db-" + dataSource.ID}, "properties": properties,
	}
}

func databaseObject(database *Database) map[string]any {
	sources := make([]any, 0, len(database.DataSources))
	for _, source := range database.DataSources {
		sources = append(sources, map[string]any{"id": source.ID, "name": source.Name})
	}
	return map[string]any{
		"object":       "database",
		"id":           database.ID,
		"title":        []any{richText(database.Title)},
		"is_inline":    database.IsInline,
		"data_sources": sources,
		"url":          "https://notion.so/" + database.ID,
	}
}

func richText(text string) map[string]any {
	return map[string]any{"type": "text", "text": map[string]any{"content": text, "link": nil}, "plain_text": text, "href": nil,
		"annotations": map[string]any{"bold": false, "italic": false, "strikethrough": false, "underline": false, "code": false, "color": "default"}}
}

// TitleProperty and the helpers below build Notion-shaped property values for test fixtures.
func TitleProperty(text string) map[string]any {
	return map[string]any{"id": "title", "type": "title", "title": []any{richText(text)}}
}

func RichTextProperty(text string) map[string]any {
	return map[string]any{"id": "rt", "type": "rich_text", "rich_text": []any{richText(text)}}
}

func StatusProperty(name string) map[string]any {
	return map[string]any{"id": "st", "type": "status", "status": map[string]any{"id": "opt-" + name, "name": name, "color": "blue"}}
}

func DateProperty(start string) map[string]any {
	return map[string]any{"id": "dt", "type": "date", "date": map[string]any{"start": start, "end": nil, "time_zone": nil}}
}

func CheckboxProperty(checked bool) map[string]any {
	return map[string]any{"id": "cb", "type": "checkbox", "checkbox": checked}
}

func SchemaProperty(kind string) map[string]any {
	return map[string]any{"id": "prop-" + kind, "name": kind, "type": kind, kind: map[string]any{}}
}

func Paragraph(text string) map[string]any {
	return map[string]any{"type": "paragraph", "paragraph": map[string]any{"rich_text": []any{richText(text)}, "color": "default"}}
}

func Heading(level int, text string) map[string]any {
	kind := fmt.Sprintf("heading_%d", level)
	return map[string]any{"type": kind, kind: map[string]any{"rich_text": []any{richText(text)}, "color": "default", "is_toggleable": false}}
}

func Bullet(text string) map[string]any {
	return map[string]any{"type": "bulleted_list_item", "bulleted_list_item": map[string]any{"rich_text": []any{richText(text)}, "color": "default"}}
}

// ChildDatabase is an inline table inside a page. Its block ID is the database ID, which is what
// the connector must surface for the table's rows to be reachable.
func ChildDatabase(databaseID, title string) map[string]any {
	return map[string]any{"type": "child_database", "id": databaseID, "child_database": map[string]any{"title": title}}
}

func ToDo(text string, checked bool) map[string]any {
	return map[string]any{"type": "to_do", "to_do": map[string]any{"rich_text": []any{richText(text)}, "checked": checked, "color": "default"}}
}

func writeJSON(w http.ResponseWriter, value any) {
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "error", "status": status, "code": code, "message": message})
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func matches(title, query string) bool {
	return query == "" || strings.Contains(strings.ToLower(title), strings.ToLower(query))
}
